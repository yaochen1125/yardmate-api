package referral

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store for the handler + settlement tests. It keeps
// the same state machine as pgstore.go / 013_referral.sql so the settle tests
// exercise real transitions, and exposes a few knobs for the handler tests.
type memStore struct {
	mu sync.Mutex

	// handler knobs
	meStanding Standing
	meErr      error
	claimOffer string
	claimErr   error
	redeemErr  error
	gotUser    string
	gotCode    string
	gotOffer   string
	deleted    []string

	// settlement state
	dueNow      time.Time // the "database clock" ListDue compares against
	nextID      int64
	refs        map[int64]*memRef
	ledgers     map[string]Ledger
	holdDays    int
	holdErr     error
	expired     int64
	recordErr   error
	recordCalls int
	pool        PoolStatus
	poolErr     error
}

type memRef struct {
	Referral
	qualifiedAt time.Time
	voidReason  string
}

func newMemStore() *memStore {
	return &memStore{refs: map[int64]*memRef{}, ledgers: map[string]Ledger{}, holdDays: 7}
}

// add inserts a referral in the given state and returns its id.
func (m *memStore) add(referrer, referee, state string, claimedAt time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.refs[m.nextID] = &memRef{Referral: Referral{
		ID: m.nextID, Referrer: referrer, Referee: referee, State: state, ClaimedAt: claimedAt, OfferCode: "OFFER",
	}}
	return m.nextID
}

func (m *memStore) get(id int64) Referral {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refs[id].Referral
}

func (m *memStore) byState(state string) []Referral {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Referral
	for _, r := range m.refs {
		if r.State == state {
			out = append(out, r.Referral)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *memStore) Me(_ context.Context, userID string) (Standing, error) {
	m.gotUser = userID
	return m.meStanding, m.meErr
}

func (m *memStore) Claim(_ context.Context, userID, code string) (string, error) {
	m.gotUser, m.gotCode = userID, code
	return m.claimOffer, m.claimErr
}

func (m *memStore) ReportRedeemed(_ context.Context, userID, offer string) error {
	m.gotUser, m.gotOffer = userID, offer
	return m.redeemErr
}

func (m *memStore) DeleteUser(_ context.Context, userID string) error {
	m.deleted = append(m.deleted, userID)
	return nil
}

func (m *memStore) ExpireCodes(context.Context) (int64, error) { return m.expired, nil }

func (m *memStore) PoolStatus(context.Context) (PoolStatus, error) { return m.pool, m.poolErr }

// fakeAlerter records every alert; err makes each send fail.
type fakeAlerter struct {
	mu    sync.Mutex
	sent  []alertCall
	err   error
	calls int
}

type alertCall struct{ subject, body string }

func (f *fakeAlerter) Alert(subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, alertCall{subject, body})
	return nil
}

func (m *memStore) HoldDays(context.Context) (int, error) { return m.holdDays, m.holdErr }

func (m *memStore) ListClaimed(_ context.Context, limit int) ([]Referral, error) {
	return capped(m.byState(StateClaimed), limit), nil
}

func (m *memStore) MarkRedeemed(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateClaimed {
		return errors.New("not claimed")
	}
	r.State, r.RedeemedAt = StateRedeemed, at
	return nil
}

func (m *memStore) ListDue(_ context.Context, hold time.Duration, limit int) ([]Referral, error) {
	// The real query compares against the database clock; here the caller's
	// clock is injected through dueNow.
	m.mu.Lock()
	now := m.dueNow
	m.mu.Unlock()
	var out []Referral
	for _, r := range m.byState(StateRedeemed) {
		if !r.RedeemedAt.After(now.Add(-hold)) {
			out = append(out, r)
		}
	}
	return capped(out, limit), nil
}

func (m *memStore) MarkVoid(_ context.Context, id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[id].State, m.refs[id].voidReason = StateVoid, reason
	return nil
}

func (m *memStore) MarkQualified(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateRedeemed {
		return errors.New("not redeemed")
	}
	r.State, r.qualifiedAt = StateQualified, m.dueNow
	return nil
}

func (m *memStore) ListQualified(_ context.Context, limit int) ([]Referral, error) {
	return capped(m.byState(StateQualified), limit), nil
}

func (m *memStore) MarkSaved(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateQualified {
		return errors.New("not qualified")
	}
	r.State = StateSaved
	return nil
}

func (m *memStore) Ledger(_ context.Context, userID string) (Ledger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	led := m.ledgers[userID]
	led.UserID = userID
	return led, nil
}

func (m *memStore) ListSavers(_ context.Context, limit int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.byState(StateSaved) {
		if !seen[r.Referrer] {
			seen[r.Referrer] = true
			out = append(out, r.Referrer)
		}
	}
	return capped(out, limit), nil
}

func (m *memStore) SavedIDs(_ context.Context, userID string) ([]int64, error) {
	var out []int64
	for _, r := range m.byState(StateSaved) {
		if r.Referrer == userID {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

func (m *memStore) RecordGranted(_ context.Context, userID string, ids []int64, until time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordCalls++
	if m.recordErr != nil {
		return m.recordErr
	}
	led := m.ledgers[userID]
	led.UserID = userID
	led.MonthsGranted += len(ids)
	led.PromoUntil = until
	m.ledgers[userID] = led
	for _, id := range ids {
		r := m.refs[id]
		if r.State == StateQualified || r.State == StateSaved {
			r.State = StateRewarded
		}
	}
	return nil
}

func capped[T any](in []T, limit int) []T {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}

// fakeRC is an in-memory Subscribers keyed by RevenueCat app user id.
type fakeRC struct {
	mu     sync.Mutex
	subs   map[string]*Subscriber
	err    error
	grants []grantCall
	gets   int
	gerr   error
}

type grantCall struct {
	user, ent string
	until     time.Time
}

func newFakeRC() *fakeRC { return &fakeRC{subs: map[string]*Subscriber{}} }

func (f *fakeRC) set(userID string, sub *Subscriber) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[AppUserID(userID)] = sub
}

func (f *fakeRC) Subscriber(_ context.Context, appUserID string) (*Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return nil, f.err
	}
	if sub, ok := f.subs[appUserID]; ok {
		return sub, nil
	}
	return &Subscriber{}, nil // RevenueCat creates an empty customer
}

func (f *fakeRC) GrantPromotional(_ context.Context, appUserID, ent string, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gerr != nil {
		return f.gerr
	}
	f.grants = append(f.grants, grantCall{appUserID, ent, until})
	return nil
}

// Subscriber builders.
func tp(t time.Time) *time.Time { return &t }

func storeSub(start, expires time.Time) Subscription {
	return Subscription{Store: "app_store", PeriodType: "normal", OwnershipType: "PURCHASED",
		PurchaseDate: tp(start), OriginalPurchaseDate: tp(start), ExpiresDate: tp(expires)}
}

func promoSub(expires time.Time) Subscription {
	return Subscription{Store: "promotional", OwnershipType: "PURCHASED", PurchaseDate: tp(expires.Add(-RewardMonth)), ExpiresDate: tp(expires)}
}

func withSubs(subs map[string]Subscription) *Subscriber {
	return &Subscriber{Subscriptions: subs, Entitlements: map[string]Entitlement{}}
}
