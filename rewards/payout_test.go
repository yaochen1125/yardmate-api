package rewards

import (
	"encoding/json"
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

const okBody = `{"email":"friend@privaterelay.appleid.com"}`

// richSvc: the referrer has $230.00 available, $12.00 pending.
func richSvc(t *testing.T) (*Service, *memStore, *fakeMailer) {
	t.Helper()
	svc, store, mailer := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 12000, LedgerAvailable, t0.Add(-time.Hour))
	store.credit(referrer, "a2", "t2", 11000, LedgerAvailable, t0.Add(-time.Hour))
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
	good := []string{"friend@example.com", "First.Last+yardmate@privaterelay.appleid.com", "  padded@example.com  "}
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
	if p.ID == 0 || p.AmountCents != 10000 || p.State != PayoutRequested || p.RequestedAt != "2026-10-20T12:00:00Z" || p.SentAt != nil {
		t.Fatalf("payout = %+v", p)
	}
	if got := store.payouts[0]; got.UserID != referrer || got.Email != "friend@privaterelay.appleid.com" {
		t.Fatalf("stored = %+v", got)
	}

	// /me now shows $130.00 left and the request in progress.
	s := summaryOf(t, svc, referrer)
	if s.AvailableCents != 13000 || s.PendingCents != 1200 || s.LifetimeCents != 23000 ||
		s.Payout == nil || s.Payout.State != PayoutRequested || s.Payout.ID != p.ID {
		t.Fatalf("summary after payout = %+v / %+v", s, s.Payout)
	}

	// Operator mail.
	if len(mailer.subjects) != 1 {
		t.Fatalf("mails = %d", len(mailer.subjects))
	}
	wantSubject := "[YardMate] Referral gift card request #" + itoa(p.ID)
	if mailer.subjects[0] != wantSubject {
		t.Fatalf("subject = %q, want %q", mailer.subjects[0], wantSubject)
	}
	body := mailer.bodies[0]
	for _, want := range []string{
		"User id:      " + referrer,
		"Send to:      friend@privaterelay.appleid.com",
		"$100.00 Amazon.com (US) gift card",
		"calendar year 2026 (UTC)",
		"Sent:                    $0.00",
		"Requested (incl. this):  $100.00",
		"Total:                   $100.00",
		"deploy/referral-payout.sh mark-sent " + itoa(p.ID),
		"deploy/referral-payout.sh cancel " + itoa(p.ID),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mail body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "W-9") {
		t.Errorf("W-9 line at $100:\n%s", body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestPayoutBelowThreshold(t *testing.T) {
	svc, store, mailer := newTestSvc(t0)
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.credit(referrer, "a1", "t1", 9999, LedgerAvailable, t0)
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
	// $230: the first request goes through, the second (still $130 left) is
	// refused while the first is requested; once it is sent, the next one works.
	svc, store, _ := richSvc(t)
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("first: %d", w.Code)
	}
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusConflict || errOf(t, w) != "payout_in_progress" {
		t.Fatalf("second: %d %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	store.payouts[0].State, store.payouts[0].SentAt = PayoutSent, t0
	store.mu.Unlock()
	if w := callPayout(t, svc, okBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("after sent: %d %s", w.Code, w.Body.String())
	}
	// $30 left now.
	store.mu.Lock()
	store.payouts[1].State, store.payouts[1].SentAt = PayoutSent, t0
	store.mu.Unlock()
	if w := callPayout(t, svc, okBody, nil); errOf(t, w) != "below_threshold" {
		t.Fatalf("$30 left: %s", w.Body.String())
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
	cases := []struct {
		name      string
		sentAt    []time.Time
		total     string
		wantW9    bool
		sentTotal string
	}{
		{"first card", nil, "$100.00", false, "$0.00"},
		{"three sent this year → $400", []time.Time{thisYear, thisYear, thisYear}, "$400.00", false, "$300.00"},
		{"four sent this year → $500", []time.Time{thisYear, thisYear, thisYear, thisYear}, "$500.00", true, "$400.00"},
		{"six sent this year → $700", []time.Time{thisYear, thisYear, thisYear, thisYear, thisYear, thisYear}, "$700.00", true, "$600.00"},
		{"last year's cards do not count", []time.Time{lastYear, lastYear, lastYear, lastYear, lastYear}, "$100.00", false, "$0.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, mailer := newTestSvc(t0)
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
			if got := strings.Contains(body, "W-9 FIRST (先收 W-9 再发)"); got != c.wantW9 {
				t.Fatalf("W-9 line = %v, want %v:\n%s", got, c.wantW9, body)
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
		YearTotals{RequestedCents: 10000}, 2026)
	if subject != "[YardMate] Referral gift card request #17" {
		t.Fatalf("subject = %q", subject)
	}
	for _, c := range subject {
		if c > 0x7e {
			t.Fatalf("non-ASCII subject %q", subject)
		}
	}
}
