package rewards

import (
	"sort"
	"time"
)

// The card's per-referral progress (rewards.md ### 每条推荐的进度): the three
// counts and the "ON THE WAY" list, computed from referrals + reward_ledger on
// every /me. Read-only, no migration: everything it needs is already stored.

// Phases on the wire (rewards.referrals[].phase).
const (
	PhasePaying     = "paying"     // has a ledger row that is not voided
	PhaseFreeMonth  = "free_month" // redeemed, still inside the friend's free month
	PhaseUnredeemed = "unredeemed" // claimed a code, not redeemed yet
)

// Plans on the wire (rewards.referrals[].plan).
const (
	PlanYearly  = "yearly"
	PlanMonthly = "monthly"
)

// progressMaxRows caps the list; the counts still cover everyone.
const progressMaxRows = 50

// unredeemedWindow is how long a claimed-but-not-redeemed referral stays on
// the list: settlement ① stops looking at it after this (referral/pgstore.go
// ListClaimed), and the offer code it got has expired long before (≤ 6 months).
const unredeemedWindow = 200 * 24 * time.Hour

// freeMonthGrace keeps a free_month row for three days after the free month
// ends with nothing credited yet: a friend who bought premium.yearly.3d
// during the free month (a downgrade, effective at its end) may sit in its
// 3-day free trial before the first charge, and a charge's webhook can land a
// few minutes late. The row then shows the grace's end as
// free_month_ends_at — the app drops a free_month row whose date has passed
// (rewards.md ### 每条推荐的进度).
const freeMonthGrace = 3 * 24 * time.Hour

// planOf maps a store product id to its plan. The old
// "com.chenyao.plantapp.premium.monthly" is a YEARLY product despite its
// name (yardmate-swiftui RemoteFlags.DefaultPaywallProducts). Anything else
// has no plan (the app writes just "Joined {date}").
var planOf = map[string]string{
	"com.chenyao.plantapp.premium.yearly.3d": PlanYearly,
	"com.chenyao.plantapp.premium.yearly":    PlanYearly,
	"com.chenyao.plantapp.premium.monthly":   PlanYearly,
	"com.chenyao.plantapp.premium.month":     PlanMonthly,
}

// TrackReferral is one of the referrer's referrals that is not void.
type TrackReferral struct {
	ID         int64
	State      string    // 016 referrals.state, never "void"
	ClaimedAt  time.Time // the friend put the code in
	RedeemedAt time.Time // subscription start settlement ① read from RevenueCat; zero while claimed
	// IssuedAt is coalesce(reissued_at, claimed_at): when the friend last got
	// an offer code — the start of the unredeemed window.
	IssuedAt time.Time
}

// TrackCredit is one of the referrer's ledger rows that still counts
// (pending or available).
type TrackCredit struct {
	ReferralID  int64 // 0 when the friend deleted their account (referral_id set null)
	State       string
	CreditCents int64
	ProductID   string
	PurchasedAt time.Time
	AvailableAt time.Time
}

// Counts is rewards.counts.
type Counts struct {
	Joined    int `json:"joined"`
	FreeMonth int `json:"free_month"`
	Paying    int `json:"paying"`
}

// ReferralView is one rewards.referrals[] row. Times are RFC 3339, UTC, whole
// seconds; the keys without omitempty are always there (null when absent).
type ReferralView struct {
	JoinedAt        *string `json:"joined_at"` // null: the friend deleted their account
	Phase           string  `json:"phase"`
	Plan            string  `json:"plan,omitempty"`
	PendingCents    int64   `json:"pending_cents"`
	PendingCount    int     `json:"pending_count"`
	NextCents       int64   `json:"next_cents"`
	NextAvailableAt *string `json:"next_available_at"`
	FreeMonthEndsAt *string `json:"free_month_ends_at"`
}

// FreeMonthEnd is when a friend's free month ends: the subscription start
// (redeemed_at) plus one calendar month, UTC — the "Referral Friend Month"
// offer is a ONE_MONTH free trial. When next month has no such day it is the
// last day of next month (Jan 31 → Feb 28 / 29), as Apple dates a monthly
// period; time.AddDate would roll over into March instead.
func FreeMonthEnd(redeemedAt time.Time) time.Time {
	t := redeemedAt.UTC()
	first := time.Date(t.Year(), t.Month()+1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	lastDay := first.AddDate(0, 1, -1).Day()
	day := t.Day()
	if day > lastDay {
		day = lastDay
	}
	return first.AddDate(0, 0, day-1)
}

// one referral's (or the deleted friends') money, folded from its credits.
type creditSum struct {
	paid         bool // any row that is not voided (the caller only passes those)
	pendingCents int64
	pendingCount int
	next         *TrackCredit // the pending row that becomes available first
	latest       *TrackCredit // the most recent purchase, for the plan
}

func (s *creditSum) add(c *TrackCredit) {
	s.paid = true
	if s.latest == nil || c.PurchasedAt.After(s.latest.PurchasedAt) {
		s.latest = c
	}
	if c.State != LedgerPending {
		return
	}
	s.pendingCents += c.CreditCents
	s.pendingCount++
	if s.next == nil || c.AvailableAt.Before(s.next.AvailableAt) {
		s.next = c
	}
}

func (s *creditSum) view(joined *string) ReferralView {
	v := ReferralView{
		JoinedAt:     joined,
		Phase:        PhasePaying,
		PendingCents: s.pendingCents,
		PendingCount: s.pendingCount,
	}
	if s.latest != nil {
		v.Plan = planOf[s.latest.ProductID]
	}
	if s.next != nil {
		v.NextCents = s.next.CreditCents
		at := rfc3339(s.next.AvailableAt)
		v.NextAvailableAt = &at
	}
	return v
}

// row is a list row with what it sorts by.
type row struct {
	view  ReferralView
	rank  int       // phase order: paying, free month, unredeemed
	key   time.Time // paying: next available; free month: its end; unredeemed: claimed (newest first)
	newer bool      // key sorts descending
	id    int64
}

// Progress turns the referrer's referrals and credits into rewards.counts and
// rewards.referrals (rewards.md ### 每条推荐的进度):
//
//   - paying: a credit that is not voided. Listed only while something is
//     pending; once it has all become available the row leaves the list.
//   - unredeemed: still claimed, offer code issued within unredeemedWindow.
//     Listed; older ones count as Joined only.
//   - free_month: redeemed, now before FreeMonthEnd. Listed.
//   - otherwise (redeemed, free month over, nothing credited): only Joined.
//
// Credits whose referral is not in refs belong to a void referral and are
// skipped; credits with no referral (the friend deleted their account) fold
// into one row with a null joined_at, outside the counts. Sorted paying (first
// to mature first), free month (first to end first), unredeemed (newest
// first); at most progressMaxRows rows.
func Progress(refs []TrackReferral, credits []TrackCredit, now time.Time) (Counts, []ReferralView) {
	sums := map[int64]*creditSum{}
	var deleted creditSum
	for i := range credits {
		c := &credits[i]
		if c.State != LedgerPending && c.State != LedgerAvailable {
			continue
		}
		if c.ReferralID == 0 {
			deleted.add(c)
			continue
		}
		s := sums[c.ReferralID]
		if s == nil {
			s = &creditSum{}
			sums[c.ReferralID] = s
		}
		s.add(c)
	}

	counts := Counts{Joined: len(refs)}
	var rows []row
	for _, r := range refs {
		joined := rfc3339(r.ClaimedAt)
		if s := sums[r.ID]; s != nil && s.paid {
			counts.Paying++
			if s.pendingCount > 0 {
				rows = append(rows, row{view: s.view(&joined), rank: 0, key: s.next.AvailableAt, id: r.ID})
			}
			continue
		}
		if r.State == refClaimed {
			issued := r.IssuedAt
			if issued.IsZero() {
				issued = r.ClaimedAt
			}
			if now.Sub(issued) > unredeemedWindow {
				continue // settlement gave up on it: Joined only
			}
			rows = append(rows, row{
				view: ReferralView{JoinedAt: &joined, Phase: PhaseUnredeemed},
				rank: 2, key: r.ClaimedAt, newer: true, id: r.ID,
			})
			continue
		}
		start := r.RedeemedAt
		if start.IsZero() {
			start = r.ClaimedAt // redeemed always has redeemed_at; belt and braces
		}
		end := FreeMonthEnd(start)
		if !now.Before(end) {
			end = end.Add(freeMonthGrace)
			if !now.Before(end) {
				continue // free month (and the grace) over and nothing paid: Joined only
			}
		}
		counts.FreeMonth++
		ends := rfc3339(end)
		rows = append(rows, row{
			view: ReferralView{JoinedAt: &joined, Phase: PhaseFreeMonth, FreeMonthEndsAt: &ends},
			rank: 1, key: end, id: r.ID,
		})
	}
	if deleted.pendingCount > 0 {
		rows = append(rows, row{view: deleted.view(nil), rank: 0, key: deleted.next.AvailableAt})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if !a.key.Equal(b.key) {
			if a.newer {
				return a.key.After(b.key)
			}
			return a.key.Before(b.key)
		}
		return a.id < b.id
	})
	if len(rows) > progressMaxRows {
		rows = rows[:progressMaxRows]
	}
	list := make([]ReferralView, len(rows))
	for i, r := range rows {
		list[i] = r.view
	}
	return counts, list
}
