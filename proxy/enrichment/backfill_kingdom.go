package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// backfill_kingdom.go — one-shot repair of EXISTING plants_pending rows that were
// written before the `kingdom` field existed (source_version <= v5). New rows get
// it at generation time (service.go applyKingdom); rows already cached in Supabase
// ship `kingdom: null` until something stamps them. English requests self-heal on
// read (the per-request iNat lookup fills it in), but non-English rows never
// consult iNat, so a cached non-English mushroom would stay unflagged forever
// without this.
//
// For every plant with at least one served row lacking `kingdom` it asks iNat
// ONCE (authoritative, free — no LLM call), then patches every such language row:
// `kingdom`, plus — for fungi — the hard-filtered `uses_list` / `attributes`
// (proxy.SanitizeFungiDetail), so legacy v1 rows stop carrying culinary uses in
// storage too. Plants iNat cannot resolve are left untouched (still null) and
// reported; a re-run retries them. Idempotent: the list query selects on the
// missing field itself and the patch re-checks it.
//
// Run via cmd/backfill-kingdom (dry-run by default). It is a standalone one-shot,
// NOT part of the resident service.

// kingdomBackfillLookupTimeout bounds one iNat lookup; kingdomBackfillWriteTimeout
// one row patch.
const (
	kingdomBackfillLookupTimeout = 10 * time.Second
	kingdomBackfillWriteTimeout  = 15 * time.Second
)

// KingdomBackfillRow is one served plants_pending row whose data has no kingdom.
type KingdomBackfillRow struct {
	Normalized     string             // PK part 1
	Lang           string             // PK part 2
	ScientificName string             // original (un-normalized) name, the iNat query
	Data           *proxy.PlantDetail // decoded payload (for the fungi hard filter)
}

// KingdomBackfillDB is the slice of the DB layer the backfill needs.
// *DB satisfies it; tests substitute a stub.
type KingdomBackfillDB interface {
	ListKingdomBackfillRows(ctx context.Context) ([]KingdomBackfillRow, error)
	PatchKingdom(ctx context.Context, normalized, lang string, patch map[string]any) (int64, error)
}

// KingdomResolver is the authoritative kingdom source. *proxy.INatClient
// satisfies it; tests substitute a stub.
type KingdomResolver interface {
	LookupTaxon(ctx context.Context, sciName string) (proxy.INatTaxon, bool)
}

// KingdomBackfillReport summarizes a run for the operator / caller.
type KingdomBackfillReport struct {
	Plants       int // distinct plants with >= 1 row lacking kingdom
	Rows         int // rows lacking kingdom
	Fungi        int // plants resolved to Fungi
	Plantae      int // plants resolved to Plantae
	Undetermined int // plants iNat could not resolve (rows left null; retried next run)
	Updated      int // rows patched (apply) / that would be patched (dry-run)
	Sanitized    int // of Updated: fungal rows that also lost edible/culinary content
	Vanished     int // patch matched 0 rows — changed between list and write (benign)
	Failed       int // row patch errored (logged, run continues)
}

// RunKingdomBackfill stamps `kingdom` on every served row that lacks it. apply=false
// is a dry-run: iNat is still consulted (read-only) so the report shows exactly
// what WOULD change, but nothing is written. interval is the pause between iNat
// lookups (be a polite API citizen; <= 0 disables it, used by tests). logf
// receives progress lines (pass log.Printf; nil is a no-op). The returned error is
// non-nil only on the initial list query or context cancellation; per-plant and
// per-row failures are counted and the run continues.
func RunKingdomBackfill(ctx context.Context, db KingdomBackfillDB, resolver KingdomResolver, apply bool, interval time.Duration, logf func(format string, args ...any)) (KingdomBackfillReport, error) {
	var rep KingdomBackfillReport
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if db == nil || resolver == nil {
		return rep, fmt.Errorf("enrichment kingdom backfill: nil db or resolver")
	}
	rows, err := db.ListKingdomBackfillRows(ctx)
	if err != nil {
		return rep, fmt.Errorf("enrichment kingdom backfill: list rows: %w", err)
	}
	rep.Rows = len(rows)

	// Group by plant, preserving first-seen order: the kingdom is a property of
	// the species, so one lookup serves every language row.
	var order []string
	byPlant := make(map[string][]KingdomBackfillRow)
	for _, r := range rows {
		if _, seen := byPlant[r.Normalized]; !seen {
			order = append(order, r.Normalized)
		}
		byPlant[r.Normalized] = append(byPlant[r.Normalized], r)
	}
	rep.Plants = len(order)
	logf("kingdom backfill: %d rows across %d plants lack kingdom", rep.Rows, rep.Plants)

	verb := "WOULD UPDATE"
	if apply {
		verb = "UPDATE"
	}
	for i, normalized := range order {
		if err := ctx.Err(); err != nil {
			return rep, err // shutting down — stop cleanly, resume next run
		}
		if i > 0 && interval > 0 {
			select {
			case <-ctx.Done():
				return rep, ctx.Err()
			case <-time.After(interval):
			}
		}
		group := byPlant[normalized]

		kingdom := resolveKingdomForBackfill(ctx, resolver, group[0].ScientificName, normalized)
		if kingdom == nil {
			rep.Undetermined++
			logf("UNDETERMINED %s (%q): iNat has no exact match — left null", normalized, group[0].ScientificName)
			continue
		}
		if proxy.IsFungi(kingdom) {
			rep.Fungi++
		} else {
			rep.Plantae++
		}

		for _, r := range group {
			patch, sanitized := kingdomPatch(r.Data, kingdom)
			logf("%s %s/%s: kingdom=%s sanitized=%v", verb, r.Normalized, r.Lang, *kingdom, sanitized)
			if !apply {
				rep.Updated++
				if sanitized {
					rep.Sanitized++
				}
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, kingdomBackfillWriteTimeout)
			n, err := db.PatchKingdom(wctx, r.Normalized, r.Lang, patch)
			cancel()
			if err != nil {
				rep.Failed++
				logf("FAIL %s/%s: %v", r.Normalized, r.Lang, err)
				continue
			}
			if n == 0 {
				rep.Vanished++
				continue
			}
			rep.Updated++
			if sanitized {
				rep.Sanitized++
			}
		}
	}
	logf("kingdom backfill: done plants=%d rows=%d fungi=%d plantae=%d undetermined=%d updated=%d sanitized=%d vanished=%d failed=%d apply=%v",
		rep.Plants, rep.Rows, rep.Fungi, rep.Plantae, rep.Undetermined, rep.Updated, rep.Sanitized, rep.Vanished, rep.Failed, apply)
	return rep, nil
}

// resolveKingdomForBackfill asks the resolver for the stored scientific name and,
// on a miss, once more for the normalized species form (the stored name can carry
// an infraspecific rank or stray formatting that defeats iNat's exact-name match).
// nil when neither resolves to Fungi / Plantae.
func resolveKingdomForBackfill(ctx context.Context, resolver KingdomResolver, scientificName, normalized string) *string {
	for _, q := range []string{scientificName, normalized} {
		if q == "" {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, kingdomBackfillLookupTimeout)
		t, ok := resolver.LookupTaxon(lctx, q)
		cancel()
		if !ok {
			continue
		}
		if k := proxy.NormalizeKingdom(t.Kingdom); k != nil {
			return k
		}
	}
	return nil
}

// kingdomPatch builds the top-level JSONB merge patch for one row: always
// `kingdom`; for a fungal row whose payload holds edible/culinary content, also
// the hard-filtered `uses_list` / `attributes`. Pure (no DB) so the decision is
// unit-testable. data is not mutated. sanitized reports whether food content was
// removed.
func kingdomPatch(data *proxy.PlantDetail, kingdom *string) (patch map[string]any, sanitized bool) {
	patch = map[string]any{"kingdom": *kingdom}
	if data == nil {
		return patch, false
	}
	cp := *data
	cp.Kingdom = kingdom
	if proxy.SanitizeFungiDetail(&cp) {
		patch["uses_list"] = cp.UsesList
		patch["attributes"] = cp.Attributes
		return patch, true
	}
	return patch, false
}

// ListKingdomBackfillRows returns every served (pending/approved) row whose data
// has no kingdom (key absent or JSON null — `->>` yields SQL NULL for both).
// Read-only; safe to call repeatedly.
func (d *DB) ListKingdomBackfillRows(ctx context.Context) ([]KingdomBackfillRow, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		SELECT scientific_name_normalized, lang, scientific_name, data
		FROM plants_pending
		WHERE status IN ('pending', 'approved')
		  AND data->>'kingdom' IS NULL
		ORDER BY scientific_name_normalized, lang`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: list kingdom backfill: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()
	var out []KingdomBackfillRow
	for rows.Next() {
		var r KingdomBackfillRow
		var raw []byte
		if err := rows.Scan(&r.Normalized, &r.Lang, &r.ScientificName, &raw); err != nil {
			return nil, fmt.Errorf("%w: scan kingdom row: %v", ErrDBUnavailable, err)
		}
		var pd proxy.PlantDetail
		if err := json.Unmarshal(raw, &pd); err != nil {
			return nil, fmt.Errorf("%w: decode kingdom row %s/%s: %v", ErrDBUnavailable, r.Normalized, r.Lang, err)
		}
		r.Data = &pd
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate kingdom rows: %v", ErrDBUnavailable, err)
	}
	return out, nil
}

// PatchKingdom merges patch into one row's data JSONB (`data || patch` replaces
// only the top-level keys in patch — every other field is preserved byte-for-byte,
// unlike a struct round-trip). The WHERE re-checks both the served status and the
// still-missing kingdom, so a row rejected or stamped between list and write is
// left alone. Returns the number of rows updated.
func (d *DB) PatchKingdom(ctx context.Context, normalized, lang string, patch map[string]any) (int64, error) {
	if d == nil || d.pool == nil {
		return 0, ErrDBUnavailable
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return 0, fmt.Errorf("enrichment/db: marshal kingdom patch: %w", err)
	}
	const stmt = `
		UPDATE plants_pending
		SET data = data || $3::jsonb
		WHERE scientific_name_normalized = $1 AND lang = $2
		  AND status IN ('pending', 'approved')
		  AND data->>'kingdom' IS NULL`
	tag, err := d.pool.Exec(ctx, stmt, normalized, lang, raw)
	if err != nil {
		return 0, fmt.Errorf("%w: patch kingdom: %v", ErrDBUnavailable, err)
	}
	return tag.RowsAffected(), nil
}
