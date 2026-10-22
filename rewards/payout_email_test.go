package rewards

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

// POST /v1/referral/payout/email (rewards.md ### 邮箱：确认、拼写提示、修改).

func callPayoutEmail(t *testing.T, svc *Service, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/referral/payout/email", strings.NewReader(body))
	r.Header.Set("X-Device-Install-Id", device)
	r.Header.Set("X-App-Version", "2.2.0 (50)")
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	svc.HandlePayoutEmail()(w, r)
	return w
}

func payoutOf(t *testing.T, w *httptest.ResponseRecorder) PayoutView {
	t.Helper()
	var out struct {
		Payout PayoutView `json:"payout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not json: %q", w.Body.String())
	}
	return out.Payout
}

func TestPayoutCarriesEmail(t *testing.T) {
	svc, _, _ := richSvc(t)
	w := callPayout(t, svc, okBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if p := payoutOf(t, w); p.Email != "friend@example.com" || !p.EmailEditable {
		t.Fatalf("201 payout = %+v", p)
	}
	// The wire names the App reads.
	if !strings.Contains(w.Body.String(), `"email":"friend@example.com"`) || !strings.Contains(w.Body.String(), `"email_editable":true`) {
		t.Fatalf("wire = %s", w.Body.String())
	}
}

func TestPayoutEmailChange(t *testing.T) {
	svc, store, mailer := richSvc(t)
	w := callPayout(t, svc, okBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	id := payoutOf(t, w).ID
	w = callPayoutEmail(t, svc, `{"email":"  anna@gmail.com "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	if p := payoutOf(t, w); p.Email != "anna@gmail.com" || !p.EmailEditable || p.State != PayoutRequested {
		t.Fatalf("200 payout = %+v", p)
	}
	if got := store.payouts[0].Email; got != "anna@gmail.com" {
		t.Fatalf("stored email = %q", got)
	}
	if len(mailer.subjects) != 2 {
		t.Fatalf("mails = %v", mailer.subjects)
	}
	subject, body := mailer.subjects[1], mailer.bodies[1]
	if subject != fmt.Sprintf("[YardMate] Referral gift card request #%d: email changed", id) {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{"Old email:    friend@example.com", "New email:    anna@gmail.com",
		fmt.Sprintf("deploy/referral-payout.sh mark-sent %d", id), "do not buy another card"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mail lacks %q:\n%s", want, body)
		}
	}
	for _, c := range subject {
		if c > 0x7e {
			t.Fatalf("non-ASCII subject %q", subject)
		}
	}

	// The same address again, another case: 200, nothing changes, no mail.
	w = callPayoutEmail(t, svc, `{"email":"Anna@Gmail.com"}`)
	if w.Code != http.StatusOK || payoutOf(t, w).Email != "anna@gmail.com" {
		t.Fatalf("same again: %d %s", w.Code, w.Body.String())
	}
	if len(mailer.subjects) != 2 {
		t.Fatalf("same address mailed: %v", mailer.subjects)
	}
}

func TestPayoutEmailNotEditable(t *testing.T) {
	// Never asked.
	svc, _, _ := richSvc(t)
	if w := callPayoutEmail(t, svc, `{"email":"anna@gmail.com"}`); w.Code != http.StatusConflict || errOf(t, w) != "payout_not_editable" {
		t.Fatalf("no payout: %d %s", w.Code, w.Body.String())
	}
	// Sent and cancelled ones are not editable either.
	for _, state := range []string{PayoutSent, PayoutCancelled} {
		svc, store, mailer := richSvc(t)
		p := store.payout(referrer, state, t0.Add(-2*time.Hour), t0.Add(-time.Hour))
		w := callPayoutEmail(t, svc, `{"email":"anna@gmail.com"}`)
		if w.Code != http.StatusConflict || errOf(t, w) != "payout_not_editable" {
			t.Fatalf("%s: %d %s", state, w.Code, w.Body.String())
		}
		if p.Email != "seed@example.com" || len(mailer.subjects) != 0 {
			t.Fatalf("%s: changed or mailed", state)
		}
	}
}

func TestPayoutEmailBadInput(t *testing.T) {
	bucket := ratelimit.NewBucket(1, 24*time.Hour)
	store := newMemStore(t0)
	svc := New(store, authAs(referrer), Options{DeviceBucket: bucket})
	svc.now = func() time.Time { return t0 }
	svc.async = func(f func()) { f() }
	store.addReferral(referrer, referee, "rewarded", t0.Add(-200*24*time.Hour))
	store.payout(referrer, PayoutRequested, t0.Add(-time.Hour), time.Time{})

	for _, body := range []string{`{"email":"nope"}`, `{"email":"x@privaterelay.appleid.com"}`, `{"email":"X@PRIVATERELAY.APPLEID.COM"}`} {
		if w := callPayoutEmail(t, svc, body); w.Code != http.StatusBadRequest || errOf(t, w) != "invalid_email" {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := callPayoutEmail(t, svc, `not json`); w.Code != http.StatusBadRequest || errOf(t, w) != "bad_json" {
		t.Fatalf("bad json: %d %s", w.Code, w.Body.String())
	}
	// The bucket holds one try and the bad inputs spent none of it.
	if w := callPayoutEmail(t, svc, `{"email":"anna@gmail.com"}`); w.Code != http.StatusOK {
		t.Fatalf("first good try: %d %s", w.Code, w.Body.String())
	}
	if w := callPayoutEmail(t, svc, `{"email":"bob@gmail.com"}`); w.Code != http.StatusTooManyRequests || errOf(t, w) != "rate_limit_device" {
		t.Fatalf("second try: %d %s", w.Code, w.Body.String())
	}
}

func TestPayoutEmailStoreError(t *testing.T) {
	svc, store, _ := richSvc(t)
	store.payoutErr = errors.New("db down")
	if w := callPayoutEmail(t, svc, `{"email":"anna@gmail.com"}`); w.Code != http.StatusBadGateway || errOf(t, w) != "store_failed" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPayoutEmailAuth(t *testing.T) {
	svc := New(newMemStore(t0), func(*http.Request) (string, error) { return "", errors.New("bad token") }, Options{})
	if w := callPayoutEmail(t, svc, `{"email":"anna@gmail.com"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
