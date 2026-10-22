package referral

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Auto-restock — invite-friends.md ### 码池自动补货 (default off). Runs inside
// the pool watch, once per settlement pass, BEFORE the low-stock / expiring
// alerts are judged:
//
//	trigger   usable < restock_below, or the earliest usable batch expires
//	          within restock_lead_days AND what stays usable after it
//	          (Stock.AfterEarliest) < restock_below
//	start     code_restocks row 'creating' (one unfinished at a time — partial
//	          unique index), POST a batch of restock_batch_size codes expiring
//	          today (UTC) + 179 days
//	created   Apple named the batch: asc_batch_id recorded at once
//	imported  GET the batch's values (CSV) → code_pool, on conflict do nothing
//	failed    Apple plainly refused / nothing matched within 30 minutes of an
//	          unknown outcome / not imported within 6 hours of the request
//
// An unknown outcome (the request went out, then a timeout, a dropped
// connection or a 5xx — or the process died mid-request) leaves the row
// creating; every pass then lists the offer's batches and claims the one that
// matches (created ≥ started − 5 min, same count / expiry / environment, not
// held by another code_restocks row). Values that cannot be fetched yet are
// fetched again next pass, never re-requested.
//
// Limits: at least an hour between starts, restock_daily_max starts per UTC
// day. Not configured / code environment mismatch never starts anything and
// does not count as a start. While an unfinished restock has not timed out,
// the low-stock / expiring alerts only log (a mail would send the operator
// to restock by hand on top of it). Every failure mails the operator — one a
// day per kind, or one per Apple batch when it carries a batch id — and a
// success sends a notice too.

const (
	// RestockOfferRefName is code_pool.offer_ref_name for restocked codes:
	// the offer's reference name in App Store Connect, same as hand imports.
	RestockOfferRefName = "Referral Friend Month"
	// restockValidDays: Apple's one-time codes live at most six months; the
	// batch expires on today (UTC) + 179 days.
	restockValidDays = 179
	// restockMinGap: two starts at least this far apart.
	restockMinGap = time.Hour
	// restockReconcileWindow: an unknown outcome is reconciled this long
	// after the start before it is called failed.
	restockReconcileWindow = 30 * time.Minute
	// restockImportWindow: a batch not in code_pool this long after the
	// start is given up (failed + alert with its batch id).
	restockImportWindow = 6 * time.Hour
	// restockClockSlack: Apple's createdDate may be this much earlier than
	// our started_at (two clocks).
	restockClockSlack = 5 * time.Minute
	// Apple's batch bounds; the 500 minimum binds PRODUCTION codes only
	// (a 10-code SANDBOX batch was created through the API on 2026-10-19).
	ascProductionMinBatch = 500
	ascMaxBatch           = 25000
)

// RestockConfig is the App Store Connect side of auto-restock, from the vault
// (main: buildRestockConfig). The switch and the numbers live in
// referral_settings; this is only what may not live in the database.
type RestockConfig struct {
	// API is the App Store Connect client (nil when ASC_KEY_ID /
	// ASC_ISSUER_ID / ASC_PRIVATE_KEY are missing or the key is unusable).
	API OfferCodeAPI
	// OfferID is ASC_REFERRAL_OFFER_ID (the offer's App Store Connect id).
	OfferID string
	// Environment is ASC_OFFER_CODE_ENVIRONMENT: PRODUCTION / SANDBOX.
	Environment string
	// Missing names the vault keys absent or unusable (never their values).
	Missing []string
	// CallTimeout bounds each App Store Connect call (default 60 s).
	CallTimeout time.Duration
}

// missing is what keeps auto-restock from running: the names main listed,
// or, for a config assembled by hand (tests), whatever is visibly absent.
func (c RestockConfig) missing() []string {
	if len(c.Missing) > 0 {
		return c.Missing
	}
	var m []string
	if c.API == nil {
		m = append(m, "ASC_KEY_ID / ASC_ISSUER_ID / ASC_PRIVATE_KEY")
	}
	if strings.TrimSpace(c.OfferID) == "" {
		m = append(m, "ASC_REFERRAL_OFFER_ID")
	}
	if NormaliseOfferEnvironment(c.Environment) == "" {
		m = append(m, "ASC_OFFER_CODE_ENVIRONMENT")
	}
	return m
}

// restockPlan is the settings row made usable: the batch size and lead days
// actually used, and what is odd about the combination.
type restockPlan struct {
	batch    int
	leadDays int
	problems []string
}

// planRestock checks the settings combination. Every problem is only a WARN
// (logged once per distinct set) and the restock goes ahead — restocking
// beats not restocking (docs: 后端每轮检查设置组合). Two of them also change a
// number: a PRODUCTION batch outside 500..25,000 is clamped, and a lead time
// that leaves no room for a new batch to count as "after the earliest" is
// clamped to 179 − issue_margin_days − 1.
func planRestock(rs RestockSettings, ps PoolSettings, env string) restockPlan {
	p := restockPlan{batch: rs.BatchSize, leadDays: rs.LeadDays}
	if p.batch > ascMaxBatch {
		p.batch = ascMaxBatch
	}
	if p.batch < 1 {
		p.batch = 1
	}
	if env == EnvProduction && (rs.BatchSize < ascProductionMinBatch || rs.BatchSize > ascMaxBatch) {
		if p.batch < ascProductionMinBatch {
			p.batch = ascProductionMinBatch
		}
		p.problems = append(p.problems, fmt.Sprintf("restock_batch_size=%d is outside Apple's %d..%d for PRODUCTION codes: generating %d",
			rs.BatchSize, ascProductionMinBatch, ascMaxBatch, p.batch))
	}
	if p.batch <= rs.Below {
		p.problems = append(p.problems, fmt.Sprintf("restock_batch_size=%d <= restock_below=%d: one batch never lifts usable stock past the trigger, so restock_daily_max gets used up every day",
			p.batch, rs.Below))
	}
	if ps.LowStockThreshold > 0 && rs.Below <= ps.LowStockThreshold {
		p.problems = append(p.problems, fmt.Sprintf("restock_below=%d <= low_stock_threshold=%d: the low-stock alert can fire before auto-restock is even triggered",
			rs.Below, ps.LowStockThreshold))
	}
	if ps.ExpiryWarnDays > 0 && rs.LeadDays <= ps.ExpiryWarnDays {
		p.problems = append(p.problems, fmt.Sprintf("restock_lead_days=%d <= expiry_warn_days=%d: the expiring alert fires before the early restock does",
			rs.LeadDays, ps.ExpiryWarnDays))
	}
	if rs.LeadDays+ps.IssueMarginDays >= restockValidDays {
		p.leadDays = restockValidDays - ps.IssueMarginDays - 1
		if p.leadDays < 0 {
			p.leadDays = 0
		}
		p.problems = append(p.problems, fmt.Sprintf("restock_lead_days=%d + issue_margin_days=%d >= %d: a new batch could never count as usable after the earliest one, so the early restock would fire forever; using restock_lead_days=%d",
			rs.LeadDays, ps.IssueMarginDays, restockValidDays, p.leadDays))
	}
	return p
}

// restockTrigger names why the pool needs a batch ("" = it does not):
// usable stock under restock_below, or the earliest usable batch inside the
// lead window with too little behind it (TakeStock's AfterEarliest).
func restockTrigger(st Stock, below, leadDays int, now time.Time) string {
	if st.Usable < below {
		return fmt.Sprintf("usable %d < restock_below %d", st.Usable, below)
	}
	if !st.Earliest.IsZero() && !st.Earliest.After(now.Add(time.Duration(leadDays)*poolDay)) && st.AfterEarliest < below {
		return fmt.Sprintf("earliest batch expires %s (within %d days) and only %d stay usable after it (< restock_below %d)",
			st.Earliest.UTC().Format("2006-01-02"), leadDays, st.AfterEarliest, below)
	}
	return ""
}

// utcDayStart is 00:00 UTC of now's UTC day.
func utcDayStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// restockExpiry is the new batch's expiration date — today (UTC) + 179 days
// — as code_pool stores it: that date at 00:00 UTC, a few hours before Apple
// retires it (midnight Pacific), like import-codes.sh.
func restockExpiry(now time.Time) time.Time {
	return utcDayStart(now).AddDate(0, 0, restockValidDays)
}

func ymd(t time.Time) string { return t.UTC().Format("2006-01-02") }

// restockStep is auto-restock's part of a pass. busy: an unfinished restock
// that has not timed out is still open afterwards (the caller holds the
// low-stock / expiring mail back); refreshed: codes went into the pool, so
// the caller re-reads PoolStatus before judging.
func (s *Service) restockStep(ctx context.Context, now time.Time, ps PoolStatus, rep *Report) (busy, refreshed bool) {
	status, err := s.store.RestockStatus(ctx, utcDayStart(now))
	if errors.Is(err, ErrRestockNotMigrated) {
		if !s.restockUnmigratedLogged {
			log.Printf("referral restock: off (migration 021_referral_auto_restock.sql not applied)")
			s.restockUnmigratedLogged = true
		}
		return false, false
	}
	s.restockUnmigratedLogged = false
	if err != nil {
		log.Printf("referral restock: status: %v", err)
		rep.Errors++
		return false, false
	}
	rs := status.Settings
	if !rs.Enabled {
		s.restockWarned = ""
		// Off means off: Apple is not called, nothing is mailed. An unfinished
		// row left behind is only pointed out in the log, once a UTC day.
		if o := status.Open; o != nil && s.restockOffOpenDay != ymd(now) {
			s.restockOffOpenDay = ymd(now)
			log.Printf("WARN referral restock: auto_restock_enabled is off but restock %d is still %s (batch %q, %d %s codes, started %s); left alone while the switch is off — turn it back on to let it finish, or import the batch by hand (deploy/supabase/import-codes.sh)",
				o.ID, o.State, o.BatchID, o.Codes, o.Environment, restockStamp(o.StartedAt))
		}
		return false, false
	}
	env := NormaliseOfferEnvironment(s.restock.Environment)
	plan := planRestock(rs, ps.Settings, env)
	s.warnRestockSettings(plan.problems)

	if missing := s.restock.missing(); len(missing) > 0 {
		log.Printf("WARN referral restock: auto_restock_enabled but App Store Connect is not configured (missing %v); not restocking", missing)
		s.restockAlert(rep, "restock:not_configured", now,
			restockSubject(envTag(env, rs.Environment), "not configured"), notConfiguredMail(missing, now))
		return false, false
	}
	if rs.Environment == "" || rs.Environment != env {
		log.Printf("WARN referral restock: code environment mismatch (ASC_OFFER_CODE_ENVIRONMENT=%s, referral_settings.restock_environment=%q); not restocking", env, rs.Environment)
		s.restockAlert(rep, "restock:environment", now,
			restockSubject(env, "refused, code environment mismatch"), environmentMail(env, rs.Environment, now))
		return false, false
	}
	if status.Open != nil {
		return s.finishRestock(ctx, now, status.Open, rep)
	}

	st := TakeStock(ps, now)
	why := restockTrigger(st, rs.Below, plan.leadDays, now)
	if why == "" {
		return false, false
	}
	if rs.DailyMax <= 0 {
		// restock_daily_max = 0 pauses auto-restock: nothing new is started
		// and there is no "daily max" mail (an unfinished restock above is
		// still finished; the low-stock alert works as usual).
		if s.restockPausedDay != ymd(now) {
			s.restockPausedDay = ymd(now)
			log.Printf("referral restock: needed (%s) but paused (restock_daily_max = 0)", why)
		}
		return false, false
	}
	if status.StartedToday >= rs.DailyMax {
		log.Printf("WARN referral restock: needed (%s) but %d of restock_daily_max=%d already started today (UTC)", why, status.StartedToday, rs.DailyMax)
		if st.Usable < rs.Below {
			s.restockAlert(rep, "restock:daily_max", now,
				restockSubject(env, fmt.Sprintf("daily max reached, %d usable", st.Usable)), dailyMaxMail(status.StartedToday, rs, st, now))
		}
		return false, false
	}
	if !status.LastStarted.IsZero() && now.Sub(status.LastStarted) < restockMinGap {
		log.Printf("referral restock: needed (%s) but the last restock started %s ago (< %s)", why, now.Sub(status.LastStarted).Round(time.Minute), restockMinGap)
		return false, false
	}
	return s.startRestock(ctx, now, env, plan.batch, rs.DailyMax, why, rep)
}

// warnRestockSettings logs the settings problems once per distinct set.
func (s *Service) warnRestockSettings(problems []string) {
	key := strings.Join(problems, "\n")
	if key == s.restockWarned {
		return
	}
	s.restockWarned = key
	for _, p := range problems {
		log.Printf("WARN referral restock settings: %s", p)
	}
}

// restockListBudgetPages: the reconcile list's overall budget, in pages.
const restockListBudgetPages = 3

func (s *Service) ascContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.restock.CallTimeout)
}

// startRestock records a new restock, asks Apple for the batch and, when
// Apple names it, goes straight on to fetch and import its codes.
func (s *Service) startRestock(ctx context.Context, now time.Time, env string, n, dailyMax int, why string, rep *Report) (busy, refreshed bool) {
	expires := restockExpiry(now)
	id, err := s.store.BeginRestock(ctx, RestockStart{
		Environment: env, Codes: n, ExpiresAt: expires,
		StartedAt: now, DayStart: utcDayStart(now), DailyMax: dailyMax, MinGap: restockMinGap,
	})
	switch {
	case errors.Is(err, errRestockOpen):
		log.Printf("referral restock: another process started a restock first")
		return true, false
	case errors.Is(err, errRestockLimited):
		log.Printf("referral restock: another process started a restock meanwhile (daily max / gap)")
		return false, false
	case err != nil:
		log.Printf("referral restock: begin: %v", err)
		rep.Errors++
		return false, false
	}
	rep.RestockStarted++
	log.Printf("referral restock: restock %d started (%s): %d %s codes expiring %s", id, why, n, env, ymd(expires))

	cctx, cancel := s.ascContext(ctx)
	batchID, err := s.restock.API.CreateOneTimeCodes(cctx, CreateCodesRequest{
		OfferID: s.restock.OfferID, NumberOfCodes: n, ExpirationDate: ymd(expires), Environment: env,
	})
	cancel()
	open := &Restock{ID: id, Environment: env, Codes: n, ExpiresAt: expires, State: RestockCreating, StartedAt: now}
	if err != nil {
		uncertain, kind := ascOutcome(err)
		if uncertain {
			log.Printf("referral restock: restock %d: outcome unknown (%v); reconciling against the batch list from the next pass", id, err)
			return true, false
		}
		s.failRestock(ctx, now, open, err.Error(), "restock:create_failed:"+kind,
			restockSubject(env, "Apple refused the batch ("+kind+")"), createFailedMail(open, err.Error(), now), rep)
		return false, false
	}
	ok, err := s.store.MarkRestockCreated(ctx, id, batchID)
	if err != nil {
		// Apple has the batch; the row is still creating, so the next pass
		// finds the batch in the list and claims it.
		log.Printf("referral restock: APPLE CREATED BATCH %s BUT RESTOCK %d IS NOT RECORDED AS CREATED (%v); the next pass reconciles it", batchID, id, err)
		rep.Errors++
		return true, false
	}
	if !ok {
		// Another process reconciled the row while our request was out.
		log.Printf("referral restock: restock %d (batch %s) was already moved on by another process", id, batchID)
		return true, false
	}
	log.Printf("referral restock: restock %d created: App Store Connect batch %s", id, batchID)
	open.BatchID, open.State = batchID, RestockCreated
	return s.importRestock(ctx, now, open, rep)
}

// finishRestock moves the unfinished restock on: reconcile a creating one,
// import a created one, or give up on either past its window. Never starts a
// new one in the same pass.
func (s *Service) finishRestock(ctx context.Context, now time.Time, open *Restock, rep *Report) (busy, refreshed bool) {
	if open.State == RestockCreating {
		batchID, why := s.reconcile(ctx, open, rep)
		if batchID == "" {
			if now.Sub(open.StartedAt) < restockReconcileWindow {
				log.Printf("referral restock: restock %d outcome still unknown (%s)", open.ID, why)
				return true, false
			}
			reason := fmt.Sprintf("outcome unknown and no matching App Store Connect batch within %s of the request (%s)", restockReconcileWindow, why)
			s.failRestock(ctx, now, open, reason, "restock:reconcile_timeout",
				restockSubject(open.Environment, "outcome unknown, no matching batch"), reconcileTimeoutMail(open, why, now), rep)
			return false, false
		}
		ok, err := s.store.MarkRestockCreated(ctx, open.ID, batchID)
		if err != nil {
			log.Printf("referral restock: restock %d: record reconciled batch %s: %v", open.ID, batchID, err)
			rep.Errors++
			return true, false
		}
		if !ok {
			log.Printf("referral restock: restock %d is no longer creating (another process moved it)", open.ID)
			return true, false
		}
		log.Printf("referral restock: restock %d reconciled: App Store Connect batch %s", open.ID, batchID)
		open.BatchID, open.State = batchID, RestockCreated
	}
	if now.Sub(open.StartedAt) >= restockImportWindow {
		// One last try before giving up on the batch.
		if busy, refreshed := s.importRestock(ctx, now, open, rep); !busy {
			return false, refreshed // imported, or moved on by another process
		}
		reason := fmt.Sprintf("batch %s not imported within %s of the request", open.BatchID, restockImportWindow)
		s.failRestock(ctx, now, open, reason, "batch:restock:import_timeout:"+open.BatchID,
			restockSubject(open.Environment, "batch "+open.BatchID+" not imported in 6h"), importTimeoutMail(open, now), rep)
		return false, false
	}
	return s.importRestock(ctx, now, open, rep)
}

// reconcile looks for the batch an unknown-outcome request may have made:
// created no earlier than 5 minutes before the start, same number of codes,
// expiry and environment, and not held by another code_restocks row (two
// restocks on one day ask for the same count and date). The earliest such
// batch wins. why says what was seen when nothing matched.
func (s *Service) reconcile(ctx context.Context, open *Restock, rep *Report) (batchID, why string) {
	// The client bounds every page on its own (CallTimeout each); the whole
	// list gets three pages' worth, so a few slow pages still fit but a slow
	// long list cannot eat the pass's RevenueCat settlement (one pass is 8
	// minutes and the restock runs before it).
	lctx, cancel := context.WithTimeout(ctx, restockListBudgetPages*s.restock.CallTimeout)
	defer cancel()
	batches, err := s.restock.API.ListOneTimeCodeBatches(lctx, s.restock.OfferID)
	if err != nil {
		log.Printf("referral restock: restock %d: list batches: %v", open.ID, err)
		rep.Errors++
		return "", "listing batches failed: " + err.Error()
	}
	since := open.StartedAt.Add(-restockClockSlack)
	exp := ymd(open.ExpiresAt)
	var cands []OneTimeCodeBatch
	for _, b := range batches {
		if b.ID == "" || b.CreatedDate.IsZero() || b.CreatedDate.Before(since) {
			continue
		}
		if b.NumberOfCodes != open.Codes || b.ExpirationDate != exp || NormaliseOfferEnvironment(b.Environment) != open.Environment {
			continue
		}
		cands = append(cands, b)
	}
	if len(cands) == 0 {
		return "", fmt.Sprintf("%d batches listed, none matches", len(batches))
	}
	ids := make([]string, len(cands))
	for i, b := range cands {
		ids[i] = b.ID
	}
	taken, err := s.store.RestockBatchesTaken(ctx, ids)
	if err != nil {
		log.Printf("referral restock: restock %d: batches taken: %v", open.ID, err)
		rep.Errors++
		return "", "reading claimed batches failed"
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].CreatedDate.Before(cands[j].CreatedDate) })
	for _, b := range cands {
		if !taken[b.ID] {
			return b.ID, ""
		}
	}
	return "", fmt.Sprintf("%d matching batches, all claimed by other restocks", len(cands))
}

// importRestock fetches the batch's code values and puts them in the pool.
// Values that cannot be had yet (Apple still generating, the network, fewer
// than asked for) are fetched again next pass — never re-requested.
func (s *Service) importRestock(ctx context.Context, now time.Time, open *Restock, rep *Report) (busy, refreshed bool) {
	cctx, cancel := s.ascContext(ctx)
	codes, lines, err := s.restock.API.OneTimeCodeValues(cctx, open.BatchID)
	cancel()
	if err != nil {
		log.Printf("referral restock: restock %d: fetch values of batch %s: %v; trying again next pass", open.ID, open.BatchID, err)
		rep.Errors++
		return true, false
	}
	// Counts only — never a code value.
	log.Printf("referral restock: restock %d: batch %s values: %d non-empty CSV lines, %d codes recognised, %d asked for",
		open.ID, open.BatchID, lines, len(codes), open.Codes)
	if len(codes) < open.Codes {
		if lines >= open.Codes {
			// The CSV has enough lines but not enough of them look like codes:
			// waiting will not fix that. Say so now (once per batch) instead of
			// only at the 6-hour timeout; keep trying meanwhile.
			log.Printf("WARN referral restock: restock %d: batch %s code values not recognised (%d lines, %d codes)", open.ID, open.BatchID, lines, len(codes))
			s.restockBatchAlert(rep, "restock:values_unrecognised:"+open.BatchID,
				restockSubject(open.Environment, "batch "+open.BatchID+" code values not recognised"), unrecognisedMail(open, lines, len(codes), now))
		}
		return true, false
	}
	inserted, err := s.store.ImportRestock(ctx, open.ID, codes, RestockOfferRefName, open.ExpiresAt, now)
	if errors.Is(err, errRestockNotCreated) {
		log.Printf("referral restock: restock %d is no longer created (another process moved it); nothing imported", open.ID)
		return false, false
	}
	if err != nil {
		log.Printf("referral restock: restock %d: import batch %s: %v; trying again next pass", open.ID, open.BatchID, err)
		rep.Errors++
		return true, false
	}
	rep.Restocked += inserted
	log.Printf("referral restock: restock %d imported: batch %s, %d values, %d new in the pool, %s, expires %s",
		open.ID, open.BatchID, len(codes), inserted, open.Environment, ymd(open.ExpiresAt))
	s.restockBatchAlert(rep, "restock:imported:"+open.BatchID,
		restockSubject(open.Environment, fmt.Sprintf("added %d codes, expires %s", inserted, ymd(open.ExpiresAt))),
		importedMail(open, len(codes), inserted, now))
	return false, true
}

// failRestock closes the restock as failed and mails the operator under key
// (a "batch:" key goes once per batch, any other once a day).
func (s *Service) failRestock(ctx context.Context, now time.Time, open *Restock, reason, key, subject, body string, rep *Report) {
	if err := s.store.FailRestock(ctx, open.ID, reason, now); err != nil {
		log.Printf("referral restock: restock %d: record failure: %v", open.ID, err)
		rep.Errors++
		return
	}
	log.Printf("WARN referral restock: restock %d failed: %s", open.ID, reason)
	if k, ok := strings.CutPrefix(key, "batch:"); ok {
		s.restockBatchAlert(rep, k, subject, body)
		return
	}
	s.restockAlert(rep, key, now, subject, body)
}

// restockBatchAlert mails what carries an Apple batch id: once per key for
// the life of the process, not once a day (a batch that keeps failing across
// midnight is still one mail). In memory, like alertOnce: a restart may
// repeat one; a failed send is not remembered, so the next pass retries.
func (s *Service) restockBatchAlert(rep *Report, key, subject, body string) {
	if s.alerter == nil || s.restockBatchMailed[key] {
		return
	}
	if err := s.alerter.Alert(subject, body); err != nil {
		log.Printf("referral restock alert mail: %v", err)
		rep.Errors++
		return
	}
	if s.restockBatchMailed == nil {
		s.restockBatchMailed = map[string]bool{}
	}
	s.restockBatchMailed[key] = true
	rep.RestockMails++
}

// restockAlert mails through the shared once-a-day dedup (alertOnce).
func (s *Service) restockAlert(rep *Report, key string, now time.Time, subject, body string) {
	sent, err := s.alertOnce(key, now, subject, body)
	if err != nil {
		log.Printf("referral restock alert mail: %v", err)
		rep.Errors++
		return
	}
	if sent {
		rep.RestockMails++
	}
}

// --- mails --------------------------------------------------------------------
//
// Subject ASCII with the code environment in brackets ([SANDBOX]); body UTF-8
// Chinese like every other ops note here. Dates UTC. Never a code value.

// envTag is the subject's environment: the one secrets name, else the
// database's, else UNSET.
func envTag(secretsEnv, dbEnv string) string {
	if e := NormaliseOfferEnvironment(secretsEnv); e != "" {
		return e
	}
	if e := NormaliseOfferEnvironment(dbEnv); e != "" {
		return e
	}
	return "UNSET"
}

func restockSubject(env, what string) string {
	if env == "" {
		env = "UNSET"
	}
	return "[YardMate] [" + env + "] Referral code restock: " + what
}

func restockStamp(now time.Time) string { return now.UTC().Format("2006-01-02 15:04 UTC") }

func manualImportLine(expires string) string {
	return `  SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… deploy/supabase/import-codes.sh codes.csv "Referral Friend Month" ` + expires + "\n"
}

func notConfiguredMail(missing []string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货没配置（%s）\n\n", restockStamp(now))
	fmt.Fprintf(&b, "referral_settings.auto_restock_enabled 是 true，但服务端 secrets 缺：%s\n", strings.Join(missing, ", "))
	b.WriteString("这期间不会自动补货（不算发起），码池低库存 / 临期告警照常。\n\n")
	b.WriteString("要自动补：服务器 secrets 加齐 ASC_KEY_ID / ASC_ISSUER_ID / ASC_PRIVATE_KEY / ASC_REFERRAL_OFFER_ID / ASC_OFFER_CODE_ENVIRONMENT，重新部署（deploy/README.md §10）。\n")
	b.WriteString("不想自动补：update referral_settings set auto_restock_enabled = false where id = 1;\n")
	b.WriteString("手动补货（老路子）：App Store Connect 生成一批一次性码、导出 CSV 后\n")
	b.WriteString(manualImportLine("YYYY-MM-DD"))
	b.WriteString("\n同一种问题一天最多一封。\n")
	return b.String()
}

func environmentMail(secretsEnv, dbEnv string, now time.Time) string {
	if secretsEnv == "" {
		secretsEnv = "（空）"
	}
	if dbEnv == "" {
		dbEnv = "（空）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货被拒：码环境两处说法不一致（%s）\n\n", restockStamp(now))
	fmt.Fprintf(&b, "服务端 secrets ASC_OFFER_CODE_ENVIRONMENT：%s\n", secretsEnv)
	fmt.Fprintf(&b, "本环境库里 referral_settings.restock_environment：%s\n\n", dbEnv)
	b.WriteString("两处一致才补：prod 都是 PRODUCTION，staging 都是 SANDBOX（TestFlight / 沙盒只认沙盒码）。\n")
	b.WriteString("这期间不补货、不算一次发起，码池告警照常。\n\n")
	b.WriteString("改库：update referral_settings set restock_environment = 'SANDBOX' where id = 1;（prod 填 'PRODUCTION'）\n")
	b.WriteString("或改 secrets 后重新部署。\n\n同一种问题一天最多一封。\n")
	return b.String()
}

func dailyMaxMail(started int, rs RestockSettings, st Stock, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池今天的自动补货次数用完了（%s）\n\n", restockStamp(now))
	fmt.Fprintf(&b, "今天（UTC）已发起 %d 次，上限 restock_daily_max = %d；可发 %d 张，仍少于 restock_below = %d。\n",
		started, rs.DailyMax, st.Usable, rs.Below)
	b.WriteString("UTC 0 点之后会自动再补。急的话手动补货：App Store Connect 生成一批一次性码、导出 CSV 后\n")
	b.WriteString(manualImportLine("YYYY-MM-DD"))
	b.WriteString("或调高上限：update referral_settings set restock_daily_max = … where id = 1;\n")
	b.WriteString("看现状：deploy/referral-pool.sh\n\n一天最多一封。\n")
	return b.String()
}

func restockLine(open *Restock) string {
	return fmt.Sprintf("补货 #%d：%s 向苹果要 %d 张 %s 码，到期 %s", open.ID, restockStamp(open.StartedAt), open.Codes, open.Environment, ymd(open.ExpiresAt))
}

func createFailedMail(open *Restock, reason string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货失败：App Store Connect 拒绝了生成请求（%s）\n\n", restockStamp(now))
	b.WriteString(restockLine(open) + "\n")
	fmt.Fprintf(&b, "原因：%s\n\n", reason)
	b.WriteString("（HTTP 401 / 403 = 密钥失效或权限不够；其他 4xx = 苹果拒绝，比如季度额度用完；unreachable = 请求根本没发出去，如 DNS / 连接被拒）\n")
	b.WriteString("这次记 failed。1 小时后在每天上限内会再试。急的话手动补货：\n")
	b.WriteString(manualImportLine("YYYY-MM-DD"))
	b.WriteString("\n同一种失败一天最多一封。\n")
	return b.String()
}

func reconcileTimeoutMail(open *Restock, why string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货结果不确定，%d 分钟内没对上批次（%s）\n\n", int(restockReconcileWindow.Minutes()), restockStamp(now))
	b.WriteString(restockLine(open) + "\n")
	b.WriteString("请求发出后没收到明确答复（超时 / 断连 / 5xx / 服务在途中重启），之后每轮列这个 offer 的一次性码批次都没找到对得上的。\n")
	fmt.Fprintf(&b, "最后一次对账：%s\n\n", why)
	b.WriteString("已记 failed。请去 App Store Connect → offer「Referral Friend Month」→ 一次性码核对：\n")
	fmt.Fprintf(&b, "如果有一批 %d 张、%s、到期 %s、创建时间在 %s 前后的，下载它的 CSV 手动导入（重复导入安全）：\n",
		open.Codes, open.Environment, ymd(open.ExpiresAt), restockStamp(open.StartedAt))
	b.WriteString(manualImportLine(ymd(open.ExpiresAt)))
	b.WriteString("可发仍不够时后端会在间隔和每天上限内再补一批。\n\n同一种失败一天最多一封。\n")
	return b.String()
}

func importTimeoutMail(open *Restock, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货：苹果批次 %s 要码后 %d 小时还没灌进码池（%s）\n\n", open.BatchID, int(restockImportWindow.Hours()), restockStamp(now))
	b.WriteString(restockLine(open) + "\n")
	fmt.Fprintf(&b, "苹果批次 ID：%s\n", open.BatchID)
	b.WriteString("苹果已经给了批次，但取码值 / 灌池一直没成功。已记 failed。\n")
	b.WriteString("这批码在苹果那边已经存在（占季度额度）。手动导入：App Store Connect 下载这个批次的 CSV，然后\n")
	b.WriteString(manualImportLine(ymd(open.ExpiresAt)))
	b.WriteString("（重复导入安全）。可发仍不够时后端会再补一批。\n")
	return b.String()
}

func unrecognisedMail(open *Restock, lines, codes int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货：苹果批次 %s 的码值格式认不出（%s）\n\n", open.BatchID, restockStamp(now))
	b.WriteString(restockLine(open) + "\n")
	fmt.Fprintf(&b, "苹果批次 ID：%s\n", open.BatchID)
	fmt.Fprintf(&b, "取到的 CSV 有 %d 行非空，但只认出 %d 个码（要 %d 张）。认码规则同 import-codes.sh：每行第一个像 ^[A-Z0-9]{6,20}$ 的字段。\n", lines, codes, open.Codes)
	b.WriteString("苹果的 CSV 格式可能变了：后端会继续试，要码满 6 小时还灌不进就记 failed。\n")
	b.WriteString("请在 App Store Connect 下载这个批次的 CSV 看一眼格式；能认出的话手动导入（重复导入安全）：\n")
	b.WriteString(manualImportLine(ymd(open.ExpiresAt)))
	b.WriteString("（import-codes.sh 用的是同一条认码规则，格式真变了的话 CSV 要先整理一下；后端的解析也要跟着改。）\n\n每个批次只发一封。\n")
	return b.String()
}

func importedMail(open *Restock, values, inserted int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池自动补货完成（%s）\n\n", restockStamp(now))
	fmt.Fprintf(&b, "自动补了 %d 张（%s），到期 %s，苹果批次 %s\n", inserted, open.Environment, ymd(open.ExpiresAt), open.BatchID)
	fmt.Fprintf(&b, "补货 #%d：要了 %d 张，苹果给了 %d 个码值，新进码池 %d 张", open.ID, open.Codes, values, inserted)
	if dup := values - inserted; dup > 0 {
		fmt.Fprintf(&b, "（%d 张库里已有，忽略）", dup)
	}
	b.WriteString("。\n\n")
	b.WriteString("这批占 App Store Connect 的季度额度（每 App 每季度 100 万张，所有订阅共用）。\n")
	b.WriteString("看现状：deploy/referral-pool.sh\n")
	return b.String()
}
