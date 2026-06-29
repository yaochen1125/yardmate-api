package enrichment

import (
	"context"
	"log"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// DiseaseBackfillJob is enqueued after a fresh disease master row is persisted.
// The worker translates the master's prose fields into every other supported
// language (English first) and persists each as its own (normalized, lang) row,
// REUSING the master's O id. Mirrors BackfillJob on the plant side.
type DiseaseBackfillJob struct {
	Normalized  string
	DiseaseName string
	CatalogID   string // the master's O id; every translated row reuses it (§7)
	SourceLang  string // the master's language; excluded from the target set
	Master      *proxy.StructuredDiseaseDetail
	// OnlyLangs, when non-nil, restricts the job to exactly these target
	// languages instead of every supported language minus SourceLang. The
	// periodic Sweeper uses it to translate ONLY the languages a disease is
	// actually missing. nil → full backfill (the request-path default).
	OnlyLangs []string
}

// DiseaseBackfiller runs translation backfill on a bounded worker pool, decoupled
// from request handling (SPEC §7). Jobs run on context.Background() — never the
// request ctx, which is cancelled when the response is written. Mirrors
// Backfiller; reuses the same backfillWorkers / backfillQueueSize / backfillTimeout
// tuning and the backfillTargets ordering.
type DiseaseBackfiller struct {
	db   DiseaseDB
	llm  DiseaseLLM
	jobs chan DiseaseBackfillJob
}

// NewDiseaseBackfiller starts the worker pool. Workers live for the process
// lifetime (no graceful drain in V1 — an in-flight translation is simply lost
// and re-triggered on the next master generation).
func NewDiseaseBackfiller(db DiseaseDB, llm DiseaseLLM) *DiseaseBackfiller {
	b := &DiseaseBackfiller{
		db:   db,
		llm:  llm,
		jobs: make(chan DiseaseBackfillJob, backfillQueueSize),
	}
	for i := 0; i < backfillWorkers; i++ {
		go b.worker()
	}
	return b
}

// Enqueue submits a job, dropping it (with a log) if the queue is saturated so
// the request path never blocks on backfill. Dropped languages fall back to
// English on read until a later master generation re-triggers backfill.
func (b *DiseaseBackfiller) Enqueue(job DiseaseBackfillJob) {
	if b == nil || job.Master == nil {
		return
	}
	select {
	case b.jobs <- job:
	default:
		log.Printf("disease backfill: queue full, dropped name=%q sourceLang=%s",
			job.DiseaseName, job.SourceLang)
	}
}

func (b *DiseaseBackfiller) worker() {
	for job := range b.jobs {
		b.run(job)
	}
}

// run translates the master into each target language and persists it (reusing
// the master's O id). Failures are logged and skipped per-language — one bad
// translation never blocks the others, and the missing language simply falls
// back to English on read.
func (b *DiseaseBackfiller) run(job DiseaseBackfillJob) {
	targets := job.OnlyLangs
	if targets == nil {
		targets = backfillTargets(job.SourceLang)
	}
	for _, lang := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
		translated, reqID, err := b.llm.DiseaseTranslate(ctx, job.Master, lang)
		if err != nil {
			cancel()
			log.Printf("disease backfill: translate failed name=%q lang=%s err=%v",
				job.DiseaseName, lang, err)
			continue
		}
		_, _, err = b.db.InsertDisease(ctx, DiseaseInsertParams{
			Normalized:      job.Normalized,
			Lang:            lang,
			CatalogID:       job.CatalogID, // share the master's O id
			DiseaseName:     job.DiseaseName,
			Detail:          translated,
			Source:          DiseaseTranslatedSourceTag,
			SourceVersion:   DiseasePromptVersion,
			GenerationReqID: reqID,
		})
		cancel()
		if err != nil {
			log.Printf("disease backfill: insert failed name=%q lang=%s err=%v",
				job.DiseaseName, lang, err)
		}
	}
}
