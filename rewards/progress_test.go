package rewards

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	productYearly    = "com.chenyao.plantapp.premium.yearly.3d"
	productMonthly   = "com.chenyao.plantapp.premium.month"
	productOldYearly = "com.chenyao.plantapp.premium.monthly" // a YEARLY product despite its name
)

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func pending(refID, cents int64, product string, purchased time.Time) TrackCredit {
	return TrackCredit{ReferralID: refID, State: LedgerPending, CreditCents: cents, ProductID: product,
		PurchasedAt: purchased, AvailableAt: purchased.Add(RefundWindow)}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// The comparison card: a yearly payer 5 days from ready, a monthly payer with
// two months on the way, a friend in their free month, one who has not
// redeemed — and one whose free month ran out without paying (Joined only).
func TestProgressComparisonCard(t *testing.T) {
	now := t0
	refs := []TrackReferral{
		{ID: 1, State: "rewarded", ClaimedAt: now.Add(-day(115)), RedeemedAt: now.Add(-day(115))},
		{ID: 2, State: "rewarded", ClaimedAt: now.Add(-day(100)), RedeemedAt: now.Add(-day(100))},
		{ID: 3, State: "redeemed", ClaimedAt: now.Add(-day(15)), RedeemedAt: now.Add(-day(15))},
		{ID: 4, State: "claimed", ClaimedAt: now.Add(-day(1))},
		{ID: 5, State: "qualified", ClaimedAt: now.Add(-day(60)), RedeemedAt: now.Add(-day(60))},
	}
	credits := []TrackCredit{
		pending(1, 1020, productYearly, now.Add(-day(85))),
		pending(2, 178, productMonthly, now.Add(-day(42))),
		pending(2, 178, productMonthly, now.Add(-day(12))),
	}
	counts, list := Progress(refs, credits, now)
	if counts != (Counts{Joined: 5, FreeMonth: 1, Paying: 2}) {
		t.Fatalf("counts = %+v", counts)
	}
	if len(list) != 4 {
		t.Fatalf("list = %+v", list)
	}

	y := list[0]
	if y.Phase != PhasePaying || y.Plan != PlanYearly || y.PendingCents != 1020 || y.PendingCount != 1 || y.NextCents != 1020 {
		t.Errorf("yearly row = %+v", y)
	}
	if str(y.NextAvailableAt) != rfc3339(now.Add(day(5))) || str(y.JoinedAt) != rfc3339(refs[0].ClaimedAt) || y.FreeMonthEndsAt != nil {
		t.Errorf("yearly times = %s %s %s", str(y.NextAvailableAt), str(y.JoinedAt), str(y.FreeMonthEndsAt))
	}

	m := list[1]
	if m.Phase != PhasePaying || m.Plan != PlanMonthly || m.PendingCents != 356 || m.PendingCount != 2 || m.NextCents != 178 {
		t.Errorf("monthly row = %+v", m)
	}
	if str(m.NextAvailableAt) != rfc3339(now.Add(day(48))) {
		t.Errorf("monthly next = %s, want the earliest of the two", str(m.NextAvailableAt))
	}

	f := list[2]
	if f.Phase != PhaseFreeMonth || f.Plan != "" || f.PendingCents != 0 || f.NextAvailableAt != nil {
		t.Errorf("free month row = %+v", f)
	}
	if str(f.FreeMonthEndsAt) != rfc3339(refs[2].RedeemedAt.AddDate(0, 1, 0)) {
		t.Errorf("free month ends = %s", str(f.FreeMonthEndsAt))
	}

	u := list[3]
	if u.Phase != PhaseUnredeemed || str(u.JoinedAt) != rfc3339(refs[3].ClaimedAt) || u.FreeMonthEndsAt != nil || u.NextAvailableAt != nil {
		t.Errorf("unredeemed row = %+v", u)
	}
}

func TestProgressWireFormat(t *testing.T) {
	now := t0
	refs := []TrackReferral{
		{ID: 1, State: "rewarded", ClaimedAt: time.Date(2026, 6, 28, 17, 4, 11, 500, time.UTC)},
		{ID: 2, State: "claimed", ClaimedAt: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)},
	}
	credits := []TrackCredit{pending(1, 1020, productYearly, time.Date(2026, 7, 28, 17, 4, 11, 0, time.UTC))}
	counts, list := Progress(refs, credits, now)
	raw, _ := json.Marshal(Summary{Counts: &counts, Referrals: list})
	want := `{"available_cents":0,"pending_cents":0,"lifetime_cents":0,"payout":null,` +
		`"counts":{"joined":2,"free_month":0,"paying":1},"referrals":[` +
		`{"joined_at":"2026-06-28T17:04:11Z","phase":"paying","plan":"yearly","pending_cents":1020,"pending_count":1,` +
		`"next_cents":1020,"next_available_at":"2026-10-26T17:04:11Z","free_month_ends_at":null},` +
		`{"joined_at":"2026-10-20T15:00:00Z","phase":"unredeemed","pending_cents":0,"pending_count":0,` +
		`"next_cents":0,"next_available_at":null,"free_month_ends_at":null}]}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant  %s", raw, want)
	}
}

// Once everything a payer brought in has matured, their row leaves the list;
// they still count as Paying. A renewal puts them back.
func TestProgressMaturedRowLeavesTheList(t *testing.T) {
	now := t0
	refs := []TrackReferral{{ID: 1, State: "rewarded", ClaimedAt: now.Add(-day(200)), RedeemedAt: now.Add(-day(200))}}
	matured := TrackCredit{ReferralID: 1, State: LedgerAvailable, CreditCents: 1020, ProductID: productYearly,
		PurchasedAt: now.Add(-day(170)), AvailableAt: now.Add(-day(80))}
	counts, list := Progress(refs, []TrackCredit{matured}, now)
	if counts.Paying != 1 || counts.Joined != 1 || len(list) != 0 {
		t.Fatalf("all matured: counts=%+v list=%+v", counts, list)
	}
	renewal := pending(1, 1020, productYearly, now.Add(-day(1)))
	_, list = Progress(refs, []TrackCredit{matured, renewal}, now)
	if len(list) != 1 || list[0].PendingCount != 1 || list[0].PendingCents != 1020 {
		t.Fatalf("after renewal: %+v", list)
	}
}

func TestFreeMonthEndClampsToMonthEnd(t *testing.T) {
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 30, 15, 0, time.UTC) }
	for _, c := range []struct{ from, want time.Time }{
		{at(2026, 10, 6), at(2026, 11, 6)},
		{at(2026, 1, 31), at(2026, 2, 28)},
		{at(2028, 1, 31), at(2028, 2, 29)}, // leap year
		{at(2026, 1, 29), at(2026, 2, 28)},
		{at(2026, 3, 31), at(2026, 4, 30)},
		{at(2026, 8, 31), at(2026, 9, 30)},
		{at(2026, 12, 31), at(2027, 1, 31)},
		{at(2026, 2, 28), at(2026, 3, 28)},
	} {
		if got := FreeMonthEnd(c.from); !got.Equal(c.want) {
			t.Errorf("FreeMonthEnd(%s) = %s, want %s", c.from.Format(time.DateOnly), got, c.want)
		}
	}
	// Taken in UTC whatever zone it comes in.
	pdt := time.FixedZone("PDT", -7*3600)
	if got := FreeMonthEnd(time.Date(2026, 1, 30, 20, 0, 0, 0, pdt)); !got.Equal(time.Date(2026, 2, 28, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("zoned = %s", got)
	}
}

func TestProgressFreeMonthBoundary(t *testing.T) {
	redeemed := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	refs := []TrackReferral{{ID: 1, State: "redeemed", ClaimedAt: redeemed.Add(-time.Hour), RedeemedAt: redeemed}}
	end := FreeMonthEnd(redeemed)
	if !end.Equal(time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %s", end)
	}
	counts, list := Progress(refs, nil, end.Add(-time.Second))
	if counts.FreeMonth != 1 || len(list) != 1 || list[0].Phase != PhaseFreeMonth {
		t.Fatalf("just before the end: %+v %+v", counts, list)
	}
	// Ended and nothing paid: Joined only, not listed.
	counts, list = Progress(refs, nil, end)
	if counts != (Counts{Joined: 1}) || len(list) != 0 {
		t.Fatalf("at the end: %+v %+v", counts, list)
	}
	// The conversion charge arrives: paying again.
	counts, list = Progress(refs, []TrackCredit{pending(1, 178, productMonthly, end)}, end.Add(time.Minute))
	if counts != (Counts{Joined: 1, Paying: 1}) || len(list) != 1 || list[0].Phase != PhasePaying {
		t.Fatalf("after conversion: %+v %+v", counts, list)
	}
}

// A friend who put the code in and paid straight away (still claimed: the
// first charge beats settlement ①) is a payer, not "hasn't redeemed".
func TestProgressClaimedButPaidIsPaying(t *testing.T) {
	now := t0
	refs := []TrackReferral{{ID: 7, State: "claimed", ClaimedAt: now.Add(-time.Hour)}}
	counts, list := Progress(refs, []TrackCredit{pending(7, 1020, productYearly, now.Add(-time.Minute))}, now)
	if counts != (Counts{Joined: 1, Paying: 1}) || len(list) != 1 || list[0].Phase != PhasePaying {
		t.Fatalf("%+v %+v", counts, list)
	}
}

func TestProgressDeletedFriendsFoldIntoOneRow(t *testing.T) {
	now := t0
	credits := []TrackCredit{
		pending(0, 178, productMonthly, now.Add(-day(80))),
		pending(0, 1020, productYearly, now.Add(-day(30))),
		{ReferralID: 0, State: LedgerAvailable, CreditCents: 500, ProductID: productMonthly,
			PurchasedAt: now.Add(-day(120)), AvailableAt: now.Add(-day(30))},
	}
	counts, list := Progress(nil, credits, now)
	if counts != (Counts{}) {
		t.Fatalf("deleted friends counted: %+v", counts)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	r := list[0]
	if r.JoinedAt != nil || r.Phase != PhasePaying || r.PendingCents != 1198 || r.PendingCount != 2 || r.NextCents != 178 {
		t.Errorf("deleted row = %+v", r)
	}
	if r.Plan != PlanYearly { // the most recent purchase
		t.Errorf("plan = %q", r.Plan)
	}
	// Only matured money left: no row.
	if _, list := Progress(nil, credits[2:], now); len(list) != 0 {
		t.Errorf("matured-only deleted row listed: %+v", list)
	}
}

// Credits of a void referral (not in refs) stay out; so do voided rows.
func TestProgressSkipsVoid(t *testing.T) {
	now := t0
	refs := []TrackReferral{{ID: 1, State: "redeemed", ClaimedAt: now.Add(-day(3)), RedeemedAt: now.Add(-day(3))}}
	credits := []TrackCredit{
		pending(99, 1020, productYearly, now.Add(-day(1))), // referral 99 is void
		{ReferralID: 1, State: LedgerVoided, CreditCents: 178, ProductID: productMonthly,
			PurchasedAt: now.Add(-day(2)), AvailableAt: now.Add(day(88))},
	}
	counts, list := Progress(refs, credits, now)
	if counts != (Counts{Joined: 1, FreeMonth: 1}) || len(list) != 1 || list[0].Phase != PhaseFreeMonth {
		t.Fatalf("%+v %+v", counts, list)
	}
}

func TestProgressPlans(t *testing.T) {
	now := t0
	for product, want := range map[string]string{
		productYearly: PlanYearly, "com.chenyao.plantapp.premium.yearly": PlanYearly, productOldYearly: PlanYearly,
		productMonthly: PlanMonthly, "com.example.other": "", "": "",
	} {
		refs := []TrackReferral{{ID: 1, State: "rewarded", ClaimedAt: now.Add(-day(30))}}
		_, list := Progress(refs, []TrackCredit{pending(1, 100, product, now.Add(-day(1)))}, now)
		if len(list) != 1 || list[0].Plan != want {
			t.Errorf("%q → %+v, want plan %q", product, list, want)
		}
	}
	// The plan follows the latest purchase: monthly → yearly upgrade.
	refs := []TrackReferral{{ID: 1, State: "rewarded", ClaimedAt: now.Add(-day(60))}}
	_, list := Progress(refs, []TrackCredit{
		pending(1, 1020, productYearly, now.Add(-day(2))),
		pending(1, 178, productMonthly, now.Add(-day(32))),
	}, now)
	if len(list) != 1 || list[0].Plan != PlanYearly || list[0].NextCents != 178 {
		t.Errorf("upgrade: %+v", list)
	}
}

func TestProgressOrderAndCap(t *testing.T) {
	now := t0
	var refs []TrackReferral
	var credits []TrackCredit
	id := int64(0)
	add := func(state string, claimed, redeemed time.Time) int64 {
		id++
		refs = append(refs, TrackReferral{ID: id, State: state, ClaimedAt: claimed, RedeemedAt: redeemed})
		return id
	}
	// Unredeemed: newest first. Free month: first to end first. Paying: first to mature first.
	u1 := add("claimed", now.Add(-day(5)), time.Time{})
	u2 := add("claimed", now.Add(-day(1)), time.Time{})
	f1 := add("redeemed", now.Add(-day(10)), now.Add(-day(10)))
	f2 := add("redeemed", now.Add(-day(20)), now.Add(-day(20)))
	p1 := add("rewarded", now.Add(-day(90)), now.Add(-day(90)))
	credits = append(credits, pending(p1, 178, productMonthly, now.Add(-day(10))))
	p2 := add("rewarded", now.Add(-day(80)), now.Add(-day(80)))
	credits = append(credits, pending(p2, 178, productMonthly, now.Add(-day(70))))
	_, list := Progress(refs, credits, now)
	joined := func(id int64) string { return rfc3339(refs[id-1].ClaimedAt) }
	want := []string{joined(p2), joined(p1), joined(f2), joined(f1), joined(u2), joined(u1)}
	if len(list) != len(want) {
		t.Fatalf("list = %+v", list)
	}
	for i := range want {
		if str(list[i].JoinedAt) != want[i] {
			t.Errorf("row %d joined %s, want %s (%s)", i, str(list[i].JoinedAt), want[i], list[i].Phase)
		}
	}

	// 60 unredeemed: 50 rows, counts cover all.
	refs = nil
	for i := 0; i < 60; i++ {
		refs = append(refs, TrackReferral{ID: int64(i + 1), State: "claimed", ClaimedAt: now.Add(-time.Duration(i) * time.Hour)})
	}
	counts, list := Progress(refs, nil, now)
	if counts.Joined != 60 || len(list) != progressMaxRows {
		t.Fatalf("cap: joined=%d rows=%d", counts.Joined, len(list))
	}
	if str(list[0].JoinedAt) != rfc3339(now) {
		t.Errorf("cap kept the wrong end: first = %s", str(list[0].JoinedAt))
	}
}

// /me end to end through the fake store: the progress rides along with the
// balance; a failing progress read drops only counts / referrals.
func TestRewardsCarriesProgress(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	ref := store.addReferral(referrer, referee, "rewarded", t0.Add(-day(100)))
	store.mu.Lock()
	store.redeemed[ref.ID] = t0.Add(-day(100))
	store.ledger = append(store.ledger, &ledgerRow{Credit: Credit{ReferralID: ref.ID, Referrer: referrer,
		EventID: "e1", TransactionID: "t1", ProductID: productYearly, CreditCents: 1020,
		PurchasedAt: t0.Add(-day(85)), AvailableAt: t0.Add(day(5))}, State: LedgerPending})
	store.mu.Unlock()

	s := summaryOf(t, svc, referrer)
	if s.PendingCents != 1020 || s.Counts == nil || *s.Counts != (Counts{Joined: 1, Paying: 1}) || len(s.Referrals) != 1 {
		t.Fatalf("summary = %+v counts=%+v", s, s.Counts)
	}

	store.progressErr = errBoom
	s = summaryOf(t, svc, referrer)
	if s.PendingCents != 1020 || s.Counts != nil || s.Referrals != nil {
		t.Fatalf("progress error: %+v", s)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "counts") || strings.Contains(string(raw), "referrals") {
		t.Fatalf("progress error wire = %s", raw)
	}
	// Someone with nothing: counts of zero, no list.
	store.progressErr = nil
	empty := summaryOf(t, svc, stranger)
	if empty.Counts == nil || *empty.Counts != (Counts{}) || len(empty.Referrals) != 0 {
		t.Fatalf("stranger: %+v counts=%+v", empty, empty.Counts)
	}
}

// Claimed but never redeemed: listed for 200 days after the (last) code, then
// Joined only — settlement stops looking at it and the code has expired.
func TestProgressUnredeemedWindow(t *testing.T) {
	now := t0
	refs := []TrackReferral{
		{ID: 1, State: "claimed", ClaimedAt: now.Add(-day(199)), IssuedAt: now.Add(-day(199))},
		{ID: 2, State: "claimed", ClaimedAt: now.Add(-day(201)), IssuedAt: now.Add(-day(201))},
		{ID: 3, State: "claimed", ClaimedAt: now.Add(-day(300)), IssuedAt: now.Add(-day(20))}, // code reissued
	}
	counts, list := Progress(refs, nil, now)
	if counts != (Counts{Joined: 3}) || len(list) != 2 {
		t.Fatalf("%+v %+v", counts, list)
	}
	for _, r := range list {
		if str(r.JoinedAt) == rfc3339(refs[1].ClaimedAt) {
			t.Errorf("201-day-old unredeemed listed: %+v", r)
		}
	}
}
