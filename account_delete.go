package main

// account_delete.go — POST /v1/account/delete.
//
// Deletes a Supabase user's account end-to-end and revokes their Sign in with
// Apple token. Contract (locked with the iOS side):
//
//	Request : Authorization: Bearer <supabase access-token JWT> (ES256, verified via the project JWKS)
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
// SECURITY: the service_role key and Apple signing key come ONLY from the
// secrets vault (never hardcoded). Access tokens are verified against the
// project's public JWKS (ES256), not a shared secret. No token, secret, code,
// or signed JWT value is ever logged — only opaque event/status strings.

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/secrets"
)

const (
	// bestEffortTimeout caps EACH best-effort phase (Apple revoke, Storage
	// cleanup) on its own child of context.Background(). It must comfortably
	// cover that phase's own HTTP client timeout (Apple/Supabase use 10 s) so
	// the cap is a backstop, not the primary limit. Crucially it is per-phase
	// and Background-derived, so a slow phase can't bleed its budget into the
	// load-bearing deletes below.
	bestEffortTimeout = 10 * time.Second

	// hardDeleteTimeout bounds the load-bearing deletes (DeleteUserRows +
	// deleteAuthUser) on their OWN fresh context, untouched by the best-effort
	// phases. Generous enough for two sequential round-trips, capped so a hung
	// DB/Admin API can't wedge the handler past the server's 35 s WriteTimeout.
	hardDeleteTimeout = 15 * time.Second
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
	if s.supabaseURL == "" || s.serviceRoleKey == "" {
		return s, false
	}
	return s, true
}

// handleAccountDelete returns the POST /v1/account/delete handler. enrichDB is
// the shared Supabase pgx pool (same pool the enrichment service uses); it may
// be nil if the DB was never configured — in that case account deletion can't
// run its row deletes, so we 500 (server_error) rather than silently skipping.
func handleAccountDelete(vault *secrets.Vault, enrichDB *enrichment.DB) http.HandlerFunc {
	// Build the JWKS verifier once (long-lived cache) from SUPABASE_URL. Access
	// tokens are ES256, verified against the project's public JWKS.
	jwks := newJWKSCache(strings.TrimRight(vault.Get("SUPABASE_URL"), "/"))
	return func(w http.ResponseWriter, r *http.Request) {
		// ---- 0. config preconditions (operator faults => 500) ----
		cfg, ok := loadAccountDeleteSecrets(vault)
		if !ok {
			log.Printf("account/delete: missing Supabase config (SUPABASE_URL / SUPABASE_SERVICE_ROLE_KEY)")
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
		userID, err := verifySupabaseToken(r.Context(), bearerToken(r), jwks.keyForKID)
		if err != nil {
			// Do not echo the token or the parse error detail to the client; a
			// generic code is enough and avoids leaking which check failed.
			log.Printf("account/delete: token verification failed: %v", err)
			writeError(w, http.StatusUnauthorized, "invalid_token")
			return
		}

		// Context strategy (Codex #55): the best-effort phases (Apple revoke,
		// Storage cleanup) and the load-bearing deletes run on SEPARATE contexts
		// so a slow/hung best-effort dependency can NEVER starve the deletes into
		// a spurious 500. Each best-effort phase gets its own capped child of
		// context.Background() (NOT the request ctx) — its slowness is contained
		// and a client disconnect doesn't abort a delete mid-flight. The
		// load-bearing deletes get their own fresh, adequate deadline that the
		// best-effort phases have not consumed. Every context derives from
		// Background (not r.Context()) because deletion must complete regardless
		// of client disconnect; the per-phase caps keep a hung upstream/DB from
		// wedging the handler past the server's 35 s WriteTimeout. Worst case is
		// ~10 (Apple) + ~10 (Storage) + ~15 (deletes) = ~35 s of wall clock, and
		// in practice each phase's own HTTP client timeout (10 s) trips first.

		// ---- 2. revoke Apple token (BEST-EFFORT: log + continue) ----
		if code := strings.TrimSpace(req.AppleAuthorizationCode); code != "" {
			if cfg.apple.valid() {
				appleCtx, appleCancel := context.WithTimeout(context.Background(), bestEffortTimeout)
				err := revokeAppleToken(appleCtx, cfg.apple, code)
				appleCancel()
				if err != nil {
					// Never log `code`, the client_secret, or upstream token values.
					log.Printf("account/delete: apple revoke failed (best-effort, continuing): %v", err)
				}
			} else {
				log.Printf("account/delete: apple authorization code present but Apple revoke not configured; skipping (best-effort)")
			}
		}

		// ---- 3. delete Storage objects diary-images/{userID}/ (BEST-EFFORT) ----
		storageCtx, storageCancel := context.WithTimeout(context.Background(), bestEffortTimeout)
		err = deleteUserStorageObjects(storageCtx, cfg.supabaseURL, cfg.serviceRoleKey, userID)
		storageCancel()
		if err != nil {
			log.Printf("account/delete: storage cleanup failed (best-effort, continuing): %v", err)
		}

		// ---- 4 & 5. load-bearing deletes on a FRESH deadline the best-effort
		// phases above did not touch (rows then auth user; both 500 on failure). ----
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), hardDeleteTimeout)
		defer deleteCancel()

		if err := enrichDB.DeleteUserRows(deleteCtx, userID); err != nil {
			log.Printf("account/delete: delete user rows failed: %v", err)
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		if err := deleteAuthUser(deleteCtx, cfg.supabaseURL, cfg.serviceRoleKey, userID); err != nil {
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

// verifySupabaseToken validates a Supabase access token (ES256, asymmetric) and
// returns the `sub` claim (the auth user id, a UUID string). It enforces:
//   - non-empty token,
//   - ES256 algorithm ONLY (rejects alg=none, HS*, and RS* before the signature
//     is checked — closes algorithm-confusion vectors),
//   - signature validity against the project's JWKS public key matching the
//     token's `kid` (resolved via keyForKID),
//   - expiry (exp) — REQUIRED + enforced (jwt/v5 only checks exp when present
//     unless WithExpirationRequired is set; for an account-destroying endpoint
//     we refuse any token that omits exp so a leaked token can never be
//     non-expiring),
//   - a present, non-empty `sub`.
//
// Any failure returns an error (the handler maps all of them to a single 401
// without leaking which check failed).
func verifySupabaseToken(ctx context.Context, tokenStr string, keyForKID func(context.Context, string) (*ecdsa.PublicKey, error)) (string, error) {
	if tokenStr == "" {
		return "", errors.New("missing bearer token")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(t *jwt.Token) (any, error) {
			// Pin the algorithm: only ES256 is accepted. This rejects alg=none,
			// HS*, and RS* before the signature is checked.
			if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, errors.New("unexpected signing method")
			}
			kid, _ := t.Header["kid"].(string)
			return keyForKID(ctx, kid)
		},
		jwt.WithValidMethods([]string{"ES256"}),
		// Reject tokens with no exp claim. jwt/v5 validates exp only when it is
		// present by default; for account deletion we require it so a token can
		// never be effectively non-expiring.
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", err // covers bad signature, expired, malformed, wrong alg, unknown kid
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
