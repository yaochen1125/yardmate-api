// Package training is the server-side intake for the user-photo flywheel
// (identify-accuracy L1/L2 data layer). It receives opt-in user identification
// photos + labels, strips their metadata, stores them on the local training
// corpus, and records one metadata row per photo. The L1 re-embed job (P2b) and
// the 7788 dashboard (P2d) read the same SQLite file this package writes.
//
// See SPEC.md. Intake + storage + deletion only; embedding + dashboard live
// elsewhere and consume this store read-only.
package training

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; required for CGO_ENABLED=0 builds
)

// Store persists training photos on the local corpus + their metadata rows.
type Store struct {
	dir string  // training root, e.g. /var/lib/yardmate-api/training
	db  *sql.DB // training_meta.db (WAL)
}

// PhotoMeta is the per-upload metadata recorded alongside the image bytes.
// Empty strings / nil are stored as SQL NULL where the column is nullable.
type PhotoMeta struct {
	InstallID      string
	UserID         string // optional soft attribution (SPEC §5)
	LabelKind      string // accept | correct | none
	PlantID        string
	ScientificName string
	SourceEngine   string
	TopConfidence  *float64
	ConsentVersion string
	AppVersion     string
}

// Stats is a cheap snapshot for /healthz-style introspection + the dashboard.
type Stats struct {
	Total    int64 `json:"total"`    // live rows (deleted=0)
	Embedded int64 `json:"embedded"` // already in the vision index
	Pending  int64 `json:"pending"`  // live, not yet embedded
}

const schema = `
CREATE TABLE IF NOT EXISTS photos (
  id              TEXT PRIMARY KEY,
  rel_path        TEXT NOT NULL,
  install_id      TEXT,
  user_id         TEXT,
  label_kind      TEXT NOT NULL,
  plant_id        TEXT,
  scientific_name TEXT,
  source_engine   TEXT,
  top_confidence  REAL,
  consent_version TEXT,
  app_version     TEXT,
  bytes           INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  embedded        INTEGER NOT NULL DEFAULT 0,
  embedded_at     INTEGER,
  exported        INTEGER NOT NULL DEFAULT 0,
  exported_at     INTEGER,
  deleted         INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_photos_install ON photos(install_id) WHERE deleted = 0;
CREATE INDEX IF NOT EXISTS idx_photos_user    ON photos(user_id)    WHERE deleted = 0;
CREATE INDEX IF NOT EXISTS idx_photos_embed   ON photos(embedded)   WHERE deleted = 0;
`

// OpenStore opens (creating if needed) the training corpus at dir: the photos/
// tree, the SQLite metadata db (WAL), and the schema. Safe to call at startup.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("training: empty dir")
	}
	if err := os.MkdirAll(filepath.Join(dir, "photos"), 0o755); err != nil {
		return nil, fmt.Errorf("training: mkdir: %w", err)
	}
	dbPath := filepath.Join(dir, "training_meta.db")
	// WAL + busy_timeout so the Python embed job (a separate process) can read
	// concurrently with our writes. modernc uses the _pragma= query form.
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("training: open db: %w", err)
	}
	// Single writer connection — modernc can surface "database is locked" under
	// concurrent in-process writers; cross-process reads are handled by WAL.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("training: schema: %w", err)
	}
	return &Store{dir: dir, db: db}, nil
}

// Close closes the underlying db.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Insert writes the (already metadata-stripped) jpeg to the corpus and records
// its row. Returns the server-generated photo id. On DB failure the file is
// removed so no orphan is left behind.
func (s *Store) Insert(ctx context.Context, m PhotoMeta, jpeg []byte) (string, error) {
	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("training: id: %w", err)
	}
	now := time.Now()
	rel := filepath.Join("photos", now.Format("2006"), now.Format("01"), id+".jpg")
	abs := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("training: mkdir photo: %w", err)
	}
	if err := writeFileAtomic(abs, jpeg); err != nil {
		return "", fmt.Errorf("training: write photo: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO photos (id, rel_path, install_id, user_id, label_kind, plant_id,
  scientific_name, source_engine, top_confidence, consent_version, app_version,
  bytes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, filepath.ToSlash(rel), nullStr(m.InstallID), nullStr(m.UserID),
		labelKind(m.LabelKind), nullStr(m.PlantID), nullStr(m.ScientificName),
		nullStr(m.SourceEngine), nullFloat(m.TopConfidence), nullStr(m.ConsentVersion),
		nullStr(m.AppVersion), len(jpeg), now.Unix())
	if err != nil {
		_ = os.Remove(abs) // no orphan file without a row
		return "", fmt.Errorf("training: insert: %w", err)
	}
	return id, nil
}

// DeleteByInstallID removes every live photo uploaded by a device: the files are
// deleted immediately and the rows are tombstoned (PII nulled, id + embedded
// kept so P2b can drop the vector). Returns the number of photos deleted.
func (s *Store) DeleteByInstallID(ctx context.Context, installID string) (int, error) {
	return s.deleteWhere(ctx, "install_id", installID)
}

// DeleteByUserID is the account-deletion cascade (called from account_delete.go
// after the Bearer token is verified). Same tombstone semantics.
func (s *Store) DeleteByUserID(ctx context.Context, userID string) (int, error) {
	return s.deleteWhere(ctx, "user_id", userID)
}

func (s *Store) deleteWhere(ctx context.Context, col, val string) (int, error) {
	if val == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, rel_path FROM photos WHERE "+col+" = ? AND deleted = 0", val)
	if err != nil {
		return 0, fmt.Errorf("training: select for delete: %w", err)
	}
	type ref struct{ id, rel string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.rel); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("training: scan for delete: %w", err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	now := time.Now().Unix()
	n := 0
	for _, r := range refs {
		// Remove the bytes first (privacy); a missing file is not fatal.
		if r.rel != "" {
			if err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(r.rel))); err != nil && !os.IsNotExist(err) {
				return n, fmt.Errorf("training: remove file: %w", err)
			}
		}
		// Tombstone: null the PII columns, keep id + embedded for index cleanup.
		if _, err := s.db.ExecContext(ctx, `
UPDATE photos SET deleted = 1, deleted_at = ?, rel_path = '',
  install_id = NULL, user_id = NULL, plant_id = NULL, scientific_name = NULL,
  source_engine = NULL, consent_version = NULL, app_version = NULL
WHERE id = ?`, now, r.id); err != nil {
			return n, fmt.Errorf("training: tombstone: %w", err)
		}
		n++
	}
	return n, nil
}

// Stats returns live/embedded/pending counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `
SELECT
  COUNT(*),
  COALESCE(SUM(CASE WHEN embedded = 1 THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN embedded = 0 THEN 1 ELSE 0 END), 0)
FROM photos WHERE deleted = 0`).Scan(&st.Total, &st.Embedded, &st.Pending)
	if err != nil {
		return Stats{}, fmt.Errorf("training: stats: %w", err)
	}
	return st, nil
}

// --- helpers ---

// newID returns a random RFC-4122 v4 UUID string (crypto/rand). We generate the
// photo id server-side; the client device id is never the photo id (SPEC §2.4).
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// writeFileAtomic writes data to a temp file in the same dir then renames it
// into place, so a reader never sees a partial file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func labelKind(s string) string {
	switch s {
	case "accept", "correct":
		return s
	default:
		return "none"
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
