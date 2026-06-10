package main

// account_delete.go — POST /v1/account/delete.
//
// Deletes a Supabase user's account end-to-end and revokes their Sign in with
// Apple token. Contract (locked with the iOS side):
//
//	Request : Authorization: Bearer <supabase access-token JWT> (HS256)
//	          Content-Type: application/json
//	          body { "apple_authorization_code": "<string, may be empty>" }
//	Success : 200 { "success": true }
//	Errors  : { "error": "<code>" } — 401 missing/invalid token, 500 server error
//
// Order of operations (see handleAccountDelete):
//  1. Verify the Supabase JWT, extract sub = userID (UUID). 401 on any failure.
//  2. Best-effort Apple token revoke (never blocks deletion).
//  3. Best-effort delete of Supabase Storage objects under diary-images/{userID}/.
//  4. Hard delete diary_entries + garden_records rows (500 on failure).
//  5. Hard delete the Supabase auth user (500 on failure).
//  6. 200 { "success": true }.
//
// Steps 2 & 3 are best-effort by design: a failed Apple revoke or an orphaned
// storage object must not leave the user unable to delete their account. Steps
// 4 & 5 are the load-bearing deletes — if either fails the client gets a 500
// and can safely retry (the operations are idempotent: re-deleting already-gone
// rows / a missing auth user is a no-op-ish 404 we tolerate, see deleteAuthUser).
//
// SECURITY: the service_role key, Supabase JWT secret, and Apple signing key
// come ONLY from the secrets vault (never hardcoded). No token, secret, code,
// or signed JWT value is ever logged — only opaque event/status strings.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// accountDeleteRequest is the JSON body. The Authorization header carries the
// Supabase access token (not the body). apple_authorization_code is the
// short-lived ASAuthorizationAppleIDCredential.authorizationCode from iOS; it
// may be empty (e.g. the user never linked Apple, or the code already expired),
// in which case the Apple revoke step is skipped.
type accountDeleteRequest struct {
	AppleAuthorizationCode string `json:"apple_authorization_code"`
}

// accountDeleteResponse is the 200 success body.
type accountDeleteResponse struct {
	Success bool `json:"success"`
}

// accountDeleteSecrets bundles the vault-resolved configuration this handler
// needs, validated once per request. Grouping them keeps the handler readable
// and makes the "any required secret missing -> 500 server_error" guard a
// single check (these are operator-configured, so absence is a server fault,
// not a client one).
type accountDeleteSecrets struct {
	supabaseURL    string // SUPABASE_URL, e.g. https://<ref>.supabase.co (no trailing slash)
	jwtSecret      string // SUPABASE_JWT_SECRET (HS256 verification key)
	serviceRoleKey string // SUPABASE_SERVICE_ROLE_KEY (admin auth + storage)
	apple          appleRevokeConfig
}

// loadAccountDeleteSecrets reads + validates the required vault keys. The Apple
// keys are validated lazily (only needed when an authorization code is present),
// so they are NOT required here — appleRevokeConfig.valid() gates that path.
// Returns ok=false when a required Supabase key is missing/blank.
func loadAccountDeleteSecrets(vault *secrets.Vault) (accountDeleteSecrets, bool) {
	s := accountDeleteSecrets{
		supabaseURL:    strings.TrimRight(vault.Get("SUPABASE_URL"), "/"),
		jwtSecret:      vault.Get("SUPABASE_JWT_SECRET"),
		serviceRoleKey: vault.Get("SUPABASE_SERVICE_ROLE_KEY"),
		apple: appleRevokeConfig{
			teamID:   vault.Get("APPLE_TEAM_ID"),
			keyID:    vault.Get("APPLE_KEY_ID"),
			bundleID: vault.Get("APPLE_BUNDLE_ID"),
			// APPLE_PRIVATE_KEY holds the .p8 PEM directly; APPLE_PRIVATE_KEY_PATH
			// is the fallback when the env can't carry the newlines (read at use).
			privateKeyPEM:  vault.Get("APPLE_PRIVATE_KEY"),
			privateKeyPath: vault.Get("APPLE_PRIVATE_KEY_PATH"),
		},
	}
	if s.supabaseURL == "" || s.jwtSecret == "" || s.serviceRoleKey == "" {
		return s, false
	}
	return s, true
}

// handleAccountDelete returns the POST /v1/account/delete handler. enrichDB is
// the shared Supabase pgx pool (same pool the enrichment service uses); it may
// be nil if the DB was never configured — in that case account deletion can't
// run its row deletes, so we 500 (server_error) rather than silently skipping.
func handleAccountDelete(vault *secrets.Vault, enrichDB *enrichment.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ---- 0. config preconditions (operator faults => 500) ----
		cfg, ok := loadAccountDeleteSecrets(vault)
		if !ok {
			log.Printf("account/delete: missing Supabase config (SUPABASE_URL / SUPABASE_JWT_SECRET / SUPABASE_SERVICE_ROLE_KEY)")
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}
		if enrichDB == nil {
			log.Printf("account/delete: enrichment DB pool unavailable; cannot delete user rows")
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		// ---- decode body (apple_authorization_code may be empty) ----
		var req accountDeleteRequest
		if !decodeJSON(w, r, &req) {
			return // decodeJSON already wrote the 400
		}

		// ---- 1. verify Supabase JWT -> userID (401 on any failure) ----
		userID, err := verifySupabaseToken(bearerToken(r), cfg.jwtSecret)
		if err != nil {
			// Do not echo the token or the parse error detail to the client; a
			// generic code is enough and avoids leaking which check failed.
			log.Printf("account/delete: token verification failed: %v", err)
			writeError(w, http.StatusUnauthorized, "invalid_token")
			return
		}

		// Bound the whole deletion (Apple round-trips + storage list/delete +
		// SQL + admin delete) so a stuck upstream can't pin the request past the
		// server's 35 s write timeout.
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()

		// ---- 2. revoke Apple token (BEST-EFFORT: log + continue) ----
		if code := strings.TrimSpace(req.AppleAuthorizationCode); code != "" {
			if cfg.apple.valid() {
				if err := revokeAppleToken(ctx, cfg.apple, code); err != nil {
					// Never log `code`, the client_secret, or upstream token values.
					log.Printf("account/delete: apple revoke failed (best-effort, continuing): %v", err)
				}
			} else {
				log.Printf("account/delete: apple authorization code present but Apple revoke not configured; skipping (best-effort)")
			}
		}

		// ---- 3. delete Storage objects diary-images/{userID}/ (BEST-EFFORT) ----
		if err := deleteUserStorageObjects(ctx, cfg.supabaseURL, cfg.serviceRoleKey, userID); err != nil {
			log.Printf("account/delete: storage cleanup failed (best-effort, continuing): %v", err)
		}

		// ---- 4. delete DB rows (load-bearing: 500 on failure) ----
		if err := enrichDB.DeleteUserRows(ctx, userID); err != nil {
			log.Printf("account/delete: delete user rows failed: %v", err)
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		// ---- 5. delete the auth user (load-bearing: 500 on failure) ----
		if err := deleteAuthUser(ctx, cfg.supabaseURL, cfg.serviceRoleKey, userID); err != nil {
			log.Printf("account/delete: delete auth user failed: %v", err)
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		// ---- 6. success ----
		writeJSON(w, http.StatusOK, accountDeleteResponse{Success: true})
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, case-insensitively on the scheme. Returns "" when absent/malformed
// (verifySupabaseToken then rejects it as a 401).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// verifySupabaseToken validates a Supabase access token (HS256) and returns the
// `sub` claim (the auth user id, a UUID string). It enforces:
//   - non-empty token + secret,
//   - HS256 algorithm ONLY (rejects alg=none and any asymmetric alg — prevents
//     algorithm-confusion attacks where an attacker forges a token with a
//     different alg against our symmetric secret),
//   - signature validity against SUPABASE_JWT_SECRET,
//   - expiry (exp) — REQUIRED + enforced (jwt/v5 only checks exp when present
//     unless WithExpirationRequired is set; for an account-destroying endpoint
//     we refuse any token that omits exp so a leaked token can never be
//     non-expiring),
//   - a present, non-empty `sub`.
//
// Any failure returns an error (the handler maps all of them to a single 401
// without leaking which check failed).
func verifySupabaseToken(tokenStr, secret string) (string, error) {
	if tokenStr == "" {
		return "", errors.New("missing bearer token")
	}
	if secret == "" {
		return "", errors.New("empty jwt secret")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(t *jwt.Token) (any, error) {
			// Pin the algorithm: only HMAC-SHA256 is accepted. This rejects
			// alg=none and RS/ES tokens before the signature is checked.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return []byte(secret), nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		// Reject tokens with no exp claim. jwt/v5 validates exp only when it is
		// present by default; for account deletion we require it so a token can
		// never be effectively non-expiring.
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", err // covers bad signature, expired, malformed, wrong alg
	}
	sub, err := claims.GetSubject()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sub) == "" {
		return "", errors.New("empty subject claim")
	}
	return sub, nil
}
