package enrichment

import (
	"context"
	"log"
	"time"
)

// Sweep tuning. The in-request backfill (SPEC §7) is fire-and-forget: it enqueues
// once when a fresh master is written, never retries, and silently drops jobs on
// queue saturation / process restart / transient LLM error. That leaves three
// permanent gaps the request path can't close on its own:
//   - legacy masters generated before multilingual support existed,
//   - languages whose translation call failed (logged, not retried),
//   - jobs dropped because the bounded queue was full or the process restarted.
//
// The Sweeper closes them: on a slow cadence it re-reads what's still missing and
// re-enqueues ONLY the absent languages through the existing worker pool. Enqueue
// already drops on a full queue, so the sweep is naturally throttled — anything it
// can't fit waits for the next tick. This is the system's self-heal.
const (
	// defaultSweepInterval is the gap between full sweeps. Translation gaps are
	// not time-sensitive (English is always served as the read fallback), so a
	// slow cadence keeps LLM cost and DB load negligible.
	defaultSweepInterval = 6 * time.Hour
	// sweepStartupDelay lets the process finish cold start (pool warm, request
	// traffic settling) before the first sweep competes for the worker pool.
	sweepStartupDelay = 2 * time.Minute
	// sweepQueryTimeout bounds the read query that lists incomplete entries.
	sweepQueryTimeout = 30 * time.Second
)

// Sweeper periodically re-enqueues translation backfill for any enriched plant or
// disease still missing supported languages. It owns no LLM/DB work itself — it
// reads the gaps via the *DB read queries and feeds them to the existing
// Backfiller / DiseaseBackfiller worker pools.
type Sweeper struct {
	db       *DB
	plant    *Backfiller        // nil → skip the plant pass
	disease  *DiseaseBackfiller // nil → skip the disease pass
	interval time.Duration
}

// NewSweeper builds a Sweeper. interval <= 0 falls back to defaultSweepInterval.
// A nil db, or both backfillers nil, yields a Sweeper whose Start is a no-op.
func NewSweeper(db *DB, plant *Backfiller, disease *DiseaseBackfiller, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	return &Sweeper{db: db, plant: plant, disease: disease, interval: interval}
}

// Start launches the sweep loop on a background goroutine and returns
// immediately. The loop runs until ctx is cancelled. No-op if there is nothing
// to sweep (nil DB or no backfillers wired).
func (s *Sweeper) Start(ctx context.Context) {
	if s == nil || s.db == nil || (s.plant == nil && s.disease == nil) {
		log.Printf("enrichment sweep: disabled (db or backfillers unavailable)")
		return
	}
	go s.loop(ctx)
	log.Printf("enrichment sweep: scheduled every %s (first run in %s)", s.interval, sweepStartupDelay)
}

func (s *Sweeper) loop(ctx context.Context) {
	// This loop is the only goroutine driving the sweep; an unrecovered panic here
	// would crash the whole single-instance server. Recover at the top (last
	// resort — the loop then exits, disabling further sweeps but keeping the
	// process alive) AND per-tick below so one bad sweep never stops the schedule.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("enrichment sweep: loop panic recovered (sweeps disabled): %v", r)
		}
	}()
	timer := time.NewTimer(sweepStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("enrichment sweep: runOnce panic recovered: %v", r)
					}
				}()
				s.runOnce(ctx)
			}()
			timer.Reset(s.interval)
		}
	}
}

func (s *Sweeper) runOnce(ctx context.Context) {
	s.sweepPlants(ctx)
	s.sweepDiseases(ctx)
}

func (s *Sweeper) sweepPlants(ctx context.Context) {
	if s.plant == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, sweepQueryTimeout)
	items, err := s.db.IncompletePlantMasters(qctx)
	cancel()
	if err != nil {
		log.Printf("enrichment sweep: plant query failed: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	enqueued, langs := 0, 0
	for _, it := range items {
		s.plant.Enqueue(BackfillJob{
			Normalized:     it.Normalized,
			ScientificName: it.ScientificName,
			CommonHint:     it.CommonHint,
			SourceLang:     it.SourceLang,
			Master:         it.Master,
			OnlyLangs:      it.MissingLangs,
		})
		enqueued++
		langs += len(it.MissingLangs)
	}
	log.Printf("enrichment sweep: plants incomplete=%d enqueued=%d missing_langs=%d", len(items), enqueued, langs)
}

func (s *Sweeper) sweepDiseases(ctx context.Context) {
	if s.disease == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, sweepQueryTimeout)
	items, err := s.db.IncompleteDiseaseMasters(qctx)
	cancel()
	if err != nil {
		log.Printf("enrichment sweep: disease query failed: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	enqueued, langs := 0, 0
	for _, it := range items {
		s.disease.Enqueue(DiseaseBackfillJob{
			Normalized:  it.Normalized,
			DiseaseName: it.DiseaseName,
			CatalogID:   it.CatalogID,
			SourceLang:  it.SourceLang,
			Master:      it.Master,
			OnlyLangs:   it.MissingLangs,
		})
		enqueued++
		langs += len(it.MissingLangs)
	}
	log.Printf("enrichment sweep: diseases incomplete=%d enqueued=%d missing_langs=%d", len(items), enqueued, langs)
}
