package attest

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
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Most negative tests work by minting a known-good attestation with one field
// corrupted. attestParams holds both happy-path knobs and corruption hooks.
type attestParams struct {
	AppID     string // default testAppID
	AAGUID    []byte // default aaguidProduction
	Counter   uint32
	Challenge []byte // default 32 random bytes

	// Corruption hooks for negative tests.
	BadCertChain    bool // intermediate signed by an untrusted root
	BadAppID        bool // rpIDHash = SHA256("wrong")
	BadCounter      bool // counter = 1 instead of 0
	BadCredentialID bool // authData.credentialID = zero bytes (≠ keyID)
	BadNonce        bool // cert nonce extension = zero bytes
	BadFmt          bool // attestation fmt = "wrong"
}

type attestArtifacts struct {
	KeyID     []byte
	Cbor      []byte
	Challenge []byte
	LeafKey   *ecdsa.PrivateKey
	RootPool  *x509.CertPool
}

// mintAttestation produces a CBOR attestation that the Verifier will accept
// (when given the returned RootPool), along with the matching keyID + leaf key
// for follow-up assertion tests.
func mintAttestation(t *testing.T, p attestParams) attestArtifacts {
	t.Helper()
	if p.AppID == "" {
		p.AppID = testAppID
	}
	if p.AAGUID == nil {
		p.AAGUID = aaguidProduction
	}
	if p.Challenge == nil {
		c := make([]byte, 32)
		if _, err := rand.Read(c); err != nil {
			t.Fatalf("rand challenge: %v", err)
		}
		p.Challenge = c
	}

	rootKey, rootCert, _ := genCA(t, "Test Apple Root", nil, nil)

	// Intermediate cert signed by the real root. When BadCertChain is set, we
	// sign it with a different root that's NOT in the verifier's pool, so
	// chain validation against the real root fails.
	interSigner, interSignerCert := rootKey, rootCert
	if p.BadCertChain {
		wrongKey, wrongCert, _ := genCA(t, "Untrusted Root", nil, nil)
		interSigner, interSignerCert = wrongKey, wrongCert
	}
	interKey, interCert, interDER := genCA(t, "Test Apple App Attest CA", interSignerCert, interSigner)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}

	// keyID = SHA256(uncompressed point of leaf pubkey).
	ecdhPub, err := leafKey.PublicKey.ECDH()
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	keyIDFull := sha256.Sum256(ecdhPub.Bytes())
	keyID := keyIDFull[:]

	rpIDHash := sha256.Sum256([]byte(p.AppID))
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

	// Real nonce that the Verifier will compute.
	clientDataHash := sha256.Sum256(p.Challenge)
	composite := append(append([]byte{}, authData...), clientDataHash[:]...)
	nonce := sha256.Sum256(composite)
	nonceForExt := nonce[:]
	if p.BadNonce {
		nonceForExt = make([]byte, 32)
	}

	leafDER := signLeafWithNonce(t, leafKey, interKey, interCert, nonceForExt)

	fmtStr := "apple-appattest"
	if p.BadFmt {
		fmtStr = "wrong"
	}

	attObj := attestationObject{
		Fmt:      fmtStr,
		AuthData: authData,
		AttStmt: attestationStmt{
			X5C:     [][]byte{leafDER, interDER},
			Receipt: []byte("test-receipt"),
		},
	}
	cborBytes, err := cbor.Marshal(attObj)
	if err != nil {
		t.Fatalf("cbor marshal: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(rootCert)
	return attestArtifacts{
		KeyID:     keyID,
		Cbor:      cborBytes,
		Challenge: p.Challenge,
		LeafKey:   leafKey,
		RootPool:  pool,
	}
}

// genCA generates a CA key + cert. If parentCert/parentKey are nil, the cert
// is self-signed (use this for the root). Otherwise the cert is signed by parent.
func genCA(t *testing.T, cn string, parentCert *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
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
		t.Fatalf("create ca cert %q: %v", cn, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	return key, cert, der
}

// signLeafWithNonce produces a leaf cert (signed by parent) carrying the App
// Attest nonce extension.
func signLeafWithNonce(t *testing.T, leafKey, parentKey *ecdsa.PrivateKey, parentCert *x509.Certificate, nonce []byte) []byte {
	t.Helper()
	// Apple's extension is a SEQUENCE wrapping a [1] EXPLICIT OCTET STRING.
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
			{Id: extOIDAppAttestNonce, Value: extBytes},
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
	buf = append(buf, 0) // no AT flag
	var ctr [4]byte
	binary.BigEndian.PutUint32(ctr[:], counter)
	buf = append(buf, ctr[:]...)
	return buf
}

type assertionParams struct {
	AppID     string
	Counter   uint32
	Challenge []byte

	BadAppID bool
	BadSig   bool
}

type assertionArtifacts struct {
	Cbor      []byte
	Challenge []byte
}

func mintAssertion(t *testing.T, key *ecdsa.PrivateKey, p assertionParams) assertionArtifacts {
	t.Helper()
	if p.AppID == "" {
		p.AppID = testAppID
	}
	if p.Challenge == nil {
		c := make([]byte, 32)
		if _, err := rand.Read(c); err != nil {
			t.Fatalf("rand challenge: %v", err)
		}
		p.Challenge = c
	}
	rpIDHash := sha256.Sum256([]byte(p.AppID))
	if p.BadAppID {
		rpIDHash = sha256.Sum256([]byte("WRONG.app.id"))
	}
	authData := buildAssertionAuthData(rpIDHash[:], p.Counter)

	clientDataHash := sha256.Sum256(p.Challenge)
	composite := append(append([]byte{}, authData...), clientDataHash[:]...)
	nonce := sha256.Sum256(composite)

	sig, err := ecdsa.SignASN1(rand.Reader, key, nonce[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if p.BadSig {
		sig[len(sig)-1] ^= 0xFF
	}
	obj := assertionObject{Signature: sig, AuthData: authData}
	b, err := cbor.Marshal(obj)
	if err != nil {
		t.Fatalf("cbor: %v", err)
	}
	return assertionArtifacts{Cbor: b, Challenge: p.Challenge}
}

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
