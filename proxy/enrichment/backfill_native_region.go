package enrichment

import (
	"context"
	"fmt"
	"log"
	"time"
)

// One-time native_region localization backfill (SPEC §7 v5).
//
// Before v5, native_region was a canonical English free-text array copied
// verbatim into every language row. v5 localizes it. New rows are generated /
// translated correctly going forward; this backfill fixes the rows already in
// plants_pending: every non-English row written under <= v4 still carries
// English place names.
//
// The existing async Backfiller cannot do this — it INSERTs ON CONFLICT DO
// NOTHING, so it never updates an existing row. This runner is surgical: it
// translates ONLY the native_region array (TranslateRegions) and patches it in
// place via jsonb_set, leaving the row's already-correct localized prose
// untouched. Idempotent: it stamps each patched row with the current
// PromptVersion (>= v5), and the list query selects only source_version < 'v5',
// so a re-run resumes where a crash left off and a fully-backfilled DB is a no-op.

// nativeRegionBackfillTimeout bounds one translate+update round-trip per row.
const nativeRegionBackfillTimeout = 60 * time.Second

// NativeRegionBackfillDB is the slice of the DB layer the backfill needs.
// *DB satisfies it; tests substitute a stub.
type NativeRegionBackfillDB interface {
	ListNativeRegionBackfillRows(ctx context.Context) ([]NativeRegionRow, error)
	UpdateNativeRegion(ctx context.Context, normalized, lang string, regions []string, version string) (int64, error)
}

// NativeRegionBackfillLLM is the slice of the LLM layer the backfill needs.
// *LLMClient satisfies it; tests substitute a stub.
type NativeRegionBackfillLLM interface {
	TranslateRegions(ctx context.Context, regions []string, toLang string) ([]string, string, error)
}

// NativeRegionBackfillReport summarizes a run for the operator / caller.
type NativeRegionBackfillReport struct {
	Total    int // candidate rows listed
	Updated  int // rows successfully localized + patched
	Skipped  int // translation returned a mismatched element count (retried next run)
	Vanished int // UPDATE matched 0 rows — row deleted/changed between list and update (benign)
	Failed   int // translate or update errored (logged, run continues)
}

// RunNativeRegionBackfill localizes native_region on every legacy non-English
// row. It processes rows sequentially (volume is small — out-of-catalog plants
// only — and sequential keeps OpenAI load trivial and the run easy to reason
// about). Per-row failures are logged and skipped; the run never aborts on a
// single bad row. Returns a report plus the first fatal error (only the initial
// list query is fatal — once rows are in hand, every row is best-effort).
func RunNativeRegionBackfill(ctx context.Context, db NativeRegionBackfillDB, llm NativeRegionBackfillLLM) (NativeRegionBackfillReport, error) {
	var rep NativeRegionBackfillReport
	if db == nil || llm == nil {
		return rep, fmt.Errorf("enrichment native_region backfill: nil db or llm")
	}
	rows, err := db.ListNativeRegionBackfillRows(ctx)
	if err != nil {
		return rep, fmt.Errorf("enrichment native_region backfill: list rows: %w", err)
	}
	rep.Total = len(rows)
	log.Printf("enrichment native_region backfill: %d legacy non-en rows to localize", rep.Total)

	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return rep, err // process is shutting down — stop cleanly, resume next run
		}
		rowCtx, cancel := context.WithTimeout(ctx, nativeRegionBackfillTimeout)
		translated, _, err := llm.TranslateRegions(rowCtx, r.NativeRegion, r.Lang)
		if err != nil {
			cancel()
			rep.Failed++
			log.Printf("enrichment native_region backfill: translate failed normalized=%q lang=%s err=%v",
				r.Normalized, r.Lang, err)
			continue
		}
		// Guard arity: the schema does not pin element count, so a model that drops
		// or invents an element would corrupt the array. Skip (leave English, retry
		// on a later run) rather than persist a mismatched list.
		if len(translated) != len(r.NativeRegion) {
			rep.Skipped++
			log.Printf("enrichment native_region backfill: arity mismatch normalized=%q lang=%s want=%d got=%d — skipped",
				r.Normalized, r.Lang, len(r.NativeRegion), len(translated))
			cancel()
			continue
		}
		n, err := db.UpdateNativeRegion(rowCtx, r.Normalized, r.Lang, translated, PromptVersion)
		cancel()
		if err != nil {
			rep.Failed++
			log.Printf("enrichment native_region backfill: update failed normalized=%q lang=%s err=%v",
				r.Normalized, r.Lang, err)
			continue
		}
		if n == 0 {
			rep.Vanished++
			continue
		}
		rep.Updated++
	}
	log.Printf("enrichment native_region backfill: done total=%d updated=%d skipped=%d vanished=%d failed=%d",
		rep.Total, rep.Updated, rep.Skipped, rep.Vanished, rep.Failed)
	return rep, nil
}
