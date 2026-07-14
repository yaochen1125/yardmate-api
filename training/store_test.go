package training

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countPhotoFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, "photos"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jpg") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return n
}

// readStored reads a stored photo by its DB-relative (slash) path.
func readStored(t *testing.T, st *Store, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(st.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read stored %s: %v", rel, err)
	}
	return b
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestInsertAndStats(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	conf := 0.83
	id, err := st.Insert(ctx, PhotoMeta{
		InstallID: "dev-1", LabelKind: "accept", PlantID: "AAA0701",
		ScientificName: "Juncus effusus", SourceEngine: "plantnet",
		TopConfidence: &conf, ConsentVersion: "2026-07-14", AppVersion: "1.3.0",
	}, []byte("fake-jpeg-bytes"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	if got := countPhotoFiles(t, st.dir); got != 1 {
		t.Fatalf("expected 1 photo file, got %d", got)
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Total != 1 || stats.Pending != 1 || stats.Embedded != 0 {
		t.Errorf("stats = %+v, want total1/pending1/embedded0", stats)
	}
}

func TestDeleteByInstallIDRemovesFileAndTombstones(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := st.Insert(ctx, PhotoMeta{InstallID: "dev-A", ConsentVersion: "v1"}, []byte("x")); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if _, err := st.Insert(ctx, PhotoMeta{InstallID: "dev-B", ConsentVersion: "v1"}, []byte("y")); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	n, err := st.DeleteByInstallID(ctx, "dev-A")
	if err != nil {
		t.Fatalf("DeleteByInstallID: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	// dev-B's file remains; dev-A's three files removed.
	if got := countPhotoFiles(t, st.dir); got != 1 {
		t.Errorf("photo files after delete = %d, want 1", got)
	}
	stats, _ := st.Stats(ctx)
	if stats.Total != 1 {
		t.Errorf("live total after delete = %d, want 1", stats.Total)
	}
	// Tombstone nulls PII: no dev-A rows should be findable by install id.
	var live int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM photos WHERE install_id = 'dev-A'").Scan(&live); err != nil {
		t.Fatalf("query: %v", err)
	}
	if live != 0 {
		t.Errorf("dev-A install_id still present in %d rows (PII not nulled)", live)
	}
}

func TestDeleteByUserID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.Insert(ctx, PhotoMeta{InstallID: "d1", UserID: "user-9", ConsentVersion: "v1"}, []byte("x")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Insert(ctx, PhotoMeta{InstallID: "d2", ConsentVersion: "v1"}, []byte("y")); err != nil { // no userID
		t.Fatalf("insert: %v", err)
	}
	n, err := st.DeleteByUserID(ctx, "user-9")
	if err != nil {
		t.Fatalf("DeleteByUserID: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	// Deleting an unknown / empty user id is a no-op, not an error.
	if got, err := st.DeleteByUserID(ctx, "nobody"); err != nil || got != 0 {
		t.Errorf("delete unknown user: got (%d,%v), want (0,nil)", got, err)
	}
	if got, err := st.DeleteByUserID(ctx, ""); err != nil || got != 0 {
		t.Errorf("delete empty user: got (%d,%v), want (0,nil)", got, err)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.Insert(context.Background(), PhotoMeta{InstallID: "d", ConsentVersion: "v1"}, []byte("x")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = st.Close()

	st2, err := OpenStore(dir) // schema IF NOT EXISTS + row survives
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	stats, err := st2.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 1 {
		t.Errorf("total after reopen = %d, want 1", stats.Total)
	}
}
