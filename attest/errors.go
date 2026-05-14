package attest

import "errors"

// Sentinel errors returned by Verifier. Callers (HTTP handlers) map these to
// HTTP status codes — e.g. 401 for challenge/signature failures, 409 for
// counter conflicts. Use errors.Is to compare.
var (
	// ErrBadCBOR is returned when the attestation/assertion bytes cannot be
	// decoded as CBOR or have an unexpected shape.
	ErrBadCBOR = errors.New("attest: invalid CBOR")

	// ErrCertChain is returned when the embedded cert chain does not validate
	// against the trusted Apple App Attestation Root CA.
	ErrCertChain = errors.New("attest: cert chain invalid")

	// ErrAttestNonce is returned when the nonce extension inside credCert
	// (OID 1.2.840.113635.100.8.2) does not equal SHA256(authData || clientDataHash).
	ErrAttestNonce = errors.New("attest: attestation nonce mismatch")

	// ErrAppIDMismatch is returned when authData.rpIDHash != SHA256(AppID).
	ErrAppIDMismatch = errors.New("attest: appID hash mismatch")

	// ErrAAGUIDMismatch is returned when authData.aaguid is neither the
	// production value nor (if ATTEST_ALLOW_DEV=true) the development value.
	ErrAAGUIDMismatch = errors.New("attest: aaguid not permitted")

	// ErrCounterNotZero is returned when an attestation carries counter != 0.
	ErrCounterNotZero = errors.New("attest: counter must be 0 on attestation")

	// ErrCredentialIDMismatch is returned when SHA256(uncompressed pubkey)
	// or authData.credentialID does not match the claimed keyID.
	ErrCredentialIDMismatch = errors.New("attest: credentialID does not match keyID")

	// ErrCredentialUnknown is returned when no credential is registered for keyID.
	ErrCredentialUnknown = errors.New("attest: keyID not registered")

	// ErrBadSignature is returned when the ECDSA assertion signature does not
	// verify against the stored public key over SHA256(authData || clientDataHash).
	ErrBadSignature = errors.New("attest: assertion signature invalid")

	// ErrCounterNotMonotonic is returned when an assertion's counter is not
	// strictly greater than the stored counter (replay defense, SPEC §6.2).
	ErrCounterNotMonotonic = errors.New("attest: counter not monotonic")

	// ErrChallengeUnknown is returned when the claimed challenge bytes were
	// never issued, were issued for a different purpose, or have been swept.
	ErrChallengeUnknown = errors.New("attest: challenge unknown or wrong purpose")

	// ErrChallengeExpired is returned when the challenge is older than TTL.
	ErrChallengeExpired = errors.New("attest: challenge expired")

	// ErrChallengeReplay is returned when a challenge has already been consumed
	// by a successful verification (SPEC §6.6 single-use enforcement).
	ErrChallengeReplay = errors.New("attest: challenge already consumed")
)
