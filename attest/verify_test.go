package attest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

const testAppID = "TEAM12345.com.example.test"

// makeVerifier returns a Verifier wired to a fresh temp Store, with the
// challenge from `params` pre-recorded so VerifyAttestation can consume it.
// AllowDev is auto-set when the test mints a development-aaguid attestation.
func makeVerifier(t *testing.T, params attestParams) (*Verifier, attestArtifacts) {
	t.Helper()
	if params.AppID == "" {
		params.AppID = testAppID
	}
	art := mintAttestation(t, params)
	s := tempStore(t)
	if err := s.PutChallenge(art.Challenge, PurposeRegister, time.Now()); err != nil {
		t.Fatalf("put challenge: %v", err)
	}
	v, err := New(Options{
		AppID:    params.AppID,
		AllowDev: bytes.Equal(params.AAGUID, aaguidDevelopment),
		Store:    s,
		RootPool: art.RootPool,
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v, art
}

func TestVerifyAttestation_HappyPath(t *testing.T) {
	v, art := makeVerifier(t, attestParams{})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	cred, err := v.store.GetCredential(art.KeyID)
	if err != nil {
		t.Fatalf("get cred: %v", err)
	}
	if cred.Counter != 0 {
		t.Errorf("counter = %d, want 0", cred.Counter)
	}
	if len(cred.PublicKeyDER) == 0 {
		t.Error("publicKeyDER not stored")
	}
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); !errors.Is(err, ErrChallengeReplay) {
		t.Errorf("replay: got %v, want ErrChallengeReplay", err)
	}
}

// TestVerifyAttestation_ErrorMap exercises every sentinel error a corrupted
// attestation can produce (SPEC §1.4).
func TestVerifyAttestation_ErrorMap(t *testing.T) {
	cases := []struct {
		name   string
		params attestParams
		want   error
	}{
		{"BadFmt", attestParams{BadFmt: true}, ErrBadCBOR},
		{"BadCertChain", attestParams{BadCertChain: true}, ErrCertChain},
		{"BadNonce", attestParams{BadNonce: true}, ErrAttestNonce},
		{"BadAppID", attestParams{BadAppID: true}, ErrAppIDMismatch},
		{"BadCounter", attestParams{BadCounter: true}, ErrCounterNotZero},
		{"BadAAGUID", attestParams{AAGUID: []byte("nonsense00000000")}, ErrAAGUIDMismatch},
		{"BadCredentialID", attestParams{BadCredentialID: true}, ErrCredentialIDMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, art := makeVerifier(t, tc.params)
			err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyAttestation_BadCBOR(t *testing.T) {
	v, _ := makeVerifier(t, attestParams{})
	err := v.VerifyAttestation(make([]byte, 32), []byte("garbage"), make([]byte, 32))
	if !errors.Is(err, ErrBadCBOR) {
		t.Errorf("got %v, want ErrBadCBOR", err)
	}
}

func TestVerifyAttestation_ChallengeUnknown(t *testing.T) {
	art := mintAttestation(t, attestParams{})
	s := tempStore(t)
	v, err := New(Options{AppID: testAppID, Store: s, RootPool: art.RootPool})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); !errors.Is(err, ErrChallengeUnknown) {
		t.Errorf("got %v, want ErrChallengeUnknown", err)
	}
}

func TestVerifyAttestation_ChallengePurposeMismatch(t *testing.T) {
	// A challenge issued for "secrets" must not satisfy attestation register.
	art := mintAttestation(t, attestParams{})
	s := tempStore(t)
	if err := s.PutChallenge(art.Challenge, PurposeSecrets, time.Now()); err != nil {
		t.Fatal(err)
	}
	v, _ := New(Options{AppID: testAppID, Store: s, RootPool: art.RootPool})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); !errors.Is(err, ErrChallengeUnknown) {
		t.Errorf("got %v, want ErrChallengeUnknown", err)
	}
}

func TestVerifyAttestation_ChallengeExpired(t *testing.T) {
	art := mintAttestation(t, attestParams{})
	s := tempStore(t)
	issuedAt := time.Now().Add(-10 * time.Minute)
	if err := s.PutChallenge(art.Challenge, PurposeRegister, issuedAt); err != nil {
		t.Fatal(err)
	}
	v, _ := New(Options{
		AppID:    testAppID,
		Store:    s,
		RootPool: art.RootPool,
		TTL:      5 * time.Minute,
	})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("got %v, want ErrChallengeExpired", err)
	}
}

func TestVerifyAttestation_DevAAGUIDRejectedByDefault(t *testing.T) {
	art := mintAttestation(t, attestParams{AAGUID: aaguidDevelopment})
	s := tempStore(t)
	_ = s.PutChallenge(art.Challenge, PurposeRegister, time.Now())
	v, _ := New(Options{
		AppID:    testAppID,
		AllowDev: false,
		Store:    s,
		RootPool: art.RootPool,
	})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); !errors.Is(err, ErrAAGUIDMismatch) {
		t.Errorf("got %v, want ErrAAGUIDMismatch", err)
	}
}

func TestVerifyAttestation_DevAAGUIDAcceptedWhenAllowed(t *testing.T) {
	art := mintAttestation(t, attestParams{AAGUID: aaguidDevelopment})
	s := tempStore(t)
	_ = s.PutChallenge(art.Challenge, PurposeRegister, time.Now())
	v, _ := New(Options{
		AppID:    testAppID,
		AllowDev: true,
		Store:    s,
		RootPool: art.RootPool,
	})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

// --- assertion ---

// registerCredential is the common setup for assertion tests: complete a
// successful attestation, return verifier + the artifacts (key + keyID).
func registerCredential(t *testing.T) (*Verifier, attestArtifacts) {
	t.Helper()
	v, art := makeVerifier(t, attestParams{})
	if err := v.VerifyAttestation(art.KeyID, art.Cbor, art.Challenge); err != nil {
		t.Fatalf("register: %v", err)
	}
	return v, art
}

func TestVerifyAssertion_HappyPath(t *testing.T) {
	v, art := registerCredential(t)
	challenge, err := v.IssueChallenge(PurposeSecrets)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	asArt := mintAssertion(t, art.LeafKey, assertionParams{Counter: 1, Challenge: challenge})
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); err != nil {
		t.Fatalf("verify: %v", err)
	}
	cred, _ := v.store.GetCredential(art.KeyID)
	if cred.Counter != 1 {
		t.Errorf("counter = %d, want 1", cred.Counter)
	}
	// Resubmitting the same assertion bytes now fails the counter check first
	// (stored counter is 1, assertion counter is 1 → not strictly greater).
	// This is the deeper replay defense per SPEC §6.2.
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrCounterNotMonotonic) {
		t.Errorf("replay: got %v, want ErrCounterNotMonotonic", err)
	}
}

func TestVerifyAssertion_CredentialUnknown(t *testing.T) {
	v, _ := registerCredential(t)
	challenge, _ := v.IssueChallenge(PurposeSecrets)
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	asArt := mintAssertion(t, otherKey, assertionParams{Counter: 1, Challenge: challenge})
	unknownKey := bytes.Repeat([]byte{0xAA}, 32)
	if err := v.VerifyAssertion(unknownKey, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrCredentialUnknown) {
		t.Errorf("got %v, want ErrCredentialUnknown", err)
	}
}

func TestVerifyAssertion_CounterNotMonotonic(t *testing.T) {
	v, art := registerCredential(t)
	challenge, _ := v.IssueChallenge(PurposeSecrets)
	asArt := mintAssertion(t, art.LeafKey, assertionParams{Counter: 0, Challenge: challenge})
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrCounterNotMonotonic) {
		t.Errorf("got %v, want ErrCounterNotMonotonic", err)
	}
}

func TestVerifyAssertion_BadAppID(t *testing.T) {
	v, art := registerCredential(t)
	challenge, _ := v.IssueChallenge(PurposeSecrets)
	asArt := mintAssertion(t, art.LeafKey, assertionParams{Counter: 1, Challenge: challenge, BadAppID: true})
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrAppIDMismatch) {
		t.Errorf("got %v, want ErrAppIDMismatch", err)
	}
}

func TestVerifyAssertion_BadSignature(t *testing.T) {
	v, art := registerCredential(t)
	challenge, _ := v.IssueChallenge(PurposeSecrets)
	asArt := mintAssertion(t, art.LeafKey, assertionParams{Counter: 1, Challenge: challenge, BadSig: true})
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrBadSignature) {
		t.Errorf("got %v, want ErrBadSignature", err)
	}
}

func TestVerifyAssertion_BadCBOR(t *testing.T) {
	v, art := registerCredential(t)
	challenge, _ := v.IssueChallenge(PurposeSecrets)
	if err := v.VerifyAssertion(art.KeyID, []byte("garbage"), challenge); !errors.Is(err, ErrBadCBOR) {
		t.Errorf("got %v, want ErrBadCBOR", err)
	}
}

func TestVerifyAssertion_ChallengeReplayAcrossPurpose(t *testing.T) {
	// An assertion-flow challenge cannot be reused for register, and vice versa.
	v, art := registerCredential(t)
	// Issue a register-purpose challenge and try to use it in assertion verify.
	regChallenge, _ := v.IssueChallenge(PurposeRegister)
	asArt := mintAssertion(t, art.LeafKey, assertionParams{Counter: 1, Challenge: regChallenge})
	if err := v.VerifyAssertion(art.KeyID, asArt.Cbor, asArt.Challenge); !errors.Is(err, ErrChallengeUnknown) {
		t.Errorf("got %v, want ErrChallengeUnknown", err)
	}
}

// --- challenge / store sanity ---

func TestIssueChallenge_UniquePerCall(t *testing.T) {
	v, _ := makeVerifier(t, attestParams{})
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c, err := v.IssueChallenge(PurposeRegister)
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 32 {
			t.Fatalf("len %d, want 32", len(c))
		}
		if seen[string(c)] {
			t.Fatal("duplicate challenge")
		}
		seen[string(c)] = true
	}
}

func TestIssueChallenge_BadPurpose(t *testing.T) {
	v, _ := makeVerifier(t, attestParams{})
	if _, err := v.IssueChallenge("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestStore_PutChallengeAndConsume(t *testing.T) {
	s := tempStore(t)
	keyID := bytes.Repeat([]byte{1}, 32)
	challenge := bytes.Repeat([]byte{2}, 32)
	pubDER := []byte("der-fake")
	now := time.Now()
	if err := s.PutChallenge(challenge, PurposeRegister, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AtomicConsumeAndPutCredential(challenge, PurposeRegister, now, time.Minute, keyID, pubDER); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCredential(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Counter != 0 {
		t.Errorf("counter %d, want 0", c.Counter)
	}
	if !bytes.Equal(c.PublicKeyDER, pubDER) {
		t.Error("pubKeyDER mismatch")
	}
	// Replay fails.
	if err := s.AtomicConsumeAndPutCredential(challenge, PurposeRegister, now, time.Minute, keyID, pubDER); !errors.Is(err, ErrChallengeReplay) {
		t.Errorf("replay: got %v, want ErrChallengeReplay", err)
	}
}

func TestStore_GetCredential_Unknown(t *testing.T) {
	s := tempStore(t)
	_, err := s.GetCredential(bytes.Repeat([]byte{9}, 32))
	if !errors.Is(err, ErrCredentialUnknown) {
		t.Errorf("got %v, want ErrCredentialUnknown", err)
	}
}

func TestStore_SweepExpired(t *testing.T) {
	s := tempStore(t)
	base := time.Now()
	for i := 0; i < 5; i++ {
		ch := bytes.Repeat([]byte{byte(i)}, 32)
		if err := s.PutChallenge(ch, PurposeRegister, base.Add(-10*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Add one fresh challenge that should NOT be swept.
	fresh := bytes.Repeat([]byte{0xFF}, 32)
	if err := s.PutChallenge(fresh, PurposeRegister, base); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepExpired(base, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("removed %d, want 5", n)
	}
}

func TestAppleRootPoolBytes(t *testing.T) {
	// Smoke: the embedded PEM must parse into a pool with at least one cert.
	pool := AppleAppAttestRootPool()
	if pool == nil {
		t.Fatal("nil pool")
	}
	// We can't easily count certs in a CertPool, but we can verify the PEM
	// embed is exactly the file we downloaded — checked separately on disk.
}
