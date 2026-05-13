package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// vendedKeys is the env keys returned by /v1/app-secrets. Add entries here
// as the YardMate feature set grows. Keys are uppercased here (env name) and
// lowercased on output by Vault.Snapshot (JSON convention).
var vendedKeys = []string{
	"OPENAI_API_KEY",
	"PLANT_ID_API_KEY",
}

type challengeResponse struct {
	Challenge string `json:"challenge"`
}

type registerRequest struct {
	KeyID       string `json:"keyID"`
	Attestation string `json:"attestation"`
	Challenge   string `json:"challenge"`
}

type appSecretsRequest struct {
	KeyID     string `json:"keyID"`
	Assertion string `json:"assertion"`
	Challenge string `json:"challenge"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func handleAttestChallenge(v *attest.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := v.IssueChallenge(attest.PurposeRegister)
		if err != nil {
			log.Printf("issue register challenge: %v", err)
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		writeJSON(w, http.StatusOK, challengeResponse{
			Challenge: base64.StdEncoding.EncodeToString(c),
		})
	}
}

func handleAttestRegister(v *attest.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		keyID, ok := decodeB64(w, "keyID", req.KeyID)
		if !ok {
			return
		}
		att, ok := decodeB64(w, "attestation", req.Attestation)
		if !ok {
			return
		}
		ch, ok := decodeB64(w, "challenge", req.Challenge)
		if !ok {
			return
		}
		if err := v.VerifyAttestation(keyID, att, ch); err != nil {
			writeAttestError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func handleSecretsChallenge(v *attest.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := v.IssueChallenge(attest.PurposeSecrets)
		if err != nil {
			log.Printf("issue secrets challenge: %v", err)
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		writeJSON(w, http.StatusOK, challengeResponse{
			Challenge: base64.StdEncoding.EncodeToString(c),
		})
	}
}

func handleAppSecrets(v *attest.Verifier, vault *secrets.Vault) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req appSecretsRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		keyID, ok := decodeB64(w, "keyID", req.KeyID)
		if !ok {
			return
		}
		ass, ok := decodeB64(w, "assertion", req.Assertion)
		if !ok {
			return
		}
		ch, ok := decodeB64(w, "challenge", req.Challenge)
		if !ok {
			return
		}
		if err := v.VerifyAssertion(keyID, ass, ch); err != nil {
			writeAttestError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, vault.Snapshot(vendedKeys))
	}
}

// --- helpers ---

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json")
		return false
	}
	return true
}

func decodeB64(w http.ResponseWriter, field, value string) ([]byte, bool) {
	if value == "" {
		writeError(w, http.StatusBadRequest, "missing_"+field)
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_base64_"+field)
		return nil, false
	}
	return b, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}

func writeAttestError(w http.ResponseWriter, err error) {
	status, code := mapAttestError(err)
	writeError(w, status, code)
}

// mapAttestError maps a sentinel from the attest package to an HTTP status +
// short machine-readable error tag. 401 covers auth-style failures (challenge,
// signature, cert chain); 409 covers counter conflicts; 400 covers malformed
// inputs that reached the verifier; 500 is the unmapped-error catch-all.
func mapAttestError(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, attest.ErrBadCBOR):
		return http.StatusBadRequest, "bad_cbor"
	case errors.Is(err, attest.ErrCertChain):
		return http.StatusUnauthorized, "cert_chain"
	case errors.Is(err, attest.ErrAttestNonce):
		return http.StatusUnauthorized, "nonce_mismatch"
	case errors.Is(err, attest.ErrAppIDMismatch):
		return http.StatusUnauthorized, "app_id_mismatch"
	case errors.Is(err, attest.ErrAAGUIDMismatch):
		return http.StatusUnauthorized, "aaguid_mismatch"
	case errors.Is(err, attest.ErrCredentialIDMismatch):
		return http.StatusUnauthorized, "credential_id_mismatch"
	case errors.Is(err, attest.ErrCredentialUnknown):
		return http.StatusUnauthorized, "credential_unknown"
	case errors.Is(err, attest.ErrBadSignature):
		return http.StatusUnauthorized, "signature_invalid"
	case errors.Is(err, attest.ErrChallengeUnknown):
		return http.StatusUnauthorized, "challenge_unknown"
	case errors.Is(err, attest.ErrChallengeExpired):
		return http.StatusUnauthorized, "challenge_expired"
	case errors.Is(err, attest.ErrChallengeReplay):
		return http.StatusUnauthorized, "challenge_replay"
	case errors.Is(err, attest.ErrCounterNotZero):
		return http.StatusConflict, "counter_not_zero"
	case errors.Is(err, attest.ErrCounterNotMonotonic):
		return http.StatusConflict, "counter_not_monotonic"
	default:
		log.Printf("unmapped attest error: %v", err)
		return http.StatusInternalServerError, "internal"
	}
}
