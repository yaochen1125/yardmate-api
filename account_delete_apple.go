package main

// account_delete_apple.go — Sign in with Apple token revocation, used by
// POST /v1/account/delete (best-effort step 2).
//
// Apple requires apps that offer "Sign in with Apple" to also let users revoke
// the grant on account deletion. The flow:
//
//  1. Mint a client_secret: a short-lived ES256-signed JWT
//     (iss=APPLE_TEAM_ID, sub=APPLE_BUNDLE_ID, aud=https://appleid.apple.com,
//     kid header=APPLE_KEY_ID), signed with the .p8 EC private key.
//  2. POST the iOS-provided authorization code to /auth/token to obtain a
//     refresh_token (fall back to access_token if no refresh_token is returned).
//  3. POST that token to /auth/revoke (token_type_hint=refresh_token).
//
// The whole thing is BEST-EFFORT (the caller logs + continues on any error):
// Apple endpoints being down, an expired code, or a missing key must not block
// the user's account deletion. We never log the code, the client_secret, or any
// returned token value — only opaque status text.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	appleAudience    = "https://appleid.apple.com"
	appleTokenURL    = "https://appleid.apple.com/auth/token"
	appleRevokeURL   = "https://appleid.apple.com/auth/revoke"
	appleSecretTTL   = 10 * time.Minute // Apple allows up to 6 months; 10 min is plenty for one exchange.
	appleHTTPTimeout = 10 * time.Second
)

// appleRevokeConfig holds the Sign in with Apple service-account parameters.
// Resolved from the secrets vault by loadAccountDeleteSecrets.
type appleRevokeConfig struct {
	teamID         string // APPLE_TEAM_ID (10-char Apple Developer Team ID)
	keyID          string // APPLE_KEY_ID (the .p8 key's Key ID; becomes the JWT `kid`)
	bundleID       string // APPLE_BUNDLE_ID (the app id == client_id == JWT `sub`)
	privateKeyPEM  string // APPLE_PRIVATE_KEY (.p8 PEM contents) — preferred
	privateKeyPath string // APPLE_PRIVATE_KEY_PATH (path to .p8) — fallback when env can't hold newlines
}

// valid reports whether enough config is present to attempt a revoke. The key
// material is checked here as "at least one source set"; the actual PEM parse
// happens in clientSecret (a parse failure is surfaced as a best-effort error).
func (c appleRevokeConfig) valid() bool {
	return c.teamID != "" && c.keyID != "" && c.bundleID != "" &&
		(c.privateKeyPEM != "" || c.privateKeyPath != "")
}

// revokeAppleToken runs the full token-exchange + revoke. Returns an error on
// any failure; the caller treats it as best-effort (logs + continues).
func revokeAppleToken(ctx context.Context, cfg appleRevokeConfig, authorizationCode string) error {
	secret, err := cfg.clientSecret(time.Now())
	if err != nil {
		return fmt.Errorf("mint client_secret: %w", err)
	}
	token, err := exchangeAppleCode(ctx, cfg.bundleID, secret, authorizationCode)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if token == "" {
		return errors.New("no token returned from /auth/token")
	}
	if err := revokeAppleRefreshToken(ctx, cfg.bundleID, secret, token); err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	return nil
}

// clientSecret mints the ES256 JWT Apple requires as the OAuth client_secret.
// `now` is injected for testability. The .p8 is an EC (P-256) private key in
// PEM form; jwt.ParseECPrivateKeyFromPEM handles both PKCS#8 ("BEGIN PRIVATE
// KEY") and SEC1 ("BEGIN EC PRIVATE KEY") encodings, which covers Apple's .p8.
func (c appleRevokeConfig) clientSecret(now time.Time) (string, error) {
	pemBytes, err := c.privateKeyBytes()
	if err != nil {
		return "", err
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(pemBytes)
	if err != nil {
		return "", fmt.Errorf("parse .p8 EC private key: %w", err)
	}
	claims := jwt.MapClaims{
		"iss": c.teamID,
		"iat": now.Unix(),
		"exp": now.Add(appleSecretTTL).Unix(),
		"aud": appleAudience,
		"sub": c.bundleID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = c.keyID
	signed, err := tok.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign client_secret: %w", err)
	}
	return signed, nil
}

// privateKeyBytes returns the .p8 PEM contents from APPLE_PRIVATE_KEY, or from
// the file at APPLE_PRIVATE_KEY_PATH when the inline value is empty. Some env
// stores strip real newlines; we accept the common "\n"-escaped single-line
// form by unescaping it back to real newlines so PEM parsing succeeds.
func (c appleRevokeConfig) privateKeyBytes() ([]byte, error) {
	if c.privateKeyPEM != "" {
		pem := c.privateKeyPEM
		if strings.Contains(pem, `\n`) {
			pem = strings.ReplaceAll(pem, `\n`, "\n")
		}
		return []byte(pem), nil
	}
	if c.privateKeyPath != "" {
		b, err := os.ReadFile(c.privateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read APPLE_PRIVATE_KEY_PATH: %w", err)
		}
		return b, nil
	}
	return nil, errors.New("no Apple private key (APPLE_PRIVATE_KEY / APPLE_PRIVATE_KEY_PATH)")
}

// appleTokenResponse is the subset of the /auth/token response we read.
type appleTokenResponse struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
}

// exchangeAppleCode POSTs the authorization_code grant and returns the
// refresh_token (preferred) or access_token (fallback). The client_secret is
// sent in the form body per Apple's spec — it is a value, never logged.
func exchangeAppleCode(ctx context.Context, clientID, clientSecret, code string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	body, status, err := applePostForm(ctx, appleTokenURL, form)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		// Apple returns a JSON {error,...}; surface the status only (the body can
		// contain the echoed grant context — keep it out of logs upstream).
		return "", fmt.Errorf("apple /auth/token status %d", status)
	}
	var tr appleTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode /auth/token response: %w", err)
	}
	if tr.RefreshToken != "" {
		return tr.RefreshToken, nil
	}
	return tr.AccessToken, nil
}

// revokeAppleRefreshToken POSTs the revoke request. Apple returns 200 with an
// empty body on success; any non-2xx is an error (best-effort upstream).
func revokeAppleRefreshToken(ctx context.Context, clientID, clientSecret, token string) error {
	form := url.Values{
		"token":           {token},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
		"client_secret":   {clientSecret},
	}
	_, status, err := applePostForm(ctx, appleRevokeURL, form)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("apple /auth/revoke status %d", status)
	}
	return nil
}

// applePostForm POSTs application/x-www-form-urlencoded to an Apple endpoint and
// returns the response body + status. A dedicated short-timeout client keeps a
// hung Apple endpoint from eating the request's deadline budget.
func applePostForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: appleHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// Bound the read; Apple's responses are tiny JSON blobs.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
