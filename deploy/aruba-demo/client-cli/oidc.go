package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// realmURL is the Keycloak realm the Aruba CMP API client authenticates against.
func realmURL(idp, realm string) string {
	return strings.TrimRight(idp, "/") + "/realms/" + realm
}

// jwtPublicKey materialises the realm's current RS256 signing key as a PEM public
// key. The ECP gateway verifies OFFLINE against one static key — it does not fetch
// a JWKS — so this has to be re-run after a realm key rotation.
func jwtPublicKey(ctx context.Context, realm string) ([]byte, error) {
	var jwks struct {
		Keys []struct {
			Use string   `json:"use"`
			Alg string   `json:"alg"`
			X5c []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := getJSON(ctx, realm+"/protocol/openid-connect/certs", &jwks); err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	for _, k := range jwks.Keys {
		if k.Use != "sig" || k.Alg != "RS256" || len(k.X5c) == 0 {
			continue
		}
		der, err := base64.StdEncoding.DecodeString(k.X5c[0])
		if err != nil {
			return nil, fmt.Errorf("decode x5c: %w", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("parse signing certificate: %w", err)
		}
		pub, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), nil
	}
	return nil, fmt.Errorf("no RS256 signing key in the JWKS at %s", realm)
}

// mintToken performs a client_credentials grant. It doubles as a live check that
// the credentials work — a bad client id or secret is otherwise silent until the
// operator starts logging "invalid_client" against resources stuck in Pending.
func mintToken(ctx context.Context, realm, clientID, clientSecret string) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		realm+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("no access_token in the token response")
	}
	return tok.AccessToken, nil
}

// tokenSubject reads the "sub" claim without verifying the signature — the token
// was just minted from the realm this process asked, so there is nothing to prove.
//
// For a client_credentials grant "sub" is the client's SERVICE ACCOUNT user id,
// not its clientId, so it cannot be guessed: it has to be read off a real token.
// It is what the RoleAssignment must name, because roles never travel in the token.
func tokenSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("the token carries no sub claim")
	}
	return claims.Sub, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
