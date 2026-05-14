// Package testkit synthesizes valid App Attest attestations + assertions
// against a self-generated test root CA, for use by integration tests that
// drive the real attest.Verifier end-to-end.
//
// This is a test helper. It does NOT mirror Apple's secrets — it produces
// CBOR with the same wire shape Apple emits, signed by a test key. Callers
// configure attest.Verifier with the Issuer's RootPool so the chain
// validates against the test root instead of Apple's production root.
//
// Mint logic is intentionally duplicated from attest/helpers_test.go (which
// stays for the attest package's own internal tests). Keeping the public
// testkit decoupled from internal test fixtures means external consumers
// don't pull in test-only types from the attest package.
package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// AAGUID byte sequences per Apple App Attest spec. Duplicated here to keep
// testkit self-contained.
var (
	AAGUIDProduction  = []byte("appattest\x00\x00\x00\x00\x00\x00\x00")
	AAGUIDDevelopment = []byte("appattestdevelop")
)

// ExtOIDAppAttestNonce is the credCert extension carrying the attestation nonce.
var ExtOIDAppAttestNonce = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

const flagAT byte = 0x40

// CBOR shape mirrors Apple's emit. Duplicated from attest internal types.
type attestationObject struct {
	Fmt      string          `cbor:"fmt"`
	AuthData []byte          `cbor:"authData"`
	AttStmt  attestationStmt `cbor:"attStmt"`
}

type attestationStmt struct {
	X5C     [][]byte `cbor:"x5c"`
	Receipt []byte   `cbor:"receipt"`
}

type assertionObject struct {
	Signature []byte `cbor:"signature"`
	AuthData  []byte `cbor:"authenticatorData"`
}

// Issuer owns a test root + intermediate CA pair and the leaf key produced
// by the most recent MintAttestation call. Subsequent MintAssertion calls
// sign with that leaf key — so a typical e2e test:
//
//	issuer := testkit.NewIssuer(t, "TEAM.bundle")
//	keyID, attCBOR := issuer.MintAttestation(t, testkit.AttestParams{...})
//	// ... feed attCBOR to /v1/attest/register ...
//	assCBOR := issuer.MintAssertion(t, testkit.AssertionParams{Counter: 1, ...})
//	// ... feed assCBOR to /v1/app-secrets ...
type Issuer struct {
	AppID    string
	RootPool *x509.CertPool

	rootKey   *ecdsa.PrivateKey
	rootCert  *x509.Certificate
	interKey  *ecdsa.PrivateKey
	interCert *x509.Certificate
	interDER  []byte

	leafKey *ecdsa.PrivateKey
}

// NewIssuer builds a fresh test trust hierarchy. The Issuer's RootPool should
// be passed to attest.Options.RootPool so the Verifier trusts this test root.
func NewIssuer(t *testing.T, appID string) *Issuer {
	t.Helper()
	rootKey, rootCert, _ := genCA(t, "Test App Attest Root", nil, nil)
	interKey, interCert, interDER := genCA(t, "Test App Attest CA", rootCert, rootKey)

	pool := x509.NewCertPool()
	pool.AddCert(rootCert)
	return &Issuer{
		AppID:     appID,
		RootPool:  pool,
		rootKey:   rootKey,
		rootCert:  rootCert,
		interKey:  interKey,
		interCert: interCert,
		interDER:  interDER,
	}
}

// AttestParams controls MintAttestation. Zero values produce a happy-path
// attestation. Each Bad* hook produces a single-field corruption for
// negative-path tests.
type AttestParams struct {
	AAGUID    []byte // default AAGUIDProduction
	Counter   uint32
	Challenge []byte // must be set; the caller has it from /v1/attest/challenge

	BadCertChain    bool // intermediate signed by an untrusted root
	BadAppID        bool // rpIDHash = SHA256("wrong")
	BadCounter      bool // counter = 1 instead of 0
	BadCredentialID bool // authData.credentialID = zero bytes (≠ keyID)
	BadNonce        bool // cert nonce extension = zero bytes
	BadFmt          bool // attestation fmt = "wrong"
}

// MintAttestation returns (keyID, attestationCBOR). The leaf key is stored
// on the Issuer so subsequent MintAssertion calls sign with the matching key.
func (i *Issuer) MintAttestation(t *testing.T, p AttestParams) (keyID, cborBytes []byte) {
	t.Helper()
	if p.AAGUID == nil {
		p.AAGUID = AAGUIDProduction
	}
	if p.Challenge == nil {
		t.Fatal("testkit: AttestParams.Challenge required")
	}

	signerKey, signerCert := i.interKey, i.interCert
	if p.BadCertChain {
		wrongKey, wrongCert, _ := genCA(t, "Untrusted Root", nil, nil)
		signerKey, signerCert = wrongKey, wrongCert
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	i.leafKey = leafKey

	ecdhPub, err := leafKey.PublicKey.ECDH()
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	keyIDFull := sha256.Sum256(ecdhPub.Bytes())
	keyID = keyIDFull[:]

	rpIDHash := sha256.Sum256([]byte(i.AppID))
	if p.BadAppID {
		rpIDHash = sha256.Sum256([]byte("WRONG.app.id"))
	}
	counter := p.Counter
	if p.BadCounter {
		counter = 1
	}
	credID := keyID
	if p.BadCredentialID {
		credID = make([]byte, 32)
	}
	authData := buildAttestationAuthData(rpIDHash[:], counter, p.AAGUID, credID)

	clientDataHash := sha256.Sum256(p.Challenge)
	composite := append(append([]byte{}, authData...), clientDataHash[:]...)
	nonce := sha256.Sum256(composite)
	nonceForExt := nonce[:]
	if p.BadNonce {
		nonceForExt = make([]byte, 32)
	}

	leafDER := signLeafWithNonce(t, leafKey, signerKey, signerCert, nonceForExt)

	fmtStr := "apple-appattest"
	if p.BadFmt {
		fmtStr = "wrong"
	}

	obj := attestationObject{
		Fmt:      fmtStr,
		AuthData: authData,
		AttStmt: attestationStmt{
			X5C:     [][]byte{leafDER, i.interDER},
			Receipt: []byte("test-receipt"),
		},
	}
	cborBytes, err = cbor.Marshal(obj)
	if err != nil {
		t.Fatalf("cbor marshal attestation: %v", err)
	}
	return keyID, cborBytes
}

// AssertionParams controls MintAssertion. Counter must be > the stored counter
// (which is 0 immediately after the first MintAttestation, then whatever the
// last successful VerifyAssertion left it at).
type AssertionParams struct {
	Counter   uint32
	Challenge []byte // must be set

	BadAppID bool
	BadSig   bool
}

// MintAssertion returns assertionCBOR signed by the leaf key from the most
// recent MintAttestation call.
func (i *Issuer) MintAssertion(t *testing.T, p AssertionParams) []byte {
	t.Helper()
	if i.leafKey == nil {
		t.Fatal("testkit: MintAttestation must be called before MintAssertion")
	}
	if p.Challenge == nil {
		t.Fatal("testkit: AssertionParams.Challenge required")
	}

	rpIDHash := sha256.Sum256([]byte(i.AppID))
	if p.BadAppID {
		rpIDHash = sha256.Sum256([]byte("WRONG.app.id"))
	}
	authData := buildAssertionAuthData(rpIDHash[:], p.Counter)

	clientDataHash := sha256.Sum256(p.Challenge)
	composite := append(append([]byte{}, authData...), clientDataHash[:]...)
	nonce := sha256.Sum256(composite)

	sig, err := ecdsa.SignASN1(rand.Reader, i.leafKey, nonce[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if p.BadSig {
		sig[len(sig)-1] ^= 0xFF
	}
	out, err := cbor.Marshal(assertionObject{Signature: sig, AuthData: authData})
	if err != nil {
		t.Fatalf("cbor marshal assertion: %v", err)
	}
	return out
}

// --- low-level helpers ---

func genCA(t *testing.T, cn string, parentCert *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey %q: %v", cn, err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<60))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	signer := tmpl
	signKey := key
	if parentCert != nil {
		signer = parentCert
		signKey = parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatalf("create cert %q: %v", cn, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert %q: %v", cn, err)
	}
	return key, cert, der
}

func signLeafWithNonce(t *testing.T, leafKey, parentKey *ecdsa.PrivateKey, parentCert *x509.Certificate, nonce []byte) []byte {
	t.Helper()
	type nonceWrapper struct {
		Inner []byte `asn1:"explicit,tag:1"`
	}
	extBytes, err := asn1.Marshal(nonceWrapper{Inner: nonce})
	if err != nil {
		t.Fatalf("marshal nonce ext: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<60))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Test App Attest Leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{
			{Id: ExtOIDAppAttestNonce, Value: extBytes},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, &leafKey.PublicKey, parentKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	return der
}

func buildAttestationAuthData(rpIDHash []byte, counter uint32, aaguid, credID []byte) []byte {
	buf := make([]byte, 0, 64+len(credID))
	buf = append(buf, rpIDHash...)
	buf = append(buf, flagAT)
	var ctr [4]byte
	binary.BigEndian.PutUint32(ctr[:], counter)
	buf = append(buf, ctr[:]...)
	buf = append(buf, aaguid...)
	var clen [2]byte
	binary.BigEndian.PutUint16(clen[:], uint16(len(credID)))
	buf = append(buf, clen[:]...)
	buf = append(buf, credID...)
	return buf
}

func buildAssertionAuthData(rpIDHash []byte, counter uint32) []byte {
	buf := make([]byte, 0, 37)
	buf = append(buf, rpIDHash...)
	buf = append(buf, 0)
	var ctr [4]byte
	binary.BigEndian.PutUint32(ctr[:], counter)
	buf = append(buf, ctr[:]...)
	return buf
}
