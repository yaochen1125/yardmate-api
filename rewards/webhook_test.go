package rewards

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// purchase is a PRODUCTION INITIAL_PURCHASE of the annual plan by the friend
// (upper-case app user id, as RevenueCat has it), one day after t0.
func purchase(id string) map[string]any {
	return map[string]any{
		"id":                      id,
		"type":                    "INITIAL_PURCHASE",
		"environment":             "PRODUCTION",
		"app_user_id":             strings.ToUpper(referee),
		"product_id":              "com.chenyao.plantapp.premium.year",
		"period_type":             "NORMAL",
		"price":                   39.99,
		"tax_percentage":          0.0,
		"commission_percentage":   0.15,
		"transaction_id":          "2000000111111111",
		"original_transaction_id": "2000000111111111",
		"purchased_at_ms":         t0.Add(24 * time.Hour).UnixMilli(),
	}
}

func with(ev map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range ev {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		if kv[i+1] == nil {
			delete(out, k)
		} else {
			out[k] = kv[i+1]
		}
	}
	return out
}

func post(t *testing.T, svc *Service, auth string, ev map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"event": ev, "api_version": "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	return postRaw(t, svc, auth, string(raw))
}

func postRaw(t *testing.T, svc *Service, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/revenuecat/webhook", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	svc.HandleWebhook()(w, r)
	return w
}

func status(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not json: %q", w.Body.String())
	}
	if out["error"] != "" {
		return "error:" + out["error"]
	}
	return out["status"]
}

// redeemedSvc: referrer → referee, redeemed, claimed a week before t0.
func redeemedSvc(t *testing.T) (*Service, *memStore) {
	t.Helper()
	svc, store, _ := newTestSvc(t0)
	store.addReferral(referrer, referee, "redeemed", t0.Add(-7*24*time.Hour))
	return svc, store
}

func TestWebhookAuth(t *testing.T) {
	cases := []struct {
		name, configured, header string
		want                     int
	}{
		{"no header", secret, "", http.StatusUnauthorized},
		{"wrong secret", secret, "nope", http.StatusUnauthorized},
		{"wrong secret with bearer", secret, "Bearer nope", http.StatusUnauthorized},
		{"bare secret", secret, secret, http.StatusOK},
		{"bearer + secret", secret, "Bearer " + secret, http.StatusOK},
		{"lower-case bearer", secret, "bearer " + secret, http.StatusOK},
		{"configured with bearer, sent bare", "Bearer " + secret, secret, http.StatusOK},
		{"configured with bearer, sent with bearer", "Bearer " + secret, "Bearer " + secret, http.StatusOK},
		{"prefix of the secret", secret, secret[:len(secret)-1], http.StatusUnauthorized},
		{"just the word bearer", secret, "Bearer ", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, _ := redeemedSvc(t)
			svc.secret = c.configured
			w := post(t, svc, c.header, purchase("evt-auth"))
			if w.Code != c.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestWebhookUnconfigured(t *testing.T) {
	// No secret: 503 whatever the header says (never "open").
	svc, store := redeemedSvc(t)
	svc.secret = ""
	for _, h := range []string{"", "Bearer ", secret} {
		if w := post(t, svc, h, purchase("evt-1")); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("header %q: status %d", h, w.Code)
		}
	}
	if len(store.rows()) != 0 {
		t.Fatal("recorded without a secret")
	}
	// Secret but no database: 503 after auth.
	noDB := New(nil, nil, Options{WebhookSecret: secret})
	if w := post(t, noDB, "", purchase("evt-1")); w.Code != http.StatusUnauthorized {
		t.Fatalf("no db, no auth: %d", w.Code)
	}
	if w := post(t, noDB, secret, purchase("evt-1")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no db: %d", w.Code)
	}
}

func TestWebhookBadBodies(t *testing.T) {
	svc, _ := redeemedSvc(t)
	if w := postRaw(t, svc, secret, "{not json"); w.Code != http.StatusBadRequest || status(t, w) != "error:bad_json" {
		t.Fatalf("bad json: %d %s", w.Code, w.Body.String())
	}
	big := `{"event":{"type":"RENEWAL","note":"` + strings.Repeat("x", webhookBodyCap) + `"}}`
	if w := postRaw(t, svc, secret, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("64 KB+: %d", w.Code)
	}
	// A counted event without the ids we key on.
	if w := post(t, svc, secret, with(purchase(""), "id", "")); w.Code != http.StatusBadRequest || status(t, w) != "error:bad_event" {
		t.Fatalf("no id: %d %s", w.Code, w.Body.String())
	}
	if w := post(t, svc, secret, with(purchase("e"), "transaction_id", nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("no transaction id: %d", w.Code)
	}
}

func TestWebhookRecordsAnnualPurchase(t *testing.T) {
	svc, store := redeemedSvc(t)
	w := post(t, svc, secret, purchase("evt-annual"))
	if w.Code != http.StatusOK || status(t, w) != outRecorded {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	rows := store.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	c := rows[0]
	purchased := t0.Add(24 * time.Hour)
	if c.CreditCents != 1020 || c.PriceUSDCents != 3999 || c.Takehome != "0.8500" || c.State != LedgerPending {
		t.Fatalf("credit = %+v", c)
	}
	if !c.PurchasedAt.Equal(purchased) || !c.AvailableAt.Equal(purchased.Add(90*24*time.Hour)) {
		t.Fatalf("purchased=%v available=%v", c.PurchasedAt, c.AvailableAt)
	}
	if c.Referrer != referrer || c.Referee != referee || c.ReferralID == 0 || c.EventID != "evt-annual" ||
		c.EventType != "INITIAL_PURCHASE" || c.TransactionID != "2000000111111111" || c.PeriodType != "NORMAL" {
		t.Fatalf("row = %+v", c)
	}
}

// A referral voided between the webhook's lookup and its write (settlement's
// duplicate_apple_id, an operator's reject) gets no fresh pending credit
// (final review P2): referral_void has already swept the ledger, so a row
// written now would mature into spendable money.
func TestWebhookReferralVoidedMeanwhileIsNotCredited(t *testing.T) {
	svc, store := redeemedSvc(t)
	store.beforeRecord = func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		r := store.refs[referee]
		r.State = "void"
		store.refs[referee] = r
	}
	w := post(t, svc, secret, purchase("evt-raced"))
	if w.Code != http.StatusOK || status(t, w) != outNotRedeemed {
		t.Fatalf("status %d %s, want 200 %s", w.Code, w.Body.String(), outNotRedeemed)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for a void referral", rows)
	}
}

func TestWebhookCountsWhatTheDocCounts(t *testing.T) {
	cases := []struct {
		name  string
		ev    map[string]any
		want  string
		cents int64
	}{
		{"monthly renewal", with(purchase("e"), "type", "RENEWAL", "price", 6.99), outRecorded, 178},
		{"intro first month $0.29", with(purchase("e"), "period_type", "INTRO", "price", 0.29), outRecorded, 7},
		// payout rate = 1 − tax_percentage − commission_percentage; anything
		// missing or senseless → 0.70 (rewards.md ### 记账来源).
		{"VAT 0.2 + commission 0.15 → 0.65", with(purchase("e"), "tax_percentage", 0.2), outRecorded, 780},
		{"standard commission 0.30", with(purchase("e"), "commission_percentage", 0.3), outRecorded, 840},
		{"both missing → 0.70", with(purchase("e"), "tax_percentage", nil, "commission_percentage", nil), outRecorded, 840},
		{"only the deprecated takehome → 0.70", with(purchase("e"), "tax_percentage", nil, "commission_percentage", nil, "takehome_percentage", 0.85), outRecorded, 840},
		{"tax missing → 0.70", with(purchase("e"), "tax_percentage", nil), outRecorded, 840},
		{"commission null → 0.70", with(purchase("e"), "commission_percentage", json.RawMessage("null")), outRecorded, 840},
		{"commission out of range → 0.70", with(purchase("e"), "commission_percentage", 1.5), outRecorded, 840},
		{"negative tax → 0.70", with(purchase("e"), "tax_percentage", -0.1), outRecorded, 840},
		{"tax + commission ≥ 1 → 0.70", with(purchase("e"), "tax_percentage", 0.6, "commission_percentage", 0.4), outRecorded, 840},
		{"price as a string", with(purchase("e"), "price", "39.99"), outRecorded, 1020},
		{"trial", with(purchase("e"), "period_type", "TRIAL", "price", 0), outNotCounted, 0},
		{"trial with a price", with(purchase("e"), "period_type", "TRIAL"), outNotCounted, 0},
		{"promotional", with(purchase("e"), "period_type", "PROMOTIONAL"), outNotCounted, 0},
		{"free month $0", with(purchase("e"), "price", 0), outNotCounted, 0},
		{"price null", with(purchase("e"), "price", json.RawMessage("null")), outNotCounted, 0},
		{"negative price", with(purchase("e"), "price", -39.99), outNotCounted, 0},
		{"family share", with(purchase("e"), "is_family_share", true), outNotCounted, 0},
		{"one cent rounds to nothing", with(purchase("e"), "price", 0.01), outNotCounted, 0},
		{"sandbox", with(purchase("e"), "environment", "SANDBOX"), outIgnoredEnvironment, 0},
		{"no environment", with(purchase("e"), "environment", nil), outIgnoredEnvironment, 0},
		{"not a friend", with(purchase("e"), "app_user_id", strings.ToUpper(stranger)), outNotReferee, 0},
		{"anonymous id", with(purchase("e"), "app_user_id", "$RCAnonymousID:abc"), outNotReferee, 0},
		{"lower-case id still matches", with(purchase("e"), "app_user_id", referee), outRecorded, 1020},
		{"expiration", with(purchase("e"), "type", "EXPIRATION"), outIgnoredType, 0},
		{"transfer", with(purchase("e"), "type", "TRANSFER"), outIgnoredType, 0},
		// A referee's PRODUCT_CHANGE is logged with a count, never credited.
		{"product change", with(purchase("e"), "type", "PRODUCT_CHANGE", "price", 0, "new_product_id", "com.chenyao.plantapp.premium.yearly"), outProductChange, 0},
		{"non-renewing", with(purchase("e"), "type", "NON_RENEWING_PURCHASE"), outIgnoredType, 0},
		{"test event", with(purchase("e"), "type", "TEST"), outIgnoredType, 0},
		{"unsubscribe is not a refund", with(purchase("e"), "type", "CANCELLATION", "cancel_reason", "UNSUBSCRIBE"), outIgnoredType, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store := redeemedSvc(t)
			w := post(t, svc, secret, c.ev)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			if got := status(t, w); got != c.want {
				t.Fatalf("outcome %q, want %q", got, c.want)
			}
			rows := store.rows()
			if c.cents == 0 {
				if len(rows) != 0 {
					t.Fatalf("recorded %+v", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].CreditCents != c.cents {
				t.Fatalf("rows = %+v, want one of %d", rows, c.cents)
			}
		})
	}
}

func TestWebhookReferralStates(t *testing.T) {
	claimedAt := t0
	cases := []struct {
		state, evType string
		purchased     time.Time
		want          string
	}{
		{"redeemed", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		{"qualified", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		// held (referral anti-abuse gate 4, waiting for review): the friend
		// has redeemed, money is recorded as usual; a reject voids it.
		{"held", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		{"held", "RENEWAL", t0.Add(time.Hour), outRecorded},
		{"saved", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		{"rewarded", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		{"redeemed", "RENEWAL", t0.Add(time.Hour), outRecorded},
		{"void", "INITIAL_PURCHASE", t0.Add(time.Hour), outNotRedeemed},
		// Not yet moved to redeemed by the settlement loop: a FIRST purchase
		// after the claim counts (what step ① will confirm within ten
		// minutes). A renewal charged before the claim never does. A renewal
		// after it goes through the Redeemer (TestWebhookRenewalBeforeSettlement).
		{"claimed", "INITIAL_PURCHASE", t0.Add(time.Hour), outRecorded},
		{"claimed", "INITIAL_PURCHASE", t0.Add(-time.Hour), outNotRedeemed},
		{"claimed", "RENEWAL", t0.Add(-time.Hour), outNotRedeemed},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%s/%s", c.state, c.evType, c.purchased.Sub(claimedAt)), func(t *testing.T) {
			svc, store, _ := newTestSvc(t0)
			store.addReferral(referrer, referee, c.state, claimedAt)
			w := post(t, svc, secret, with(purchase("e"), "type", c.evType, "purchased_at_ms", c.purchased.UnixMilli()))
			if got := status(t, w); w.Code != http.StatusOK || got != c.want {
				t.Fatalf("%d %q, want %q", w.Code, got, c.want)
			}
		})
	}
}

func TestWebhookMissingPurchaseTimeUsesNow(t *testing.T) {
	svc, store := redeemedSvc(t)
	if w := post(t, svc, secret, with(purchase("e"), "purchased_at_ms", nil)); status(t, w) != outRecorded {
		t.Fatalf("%s", w.Body.String())
	}
	if r := store.rows()[0]; !r.PurchasedAt.Equal(t0) || !r.AvailableAt.Equal(t0.Add(RefundWindow)) {
		t.Fatalf("row = %+v", r)
	}
}

func TestWebhookDedupesOnEventID(t *testing.T) {
	svc, store := redeemedSvc(t)
	for i, want := range []string{outRecorded, outDuplicate, outDuplicate} {
		w := post(t, svc, secret, purchase("evt-same"))
		if w.Code != http.StatusOK || status(t, w) != want {
			t.Fatalf("delivery %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	// A different event (next month's renewal) is a new row.
	post(t, svc, secret, with(purchase("evt-next"), "type", "RENEWAL", "transaction_id", "2000000222222222"))
	if n := len(store.rows()); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

func TestWebhookRefundVoidsOnlyPending(t *testing.T) {
	svc, store := redeemedSvc(t)
	post(t, svc, secret, purchase("evt-buy"))                                                        // pending, txn …111
	store.credit(referrer, "evt-old", "2000000999999999", 1020, LedgerAvailable, t0.Add(-time.Hour)) // already available

	refund := func(txn string) *httptest.ResponseRecorder {
		return post(t, svc, secret, with(purchase("evt-refund-"+txn), "type", "CANCELLATION",
			"cancel_reason", "CUSTOMER_SUPPORT", "price", -39.99, "transaction_id", txn))
	}
	if w := refund("2000000111111111"); w.Code != http.StatusOK || status(t, w) != outVoided {
		t.Fatalf("refund pending: %d %s", w.Code, w.Body.String())
	}
	if w := refund("2000000999999999"); status(t, w) != outNothingToVoid {
		t.Fatalf("refund available: %s", w.Body.String())
	}
	if w := refund("2000000111111111"); status(t, w) != outNothingToVoid {
		t.Fatalf("second refund: %s", w.Body.String())
	}
	states := map[string]string{}
	for _, r := range store.rows() {
		states[r.EventID] = r.State
	}
	if states["evt-buy"] != LedgerVoided || states["evt-old"] != LedgerAvailable {
		t.Fatalf("states = %v", states)
	}
	// A refund for a sandbox purchase is ignored like everything sandbox.
	if w := post(t, svc, secret, with(purchase("x"), "type", "CANCELLATION", "cancel_reason", "CUSTOMER_SUPPORT",
		"environment", "SANDBOX")); status(t, w) != outIgnoredEnvironment {
		t.Fatalf("sandbox refund: %s", w.Body.String())
	}
}

func TestWebhookDedupesOnTransactionID(t *testing.T) {
	// The same store transaction under a second event id is still one credit.
	svc, store := redeemedSvc(t)
	if w := post(t, svc, secret, purchase("evt-a")); status(t, w) != outRecorded {
		t.Fatalf("first: %s", w.Body.String())
	}
	if w := post(t, svc, secret, purchase("evt-b")); w.Code != http.StatusOK || status(t, w) != outDuplicate {
		t.Fatalf("same transaction, new event id: %d %s", w.Code, w.Body.String())
	}
	if n := len(store.rows()); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestWebhookRefundBeforeCharge(t *testing.T) {
	// The charge's first delivery failed (500) and RevenueCat retries it after
	// the refund has already come in: the charge must not be credited.
	svc, store := redeemedSvc(t)
	refund := with(purchase("evt-refund"), "type", "CANCELLATION", "cancel_reason", "CUSTOMER_SUPPORT", "price", -39.99)
	if w := post(t, svc, secret, refund); w.Code != http.StatusOK || status(t, w) != outNothingToVoid {
		t.Fatalf("refund first: %d %s", w.Code, w.Body.String())
	}
	if !store.tombstoned("2000000111111111") {
		t.Fatal("refund with nothing pending left no tombstone")
	}
	for i := 0; i < 2; i++ { // the late charge, and a redelivery of it
		if w := post(t, svc, secret, purchase("evt-late")); w.Code != http.StatusOK || status(t, w) != outVoided {
			t.Fatalf("late charge %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("credited after refund: %+v", rows)
	}
	// Another transaction of the same friend is unaffected.
	if w := post(t, svc, secret, with(purchase("evt-renewal"), "type", "RENEWAL", "transaction_id", "2000000333333333")); status(t, w) != outRecorded {
		t.Fatalf("other transaction: %s", w.Body.String())
	}
	// A refund that did void something leaves no tombstone.
	post(t, svc, secret, with(refund, "id", "evt-refund-2", "transaction_id", "2000000333333333"))
	if store.tombstoned("2000000333333333") {
		t.Fatal("tombstone written although a pending credit was voided")
	}
}

func TestWebhookStoreErrorsAre500(t *testing.T) {
	for _, knob := range []string{"find", "record", "void"} {
		t.Run(knob, func(t *testing.T) {
			svc, store := redeemedSvc(t)
			ev := purchase("e")
			switch knob {
			case "find":
				store.findErr = errBoom
			case "record":
				store.recordErr = errBoom
			case "void":
				store.voidErr = errBoom
				ev = with(ev, "type", "CANCELLATION", "cancel_reason", "CUSTOMER_SUPPORT")
			}
			if w := post(t, svc, secret, ev); w.Code != http.StatusInternalServerError || status(t, w) != "error:store_failed" {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreditCentsRounding(t *testing.T) {
	cases := []struct {
		price, takehome string
		want            int64
	}{
		{"39.99", "0.85", 1020}, // 1019.745
		{"6.99", "0.85", 178},   // 178.245
		{"0.29", "0.85", 7},     // 7.395
		{"9.99", "0.85", 255},   // 254.745
		{"1.15", "0.85", 29},    // 29.325
		{"0.05", "1", 2},        // 1.5 exactly → half up
		{"0.15", "1", 5},        // 4.5 exactly → half up (float64 would give 4.499…)
		{"39.99", "0.7", 840},   // 839.79
		{"79.99", "0.85", 2040}, // 2039.745
	}
	for _, c := range cases {
		p, _ := new(big.Rat).SetString(c.price)
		h, _ := new(big.Rat).SetString(c.takehome)
		got, ok := CreditCents(p, h)
		if !ok || got != c.want {
			t.Errorf("CreditCents(%s, %s) = %d %v, want %d", c.price, c.takehome, got, ok, c.want)
		}
	}
	huge, _ := new(big.Rat).SetString("1e30")
	if _, ok := CreditCents(huge, big.NewRat(1, 1)); ok {
		t.Error("1e30 fit the ledger column")
	}
}

func TestWebhookHugePriceIsBadEvent(t *testing.T) {
	svc, store := redeemedSvc(t)
	if w := post(t, svc, secret, with(purchase("e"), "price", 1e30)); w.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(store.rows()) != 0 {
		t.Fatal("recorded")
	}
}
