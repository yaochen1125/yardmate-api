package referral

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sample mirrors a real GET /v1/subscribers answer (shape + null handling).
const sample = `{"request_date":"2026-10-20T02:00:00Z","subscriber":{
 "first_seen":"2026-06-25T17:47:42Z",
 "entitlements":{"YardMate: AI Plant ID & Care Pro":{"expires_date":"2026-10-16T16:23:13Z","grace_period_expires_date":null,"product_identifier":"com.chenyao.plantapp.premium.yearly.3d","purchase_date":"2026-10-16T15:23:13Z"}},
 "subscriptions":{
  "com.chenyao.plantapp.premium.yearly":{"auto_resume_date":null,"billing_issues_detected_at":null,"expires_date":"2026-08-06T00:30:01Z","grace_period_expires_date":null,"is_sandbox":true,"original_purchase_date":"2026-06-25T17:47:42Z","ownership_type":"PURCHASED","period_type":"normal","purchase_date":"2026-08-05T00:30:01Z","refunded_at":null,"store":"app_store","store_transaction_id":"2000000","unsubscribe_detected_at":null},
  "rc_promo_YardMate: AI Plant ID & Care Pro_monthly":{"expires_date":"2026-12-01T00:00:00Z","is_sandbox":false,"original_purchase_date":"2026-11-01T00:00:00Z","ownership_type":"PURCHASED","period_type":"normal","purchase_date":"2026-11-01T00:00:00Z","refunded_at":null,"store":"promotional"}
 },
 "non_subscriptions":{},"other_purchases":{}}}`

func TestSubscriberDecodesRevenueCatShape(t *testing.T) {
	var out struct {
		Subscriber Subscriber `json:"subscriber"`
	}
	if err := json.Unmarshal([]byte(sample), &out); err != nil {
		t.Fatal(err)
	}
	s := &out.Subscriber
	if !s.HasPaidHistory() {
		t.Error("HasPaidHistory = false, want true (an app_store subscription exists)")
	}
	claim := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	key, start, ok := s.StartedAfter(claim)
	if !ok || key != "com.chenyao.plantapp.premium.yearly" || !start.Equal(time.Date(2026, 6, 25, 17, 47, 42, 0, time.UTC)) {
		t.Errorf("StartedAfter = (%q, %v, %v)", key, start, ok)
	}
	if _, _, ok := s.StartedAfter(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Error("a subscription that began before the claim must not count, whatever its later renewals")
	}
	if s.Refunded(key) {
		t.Error("Refunded = true for refunded_at null")
	}
	if s.Covered(time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)) {
		t.Error("Covered = true after the store subscription expired (the promotional one must not count)")
	}
	if !s.Covered(time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)) {
		t.Error("Covered = false while the store subscription was active")
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !s.PromoUntil().Equal(want) {
		t.Errorf("PromoUntil = %v, want %v", s.PromoUntil(), want)
	}
	var none *Subscriber
	if none.HasPaidHistory() || none.Covered(time.Now()) || !none.PromoUntil().IsZero() {
		t.Error("nil subscriber must read as empty")
	}
}

func TestAppUserIDMatchesTheAppsUUIDString(t *testing.T) {
	// Swift's UUID.uuidString is upper-case; RevenueCat ids are case-sensitive.
	if got := AppUserID(" 30bd9690-aaaa-bbbb-cccc-ddddeeeeffff "); got != "30BD9690-AAAA-BBBB-CCCC-DDDDEEEEFFFF" {
		t.Errorf("AppUserID = %q", got)
	}
}

func TestClientTalksToRevenueCat(t *testing.T) {
	until := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		gotBody = nil
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &gotBody)
		}
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusCreated) // RC creates the customer on first sight
			_, _ = w.Write([]byte(sample))
		case r.URL.Path == "/subscribers/BAD/entitlements/"+entID+"/promotional":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"subscriber":{}}`))
		}
	}))
	defer srv.Close()

	c := NewClient("sk_test")
	c.SetBase(srv.URL)
	sub, err := c.Subscriber(context.Background(), "30BD9690-X")
	if err != nil || !sub.HasPaidHistory() {
		t.Fatalf("Subscriber: %v %+v", err, sub)
	}
	if gotPath != "/subscribers/30BD9690-X" || gotAuth != "Bearer sk_test" || gotMethod != http.MethodGet {
		t.Errorf("GET went to %q auth=%q method=%q", gotPath, gotAuth, gotMethod)
	}

	if err := c.GrantPromotional(context.Background(), "30BD9690-X", entID, until); err != nil {
		t.Fatalf("GrantPromotional: %v", err)
	}
	if gotPath != "/subscribers/30BD9690-X/entitlements/"+entID+"/promotional" || gotMethod != http.MethodPost {
		t.Errorf("POST went to %q (%s)", gotPath, gotMethod)
	}
	if ms, _ := gotBody["end_time_ms"].(float64); int64(ms) != until.UnixMilli() {
		t.Errorf("end_time_ms = %v, want %d", gotBody["end_time_ms"], until.UnixMilli())
	}
	if err := c.GrantPromotional(context.Background(), "BAD", entID, until); err == nil {
		t.Error("a 404 (wrong entitlement) must surface as an error")
	}
	if err := c.GrantPromotional(context.Background(), "X", "", until); err == nil {
		t.Error("an empty entitlement must be refused before any call")
	}
	if NewClient("  ") != nil {
		t.Error("blank key must yield no client")
	}
}

// Errors are logged by /me, /claim and settlement: never the app user id
// (it is in the path, and net/http's *url.Error prints the whole URL).
func TestClientErrorsCarryNoUserID(t *testing.T) {
	const id = "30BD9690-AAAA-BBBB-CCCC-DDDDEEEEFFFF"
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused
	c := NewClient("sk_test")
	c.SetBase(base)
	_, err := c.Subscriber(context.Background(), id)
	if err == nil || strings.Contains(err.Error(), id) || strings.Contains(err.Error(), "/subscribers") {
		t.Errorf("Subscriber error = %v, want one without the id / URL", err)
	}
	if err != nil && !strings.Contains(err.Error(), "get subscriber") {
		t.Errorf("Subscriber error = %v, want the operation named", err)
	}
	err = c.GrantPromotional(context.Background(), id, entID, time.Now())
	if err == nil || strings.Contains(err.Error(), id) || strings.Contains(err.Error(), "/subscribers") {
		t.Errorf("GrantPromotional error = %v, want one without the id / URL", err)
	}

	// The cause stays wrapped: a timeout is still recognisable.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	c.SetBase(slow.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Subscriber(ctx, id)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), id) {
		t.Errorf("timeout error = %v, want DeadlineExceeded without the id", err)
	}
}
