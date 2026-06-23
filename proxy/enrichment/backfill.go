package enrichment

import (
	"context"
	"log"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// Backfill tuning. Bounded so a spike of first-callers never fans out into
// thousands of concurrent OpenAI translation calls (SPEC §9 #18).
const (
	backfillWorkers   = 2
	backfillQueueSize = 256
	// backfillTimeout bounds ONE translate+insert round-trip; a job loops over
	// the target languages sequentially, each with a fresh timeout.
	backfillTimeout = 60 * time.Second
)

// BackfillJob is enqueued after a fresh master row is persisted. The worker
// translates the master's prose fields into every other supported language
// (English first) and persists each as its own (normalized, lang) row.
type BackfillJob struct {
	Normalized     string
	ScientificName string
	CommonHint     string
	SourceLang     string // the master's language; excluded from the target set
	Master         *proxy.PlantDetail
}

// Backfiller runs translation backfill on a bounded worker pool, decoupled from
// request handling (SPEC §7 + §9 #18). Jobs run on context.Background() — never
// the request ctx, which is cancelled when the response is written.
type Backfiller struct {
	db   ServiceDB
	llm  ServiceLLM
	jobs chan BackfillJob
}

// NewBackfiller starts the worker pool. Workers live for the process lifetime
// (no graceful drain in V1 — systemd SIGTERM kills the process; an in-flight
// translation is simply lost and re-triggered on the next master generation).
func NewBackfiller(db ServiceDB, llm ServiceLLM) *Backfiller {
	b := &Backfiller{
		db:   db,
		llm:  llm,
		jobs: make(chan BackfillJob, backfillQueueSize),
	}
	for i := 0; i < backfillWorkers; i++ {
		go b.worker()
	}
	return b
}

// Enqueue submits a job, dropping it (with a log) if the queue is saturated so
// the request path never blocks on backfill (SPEC §9 #18). Dropped languages
// fall back to English on read until a later master generation re-triggers
// backfill for the plant.
func (b *Backfiller) Enqueue(job BackfillJob) {
	if b == nil || job.Master == nil {
		return
	}
	select {
	case b.jobs <- job:
	default:
		log.Printf("enrichment backfill: queue full, dropped sciName=%q sourceLang=%s",
			job.ScientificName, job.SourceLang)
	}
}

func (b *Backfiller) worker() {
	for job := range b.jobs {
		b.run(job)
	}
}

// run translates the master into each target language and persists it. Failures
// are logged and skipped per-language — one bad translation never blocks the
// others, and the missing language simply falls back to English on read.
func (b *Backfiller) run(job BackfillJob) {
	for _, lang := range backfillTargets(job.SourceLang) {
		ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
		translated, reqID, err := b.llm.Translate(ctx, job.Master, lang)
		if err != nil {
			cancel()
			log.Printf("enrichment backfill: translate failed sciName=%q lang=%s err=%v",
				job.ScientificName, lang, err)
			continue
		}
		_, err = b.db.Insert(ctx, InsertParams{
			Normalized:      job.Normalized,
			Lang:            lang,
			ScientificName:  job.ScientificName,
			CommonName:      job.CommonHint,
			Data:            translated,
			Source:          TranslatedSourceTag,
			SourceVersion:   PromptVersion,
			GenerationReqID: reqID,
		})
		cancel()
		if err != nil {
			log.Printf("enrichment backfill: insert failed sciName=%q lang=%s err=%v",
				job.ScientificName, lang, err)
		}
	}
}

// backfillTargets returns the supported languages minus the source language,
// with English first (the universal display-fallback pivot — SPEC §7). When the
// source is already English, English is simply absent from the result.
func backfillTargets(sourceLang string) []string {
	out := make([]string, 0, len(SupportedLangs))
	if sourceLang != "en" {
		out = append(out, "en")
	}
	for _, l := range SupportedLangs {
		if l == "en" || l == sourceLang {
			continue
		}
		out = append(out, l)
	}
	return out
}
