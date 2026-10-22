package rewards

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

func callPayout(t *testing.T, svc *Service, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/referral/payout", strings.NewReader(body))
	r.Header.Set("X-Device-Install-Id", device)
	r.Header.Set("X-App-Version", "2.2.0 (50)")
	r.Header.Set("Authorization", "Bearer x")
	for k, v := range headers {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	svc.HandlePayout()(w, r)
	return w
}

func errOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not json: %q", w.Body.String())
	}
	s, _ := out["error"].(string)
	return s
}

const okBody = `{"email":"friend@example.com"}`

// richSvc: the referrer has $231.30 available, $12.00 pending: one $230 card
// with $1.30 change (rewards.md ### 兑换金额).
func richSvc(t *testing.T) (*Service, *memStore, *fakeMailer) {
	t.Helper()
	svc, store, mailer := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 12000, LedgerAvailable, t0.Add(-time.Hour))
	store.credit(referrer, "a2", "t2", 11130, LedgerAvailable, t0.Add(-time.Hour))
	store.credit(referrer, "p1", "t3", 1200, LedgerPending, t0.Add(time.Hour))
	return svc, store, mailer
}

func TestPayoutShapeAndAuthErrors(t *testing.T) {
	svc, _, _ := richSvc(t)
	cases := []struct {
		name    string
		svc     *Service
		headers map[string]string
		body    string
		status  int
		code    string
	}{
		{"no device id", svc, map[string]string{"X-Device-Install-Id": ""}, okBody, 400, "missing_device_id"},
		{"bad device id", svc, map[string]string{"X-Device-Install-Id": "phone"}, okBody, 400, "missing_device_id"},
		{"no app version", svc, map[string]string{"X-App-Version": ""}, okBody, 400, "missing_app_version"},
		{"no database", New(nil, authAs(referrer), Options{}), nil, okBody, 503, "not_configured"},
		{"no verifier", New(newMemStore(t0), nil, Options{}), nil, okBody, 503, "not_configured"},
		{"bad token", New(newMemStore(t0), badAuth, Options{}), nil, okBody, 401, "unauthorized"},
		{"bad json", svc, nil, `{"email":`, 400, "bad_json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := callPayout(t, c.svc, c.body, c.headers)
			if w.Code != c.status || errOf(t, w) != c.code {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), c.status, c.code)
			}
		})
	}
}

func TestPayoutInvalidEmail(t *testing.T) {
	bad := []string{
		"", "   ", "friend", "friend@", "@example.com", "friend@@example.com",
		"Friend <friend@example.com>", "<friend@example.com>", "friend@example.com\r\nBcc: x@evil.com",
		"a b@example.com", "friend@example.com, other@example.com",
		strings.Repeat("a", 250) + "@x.co",
		// Sign in with Apple's private relay drops Amazon's mail: refused, any case.
		"friend@privaterelay.appleid.com", "First.Last+yardmate@PrivateRelay.AppleID.com",
	}
	for _, e := range bad {
		svc, store, mailer := richSvc(t)
		raw, _ := json.Marshal(map[string]string{"email": e})
		w := callPayout(t, svc, string(raw), nil)
		if w.Code != http.StatusBadRequest || errOf(t, w) != "invalid_email" {
			t.Errorf("%q: %d %s", e, w.Code, w.Body.String())
		}
		if len(store.payouts) != 0 || len(mailer.subjects) != 0 {
			t.Errorf("%q: stored or mailed", e)
		}
	}
	good := []string{"friend@example.com", "First.Last+yardmate@icloud.com", "  padded@example.com  ", "x@relay.appleid.com.example"}
	for _, e := range good {
		if got, ok := validEmail(e); !ok || got != strings.TrimSpace(e) {
			t.Errorf("validEmail(%q) = %q %v", e, got, ok)
		}
	}
}

func TestPayoutCreated(t *testing.T) {
	svc, store, mailer := richSvc(t)
	w := callPayout(t, svc, okBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payout PayoutView `json:"payout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	p := resp.Payout
	if p.ID == 0 || p.AmountCents != 23000 || p.State != PayoutRequested || p.RequestedAt != "2026-10-20T12:00:00Z" || p.SentAt != nil {
		t.Fatalf("payout = %+v", p)
	}
	if got := store.payouts[0]; got.UserID != referrer || got.Email != "friend@example.com" {
		t.Fatalf("stored = %+v", got)
	}

	// /me now shows the $1.30 change left and the request in progress.
	s := summaryOf(t, svc, referrer)
	if s.AvailableCents != 130 || s.PendingCents != 1200 || s.LifetimeCents != 23130 ||
		s.Payout == nil || s.Payout.State != PayoutRequested || s.Payout.ID != p.ID {
		t.Fatalf("summary after payout = %+v / %+v", s, s.Payout)
	}

	// Operator mail.
	if len(mailer.subjects) != 1 {
		t.Fatalf("mails = %d", len(mailer.subjects))
	}
	wantSubject := "[YardMate] Referral gift card request #" + itoa(p.ID) + " ($230)"
	if mailer.subjects[0] != wantSubject {
		t.Fatalf("subject = %q, want %q", mailer.subjects[0], wantSubject)
	}
	body := mailer.bodies[0]
	for _, want := range []string{
		"User id:      " + referrer,
		"Send to:      friend@example.com",
		"$230.00 Amazon.com (US) gift card",
		"buy a $230.00 Amazon.com eGift card (custom amount)",
		"calendar year 2026 (UTC)",
		"Sent:                    $0.00",
		"Requested (incl. this):  $230.00",
		"Total:                   $230.00",
		"deploy/referral-payout.sh mark-sent " + itoa(p.ID),
		"deploy/referral-payout.sh cancel " + itoa(p.ID),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mail body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "W-9") {
		t.Errorf("W-9 line at $230:\n%s", body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestPayoutBelowThreshold(t *testing.T) {
	svc, store, mailer := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 1999, LedgerAvailable, t0)
	store.credit(referrer, "p1", "t2", 50000, LedgerPending, t0.Add(time.Hour)) // pending does not count
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusConflict || errOf(t, w) != "below_threshold" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Someone who has never been a referrer at all.
	svc.auth = authAs(stranger)
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusConflict || errOf(t, w) != "below_threshold" {
		t.Fatalf("stranger: %d %s", w.Code, w.Body.String())
	}
	if len(store.payouts) != 0 || len(mailer.subjects) != 0 {
		t.Fatal("stored or mailed")
	}
}

func TestPayoutInProgress(t *testing.T) {
	// $715: the first card is capped at $500; the second request (still $215
	// left) is refused while the first is requested; once it is sent, the
	// next one is the $210 left in whole $10; then $5 is under $20.
	svc, store, _ := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 71500, LedgerAvailable, t0.Add(-time.Hour))
	amountOf := func(w *httptest.ResponseRecorder) int64 {
		t.Helper()
		var resp struct {
			Payout PayoutView `json:"payout"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Payout.AmountCents
	}
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated || amountOf(w) != 50000 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusConflict || errOf(t, w) != "payout_in_progress" {
		t.Fatalf("second: %d %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	store.payouts[0].State, store.payouts[0].SentAt = PayoutSent, t0
	store.mu.Unlock()
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated || amountOf(w) != 21000 {
		t.Fatalf("after sent: %d %s", w.Code, w.Body.String())
	}
	// $5 left now.
	store.mu.Lock()
	store.payouts[1].State, store.payouts[1].SentAt = PayoutSent, t0
	store.mu.Unlock()
	if w := callPayout(t, svc, okBody, nil); errOf(t, w) != "below_threshold" {
		t.Fatalf("$5 left: %s", w.Body.String())
	}
	if s := summaryOf(t, svc, referrer); s.AvailableCents != 500 {
		t.Fatalf("change left = %d, want 500", s.AvailableCents)
	}
}

func TestPayoutCardSizes(t *testing.T) {
	// rewards.md ## 验收: what /payout creates and what /me has left.
	for _, c := range []struct{ available, wantAmount, wantLeft int64 }{
		{2000, 2000, 0}, {5130, 5000, 130}, {48000, 48000, 0}, {50000, 50000, 0}, {300000, 50000, 250000},
	} {
		svc, store, mailer := newTestSvc(t0)
		store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
		store.credit(referrer, "a1", "t1", c.available, LedgerAvailable, t0.Add(-time.Hour))
		w := callPayout(t, svc, okBody, nil)
		var resp struct {
			Payout PayoutView `json:"payout"`
		}
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &resp) != nil || resp.Payout.AmountCents != c.wantAmount {
			t.Fatalf("available %d: %d %s", c.available, w.Code, w.Body.String())
		}
		if s := summaryOf(t, svc, referrer); s.AvailableCents != c.wantLeft {
			t.Fatalf("available %d: left %d, want %d", c.available, s.AvailableCents, c.wantLeft)
		}
		if want := "Amount:       " + usd(c.wantAmount) + " Amazon.com (US) gift card"; !strings.Contains(mailer.bodies[0], want) {
			t.Fatalf("available %d: mail missing %q", c.available, want)
		}
	}
}

func TestPayoutAmountFor(t *testing.T) {
	// rewards.md ### 兑换金额: $20 minimum, whole $10, $500 cap.
	for _, c := range []struct{ redeemable, want int64 }{
		{-100, 0}, {0, 0}, {1999, 0}, {2000, 2000}, {2999, 2000}, {5130, 5000},
		{48000, 48000}, {49999, 49000}, {50000, 50000}, {50001, 50000}, {300000, 50000},
	} {
		if got := PayoutAmountFor(c.redeemable); got != c.want {
			t.Errorf("PayoutAmountFor(%d) = %d, want %d", c.redeemable, got, c.want)
		}
	}
}

func TestPayoutW9WithBigCard(t *testing.T) {
	// A $500 card: the W-9 line still goes by sent + requested ≥ threshold −
	// $100, so the card that crosses $2,000 always carries it.
	cases := []struct {
		name   string
		sent   []int64 // cards already sent this year
		wantW9 bool
	}{
		{"$1,300 + $500 = $1,800", []int64{50000, 50000, 30000}, false},
		{"$1,350 + $500 = $1,850", []int64{50000, 50000, 35000}, false},
		{"$1,400 + $500 = $1,900", []int64{50000, 50000, 40000}, true},
		{"$1,600 + $500 = $2,100 crosses", []int64{50000, 50000, 50000, 10000}, true},
	}
	thisYear := t0.Add(-24 * time.Hour)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, mailer := newTestSvc(t0)
			store.addReferral(referrer, referee, "rewarded", t0.Add(-400*24*time.Hour))
			var spent int64
			for _, amount := range c.sent {
				store.payout(referrer, PayoutSent, thisYear.Add(-time.Hour), thisYear).AmountCents = amount
				spent += amount
			}
			store.credit(referrer, "a1", "t1", spent+80000, LedgerAvailable, t0.Add(-time.Hour))
			if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			body := mailer.bodies[0]
			if !strings.Contains(body, "Amount:       $500.00 Amazon.com (US) gift card") {
				t.Fatalf("not a $500 card:\n%s", body)
			}
			if got := strings.Contains(body, "W-9 FIRST (先收 W-9 再发"); got != c.wantW9 {
				t.Fatalf("W-9 line = %v, want %v:\n%s", got, c.wantW9, body)
			}
		})
	}
}

func TestPayoutCancelledMoneyComesBack(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 10000, LedgerAvailable, t0)
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("first: %d", w.Code)
	}
	if s := summaryOf(t, svc, referrer); s.AvailableCents != 0 {
		t.Fatalf("after request: %+v", s)
	}
	store.mu.Lock()
	store.payouts[0].State = PayoutCancelled
	store.mu.Unlock()
	if s := summaryOf(t, svc, referrer); s.AvailableCents != 10000 {
		t.Fatalf("after cancel: %+v", s)
	}
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("again after cancel: %d %s", w.Code, w.Body.String())
	}
}

func TestPayoutMailW9Threshold(t *testing.T) {
	thisYear := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lastYear := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)
	n := func(k int, at time.Time) []time.Time {
		out := make([]time.Time, k)
		for i := range out {
			out[i] = at
		}
		return out
	}
	readFailed := errors.New("column w9_threshold_cents does not exist")
	cases := []struct {
		name      string
		sentAt    []time.Time
		threshold int64 // referral_settings.w9_threshold_cents; 0 = default
		readErr   error
		total     string
		wantW9    bool
		sentTotal string
		wantLimit string // the threshold the W-9 line names
	}{
		{"first card", nil, 0, nil, "$100.00", false, "$0.00", ""},
		{"17 sent this year → $1,800", n(17, thisYear), 0, nil, "$1800.00", false, "$1700.00", ""},
		{"18 sent this year → $1,900", n(18, thisYear), 0, nil, "$1900.00", true, "$1800.00", "$2000.00"},
		{"20 sent this year → $2,100", n(20, thisYear), 0, nil, "$2100.00", true, "$2000.00", "$2000.00"},
		{"last year's cards do not count", n(19, lastYear), 0, nil, "$100.00", false, "$0.00", ""},
		{"threshold raised to $2,100: $1,900 is quiet", n(18, thisYear), 210000, nil, "$1900.00", false, "$1800.00", ""},
		{"threshold raised to $2,100: $2,000 warns", n(19, thisYear), 210000, nil, "$2000.00", true, "$1900.00", "$2100.00"},
		{"settings unreadable → $2,000", n(18, thisYear), 0, readFailed, "$1900.00", true, "$1800.00", "$2000.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, mailer := newTestSvc(t0)
			if c.threshold > 0 {
				store.w9Threshold = c.threshold
			}
			store.w9Err = c.readErr
			store.addReferral(referrer, referee, "rewarded", t0.Add(-400*24*time.Hour))
			store.credit(referrer, "a1", "t1", int64(len(c.sentAt)+1)*10000, LedgerAvailable, t0)
			for _, at := range c.sentAt {
				store.payout(referrer, PayoutSent, at.Add(-time.Hour), at)
			}
			if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			body := mailer.bodies[0]
			if !strings.Contains(body, "Total:                   "+c.total) || !strings.Contains(body, "Sent:                    "+c.sentTotal) {
				t.Fatalf("totals wrong:\n%s", body)
			}
			if got := strings.Contains(body, "W-9 FIRST (先收 W-9 再发"); got != c.wantW9 {
				t.Fatalf("W-9 line = %v, want %v:\n%s", got, c.wantW9, body)
			}
			if c.wantW9 && !strings.Contains(body, "the 1099 filing threshold is "+c.wantLimit) {
				t.Fatalf("threshold not named as %s:\n%s", c.wantLimit, body)
			}
			if strings.Contains(body, "1099-NEC") || strings.Contains(body, "$600") {
				t.Fatalf("mail still names 1099-NEC / $600:\n%s", body)
			}
		})
	}
}

func TestPayoutSharesTheDeviceBucket(t *testing.T) {
	bucket := ratelimit.NewBucket(5, 24*time.Hour)
	store := newMemStore(t0)
	svc := New(store, authAs(referrer), Options{DeviceBucket: bucket})
	svc.now = func() time.Time { return t0 }
	svc.async = func(f func()) { f() }

	// A bad address does not spend a try.
	for i := 0; i < 10; i++ {
		if w := callPayout(t, svc, `{"email":"nope"}`, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("bad email %d: %d", i, w.Code)
		}
	}
	// Two tries already spent by /claim on the same device.
	bucket.Allow(device, t0)
	bucket.Allow(device, t0)
	for i := 0; i < 3; i++ {
		if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusConflict {
			t.Fatalf("try %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	w := callPayout(t, svc, okBody, nil)
	if w.Code != http.StatusTooManyRequests || errOf(t, w) != "rate_limit_device" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("6th try: %d %s", w.Code, w.Body.String())
	}
	// Another phone is its own bucket.
	if w := callPayout(t, svc, okBody, map[string]string{"X-Device-Install-Id": "0E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"}); w.Code != http.StatusConflict {
		t.Fatalf("other device: %d", w.Code)
	}
}

func TestPayoutStoreErrorIs502(t *testing.T) {
	svc, store, mailer := richSvc(t)
	store.payoutErr = errBoom
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusBadGateway || errOf(t, w) != "store_failed" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(mailer.subjects) != 0 {
		t.Fatal("mailed on a failed request")
	}
}

func TestPayoutMailOptionalAndFailureTolerated(t *testing.T) {
	svc, _, _ := richSvc(t)
	svc.mailer = nil
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("no mailer: %d", w.Code)
	}
	svc, _, mailer := richSvc(t)
	mailer.err = errBoom
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("mail failure: %d", w.Code)
	}
}

func TestPayoutConcurrentRequestsMakeOne(t *testing.T) {
	store := newMemStore(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 100000, LedgerAvailable, t0)
	svc := New(store, authAs(referrer), Options{DeviceBucket: ratelimit.NewBucket(100, time.Hour)})
	svc.now = func() time.Time { return t0 }
	svc.async = func(f func()) { f() }

	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- callPayout(t, svc, okBody, nil).Code
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("created %d payouts, want 1", created)
	}
}

func TestPayoutMailSubjectIsASCII(t *testing.T) {
	subject, _ := composePayoutMail(Payout{ID: 17, UserID: referrer, AmountCents: 10000, Email: "é@example.com", RequestedAt: t0},
		YearTotals{RequestedCents: 10000}, nil, 2026, DefaultW9ThresholdCents)
	if subject != "[YardMate] Referral gift card request #17 ($100)" {
		t.Fatalf("subject = %q", subject)
	}
	for _, c := range subject {
		if c > 0x7e {
			t.Fatalf("non-ASCII subject %q", subject)
		}
	}
}

// MARK: gate 5 — the review numbers in the request mail

func TestPayoutMailCarriesTheReview(t *testing.T) {
	svc, store, mailer := richSvc(t)
	ref := store.refs[referee]
	store.reviewRefs = []reviewRef{
		{ref.ID, referrer, "rewarded", "", "DEV-1"},
		{90, referrer, "rewarded", "", "DEV-1"}, // same phone as the first
		{91, referrer, "held", "", "DEV-2"},
		{92, referrer, "qualified", "", ""}, // claimed before devices were recorded
		{93, referrer, "void", "duplicate_apple_id", "DEV-3"},
		{94, referrer, "void", "rejected", "DEV-4"},
		{95, referrer, "void", "refunded", "DEV-5"},
		{96, referrer, "void", "no_subscription", "DEV-6"},
		{97, stranger, "rewarded", "", "DEV-9"}, // someone else's friend
	}
	for i := range store.ledger {
		store.ledger[i].ReferralID = ref.ID
	}
	store.ledger = append(store.ledger,
		&ledgerRow{Credit: Credit{Referrer: referrer, ReferralID: 90, EventID: "r1", TransactionID: "tr1", CreditCents: 178}, State: LedgerVoided, VoidReason: VoidRefunded},
		&ledgerRow{Credit: Credit{Referrer: referrer, ReferralID: 93, EventID: "r2", TransactionID: "tr2", CreditCents: 178}, State: LedgerVoided, VoidReason: "duplicate_apple_id"},
	)
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	body := mailer.bodies[0]
	for _, want := range []string{
		"Review before buying (防刷第 5 道):",
		"Friends (not void):          4, of whom 1 brought in money",
		"Friend devices (distinct):   2 of 3 with a recorded device → NOT all different — 3 friends on 2 devices (+1 claimed before devices were recorded)",
		"Refunded charges:            1",
		"Void referrals:              duplicate_apple_id 1, rejected 1, refunded 1, no_subscription 1",
		"Held (awaiting review):      1",
		"deploy/referral-review.sh reject <referral id>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mail missing %q:\n%s", want, body)
		}
	}
}

func TestPayoutMailReviewAllDifferentAndUnavailable(t *testing.T) {
	svc, store, mailer := richSvc(t)
	store.reviewRefs = []reviewRef{{1, referrer, "rewarded", "", "A"}, {2, referrer, "saved", "", "B"}}
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
	if !strings.Contains(mailer.bodies[0], "2 of 2 with a recorded device → all different") ||
		!strings.Contains(mailer.bodies[0], "Void referrals:              duplicate_apple_id 0, rejected 0, refunded 0") {
		t.Errorf("all-different mail:\n%s", mailer.bodies[0])
	}

	// The review read failing still sends the mail (and still 201s).
	svc, store, mailer = richSvc(t)
	store.reviewErr = errBoom
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
	if len(mailer.bodies) != 1 || !strings.Contains(mailer.bodies[0], "numbers unavailable") {
		t.Errorf("review error: %d mails\n%v", len(mailer.bodies), mailer.bodies)
	}
}
