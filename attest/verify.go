// Package attest implements Apple App Attest server-side verification.
//
// See SPEC.md for the design contract, including the 9-step attestation flow
// (§3.1), the 6-step assertion flow (§3.2), pitfalls, and the resolved
// decisions in §8. This package owns crypto + persistence; HTTP layer (handlers
// in main.go) owns base64 decoding and request shape.
package attest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// AAGUID byte sequences per SPEC §6.1.
var (
	aaguidProduction  = []byte("appattest\x00\x00\x00\x00\x00\x00\x00")
	aaguidDevelopment = []byte("appattestdevelop")
)

// extOIDAppAttestNonce is the OID of the cert extension carrying the attestation nonce.
var extOIDAppAttestNonce = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

// DefaultChallengeTTL is the lifetime of an issued challenge.
const DefaultChallengeTTL = 5 * time.Minute

// Challenge purpose tags.
const (
	PurposeRegister = "register"
	PurposeSecrets  = "secrets"
)

const challengeSize = 32

// Verifier is the top-level entry point for App Attest checks. Build via New.
type Verifier struct {
	appID        string
	appIDHash    [32]byte
	rootPool     *x509.CertPool
	allowDev     bool
	store        *Store
	challengeTTL time.Duration
	now          func() time.Time
	cborDec      cbor.DecMode
}

// Options carries Verifier construction parameters. Zero values pick safe defaults.
type Options struct {
	// AppID is "TeamID.BundleID" — for YardMate, "PMX32RG52M.com.chenyao.plantapp".
	AppID string

	// AllowDev controls whether the development aaguid is accepted (SPEC §6.1).
	// MUST be false in App Store production. Default is false.
	AllowDev bool

	// Store is the credential + challenge persistence layer.
	Store *Store

	// RootPool overrides the Apple App Attestation Root CA pool. Nil → use the
	// embedded production root via AppleAppAttestRootPool(). Tests inject a
	// pool seeded with a self-signed test root.
	RootPool *x509.CertPool

	// TTL is the challenge lifetime. 0 → DefaultChallengeTTL (5 min).
	TTL time.Duration

	// Now is the clock. Nil → time.Now. Tests inject a fixed clock.
	Now func() time.Time
}

// New constructs a Verifier.
func New(o Options) (*Verifier, error) {
	if o.AppID == "" {
		return nil, errors.New("attest: AppID required")
	}
	if o.Store == nil {
		return nil, errors.New("attest: Store required")
	}
	rootPool := o.RootPool
	if rootPool == nil {
		rootPool = AppleAppAttestRootPool()
	}
	ttl := o.TTL
	if ttl == 0 {
		ttl = DefaultChallengeTTL
	}
	nowFn := o.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	dec, err := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthAllowed,
		TagsMd:      cbor.TagsForbidden,
	}.DecMode()
	if err != nil {
		return nil, fmt.Errorf("attest: cbor mode: %w", err)
	}
	return &Verifier{
		appID:        o.AppID,
		appIDHash:    sha256.Sum256([]byte(o.AppID)),
		rootPool:     rootPool,
		allowDev:     o.AllowDev,
		store:        o.Store,
		challengeTTL: ttl,
		now:          nowFn,
		cborDec:      dec,
	}, nil
}

// ChallengeTTL returns the configured challenge TTL.
func (v *Verifier) ChallengeTTL() time.Duration { return v.challengeTTL }

// IssueChallenge returns 32 cryptographically random bytes and records them
// in the store with the given purpose. Purpose must be PurposeRegister or
// PurposeSecrets.
func (v *Verifier) IssueChallenge(purpose string) ([]byte, error) {
	if purpose != PurposeRegister && purpose != PurposeSecrets {
		return nil, fmt.Errorf("attest: unknown purpose %q", purpose)
	}
	c := make([]byte, challengeSize)
	if _, err := rand.Read(c); err != nil {
		return nil, fmt.Errorf("attest: random: %w", err)
	}
	if err := v.store.PutChallenge(c, purpose, v.now()); err != nil {
		return nil, err
	}
	return c, nil
}

// --- attestation ---

// attestationObject mirrors Apple's CBOR output:
//
//	{ "fmt": "apple-appattest",
//	  "authData": <bytes>,
//	  "attStmt": { "x5c": [<credCert>, <intermediate>], "receipt": <bytes> } }
type attestationObject struct {
	Fmt      string          `cbor:"fmt"`
	AuthData []byte          `cbor:"authData"`
	AttStmt  attestationStmt `cbor:"attStmt"`
}

type attestationStmt struct {
	X5C     [][]byte `cbor:"x5c"`
	Receipt []byte   `cbor:"receipt"`
}

// VerifyAttestation runs Apple's 9-step attestation verification (SPEC §3.1).
// On success it persists (keyID → pubkey, counter=0) and consumes the challenge.
func (v *Verifier) VerifyAttestation(keyID, attestationCBOR, challenge []byte) error {
	now := v.now()

	var obj attestationObject
	if err := v.cborDec.Unmarshal(attestationCBOR, &obj); err != nil {
		return ErrBadCBOR
	}
	if obj.Fmt != "apple-appattest" {
		return ErrBadCBOR
	}
	if len(obj.AttStmt.X5C) < 2 {
		return ErrCertChain
	}

	// Step 1: parse + verify cert chain.
	credCert, err := x509.ParseCertificate(obj.AttStmt.X5C[0])
	if err != nil {
		return ErrCertChain
	}
	intermediates := x509.NewCertPool()
	for _, der := range obj.AttStmt.X5C[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return ErrCertChain
		}
		intermediates.AddCert(c)
	}
	if _, err := credCert.Verify(x509.VerifyOptions{
		Roots:         v.rootPool,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return ErrCertChain
	}

	// Steps 2-4: compute nonce, compare against credCert extension.
	clientDataHash := sha256.Sum256(challenge)
	composite := make([]byte, 0, len(obj.AuthData)+sha256.Size)
	composite = append(composite, obj.AuthData...)
	composite = append(composite, clientDataHash[:]...)
	nonce := sha256.Sum256(composite)
	if err := verifyNonceExtension(credCert, nonce[:]); err != nil {
		return err
	}

	// Step 5: SHA256(uncompressed pubkey) == keyID.
	ecdsaKey, ok := credCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return ErrCertChain
	}
	ecdhPub, err := ecdsaKey.ECDH()
	if err != nil {
		return ErrCertChain
	}
	computedKeyID := sha256.Sum256(ecdhPub.Bytes())
	if !bytes.Equal(computedKeyID[:], keyID) {
		return ErrCredentialIDMismatch
	}

	// Steps 6-9: authData fields.
	authData, err := parseAuthDataAttestation(obj.AuthData)
	if err != nil {
		return err
	}
	if !bytes.Equal(authData.RPIDHash, v.appIDHash[:]) {
		return ErrAppIDMismatch
	}
	if authData.Counter != 0 {
		return ErrCounterNotZero
	}
	if !v.aaguidAllowed(authData.AAGUID) {
		return ErrAAGUIDMismatch
	}
	if !bytes.Equal(authData.CredentialID, keyID) {
		return ErrCredentialIDMismatch
	}

	// Phase 2: atomic challenge consume + credential store.
	pubKeyDER, err := x509.MarshalPKIXPublicKey(ecdsaKey)
	if err != nil {
		return ErrCertChain
	}
	return v.store.AtomicConsumeAndPutCredential(
		challenge, PurposeRegister, now, v.challengeTTL, keyID, pubKeyDER,
	)
}

func (v *Verifier) aaguidAllowed(aaguid []byte) bool {
	if bytes.Equal(aaguid, aaguidProduction) {
		return true
	}
	if v.allowDev && bytes.Equal(aaguid, aaguidDevelopment) {
		return true
	}
	return false
}

// verifyNonceExtension finds OID 1.2.840.113635.100.8.2 on the credCert and
// confirms its encoded nonce matches expected. Apple wraps the nonce inside
// a SEQUENCE; depending on whether the inner element is [1] EXPLICIT or
// IMPLICIT, the raw bytes may be the OCTET STRING content directly or require
// a second asn1.Unmarshal — we try both.
func verifyNonceExtension(cert *x509.Certificate, expected []byte) error {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(extOIDAppAttestNonce) {
			continue
		}
		var seq []asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil || len(seq) != 1 {
			return ErrAttestNonce
		}
		inner := seq[0].Bytes
		if bytes.Equal(inner, expected) {
			return nil
		}
		var oct []byte
		if _, err := asn1.Unmarshal(inner, &oct); err == nil && bytes.Equal(oct, expected) {
			return nil
		}
		return ErrAttestNonce
	}
	return ErrAttestNonce
}

// --- assertion ---

type assertionObject struct {
	Signature []byte `cbor:"signature"`
	AuthData  []byte `cbor:"authenticatorData"`
}

// VerifyAssertion runs Apple's 6-step assertion verification (SPEC §3.2).
// On success the credential counter is updated and the challenge is consumed.
func (v *Verifier) VerifyAssertion(keyID, assertionCBOR, challenge []byte) error {
	now := v.now()

	var obj assertionObject
	if err := v.cborDec.Unmarshal(assertionCBOR, &obj); err != nil {
		return ErrBadCBOR
	}
	if len(obj.Signature) == 0 || len(obj.AuthData) == 0 {
		return ErrBadCBOR
	}

	cred, err := v.store.GetCredential(keyID)
	if err != nil {
		return err
	}
	pub, err := x509.ParsePKIXPublicKey(cred.PublicKeyDER)
	if err != nil {
		return ErrBadSignature
	}
	ecdsaKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return ErrBadSignature
	}

	authData, err := parseAuthDataAssertion(obj.AuthData)
	if err != nil {
		return err
	}
	if !bytes.Equal(authData.RPIDHash, v.appIDHash[:]) {
		return ErrAppIDMismatch
	}
	if authData.Counter <= cred.Counter {
		return ErrCounterNotMonotonic
	}

	clientDataHash := sha256.Sum256(challenge)
	composite := make([]byte, 0, len(obj.AuthData)+sha256.Size)
	composite = append(composite, obj.AuthData...)
	composite = append(composite, clientDataHash[:]...)
	nonce := sha256.Sum256(composite)
	if !ecdsa.VerifyASN1(ecdsaKey, nonce[:], obj.Signature) {
		return ErrBadSignature
	}

	return v.store.AtomicConsumeAndUpdateCounter(
		challenge, PurposeSecrets, now, v.challengeTTL, keyID, authData.Counter,
	)
}
