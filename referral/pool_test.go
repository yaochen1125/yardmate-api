package referral

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Settings as migration 014 defaults them.
func poolDefaults() PoolSettings {
	return PoolSettings{IssueMarginDays: 14, LowStockThreshold: 100, ExpiryWarnDays: 30}
}

func batch(name string, expiresIn time.Duration, free int) PoolBatch {
	return PoolBatch{Name: name, ExpiresAt: base.Add(expiresIn), Free: free, Issued: 5, Redeemed: 7, Expired: 2}
}

func poolWith(settings PoolSettings, batches ...PoolBatch) PoolStatus {
	return PoolStatus{Settings: settings, Batches: batches}
}

func newWatch(ps PoolStatus) (*Service, *memStore, *fakeAlerter) {
	store, rc, al := newMemStore(), newFakeRC(), &fakeAlerter{}
	store.pool = ps
	return New(store, rc, entID, okAuth, Options{Alerter: al}), store, al
}

// --- TakeStock: the issue-margin口径 the SQL side also applies -----------------

func TestTakeStockOnlyCountsCodesBeyondTheIssueMargin(t *testing.T) {
	// 10 days left is inside the 14-day margin: those 50 are shelf-dead.
	ps := poolWith(poolDefaults(),
		batch("Aug", 10*day, 50),
		batch("Dec", 90*day, 80),
	)
	st := TakeStock(ps, base)
	if st.Usable != 80 || st.Short != 50 {
		t.Fatalf("usable=%d short=%d, want 80 / 50", st.Usable, st.Short)
	}
	if !st.Earliest.Equal(base.Add(90 * day)) {
		t.Errorf("earliest = %v, want the December batch (the August one is not issuable)", st.Earliest)
	}
	// Exactly on the cutoff is NOT beyond it (SQL: expires_at > cutoff).
	st = TakeStock(poolWith(poolDefaults(), batch("edge", 14*day, 10)), base)
	if st.Usable != 0 || st.Short != 10 {
		t.Errorf("on the cutoff: usable=%d short=%d, want 0 / 10", st.Usable, st.Short)
	}
	st = TakeStock(poolWith(poolDefaults(), batch("edge", 14*day+time.Second, 10)), base)
	if st.Usable != 10 {
		t.Errorf("a second past the cutoff: usable=%d, want 10", st.Usable)
	}
}

func TestTakeStockEarliestIsTheSoonestUsableBatchWhateverTheOrder(t *testing.T) {
	ps := poolWith(poolDefaults(),
		batch("late", 150*day, 20),
		batch("empty-early", 40*day, 0), // nothing free: not a source of codes
		batch("soon", 60*day, 20),
	)
	st := TakeStock(ps, base)
	if !st.Earliest.Equal(base.Add(60*day)) || st.Usable != 40 {
		t.Errorf("earliest=%v usable=%d, want day 60 / 40", st.Earliest, st.Usable)
	}
	if st := TakeStock(poolWith(poolDefaults()), base); !st.Earliest.IsZero() || st.Usable != 0 || !st.LowStock || st.Expiring {
		t.Errorf("empty pool: %+v", st)
	}
}

func TestTakeStockConditions(t *testing.T) {
	cases := []struct {
		name     string
		settings PoolSettings
		batches  []PoolBatch
		low, exp bool
	}{
		{"healthy", poolDefaults(), []PoolBatch{batch("a", 120*day, 500)}, false, false},
		{"below threshold", poolDefaults(), []PoolBatch{batch("a", 120*day, 99)}, true, false},
		{"at threshold is fine", poolDefaults(), []PoolBatch{batch("a", 120*day, 100)}, false, false},
		{"earliest within 30 days", poolDefaults(), []PoolBatch{batch("a", 20*day, 500), batch("b", 150*day, 500)}, false, true},
		{"exactly 30 days counts", poolDefaults(), []PoolBatch{batch("a", 30*day, 500)}, false, true},
		{"31 days does not", poolDefaults(), []PoolBatch{batch("a", 31*day, 500)}, false, false},
		{"both", poolDefaults(), []PoolBatch{batch("a", 20*day, 10)}, true, true},
		{"only short codes: low, not expiring", poolDefaults(), []PoolBatch{batch("a", 5*day, 500)}, true, false},
		{"threshold 0 turns low-stock off", PoolSettings{IssueMarginDays: 14, LowStockThreshold: 0, ExpiryWarnDays: 30}, nil, false, false},
		{"warn_days 0 turns expiry off", PoolSettings{IssueMarginDays: 14, LowStockThreshold: 100, ExpiryWarnDays: 0}, []PoolBatch{batch("a", 15*day, 500)}, false, false},
	}
	for _, c := range cases {
		st := TakeStock(poolWith(c.settings, c.batches...), base)
		if st.LowStock != c.low || st.Expiring != c.exp {
			t.Errorf("%s: low=%v expiring=%v, want %v / %v (%+v)", c.name, st.LowStock, st.Expiring, c.low, c.exp, st)
		}
	}
}

// --- the watch inside a settlement pass ------------------------------------

func TestWatchPoolAlertsBelowThresholdAtMostOncePerDay(t *testing.T) {
	svc, store, al := newWatch(poolWith(poolDefaults(), batch("Dec", 90*day, 42)))

	rep := settleAt(svc, store, base)
	if rep.Alerted != 1 || rep.Usable != 42 || rep.Errors != 0 || len(al.sent) != 1 {
		t.Fatalf("first pass: %+v sent=%d", rep, len(al.sent))
	}
	if s := al.sent[0].subject; !strings.HasPrefix(s, "[YardMate] Referral code pool: low stock (42 usable)") {
		t.Errorf("subject = %q", s)
	}
	// Same day, every 10 minutes: WARN only, no second mail.
	for i := 1; i <= 3; i++ {
		if rep = settleAt(svc, store, base.Add(time.Duration(i)*10*time.Minute)); rep.Alerted != 0 || len(al.sent) != 1 {
			t.Fatalf("pass %d same day: %+v sent=%d", i, rep, len(al.sent))
		}
	}
	// Next day: one more.
	if rep = settleAt(svc, store, base.Add(day)); rep.Alerted != 1 || len(al.sent) != 2 {
		t.Errorf("next day: %+v sent=%d", rep, len(al.sent))
	}
}

func TestWatchPoolAlertsWhenEarliestBatchExpiresSoon(t *testing.T) {
	svc, store, al := newWatch(poolWith(poolDefaults(),
		batch("Nov", 20*day, 300),
		batch("Mar", 150*day, 300),
	))
	rep := settleAt(svc, store, base)
	if rep.Alerted != 1 || len(al.sent) != 1 {
		t.Fatalf("%+v sent=%d", rep, len(al.sent))
	}
	want := base.Add(20 * day).Format("2006-01-02")
	if s := al.sent[0].subject; s != "[YardMate] Referral code pool: expiring "+want {
		t.Errorf("subject = %q", s)
	}
	for _, needle := range []string{"Nov", "Mar", want, "还有 20 天", "预警线 30 天", restockCommand, "deploy/referral-pool.sh", "可发（free 且到期日在 14 天余量之外）：600 张"} {
		if !strings.Contains(al.sent[0].body, needle) {
			t.Errorf("body missing %q:\n%s", needle, al.sent[0].body)
		}
	}
}

func TestWatchPoolConditionChangeSameDaySendsAgain(t *testing.T) {
	svc, store, al := newWatch(poolWith(poolDefaults(), batch("Dec", 90*day, 42)))
	settleAt(svc, store, base)
	// An hour later the December batch is suddenly the one about to die.
	store.pool = poolWith(poolDefaults(), batch("Dec", 20*day, 42))
	rep := settleAt(svc, store, base.Add(time.Hour))
	if rep.Alerted != 1 || len(al.sent) != 2 {
		t.Fatalf("condition changed: %+v sent=%d", rep, len(al.sent))
	}
	if s := al.sent[1].subject; !strings.Contains(s, "low stock (42 usable), expiring ") {
		t.Errorf("subject = %q", s)
	}
	// Restocked: no longer low, but the December batch still dies in 20 days —
	// a different set of conditions, so one more mail (saying only "expiring").
	store.pool = poolWith(poolDefaults(), batch("Dec", 20*day, 42), batch("Jun", 170*day, 900))
	if rep = settleAt(svc, store, base.Add(2*time.Hour)); rep.Alerted != 1 || rep.Usable != 942 || len(al.sent) != 3 {
		t.Fatalf("restocked: %+v sent=%d", rep, len(al.sent))
	}
	if s := al.sent[2].subject; strings.Contains(s, "low stock") || !strings.Contains(s, "expiring") {
		t.Errorf("subject = %q", s)
	}
	// Nothing wrong any more: silence.
	store.pool = poolWith(poolDefaults(), batch("Jun", 170*day, 900))
	if rep = settleAt(svc, store, base.Add(3*time.Hour)); rep.Alerted != 0 || len(al.sent) != 3 {
		t.Errorf("healthy again: %+v sent=%d", rep, len(al.sent))
	}
}

func TestWatchPoolHealthyPoolIsSilent(t *testing.T) {
	svc, store, al := newWatch(poolWith(poolDefaults(), batch("Dec", 90*day, 500)))
	rep := settleAt(svc, store, base)
	if rep.Alerted != 0 || rep.Usable != 500 || rep.Errors != 0 || al.calls != 0 {
		t.Errorf("%+v calls=%d", rep, al.calls)
	}
}

func TestWatchPoolMailFailureIsCountedAndRetriedNextPass(t *testing.T) {
	svc, store, al := newWatch(poolWith(poolDefaults(), batch("Dec", 90*day, 1)))
	al.err = errors.New("smtp: 535 bad credentials")
	rep := settleAt(svc, store, base)
	if rep.Errors != 1 || rep.Alerted != 0 || al.calls != 1 {
		t.Fatalf("failed send: %+v calls=%d", rep, al.calls)
	}
	al.err = nil
	if rep = settleAt(svc, store, base.Add(10*time.Minute)); rep.Alerted != 1 || len(al.sent) != 1 {
		t.Errorf("retry: %+v sent=%d", rep, len(al.sent))
	}
}

func TestWatchPoolStoreErrorIsCountedAndSettlementGoesOn(t *testing.T) {
	svc, store, rc := newSettle()
	store.poolErr = errors.New("column issue_margin_days does not exist")
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	bSubscribed(rc, base)
	rep := settleAt(svc, store, base.Add(time.Hour))
	if rep.Errors != 1 || rep.Redeemed != 1 || store.get(id).State != StateRedeemed {
		t.Errorf("%+v state=%s", rep, store.get(id).State)
	}
}

func TestWatchPoolWithoutAlerterOnlyLogs(t *testing.T) {
	svc, store, _ := newSettle() // Options{} → no Alerter
	store.pool = poolWith(poolDefaults(), batch("Dec", 90*day, 0))
	rep := settleAt(svc, store, base)
	if rep.Alerted != 0 || rep.Errors != 0 || rep.Usable != 0 {
		t.Errorf("%+v", rep)
	}
}

func TestAlertMailListsEveryBatchAndTheRestockLine(t *testing.T) {
	ps := poolWith(poolDefaults(),
		batch("Referral Friend Month", 10*day, 30),
		batch("Referral Friend Month", 170*day, 60),
	)
	st := TakeStock(ps, base)
	subject, body := alertMail(ps, st, base)
	for _, r := range subject {
		if r > 0x7e || r < 0x20 {
			t.Fatalf("subject must stay printable ASCII: %q", subject)
		}
	}
	for _, needle := range []string{
		"低于阈值 100",
		"余量不足、不再发出的 free 码：30 张",
		base.Add(10 * day).Format("2006-01-02"),
		base.Add(170 * day).Format("2006-01-02"),
		"issued", "redeemed", "expired",
		`import-codes.sh codes.csv "Referral Friend Month" YYYY-MM-DD`,
		"issue_margin_days / low_stock_threshold / expiry_warn_days",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("body missing %q:\n%s", needle, body)
		}
	}
	if _, body := alertMail(poolWith(poolDefaults()), TakeStock(poolWith(poolDefaults()), base), base); !strings.Contains(body, "码池是空的") || !strings.Contains(body, "最早到期：没有可发的码") {
		t.Errorf("empty pool body:\n%s", body)
	}
}
