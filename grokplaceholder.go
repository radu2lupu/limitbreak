package main

// The Grok placeholder login.
//
// A Mac with no Grok login of its own (nothing in ~/.grok) runs grok on a login Limit Break installed
// from Coffer (see loginsync.go): grokAccounts.spends puts every request on it. grok will not start
// without an auth.json, so such a Mac gives each agent home a placeholder one
// (limitbreak grok-placeholder <dir>), which every request's Authorization and identity headers replace.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const (
	grokIssuer   = "https://auth.x.ai"
	grokClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// grokPlaceholderEmail marks the stand-in login: it is never an account.
	grokPlaceholderEmail = "limitbreak@placeholder.invalid"
)

// grokPlaceholderAuth is an auth.json grok accepts but that is nobody's: a login with a JWT of the
// shape grok checks (every claim, expiring in 2100) and no refresh token. grok sends no request
// without an auth.json; Limit Break swaps in a real login's token and identity on each one.
func grokPlaceholderAuth() []byte {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	jwt := enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": grokIssuer, "sub": "limitbreak", "aud": []string{grokClientID}, "exp": 4102444800, "iat": 1767225600,
		"scope": "openid profile email offline_access api:access", "principal_type": "User", "principal_id": "limitbreak",
		"client_id": grokClientID, "jti": "limitbreak", "tier": 1, "team_id": "limitbreak"}) + "." + "bGltaXRicmVhaw"
	out, _ := json.MarshalIndent(map[string]any{grokIssuer + "::" + grokClientID: map[string]any{
		"auth_mode": "oidc", "key": jwt, "expires_at": "2099-12-31T00:00:00Z", "email": grokPlaceholderEmail,
		"user_id": "limitbreak", "team_id": "limitbreak", "oidc_issuer": grokIssuer, "oidc_client_id": grokClientID,
		"principal_id": "limitbreak", "principal_type": "User", "create_time": "2026-01-01T00:00:00Z",
		"first_name": "Limit Break", "team_zero_data_retention": false, "coding_data_retention_opt_out": false}}, "", "  ")
	return out
}

// grokPlaceholderCommand writes the placeholder auth.json into dir (a Grok home for an agent) so grok
// starts there with no login. It never overwrites a login of someone's.
func grokPlaceholderCommand(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: limitbreak grok-placeholder <grok-home-dir>")
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if l, ok := readGrokLogin(dir); ok && !l.placeholder {
		return fmt.Errorf("%s holds a real login; not overwriting it", dir)
	}
	if err := writeFileAtomic(dir, "auth.json", grokPlaceholderAuth()); err != nil {
		return err
	}
	fmt.Fprintf(out, "placeholder login written to %s\n", filepath.Join(dir, "auth.json"))
	return nil
}
