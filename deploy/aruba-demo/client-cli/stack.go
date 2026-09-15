package main

import (
	"context"
	"crypto/sha3"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed manifests/catalog.yaml manifests/tenant.yaml
var embeddedManifests embed.FS

const (
	ecpRepo     = "https://eu-sovereign-cloud.github.io/ecp/"
	arubaRepo   = "https://arubacloud.github.io/helm-charts"
	ghcrServer  = "ghcr.io"
	gatewayGlob = "ecp-gateway-global"
	gatewayReg  = "ecp-gateway-regional"
)

// tenantNamespace is framework/backend/kubernetes.ComputeNamespace for a
// tenant-scoped (workspace-less) resource: hex(sha3-224("<tenant>")).
func tenantNamespace(tenant string) string {
	h := sha3.New224()
	fmt.Fprintf(h, "%s", tenant)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// manifest returns one of the two catalog templates, from --manifests-dir when
// that is set and from the embedded copy otherwise.
func (c *config) manifest(name string) ([]byte, error) {
	if c.manifestsDir != "" {
		return os.ReadFile(filepath.Join(c.manifestsDir, name))
	}
	return embeddedManifests.ReadFile("manifests/" + name)
}

func substitute(in []byte, pairs map[string]string) []byte {
	out := string(in)
	for k, v := range pairs {
		out = strings.ReplaceAll(out, k, v)
	}
	return []byte(out)
}

// install is the install.sh replica: everything the demo stack needs, in one
// cluster, idempotently. Re-running it after changing a flag is how you
// reconfigure — every step is an upgrade-or-install or a server-side apply.
func install(ctx context.Context, c *config) error {
	k, err := newKube(c.kubeconfig)
	if err != nil {
		return err
	}
	h := &helmStack{kubeconfig: c.kubeconfig}
	realm := realmURL(c.idp, c.realm)

	logf("context: kubeconfig %s | tenant %s | region %s (%s)", c.kubeconfig, c.tenant, c.region, c.location)

	// 1. namespaces + the image pull secret. The ghcr.io/eu-sovereign-cloud/ecp/*
	// packages are private: without this every ECP pod sits in ImagePullBackOff.
	// The credential is a gh CLI token and it expires, so a cluster that stops
	// pulling needs this re-run.
	ghcrUser, ghcrToken, err := c.ghcrCredentials()
	if err != nil {
		return err
	}
	for _, ns := range []string{c.ecpNS, c.arubaNS} {
		if err := k.ensureNamespace(ctx, ns); err != nil {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
		if err := k.ensurePullSecret(ctx, ns, c.pullSecret, ghcrServer, ghcrUser, ghcrToken); err != nil {
			return fmt.Errorf("create pull secret in %s: %w", ns, err)
		}
	}
	logf("namespaces %s, %s ready with pull secret %s", c.ecpNS, c.arubaNS, c.pullSecret)

	// 2. the JWT verification key, straight from the realm's JWKS.
	jwtPEM, err := jwtPublicKey(ctx, realm)
	if err != nil {
		return err
	}
	logf("RS256 verification key fetched from %s", realm)

	// 3. the subject the RoleAssignment grants to.
	subject := c.jwtSubject
	if subject == "" {
		token, err := mintToken(ctx, realm, c.clientID, c.clientSecret)
		if err != nil {
			return fmt.Errorf("the Aruba API client could not mint a token: %w", err)
		}
		if subject, err = tokenSubject(token); err != nil {
			return err
		}
	}
	logf("token subject: %s", subject)

	// 4. the Aruba backend.
	//
	// auth.mode=single holds ONE cluster-wide API client in the operator's config
	// Secret. That is the whole point of a single-tenant install: no Vault, no
	// per-tenant KV lookup, no vault-config Job — and therefore no initContainer
	// deadlock against --wait and no kubectl-patch field-manager conflict on the
	// second run, both of which multi/auto forces you to work around.
	//
	// multi.setup=manual is load-bearing even though the mode is single: the
	// chart's vault-auto templates are gated on `setup` ALONE, so at the default
	// setup=auto the vault-config and vault-cleanup Jobs still render, and the
	// post-install one then hangs forever waiting for a Vault that vault.enabled=false
	// never installed.
	logf("installing arubacloud-resource-operator %s (auth.mode=single)", c.operatorVersion)
	operatorValues := map[string]any{
		"config": map[string]any{
			"gateway": c.apiGateway,
			"auth": map[string]any{
				"idp":   c.idp,
				"realm": c.realm,
				"mode":  "single",
				"single": map[string]any{
					"clientId":     c.clientID,
					"clientSecret": c.clientSecret,
				},
				"multi": map[string]any{"setup": "manual"},
			},
		},
		"vault": map[string]any{"enabled": false},
	}
	if err := h.upgradeInstall(ctx, c.arubaNS, "arubacloud-resource-operator",
		chartRef{arubaRepo, "arubacloud-resource-operator", c.operatorVersion},
		operatorValues, installOpts{wait: true, timeout: c.timeout}); err != nil {
		return fmt.Errorf("install arubacloud-resource-operator: %w", err)
	}

	// 5. the ECP gateways. Helm installs a chart's crds/ only on a FIRST install
	// and never upgrades them, so on a version bump the cluster would silently
	// keep the old schemas. Applying them up front is a no-op on a fresh cluster
	// and the only thing that makes an upgrade correct.
	ecpChart, err := h.loadChart(c.ecpNS, chartRef{ecpRepo, "ecp", c.ecpVersion})
	if err != nil {
		return err
	}
	crds := crdObjects(ecpChart)
	for _, crd := range crds {
		if err := k.apply(ctx, crd, c.ecpNS); err != nil {
			return fmt.Errorf("apply CRDs: %w", err)
		}
	}
	logf("applied %d CRD files from ecp %s", len(crds), c.ecpVersion)

	logf("installing ecp %s", c.ecpVersion)
	ecpValues := map[string]any{
		"gatewayGlobal": map[string]any{"enabled": true},
		// NOT c.region. This is what the regional gateway STAMPS on every resource
		// it writes and every aruba converter forwards verbatim to the CMP as the
		// location — Aruba spells it "ITBG-Bergamo" and rejects any other case.
		"gatewayRegional": map[string]any{"enabled": true, "region": c.location},
		"auth": map[string]any{
			"enabled": true,
			"plugin":  "jwt",
			"jwt":     map[string]any{"signingMethod": "RS256", "key": string(jwtPEM)},
			"authz": map[string]any{
				"enabled": true,
				"impl":    "cached",
				// The region resource is tenant-less by spec, so there is no tenant
				// namespace to read policy from: serve it authn-only.
				"skipProviders": "seca.region",
			},
		},
		"imagePullSecrets": []any{map[string]any{"name": c.pullSecret}},
	}
	if c.gatewayImageTag != "" {
		ecpValues["gatewayGlobal"].(map[string]any)["image"] = map[string]any{
			"repository": c.gatewayImageGlobal, "tag": c.gatewayImageTag,
		}
		ecpValues["gatewayRegional"].(map[string]any)["image"] = map[string]any{
			"repository": c.gatewayImageRegional, "tag": c.gatewayImageTag,
		}
	}
	if err := h.upgradeInstall(ctx, c.ecpNS, "ecp",
		chartRef{ecpRepo, "ecp", c.ecpVersion},
		ecpValues, installOpts{wait: true, timeout: c.timeout}); err != nil {
		return fmt.Errorf("install ecp: %w", err)
	}

	// 6. the aruba delegator. plugin=aruba selects both the delegator-aruba image
	// and the RBAC for exactly the controllers that plugin loads.
	logf("installing ecp-delegator %s (plugin=aruba)", c.ecpVersion)
	if err := h.upgradeInstall(ctx, c.arubaNS, "ecp-delegator",
		chartRef{ecpRepo, "ecp-delegator", c.ecpVersion},
		map[string]any{
			"plugin":           "aruba",
			"imagePullSecrets": []any{map[string]any{"name": c.pullSecret}},
		}, installOpts{wait: true, timeout: c.timeout}); err != nil {
		return fmt.Errorf("install ecp-delegator: %w", err)
	}

	// 7. the catalog and the tenant's RBAC.
	if err := applyCatalog(ctx, c, k); err != nil {
		return err
	}
	if err := applyTenant(ctx, c, k, subject); err != nil {
		return err
	}

	// 8. The gateways read the JWT key file once, at startup, and the chart puts no
	// checksum annotation on the pod template — so a re-run that fetched a rotated
	// realm key would update the Secret and change nothing. Restart unconditionally.
	for _, d := range []string{gatewayGlob, gatewayReg} {
		if err := k.restartDeployment(ctx, c.ecpNS, d); err != nil {
			return fmt.Errorf("restart %s: %w", d, err)
		}
	}
	for _, d := range []string{gatewayGlob, gatewayReg} {
		if err := k.waitDeployment(ctx, c.ecpNS, d, 5*time.Minute); err != nil {
			return err
		}
	}

	logf("done. tenant namespace: %s", tenantNamespace(c.tenant))
	return nil
}

// applyCatalog writes the ONE cluster-scoped object the stack needs: the Region CR
// every tenant on this cluster shares. Re-applying it with a different regional URL
// re-points the regional API for all of them at once, which is why `tenant add`
// does not touch it.
func applyCatalog(ctx context.Context, c *config, k *kubeClient) error {
	raw, err := c.manifest("catalog.yaml")
	if err != nil {
		return err
	}
	doc := substitute(raw, map[string]string{
		"@REGION@":       c.region,
		"@REGIONAL_URL@": c.regionalURL,
	})
	if err := k.apply(ctx, doc, ""); err != nil {
		return fmt.Errorf("apply catalog.yaml: %w", err)
	}
	logf("region catalog applied: %s -> %s", c.region, c.regionalURL)
	return nil
}

// applyTenant writes the tenant's namespace, SKU catalogs, Role and RoleAssignment.
// Every object is scoped to one tenant, so applying it can never disturb another.
func applyTenant(ctx context.Context, c *config, k *kubeClient, subject string) error {
	raw, err := c.manifest("tenant.yaml")
	if err != nil {
		return err
	}
	ns := tenantNamespace(c.tenant)
	doc := substitute(raw, map[string]string{
		"@TENANT@":         c.tenant,
		"@TENANT_NS@":      ns,
		"@ARUBA_LOCATION@": c.location,
		"@SUBJECT@":        subject,
	})
	if err := k.apply(ctx, doc, ns); err != nil {
		return fmt.Errorf("apply tenant.yaml: %w", err)
	}
	logf("tenant %s applied to namespace %s", c.tenant, ns)
	return nil
}

// uninstall removes the three Helm releases. It deliberately leaves the SECA CRs,
// the CRDs and the namespaces alone: deleting the CRs is what makes the plugin
// delete the arubacloud.com CRs, which is what makes the operator reap what it
// created in the CMP. Drop those first, from Terraform or by hand.
func uninstall(ctx context.Context, c *config) error {
	h := &helmStack{kubeconfig: c.kubeconfig}
	for _, r := range []struct{ ns, name string }{
		{c.ecpNS, "ecp"},
		{c.arubaNS, "ecp-delegator"},
		{c.arubaNS, "arubacloud-resource-operator"},
	} {
		logf("uninstalling %s from %s", r.name, r.ns)
		if err := h.uninstall(r.ns, r.name, c.timeout); err != nil {
			return fmt.Errorf("uninstall %s: %w", r.name, err)
		}
	}
	return nil
}

// versions reports what is actually deployed: the Helm release metadata plus the
// container image of every Deployment, which is the honest answer when a chart
// was installed with an image override.
func versions(ctx context.Context, c *config) error {
	h := &helmStack{kubeconfig: c.kubeconfig}
	k, err := newKube(c.kubeconfig)
	if err != nil {
		return err
	}
	for _, ns := range []string{c.ecpNS, c.arubaNS} {
		fmt.Printf("\n%s\n", ns)
		rels, err := h.list(ns)
		if err != nil {
			return err
		}
		if len(rels) == 0 {
			fmt.Println("  (no helm releases)")
		}
		for _, r := range rels {
			fmt.Printf("  release %-30s chart %s-%s  app %s  rev %d  %s\n",
				r.Name, r.Chart, r.Version, r.App, r.Revision, r.Status)
		}
		images, err := k.deploymentImages(ctx, ns)
		if err != nil {
			return err
		}
		for name, imgs := range images {
			fmt.Printf("  deploy  %-30s %s\n", name, strings.Join(imgs, ", "))
		}
	}
	fmt.Printf("\ntenant %s -> namespace %s\n", c.tenant, tenantNamespace(c.tenant))
	return nil
}

// ghcrCredentials resolves the private-registry credential, defaulting to the gh
// CLI's own token — which is what the shell scripts use and what most people
// already have.
func (c *config) ghcrCredentials() (string, string, error) {
	user, token := c.ghcrUser, c.ghcrToken
	if user == "" {
		user = ghOutput("api", "user", "-q", ".login")
	}
	if token == "" {
		token = ghOutput("auth", "token")
	}
	if user == "" || token == "" {
		return "", "", fmt.Errorf("no GHCR credential: the ECP images are private, so pass --ghcr-user/--ghcr-token " +
			"or log in with `gh auth login` and `gh auth refresh -h github.com -s read:packages`")
	}
	return user, token, nil
}

func ghOutput(args ...string) string {
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
