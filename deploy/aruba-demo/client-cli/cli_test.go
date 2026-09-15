package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/Arubacloud/sdk-go/pkg/aruba"
	"github.com/Arubacloud/sdk-go/pkg/types"
	"github.com/spf13/cobra"
)

// The tenant namespace is what every SECA object for a tenant lands in. Get it
// wrong and the gateway writes to a namespace nothing reads. This is the value
// `printf %s ARU-348095 | openssl dgst -sha3-224` produces, which is what
// install.sh used and what framework/backend/kubernetes.ComputeNamespace computes.
func TestTenantNamespace(t *testing.T) {
	const want = "f6517c77a825871ce5e9fdf53dbc516bd510b0a94b5e806e10e008b6"
	if got := tenantNamespace("ARU-348095"); got != want {
		t.Fatalf("tenantNamespace = %s, want %s", got, want)
	}
}

// Every @PLACEHOLDER@ must be substituted: one left behind is applied verbatim
// and produces an object nothing matches.
func TestEmbeddedManifestsHaveNoLeftoverPlaceholders(t *testing.T) {
	c := &config{tenant: "ARU-348095", region: "itbg-bergamo", location: "ITBG-Bergamo", regionalURL: "http://127.0.0.1:3001"}

	catalog, err := c.manifest("catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := c.manifest("tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}

	docs := [][]byte{
		substitute(catalog, map[string]string{"@REGION@": c.region, "@REGIONAL_URL@": c.regionalURL}),
		substitute(tenant, map[string]string{
			"@TENANT@": c.tenant, "@TENANT_NS@": tenantNamespace(c.tenant),
			"@ARUBA_LOCATION@": c.location, "@SUBJECT@": "d0d0-beef",
		}),
	}
	// Checked on the decoded objects, not the raw text: the templates' own prose
	// mentions "@PLACEHOLDERS@" in comments, which YAML drops.
	placeholder := regexp.MustCompile(`@[A-Z_]+@`)
	for _, doc := range docs {
		objs, err := decodeYAML(doc)
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) == 0 {
			t.Fatal("decoded no objects")
		}
		for _, o := range objs {
			if o.GetKind() == "" || o.GetName() == "" {
				t.Fatalf("object with no kind or name: %v", o.Object)
			}
			blob, err := json.Marshal(o.Object)
			if err != nil {
				t.Fatal(err)
			}
			if m := placeholder.Find(blob); m != nil {
				t.Errorf("%s/%s still carries %s", o.GetKind(), o.GetName(), m)
			}
		}
	}
}

// The embedded copies must stay identical to the originals they were copied from,
// modulo the three-line header that says so.
func TestEmbeddedManifestsMatchOriginals(t *testing.T) {
	for _, name := range []string{"catalog.yaml", "tenant.yaml"} {
		original, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Skipf("original %s not available: %v", name, err)
		}
		embedded, err := embeddedManifests.ReadFile("manifests/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(embedded), string(original)) {
			t.Errorf("manifests/%s has drifted from ../%s — re-copy it", name, name)
		}
	}
}

// jwtPublicKey has to turn a JWKS x5c CERTIFICATE into a PEM PUBLIC KEY: the
// gateway verifies against a bare public key, and handing it a certificate fails
// at startup rather than at request time.
func TestJWTPublicKeyFromJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/protocol/openid-connect/certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{
			map[string]any{"use": "enc", "alg": "RSA-OAEP"},
			map[string]any{"use": "sig", "alg": "RS256", "x5c": []string{base64.StdEncoding.EncodeToString(der)}},
		}})
	}))
	defer srv.Close()

	got, err := jwtPublicKey(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(got)
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("not a PEM PUBLIC KEY: %q", got)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(pub) {
		t.Fatal("the PEM does not carry the signing key")
	}
}

// The RoleAssignment must name the token's `sub`, which for a client_credentials
// grant is the service account's user id and cannot be guessed.
func TestTokenSubject(t *testing.T) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	tok := enc(map[string]string{"alg": "RS256"}) + "." +
		enc(map[string]string{"sub": "service-account-42", "scope": "email"}) + ".sig"
	got, err := tokenSubject(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != "service-account-42" {
		t.Fatalf("sub = %q", got)
	}
	if _, err := tokenSubject("not-a-jwt"); err == nil {
		t.Fatal("expected an error for a non-JWT")
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# a comment\nexport ARUBA_CLIENT_ID=abc\nARUBA_CLIENT_SECRET=\"s3cr3t\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := readEnvFile(path)
	if env["ARUBA_CLIENT_ID"] != "abc" || env["ARUBA_CLIENT_SECRET"] != "s3cr3t" {
		t.Fatalf("parsed %v", env)
	}
}

// The cluster flags are plain strings cast straight to the SDK's enum types, so a
// typo in a default reaches Aruba as an invalid value and fails at create time —
// after the VPC and subnet have already been billed. Pin them to the SDK constants.
func TestClusterSpecDefaultsAreValidSDKEnums(t *testing.T) {
	c := &config{}
	cmd := &cobra.Command{}
	c.addClusterFlags(cmd)
	c.clusterName = "ecp-demo"
	c.location = string(aruba.RegionITBGBergamo)

	spec, err := c.clusterSpec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.region != aruba.RegionITBGBergamo {
		t.Errorf("region = %q", spec.region)
	}
	if spec.zone != aruba.ZoneITBG1 {
		t.Errorf("zone = %q, want %q", spec.zone, aruba.ZoneITBG1)
	}
	if spec.k8sVersion != aruba.KubernetesVersion1341 {
		t.Errorf("k8sVersion = %q, want %q", spec.k8sVersion, aruba.KubernetesVersion1341)
	}
	if spec.nodeFlavor != aruba.NodePoolInstanceK4A8 {
		t.Errorf("nodeFlavor = %q, want %q", spec.nodeFlavor, aruba.NodePoolInstanceK4A8)
	}

	c.nodeCount = 0
	if _, err := c.clusterSpec(); err == nil {
		t.Error("expected --nodes 0 to be rejected")
	}
}

// The Aruba API validates every tag against this regex and the SDK does not check
// client-side, so an invalid tag is only rejected once the request lands — as a
// bare "One or more validation errors occurred." naming no field. A tag of three
// characters ("ecp") failed every single create.
func TestDemoTagsAreValid(t *testing.T) {
	arubaTag := regexp.MustCompile(`^[A-Za-z0-9-]{4,30}$`)
	if len(demoTags) == 0 {
		t.Fatal("no tags")
	}
	for _, tag := range demoTags {
		if !arubaTag.MatchString(tag) {
			t.Errorf("tag %q is rejected by the Aruba API: it must match %s", tag, arubaTag)
		}
	}
}

// A 400 whose body names the offending field must say so: the SDK's own
// HTTPError.Error() prints only the title, which is why the first failure was
// undebuggable from the CLI's output.
func TestArubaErrSurfacesFieldErrors(t *testing.T) {
	title := "One or more validation errors occurred."
	err := arubaErr(&aruba.HTTPError{
		StatusCode: 400,
		ErrResp: &types.ErrorResponse{
			Title:  &title,
			Errors: []types.ValidationError{{Field: "metadata.tags[0]", Message: "invalid format"}},
		},
	})
	if !strings.Contains(err.Error(), "metadata.tags[0]") || !strings.Contains(err.Error(), "invalid format") {
		t.Fatalf("field errors dropped: %s", err)
	}

	// Anything that is not an Aruba HTTP error must come back untouched.
	plain := errors.New("connection refused")
	if got := arubaErr(plain); got != plain {
		t.Fatalf("non-HTTP error rewritten: %v", got)
	}
}
