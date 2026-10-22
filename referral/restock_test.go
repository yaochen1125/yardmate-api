package referral

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Auto-restock against memStore + fakeASC. One test per line of
// invite-friends.md ## 验收 that is about restocking, plus the edges.

const testOfferID = "offer-test-2"

// base (2026-10-20, settle_test.go) + 179 days.
const wantExpiry = "2027-04-17"

func restockOn(env string) RestockSettings {
	return RestockSettings{Enabled: true, Below: 300, BatchSize: 1000, DailyMax: 2, LeadDays: 45, Environment: env}
}

func newRestock(env string, rs RestockSettings, batches ...PoolBatch) (*Service, *memStore, *fakeASC, *fakeAlerter) {
	store, al, api := newMemStore(), &fakeAlerter{}, newFakeASC()
	store.pool = poolWith(poolDefaults(), batches...)
	store.rs.settings = rs
	svc := New(store, newFakeRC(), entID, okAuth, Options{
		Alerter: al,
		Restock: RestockConfig{API: api, OfferID: testOfferID, Environment: env},
	})
	return svc, store, api, al
}

// passAt runs one settlement pass at `at`, with Apple's clock on the same time.
func passAt(svc *Service, store *memStore, api *fakeASC, at time.Time) Report {
	api.mu.Lock()
	api.at = at
	api.mu.Unlock()
	return settleAt(svc, store, at)
}

func mailsWith(al *fakeAlerter, needle string) []alertCall {
	al.mu.Lock()
	defer al.mu.Unlock()
	var out []alertCall
	for _, m := range al.sent {
		if strings.Contains(m.subject, needle) {
			out = append(out, m)
		}
	}
	return out
}

func lowPool(free int) PoolStatus { return poolWith(poolDefaults(), batch("Dec", 90*day, free)) }

// captureLog collects log output for the test's duration.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// --- off / on ------------------------------------------------------------------

func TestRestockOffNeverCallsAppStoreConnect(t *testing.T) {
	rs := restockOn(EnvSandbox)
	rs.Enabled = false
	svc, store, api, al := newRestock(EnvSandbox, rs, batch("Dec", 90*day, 42))
	// Even an unfinished restock left over is not touched while the switch is off.
	store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), BatchID: "622437", State: RestockCreated, StartedAt: base.Add(-time.Hour)})

	rep := passAt(svc, store, api, base)
	if api.calls() != 0 || rep.RestockStarted != 0 {
		t.Fatalf("switch off: %d App Store Connect calls, %+v", api.calls(), rep)
	}
	// The pool alert is what it always was: mailed, not held for the open row.
	if rep.Alerted != 1 || len(al.sent) != 1 || !strings.HasPrefix(al.sent[0].subject, "[YardMate] Referral code pool: low stock (42 usable)") {
		t.Fatalf("pool alert changed with the switch off: %+v %+v", rep, al.sent)
	}
	if rows := store.restockRows(); rows[0].State != RestockCreated {
		t.Errorf("open restock touched: %+v", rows[0])
	}
}

func TestRestockBelowThresholdGeneratesImportsAndNotifies(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 50))

	rep := passAt(svc, store, api, base.Add(9*time.Hour))
	if api.createCount() != 1 {
		t.Fatalf("creates = %d, want 1", api.createCount())
	}
	want := CreateCodesRequest{OfferID: testOfferID, NumberOfCodes: 1000, ExpirationDate: wantExpiry, Environment: EnvSandbox}
	if got := api.creates[0]; got != want {
		t.Errorf("create = %+v, want %+v", got, want)
	}
	rows := store.restockRows()
	batchID := api.batches[0].ID
	if len(rows) != 1 || rows[0].State != RestockImported || rows[0].ImportedCount != 1000 || rows[0].BatchID != batchID {
		t.Fatalf("rows = %+v", rows)
	}
	if !rows[0].ExpiresAt.Equal(time.Date(2027, 4, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expires_at = %v, want 2027-04-17 00:00 UTC", rows[0].ExpiresAt)
	}
	// Restock first, verdict after: 50 usable was low stock, 1,050 is not —
	// and the pool alert never went out.
	if rep.RestockStarted != 1 || rep.Restocked != 1000 || rep.Usable != 1050 || rep.Alerted != 0 || rep.Errors != 0 {
		t.Errorf("report = %+v", rep)
	}
	ps, _ := store.PoolStatus(context.Background())
	var found bool
	for _, b := range ps.Batches {
		if b.Name == "Referral Friend Month" && b.ExpiresAt.Equal(rows[0].ExpiresAt) && b.Free == 1000 {
			found = true
		}
	}
	if !found {
		t.Errorf("codes not in the pool as offer %q: %+v", RestockOfferRefName, ps.Batches)
	}
	// One success notice: count, environment, expiry, Apple's batch id.
	if len(al.sent) != 1 || rep.RestockMails != 1 {
		t.Fatalf("mails = %+v", al.sent)
	}
	m := al.sent[0]
	if m.subject != "[YardMate] [SANDBOX] Referral code restock: added 1000 codes, expires "+wantExpiry {
		t.Errorf("subject = %q", m.subject)
	}
	if !strings.Contains(m.body, "自动补了 1000 张（SANDBOX），到期 "+wantExpiry+"，苹果批次 "+batchID) {
		t.Errorf("body:\n%s", m.body)
	}
	for _, code := range api.values[batchID][:3] {
		if strings.Contains(m.body, code) || strings.Contains(m.subject, code) {
			t.Fatalf("a code value leaked into the mail")
		}
	}
	// Plenty now: the next pass leaves Apple alone.
	passAt(svc, store, api, base.Add(10*time.Hour))
	if api.createCount() != 1 {
		t.Errorf("creates after restock = %d", api.createCount())
	}
}

func TestRestockGeneratesTheEnvironmentItIsConfiguredFor(t *testing.T) {
	for _, env := range []string{EnvSandbox, EnvProduction} {
		svc, store, api, al := newRestock(env, restockOn(env), batch("Dec", 90*day, 0))
		passAt(svc, store, api, base)
		if api.createCount() != 1 || api.creates[0].Environment != env {
			t.Fatalf("%s: creates %+v", env, api.creates)
		}
		if rows := store.restockRows(); rows[0].Environment != env {
			t.Errorf("%s: row environment %q", env, rows[0].Environment)
		}
		if len(mailsWith(al, "["+env+"] Referral code restock: added")) != 1 {
			t.Errorf("%s: notice subject without the environment: %+v", env, al.sent)
		}
	}
}

// --- the early (lead) trigger ----------------------------------------------------

func TestRestockEarlyWhenTheOnlyBatchDiesWithinLeadDays(t *testing.T) {
	// 600 usable — plenty — but all in one batch that dies in 40 days.
	svc, store, api, al := newRestock(EnvProduction, restockOn(EnvProduction), batch("Referral Friend Month", 40*day, 600))
	passAt(svc, store, api, base)
	if api.createCount() != 1 {
		t.Fatalf("creates = %d, want an early restock", api.createCount())
	}
	// From then on the old batch slides into the 30-day warning, the margin
	// and out: no expiring alert, no second batch.
	for i := 1; i <= 45; i++ {
		if rep := passAt(svc, store, api, base.Add(time.Duration(i)*day)); rep.Alerted != 0 {
			t.Fatalf("day %d: pool alert %+v", i, rep)
		}
	}
	if api.createCount() != 1 || len(mailsWith(al, "Referral code pool")) != 0 {
		t.Errorf("creates=%d pool mails=%+v", api.createCount(), mailsWith(al, "Referral code pool"))
	}
}

func TestRestockLeadTriggerBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		batches []PoolBatch
		want    int
	}{
		{"one batch dying in 45 days", []PoolBatch{batch("a", 45*day, 600)}, 1},
		{"one batch dying in 46 days", []PoolBatch{batch("a", 46*day, 600)}, 0},
		{"dying soon, 300 behind it", []PoolBatch{batch("a", 20*day, 600), batch("b", 150*day, 300)}, 0},
		{"dying soon, 299 behind it", []PoolBatch{batch("a", 20*day, 600), batch("b", 150*day, 299)}, 1},
		// Behind it but dying within the margin of it: no stand-in (TakeStock).
		{"next batch only a margin behind", []PoolBatch{batch("a", 20*day, 600), batch("b", 34*day, 900)}, 1},
	}
	for _, c := range cases {
		svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), c.batches...)
		passAt(svc, store, api, base)
		if api.createCount() != c.want {
			t.Errorf("%s: creates = %d, want %d", c.name, api.createCount(), c.want)
		}
	}
}

func TestRestockLeadDaysClampedWhenANewBatchCouldNeverCount(t *testing.T) {
	buf := captureLog(t)
	rs := restockOn(EnvSandbox)
	rs.LeadDays = 170 // + margin 14 >= 179 → 164
	svc, store, api, _ := newRestock(EnvSandbox, rs, batch("a", 165*day, 600))
	passAt(svc, store, api, base)
	if api.createCount() != 0 {
		t.Fatalf("165 days out is past the clamped 164: creates = %d", api.createCount())
	}
	passAt(svc, store, api, base.Add(10*time.Minute))
	if n := strings.Count(buf.String(), "using restock_lead_days=164"); n != 1 {
		t.Errorf("clamp WARN logged %d times, want once:\n%s", n, buf.String())
	}
	store.pool = poolWith(poolDefaults(), batch("a", 164*day, 600))
	passAt(svc, store, api, base.Add(20*time.Minute))
	if api.createCount() != 1 {
		t.Fatalf("164 days out: creates = %d, want 1", api.createCount())
	}
	// The new batch (179 days) outlives the old one by more than the margin:
	// it counts, so the early restock does not fire again.
	passAt(svc, store, api, base.Add(2*time.Hour))
	if api.createCount() != 1 {
		t.Errorf("early restock fired again: creates = %d", api.createCount())
	}
}

// --- alerts while a restock is under way ------------------------------------------

func TestRestockUnderWayHoldsTheLowStockMailUntilItTimesOut(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	api.createErr = &ASCError{Op: "create", Uncertain: true, Err: context.DeadlineExceeded}

	rep := passAt(svc, store, api, base)
	if rows := store.restockRows(); len(rows) != 1 || rows[0].State != RestockCreating {
		t.Fatalf("after an unknown outcome: %+v", rows)
	}
	// 42 < low_stock_threshold 100: logged, not mailed.
	if rep.Alerted != 0 || len(al.sent) != 0 {
		t.Fatalf("low-stock mail sent while the restock is under way: %+v", al.sent)
	}
	for _, m := range []time.Duration{10, 20, 29} {
		if rep = passAt(svc, store, api, base.Add(m*time.Minute)); rep.Alerted != 0 || len(al.sent) != 0 {
			t.Fatalf("+%dm: mails %+v", m, al.sent)
		}
	}
	if api.lists < 3 {
		t.Errorf("not reconciling every pass: %d lists", api.lists)
	}
	// 30 minutes, nothing found: failed + alert, and the low-stock mail is
	// no longer held.
	rep = passAt(svc, store, api, base.Add(30*time.Minute))
	rows := store.restockRows()
	if rows[0].State != RestockFailed || !strings.Contains(rows[0].Error, "no matching App Store Connect batch") {
		t.Fatalf("after 30 min: %+v", rows[0])
	}
	got := mailsWith(al, "[SANDBOX] Referral code restock: outcome unknown, no matching batch")
	if len(got) != 1 || rep.Alerted != 1 || len(mailsWith(al, "low stock (42 usable)")) != 1 {
		t.Fatalf("mails = %+v report %+v", al.sent, rep)
	}
	for _, needle := range []string{"已记 failed", "App Store Connect", wantExpiry, `import-codes.sh codes.csv "Referral Friend Month" ` + wantExpiry, "1000 张"} {
		if !strings.Contains(got[0].body, needle) {
			t.Errorf("reconcile alert missing %q:\n%s", needle, got[0].body)
		}
	}
	if api.createCount() != 1 {
		t.Errorf("creates = %d, want 1 (never re-requested while reconciling)", api.createCount())
	}
}

// --- unknown outcome: reconcile ------------------------------------------------------

func TestRestockUnknownOutcomeIsReconciledAgainstTheBatchList(t *testing.T) {
	svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	api.createErr = &ASCError{Op: "create", Status: 502, Uncertain: true}
	api.createdAnyway = true // Apple made it; the answer was lost

	passAt(svc, store, api, base)
	if rows := store.restockRows(); rows[0].State != RestockCreating {
		t.Fatalf("5xx: %+v", rows[0])
	}
	made := api.batches[0].ID
	api.batches[0].CreatedDate = base.Add(-3 * time.Minute) // Apple's clock a bit behind: inside the 5-minute slack
	api.createErr = nil
	// An older batch of the same shape is not it: created before start − 5 min.
	api.addBatch(1000, wantExpiry, EnvSandbox, base.Add(-6*time.Minute))
	// Nor one of another shape inside the window.
	api.addBatch(500, wantExpiry, EnvSandbox, base.Add(time.Minute))
	api.addBatch(1000, wantExpiry, EnvProduction, base.Add(time.Minute))
	api.addBatch(1000, "2027-04-18", EnvSandbox, base.Add(time.Minute))

	rep := passAt(svc, store, api, base.Add(10*time.Minute))
	rows := store.restockRows()
	if rows[0].State != RestockImported || rows[0].BatchID != made || rep.Restocked != 1000 {
		t.Fatalf("reconcile: %+v rep %+v (want batch %s)", rows[0], rep, made)
	}
	if api.createCount() != 1 {
		t.Errorf("creates = %d, want 1", api.createCount())
	}
}

func TestRestockReconcileNeverClaimsABatchAnotherRestockHolds(t *testing.T) {
	start := base.Add(2 * time.Hour)
	setup := func() (*Service, *memStore, *fakeASC, *fakeAlerter, string) {
		svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
		// Same day, same count and expiry, and (Apple's clock) inside the window.
		x := api.addBatch(1000, wantExpiry, EnvSandbox, start.Add(time.Minute))
		store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), BatchID: x, State: RestockImported, StartedAt: base})
		store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), State: RestockCreating, StartedAt: start})
		return svc, store, api, al, x
	}

	svc, store, api, al, _ := setup()
	passAt(svc, store, api, start.Add(10*time.Minute))
	if rows := store.restockRows(); rows[1].State != RestockCreating {
		t.Fatalf("claimed the first restock's batch: %+v", rows[1])
	}
	passAt(svc, store, api, start.Add(30*time.Minute))
	rows := store.restockRows()
	if rows[1].State != RestockFailed || rows[1].BatchID != "" {
		t.Fatalf("after 30 min: %+v", rows[1])
	}
	if got := mailsWith(al, "no matching batch"); len(got) != 1 || !strings.Contains(got[0].body, "all claimed by other restocks") {
		t.Errorf("mail = %+v", got)
	}

	// With an unclaimed batch of the same shape, that one is taken.
	svc, store, api, _, x := setup()
	y := api.addBatch(1000, wantExpiry, EnvSandbox, start.Add(2*time.Minute))
	passAt(svc, store, api, start.Add(10*time.Minute))
	rows = store.restockRows()
	if rows[1].State != RestockImported || rows[1].BatchID != y || rows[0].BatchID != x {
		t.Errorf("rows = %+v (want the second on %s)", rows, y)
	}
	if api.createCount() != 0 {
		t.Errorf("creates = %d", api.createCount())
	}
}

func TestRestockAfterARestartMidRequestReconciles(t *testing.T) {
	svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	// A previous process recorded creating, sent the request and died.
	store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), State: RestockCreating, StartedAt: base})
	made := api.addBatch(1000, wantExpiry, EnvSandbox, base.Add(20*time.Second))

	rep := passAt(svc, store, api, base.Add(4*time.Minute))
	rows := store.restockRows()
	if rows[0].State != RestockImported || rows[0].BatchID != made || rep.Restocked != 1000 || api.createCount() != 0 {
		t.Fatalf("rows %+v rep %+v creates %d", rows, rep, api.createCount())
	}
}

func TestRestockRecordFailureAfterAppleCreatedIsReconciled(t *testing.T) {
	svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	store.rs.markErr = errors.New("connection reset by peer")
	rep := passAt(svc, store, api, base)
	if rows := store.restockRows(); rows[0].State != RestockCreating || rep.Errors == 0 {
		t.Fatalf("rows %+v rep %+v", rows, rep)
	}
	passAt(svc, store, api, base.Add(10*time.Minute))
	rows := store.restockRows()
	if rows[0].State != RestockImported || rows[0].BatchID != api.batches[0].ID || api.createCount() != 1 {
		t.Errorf("rows %+v creates %d", rows, api.createCount())
	}
}

// --- created: import, resume, give up -------------------------------------------------

func TestRestockCreatedResumesImportWithoutANewRequest(t *testing.T) {
	svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	made := api.addBatch(1000, wantExpiry, EnvSandbox, base)
	// Batch recorded, process gone before the import.
	store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), BatchID: made, State: RestockCreated, StartedAt: base})

	api.valuesErr = &ASCError{Op: "values", Status: 503, Uncertain: true}
	rep := passAt(svc, store, api, base.Add(10*time.Minute))
	if rows := store.restockRows(); rows[0].State != RestockCreated || rep.Errors != 1 {
		t.Fatalf("values failing: %+v rep %+v", rows[0], rep)
	}
	api.valuesErr = nil
	rep = passAt(svc, store, api, base.Add(20*time.Minute))
	if rows := store.restockRows(); rows[0].State != RestockImported || rep.Restocked != 1000 {
		t.Fatalf("resumed: %+v rep %+v", rows[0], rep)
	}
	if api.createCount() != 0 {
		t.Errorf("an unfinished restock must not start another: creates = %d", api.createCount())
	}
}

func TestRestockFewerValuesThanAskedForWaits(t *testing.T) {
	svc, store, api, _ := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	api.valuesShort = 400
	passAt(svc, store, api, base)
	if rows := store.restockRows(); rows[0].State != RestockCreated {
		t.Fatalf("partial values imported: %+v", rows[0])
	}
	api.valuesShort = 0
	passAt(svc, store, api, base.Add(10*time.Minute))
	if rows := store.restockRows(); rows[0].State != RestockImported || rows[0].ImportedCount != 1000 {
		t.Fatalf("%+v", rows[0])
	}
}

func TestRestockNotImportedWithinSixHoursFailsWithTheBatchID(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 500))
	x := api.addBatch(1000, wantExpiry, EnvSandbox, base)
	store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), BatchID: x, State: RestockCreated, StartedAt: base})
	api.valuesErr = &ASCError{Op: "values", Status: 500, Uncertain: true}

	for _, at := range []time.Duration{time.Hour, 3 * time.Hour, 6*time.Hour - time.Minute} {
		passAt(svc, store, api, base.Add(at))
		if rows := store.restockRows(); rows[0].State != RestockCreated {
			t.Fatalf("+%s: %+v", at, rows[0])
		}
	}
	passAt(svc, store, api, base.Add(6*time.Hour))
	if rows := store.restockRows(); rows[0].State != RestockFailed || !strings.Contains(rows[0].Error, x) {
		t.Fatalf("after 6 h: %+v", rows[0])
	}
	got := mailsWith(al, "batch "+x+" not imported in 6h")
	if len(got) != 1 || !strings.Contains(got[0].subject, "[SANDBOX]") {
		t.Fatalf("mails = %+v", al.sent)
	}
	for _, needle := range []string{"苹果批次 ID：" + x, `import-codes.sh codes.csv "Referral Friend Month" ` + wantExpiry, "重复导入安全"} {
		if !strings.Contains(got[0].body, needle) {
			t.Errorf("missing %q:\n%s", needle, got[0].body)
		}
	}
	// Mails carrying a batch id go one per batch, even on the same day.
	y := api.addBatch(1000, wantExpiry, EnvSandbox, base.Add(6*time.Hour))
	store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), BatchID: y, State: RestockCreated, StartedAt: base.Add(6 * time.Hour)})
	passAt(svc, store, api, base.Add(12*time.Hour))
	if len(mailsWith(al, "not imported in 6h")) != 2 || len(mailsWith(al, "batch "+y)) != 1 {
		t.Errorf("second batch's mail: %+v", al.sent)
	}
}

// --- limits ----------------------------------------------------------------------------

func TestRestockOneHourApartAndDailyMax(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	t0 := base.Add(time.Hour)
	passAt(svc, store, api, t0)
	if api.createCount() != 1 {
		t.Fatalf("first: creates = %d", api.createCount())
	}
	drain := func() { store.pool = lowPool(42) } // the new codes all handed out

	drain()
	passAt(svc, store, api, t0.Add(59*time.Minute))
	if api.createCount() != 1 {
		t.Fatalf("59 minutes later: creates = %d", api.createCount())
	}
	passAt(svc, store, api, t0.Add(61*time.Minute))
	if api.createCount() != 2 {
		t.Fatalf("61 minutes later: creates = %d", api.createCount())
	}
	// restock_daily_max = 2: the third is refused today and, still short,
	// mails once.
	drain()
	passAt(svc, store, api, t0.Add(3*time.Hour))
	passAt(svc, store, api, t0.Add(3*time.Hour+10*time.Minute))
	if api.createCount() != 2 {
		t.Fatalf("over the daily max: creates = %d", api.createCount())
	}
	got := mailsWith(al, "[SANDBOX] Referral code restock: daily max reached, 42 usable")
	if len(got) != 1 || !strings.Contains(got[0].body, "已发起 2 次，上限 restock_daily_max = 2") {
		t.Fatalf("daily max mail: %+v", al.sent)
	}
	// Next UTC day: allowed again.
	passAt(svc, store, api, base.Add(day+time.Minute))
	if api.createCount() != 3 {
		t.Errorf("next day: creates = %d", api.createCount())
	}
}

func TestRestockDailyMaxMailsOnlyWhenStillShort(t *testing.T) {
	// Two restocks already today (both failed); the early trigger holds but
	// 600 are usable — enough: no mail.
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("a", 40*day, 600))
	for _, h := range []time.Duration{1, 3} {
		id := store.seedRestock(Restock{Environment: EnvSandbox, Codes: 1000, ExpiresAt: restockExpiry(base), State: RestockCreating, StartedAt: base.Add(h * time.Hour)})
		_ = store.FailRestock(context.Background(), id, "test", base.Add(h*time.Hour))
	}
	passAt(svc, store, api, base.Add(5*time.Hour))
	if api.createCount() != 0 || len(mailsWith(al, "daily max")) != 0 {
		t.Fatalf("creates %d mails %+v", api.createCount(), al.sent)
	}
	store.pool = lowPool(42)
	passAt(svc, store, api, base.Add(6*time.Hour))
	if api.createCount() != 0 || len(mailsWith(al, "daily max")) != 1 {
		t.Errorf("creates %d mails %+v", api.createCount(), al.sent)
	}
}

// --- definite failures ------------------------------------------------------------------

func TestRestockAppleRefusalFailsAtOnceOneMailPerKindPerDay(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	api.createErr = &ASCError{Op: "create", Status: 403, Detail: "FORBIDDEN_ERROR — The API key in use does not allow this request"}

	rep := passAt(svc, store, api, base.Add(time.Hour))
	rows := store.restockRows()
	if rows[0].State != RestockFailed || !strings.Contains(rows[0].Error, "HTTP 403") || !strings.Contains(rows[0].Error, "FORBIDDEN_ERROR") {
		t.Fatalf("row = %+v", rows[0])
	}
	refusals := mailsWith(al, "[SANDBOX] Referral code restock: Apple refused the batch (http_403)")
	if len(refusals) != 1 || !strings.Contains(refusals[0].body, "FORBIDDEN_ERROR") {
		t.Fatalf("mails = %+v", al.sent)
	}
	// Not busy any more: the low-stock mail goes out too.
	if rep.Alerted != 1 {
		t.Errorf("pool alert held after a definite failure: %+v", rep)
	}
	// An hour later it tries again — no second mail for the same kind today.
	passAt(svc, store, api, base.Add(2*time.Hour+time.Minute))
	if api.createCount() != 2 || len(mailsWith(al, "Apple refused the batch (http_403)")) != 1 {
		t.Fatalf("retry: creates %d mails %+v", api.createCount(), al.sent)
	}
	// A different kind of failure is its own mail.
	api.createErr = &ASCError{Op: "create", Err: syscall.ECONNREFUSED} // never left: not uncertain
	svc2, store2, api2, al2 := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	api2.createErr = api.createErr
	passAt(svc2, store2, api2, base)
	if rows := store2.restockRows(); rows[0].State != RestockFailed {
		t.Fatalf("connection refused: %+v", rows[0])
	}
	if len(mailsWith(al2, "Apple refused the batch (unreachable)")) != 1 {
		t.Errorf("unreachable mail: %+v", al2.sent)
	}
	// Next day: the 403 mails again.
	api.createErr = &ASCError{Op: "create", Status: 403}
	passAt(svc, store, api, base.Add(day+time.Hour))
	if len(mailsWith(al, "Apple refused the batch (http_403)")) != 2 {
		t.Errorf("next day: %+v", al.sent)
	}
}

// --- configuration ---------------------------------------------------------------------

func TestRestockNotConfiguredMailsOnceADayAndIsNoStart(t *testing.T) {
	store, al, api := newMemStore(), &fakeAlerter{}, newFakeASC()
	store.pool = lowPool(42)
	store.rs.settings = restockOn(EnvSandbox)
	svc := New(store, newFakeRC(), entID, okAuth, Options{Alerter: al, Restock: RestockConfig{
		API: api, OfferID: testOfferID, Environment: EnvSandbox, Missing: []string{"ASC_PRIVATE_KEY"},
	}})
	passAt(svc, store, api, base)
	passAt(svc, store, api, base.Add(10*time.Minute))
	if api.calls() != 0 || len(store.restockRows()) != 0 {
		t.Fatalf("not configured: %d calls, rows %+v", api.calls(), store.restockRows())
	}
	got := mailsWith(al, "[SANDBOX] Referral code restock: not configured")
	if len(got) != 1 || !strings.Contains(got[0].body, "ASC_PRIVATE_KEY") {
		t.Fatalf("mails = %+v", al.sent)
	}
	passAt(svc, store, api, base.Add(day))
	if len(mailsWith(al, "not configured")) != 2 {
		t.Errorf("next day: %+v", al.sent)
	}
	if st, _ := store.RestockStatus(context.Background(), base); st.StartedToday != 0 {
		t.Errorf("counted as a start: %+v", st)
	}

	// Nothing at all configured: the names come from what is visibly absent.
	store2, al2 := newMemStore(), &fakeAlerter{}
	store2.pool = lowPool(42)
	store2.rs.settings = restockOn(EnvProduction)
	svc2 := New(store2, newFakeRC(), entID, okAuth, Options{Alerter: al2})
	settleAt(svc2, store2, base)
	got = mailsWith(al2, "[PRODUCTION] Referral code restock: not configured")
	if len(got) != 1 {
		t.Fatalf("bare config: %+v", al2.sent)
	}
	for _, needle := range []string{"ASC_KEY_ID", "ASC_REFERRAL_OFFER_ID", "ASC_OFFER_CODE_ENVIRONMENT"} {
		if !strings.Contains(got[0].body, needle) {
			t.Errorf("missing %q:\n%s", needle, got[0].body)
		}
	}
}

func TestRestockCodeEnvironmentMustAgree(t *testing.T) {
	for _, dbEnv := range []string{"", EnvProduction} {
		rs := restockOn(dbEnv)
		svc, store, api, al := newRestock(EnvSandbox, rs, batch("Dec", 90*day, 42))
		passAt(svc, store, api, base)
		passAt(svc, store, api, base.Add(10*time.Minute))
		if api.calls() != 0 || len(store.restockRows()) != 0 {
			t.Fatalf("db=%q: %d calls rows %+v", dbEnv, api.calls(), store.restockRows())
		}
		got := mailsWith(al, "[SANDBOX] Referral code restock: refused, code environment mismatch")
		if len(got) != 1 {
			t.Fatalf("db=%q: mails %+v", dbEnv, al.sent)
		}
		want := dbEnv
		if want == "" {
			want = "（空）"
		}
		if !strings.Contains(got[0].body, "referral_settings.restock_environment："+want) || !strings.Contains(got[0].body, "ASC_OFFER_CODE_ENVIRONMENT：SANDBOX") {
			t.Errorf("db=%q body:\n%s", dbEnv, got[0].body)
		}
		// The pool alert still goes out (nothing is under way).
		if len(mailsWith(al, "Referral code pool: low stock")) != 1 {
			t.Errorf("db=%q: pool alert %+v", dbEnv, al.sent)
		}
	}
}

func TestRestockBatchSizeClampedForProductionCodesOnly(t *testing.T) {
	buf := captureLog(t)
	rs := restockOn(EnvProduction)
	rs.BatchSize = 100
	svc, store, api, _ := newRestock(EnvProduction, rs, batch("Dec", 90*day, 0))
	passAt(svc, store, api, base)
	if api.createCount() != 1 || api.creates[0].NumberOfCodes != 500 {
		t.Fatalf("creates = %+v, want 500", api.creates)
	}
	if !strings.Contains(buf.String(), "WARN referral restock settings: restock_batch_size=100 is outside Apple's 500..25000 for PRODUCTION codes: generating 500") {
		t.Errorf("no clamp WARN:\n%s", buf.String())
	}
	// Sandbox codes have no 500 floor (a 10-code batch, as on staging).
	rs = restockOn(EnvSandbox)
	rs.BatchSize, rs.Below = 10, 5
	svc, store, api, _ = newRestock(EnvSandbox, rs, batch("Dec", 90*day, 0))
	passAt(svc, store, api, base)
	if api.createCount() != 1 || api.creates[0].NumberOfCodes != 10 {
		t.Fatalf("sandbox creates = %+v, want 10", api.creates)
	}
	if rows := store.restockRows(); rows[0].ImportedCount != 10 {
		t.Errorf("sandbox rows %+v", rows)
	}
}

func TestPlanRestockSettingsCombinations(t *testing.T) {
	if p := planRestock(restockOn(EnvProduction), poolDefaults(), EnvProduction); len(p.problems) != 0 || p.batch != 1000 || p.leadDays != 45 {
		t.Fatalf("defaults: %+v", p)
	}
	cases := []struct {
		name   string
		mutate func(*RestockSettings, *PoolSettings)
		env    string
		needle string
		batch  int
		lead   int
	}{
		{"batch <= below", func(r *RestockSettings, _ *PoolSettings) { r.BatchSize = 300 }, EnvSandbox, "restock_batch_size=300 <= restock_below=300", 300, 45},
		{"below <= low stock threshold", func(r *RestockSettings, _ *PoolSettings) { r.Below = 100 }, EnvSandbox, "restock_below=100 <= low_stock_threshold=100", 1000, 45},
		{"lead <= expiry warn", func(r *RestockSettings, _ *PoolSettings) { r.LeadDays = 30 }, EnvSandbox, "restock_lead_days=30 <= expiry_warn_days=30", 1000, 30},
		{"production batch under 500", func(r *RestockSettings, _ *PoolSettings) { r.BatchSize = 100 }, EnvProduction, "generating 500", 500, 45},
		{"lead + margin >= 179", func(r *RestockSettings, _ *PoolSettings) { r.LeadDays = 170 }, EnvSandbox, "using restock_lead_days=164", 1000, 164},
		{"lead + bigger margin", func(r *RestockSettings, p *PoolSettings) { r.LeadDays = 150; p.IssueMarginDays = 30 }, EnvSandbox, "using restock_lead_days=148", 1000, 148},
	}
	for _, c := range cases {
		rs, ps := restockOn(c.env), poolDefaults()
		c.mutate(&rs, &ps)
		p := planRestock(rs, ps, c.env)
		if !strings.Contains(strings.Join(p.problems, "\n"), c.needle) || p.batch != c.batch || p.leadDays != c.lead {
			t.Errorf("%s: %+v", c.name, p)
		}
	}
	// Problems only WARN: the sandbox 100-code batch is not clamped.
	rs := restockOn(EnvSandbox)
	rs.BatchSize = 100
	if p := planRestock(rs, poolDefaults(), EnvSandbox); p.batch != 100 {
		t.Errorf("sandbox clamped: %+v", p)
	}
}

// --- the pool --------------------------------------------------------------------------

func TestRestockDuplicateCodesAreIgnored(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	// The fake's first batch is 622601; two of its codes are already in the pool.
	store.seedPoolCodes("C622601000000", "C622601000001")
	rep := passAt(svc, store, api, base)
	rows := store.restockRows()
	if rows[0].State != RestockImported || rows[0].ImportedCount != 998 || rep.Restocked != 998 || rep.Errors != 0 {
		t.Fatalf("rows %+v rep %+v", rows, rep)
	}
	if got := mailsWith(al, "added 998 codes"); len(got) != 1 || !strings.Contains(got[0].body, "2 张库里已有，忽略") {
		t.Errorf("mail = %+v", al.sent)
	}
}

func TestRestockTwoProcessesAtOnceStartOnlyOne(t *testing.T) {
	for round := 0; round < 25; round++ {
		store, api := newMemStore(), newFakeASC()
		store.pool = lowPool(42)
		store.rs.settings = restockOn(EnvSandbox)
		api.at = base
		var svcs []*Service
		for i := 0; i < 4; i++ {
			s := New(store, newFakeRC(), entID, okAuth, Options{Alerter: &fakeAlerter{}, Restock: RestockConfig{API: api, OfferID: testOfferID, Environment: EnvSandbox}})
			s.now = func() time.Time { return base }
			svcs = append(svcs, s)
		}
		store.dueNow = base
		var wg sync.WaitGroup
		for _, s := range svcs {
			wg.Add(1)
			go func(s *Service) {
				defer wg.Done()
				s.Settle(context.Background())
			}(s)
		}
		wg.Wait()
		if api.createCount() != 1 || len(store.restockRows()) != 1 {
			t.Fatalf("round %d: creates %d rows %+v", round, api.createCount(), store.restockRows())
		}
	}
}

// --- store trouble -----------------------------------------------------------------------

func TestRestockStatusTroubleNeverStopsThePoolAlert(t *testing.T) {
	svc, store, api, al := newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	store.rs.statusErr = errors.New("connection refused")
	rep := passAt(svc, store, api, base)
	if rep.Errors != 1 || rep.Alerted != 1 || api.calls() != 0 || len(al.sent) != 1 {
		t.Fatalf("status error: %+v sent %+v", rep, al.sent)
	}
	// 021 not applied: auto-restock is simply off — no error counted.
	svc, store, api, al = newRestock(EnvSandbox, restockOn(EnvSandbox), batch("Dec", 90*day, 42))
	store.rs.statusErr = ErrRestockNotMigrated
	rep = passAt(svc, store, api, base)
	if rep.Errors != 0 || rep.Alerted != 1 || api.calls() != 0 {
		t.Fatalf("not migrated: %+v", rep)
	}
}

func TestRestockExpiryIsTodayUTCPlus179(t *testing.T) {
	// Late in the evening Pacific is already tomorrow in UTC.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata")
	}
	at := time.Date(2026, 10, 21, 20, 0, 0, 0, la) // 2026-10-22 03:00 UTC
	if got := restockExpiry(at); !got.Equal(time.Date(2027, 4, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expiry = %v", got)
	}
	if got := restockExpiry(base); ymd(got) != wantExpiry {
		t.Errorf("expiry from base = %v", got)
	}
}
