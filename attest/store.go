package attest

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketCredentials = []byte("credentials")
	bucketChallenges  = []byte("challenges")
)

// Credential is the persisted record for one registered App Attest key.
type Credential struct {
	PublicKeyDER []byte
	Counter      uint32
	RegisteredAt time.Time
}

// challengeRec is the persisted record for an issued challenge.
type challengeRec struct {
	Purpose  string
	IssuedAt time.Time
	Consumed bool
}

// Store persists credentials + challenges in a single BoltDB file (SPEC §5).
type Store struct {
	db *bbolt.DB
}

// OpenStore opens (or creates) the BoltDB file at path with chmod 0600.
func OpenStore(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("attest: open bbolt: %w", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketCredentials, bucketChallenges} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("attest: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the BoltDB file lock.
func (s *Store) Close() error { return s.db.Close() }

// PutChallenge records a freshly issued challenge with its purpose tag.
func (s *Store) PutChallenge(challenge []byte, purpose string, issuedAt time.Time) error {
	rec := challengeRec{Purpose: purpose, IssuedAt: issuedAt}
	encoded, err := encodeGob(rec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketChallenges).Put(challenge, encoded)
	})
}

// GetCredential reads a credential by keyID. Returns ErrCredentialUnknown if absent.
func (s *Store) GetCredential(keyID []byte) (Credential, error) {
	var out Credential
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketCredentials).Get(keyID)
		if raw == nil {
			return ErrCredentialUnknown
		}
		return decodeGob(raw, &out)
	})
	return out, err
}

// AtomicConsumeAndPutCredential consumes the challenge (must exist, not consumed,
// not expired, purpose match) AND writes a fresh credential in one bbolt tx.
// Used at the end of attestation verification (SPEC §3.1).
func (s *Store) AtomicConsumeAndPutCredential(
	challenge []byte, purpose string, now time.Time, ttl time.Duration,
	keyID, pubKeyDER []byte,
) error {
	credBytes, err := encodeGob(Credential{
		PublicKeyDER: pubKeyDER,
		Counter:      0,
		RegisteredAt: now,
	})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := consumeChallengeTx(tx, challenge, purpose, now, ttl); err != nil {
			return err
		}
		return tx.Bucket(bucketCredentials).Put(keyID, credBytes)
	})
}

// AtomicConsumeAndUpdateCounter consumes the challenge AND atomically updates
// the credential counter (must be strictly > stored). Used at the end of
// assertion verification (SPEC §3.2 + §6.2).
func (s *Store) AtomicConsumeAndUpdateCounter(
	challenge []byte, purpose string, now time.Time, ttl time.Duration,
	keyID []byte, newCounter uint32,
) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := consumeChallengeTx(tx, challenge, purpose, now, ttl); err != nil {
			return err
		}
		credB := tx.Bucket(bucketCredentials)
		raw := credB.Get(keyID)
		if raw == nil {
			return ErrCredentialUnknown
		}
		var cred Credential
		if err := decodeGob(raw, &cred); err != nil {
			return err
		}
		if newCounter <= cred.Counter {
			return ErrCounterNotMonotonic
		}
		cred.Counter = newCounter
		updated, err := encodeGob(cred)
		if err != nil {
			return err
		}
		return credB.Put(keyID, updated)
	})
}

// consumeChallengeTx is a tx-internal helper: look up, validate, mark consumed.
// Caller already holds an Update tx.
func consumeChallengeTx(tx *bbolt.Tx, challenge []byte, expectedPurpose string, now time.Time, ttl time.Duration) error {
	b := tx.Bucket(bucketChallenges)
	raw := b.Get(challenge)
	if raw == nil {
		return ErrChallengeUnknown
	}
	var rec challengeRec
	if err := decodeGob(raw, &rec); err != nil {
		return err
	}
	if rec.Purpose != expectedPurpose {
		return ErrChallengeUnknown
	}
	if rec.Consumed {
		return ErrChallengeReplay
	}
	if now.Sub(rec.IssuedAt) > ttl {
		return ErrChallengeExpired
	}
	rec.Consumed = true
	updated, err := encodeGob(rec)
	if err != nil {
		return err
	}
	return b.Put(challenge, updated)
}

// SweepExpired deletes challenges older than TTL. Safe to call from a
// background goroutine (each sweep is its own transaction).
func (s *Store) SweepExpired(now time.Time, ttl time.Duration) (int, error) {
	var removed int
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChallenges)
		c := b.Cursor()
		var toDelete [][]byte
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec challengeRec
			if err := decodeGob(v, &rec); err != nil {
				continue
			}
			if now.Sub(rec.IssuedAt) > ttl {
				kCopy := make([]byte, len(k))
				copy(kCopy, k)
				toDelete = append(toDelete, kCopy)
			}
		}
		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("attest: gob encode: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeGob(b []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(b)).Decode(v)
}
