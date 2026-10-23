package referral

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 023: /claim answers the friend's experiment group; app builds that do not
// say they know the groups are kept in the monthly one.

func TestClaimAnswersTheOfferVariant(t *testing.T) {
	svc, store, _, _ := configuredDC()
	store.variant = VariantYear
	w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2","device_token":"tok","device_check_env":"production","offer_variant_aware":true}`, nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	b := body(t, w)
	if b["offer_code"] != "XK7P2M9Q" || b["offer_variant"] != VariantYear {
		t.Fatalf("answer = %v", b)
	}
	if store.gotForceMonth {
		t.Fatal("an aware app was forced into the monthly group")
	}
}

func TestClaimFromAnUnawareAppIsForcedMonthly(t *testing.T) {
	svc, store, _, _ := configuredDC()
	w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2","device_token":"tok","device_check_env":"production"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if !store.gotForceMonth {
		t.Fatal("a claim without offer_variant_aware was not marked monthly")
	}
	if body(t, w)["offer_variant"] != VariantMonth {
		t.Fatalf("answer = %v", body(t, w))
	}
}

func TestClaimVariantReadFailureAnswersMonthly(t *testing.T) {
	svc, store, _, _ := configuredDC()
	store.variantErr = errors.New("db blip")
	w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2","device_token":"tok","device_check_env":"production","offer_variant_aware":true}`, nil)
	if w.Code != 200 || body(t, w)["offer_variant"] != VariantMonth || body(t, w)["offer_code"] != "XK7P2M9Q" {
		t.Fatalf("status %d %v, want the code with the monthly terms", w.Code, body(t, w))
	}
}

func TestSplitByVariantAndUnknownNames(t *testing.T) {
	ps := PoolStatus{
		Settings: PoolSettings{OfferRefMonth: "Referral Friend Month", OfferRefYear: "Referral Friend Year"},
		Batches: []PoolBatch{
			{Name: "Referral Friend Month", Free: 3},
			{Name: "Referral Friend Year", Free: 2},
			{Name: "Referral Friend Month (staging test)", Free: 1},
			{Name: "Referral Friend Month (staging test)", Free: 1},
		},
	}
	month, year := ps.SplitByVariant()
	if len(month.Batches) != 3 || len(year.Batches) != 1 || year.Batches[0].Free != 2 {
		t.Fatalf("split = %d / %d", len(month.Batches), len(year.Batches))
	}
	if got := ps.UnknownOfferNames(); len(got) != 1 || got[0] != "Referral Friend Month (staging test)" {
		t.Fatalf("unknown = %v", got)
	}
	// Without 023 everything is monthly and nothing is unknown.
	old := PoolStatus{Batches: ps.Batches}
	m, y := old.SplitByVariant()
	if len(m.Batches) != 4 || len(y.Batches) != 0 || len(old.UnknownOfferNames()) != 0 {
		t.Fatalf("pre-023 split = %d / %d, unknown %v", len(m.Batches), len(y.Batches), old.UnknownOfferNames())
	}
}

func TestYearlyAlertMailNamesTheYearlyOffer(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ps := PoolStatus{Settings: PoolSettings{LowStockThreshold: 100, ExpiryWarnDays: 30, IssueMarginDays: 14,
		OfferRefMonth: "Referral Friend Month", OfferRefYear: "Referral Friend Year"}}
	st := TakeStock(ps, now)
	subject, body := alertMailFor(ps, st, now, "SANDBOX", VariantYear)
	if !strings.Contains(subject, "(yearly group)") || !strings.Contains(subject, "[SANDBOX]") {
		t.Fatalf("subject = %q", subject)
	}
	if !strings.Contains(body, `import-codes.sh codes.csv "Referral Friend Year"`) || strings.Contains(body, "Referral Friend Month") {
		t.Fatalf("yearly body mentions the wrong offer:\n%s", body)
	}
	if strings.Contains(body, "auto_restock_enabled") {
		t.Fatal("yearly body points at auto-restock, which does not refill the yearly group")
	}
	subject, body = alertMailFor(ps, st, now, "", VariantMonth)
	if strings.Contains(subject, "yearly") || !strings.Contains(body, `"Referral Friend Month"`) {
		t.Fatalf("monthly mail changed: %q", subject)
	}
}
