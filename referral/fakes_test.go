package referral

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// memStore is an in-memory Store for the handler + settlement tests. It keeps
// the same state machine as pgstore.go / 013_referral.sql so the settle tests
// exercise real transitions, and exposes a few knobs for the handler tests.
type memStore struct {
	mu sync.Mutex
	// deviceMu is the PG store's global device lock (pg_advisory_xact_lock,
	// ClaimGate): held by a gated Claim from before its gate's Check to the
	// end. mu alone is released around the gate's Apple calls, so without
	// deviceMu two gated claims could both check before either sets.
	deviceMu sync.Mutex

	// handler knobs
	meStanding Standing
	meErr      error
	// pending / pendingErr answer PendingOffer; pendingCalls counts them.
	pending      *PendingOffer
	pendingErr   error
	pendingCalls int
	claimOffer string
	claimErr   error
	checkErr   error
	redeemErr  error
	gotUser    string
	gotCode    string
	gotInstall string
	gotOffer   string
	deleted    []string
	meInstalls []string // install ids /me was called with, in order
	claimCalls int      // Claim calls that reached the "database"

	// claim emulation (handler tests of the judgement order). When codes is
	// nil, ClaimCheck says ClaimNew and Claim hands out claimOffer (fresh),
	// as before. When set, the SQL rules of 016's referral_claim_check /
	// referral_claim are replayed: codes = invite code → owner, devices =
	// owner → install ids seen on /me, claims = referee → their claim,
	// shelf = free pool codes (taken from the front; empty = pool_empty).
	codes   map[string]string
	devices map[string]map[string]bool
	claims  map[string]*memClaim
	shelf   []string

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

	// anti-abuse (016)
	velocityLimit  int
	velocityWindow int             // hours
	usedKeys       map[string]bool // referral_used_subscriptions
	digestErr      error
	notifyErr      error

	// auto-restock (021): referral_settings columns, code_restocks, and the
	// codes in code_pool — see restock_fakes_test.go.
	rs memRestocks
}

type memClaim struct {
	referrer, offer, install string
}

type memRef struct {
	Referral
	qualifiedAt    time.Time
	voidReason     string
	subKey         string
	heldNotifiedAt time.Time
	installID      string
	// rotation (017): the last RevenueCat look (zero = never) and the last
	// /redeemed report, both on the "database clock".
	lastChecked time.Time
	reportedAt  time.Time
}

// reportedSinceCheck is ListClaimed's first sort key.
func (r *memRef) reportedSinceCheck() bool {
	return !r.reportedAt.IsZero() && r.reportedAt.After(r.lastChecked)
}

func newMemStore() *memStore {
	return &memStore{refs: map[int64]*memRef{}, ledgers: map[string]Ledger{}, holdDays: 7,
		velocityLimit: 5, velocityWindow: 24, usedKeys: map[string]bool{}}
}

// emulateClaims switches Claim / ClaimCheck to the SQL rules (see codes).
func (m *memStore) emulateClaims(shelf ...string) {
	m.codes = map[string]string{}
	m.devices = map[string]map[string]bool{}
	m.claims = map[string]*memClaim{}
	m.shelf = append([]string(nil), shelf...)
}

// own gives user an invite code (as their first /me would).
func (m *memStore) own(user, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code] = user
}

// sawDevice records install as one of user's devices (as /me does).
func (m *memStore) sawDevice(user, install string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.devices[user] == nil {
		m.devices[user] = map[string]bool{}
	}
	m.devices[user][strings.ToLower(install)] = true
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

func (m *memStore) Me(_ context.Context, userID, installID string) (Standing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotUser = userID
	m.meInstalls = append(m.meInstalls, installID)
	return m.meStanding, m.meErr
}

func (m *memStore) PendingOffer(_ context.Context, userID, installID string) (*PendingOffer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingCalls++
	return m.pending, m.pendingErr
}

func (m *memStore) ClaimCheck(_ context.Context, userID, code, installID string) (ClaimKind, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotUser, m.gotCode, m.gotInstall = userID, code, installID
	if m.checkErr != nil {
		return ClaimNew, m.checkErr
	}
	if m.codes == nil {
		return ClaimNew, nil
	}
	return m.judgeLocked(userID, code, installID)
}

// judgeLocked replays steps 2–4 of 016's referral_claim_check.
func (m *memStore) judgeLocked(userID, code, installID string) (ClaimKind, error) {
	owner, ok := m.codes[code]
	if !ok {
		return ClaimNew, Failure{ErrUnknownCode}
	}
	if c, ok := m.claims[userID]; ok {
		if c.referrer != owner {
			return ClaimNew, Failure{ErrAlreadyClaimed}
		}
		return ClaimAgain, nil
	}
	if owner == userID || m.devices[owner][strings.ToLower(installID)] {
		return ClaimNew, Failure{ErrOwnCode}
	}
	return ClaimNew, nil
}

func (m *memStore) Claim(ctx context.Context, userID, code, installID string, gate *ClaimGate) (string, error) {
	if gate != nil {
		m.deviceMu.Lock()
		defer m.deviceMu.Unlock()
	}
	m.mu.Lock()
	m.claimCalls++
	m.gotUser, m.gotCode, m.gotInstall = userID, code, installID
	if m.claimErr != nil {
		err := m.claimErr
		m.mu.Unlock()
		return "", err
	}
	emulate := m.codes != nil
	kind := ClaimNew
	if emulate {
		// Steps 2–4 again under the device lock (PG: referral_claim_check).
		k, err := m.judgeLocked(userID, code, installID)
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
		kind = k
	}
	m.mu.Unlock()

	// Apple is asked without mu, like the PG store asks it without holding
	// any row: only deviceMu keeps two gated claims apart here.
	if gate != nil && gate.Check != nil && kind == ClaimNew {
		if err := gate.Check(); err != nil {
			return "", err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	set := func(fresh bool) error {
		if gate == nil || gate.Set == nil {
			return nil
		}
		return gate.Set(fresh)
	}
	if !emulate {
		if err := set(true); err != nil {
			return "", err
		}
		return m.claimOffer, nil
	}
	// referral_claim's own atomic re-check.
	kind, err := m.judgeLocked(userID, code, installID)
	if err != nil {
		return "", err
	}
	if kind == ClaimAgain {
		offer := m.claims[userID].offer
		if err := set(false); err != nil {
			return "", err
		}
		return offer, nil
	}
	if len(m.shelf) == 0 {
		return "", Failure{ErrPoolEmpty}
	}
	offer := m.shelf[0]
	if err := set(true); err != nil {
		return "", err // rolled back: the code stays on the shelf, no claim
	}
	// The commit (PG: tx.Commit(ctx)) fails on a cancelled context.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.shelf = m.shelf[1:]
	m.claims[userID] = &memClaim{referrer: m.codes[code], offer: offer, install: strings.ToLower(installID)}
	return offer, nil
}

func (m *memStore) ReportRedeemed(_ context.Context, userID, offer string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotUser, m.gotOffer = userID, offer
	if m.redeemErr != nil {
		return m.redeemErr
	}
	for _, r := range m.refs {
		if r.Referee == userID && r.OfferCode == offer && r.State == StateClaimed {
			r.reportedAt = m.dueNow
		}
	}
	return nil
}

func (m *memStore) DeleteUser(_ context.Context, userID string) error {
	m.deleted = append(m.deleted, userID)
	return nil
}

func (m *memStore) ExpireCodes(context.Context) (int64, error) { return m.expired, nil }

func (m *memStore) PoolStatus(context.Context) (PoolStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps := m.pool
	ps.Batches = append([]PoolBatch(nil), m.pool.Batches...)
	return ps, m.poolErr
}

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

// ListClaimed replays pgstore.go's rotation order (017).
func (m *memStore) ListClaimed(_ context.Context, limit int) ([]Referral, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []*memRef
	for _, r := range m.sortedLocked() {
		if r.State == StateClaimed {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if pa, pb := a.reportedSinceCheck(), b.reportedSinceCheck(); pa != pb {
			return pa
		}
		if !a.lastChecked.Equal(b.lastChecked) {
			return a.lastChecked.Before(b.lastChecked) // zero (never) first
		}
		if !a.ClaimedAt.Equal(b.ClaimedAt) {
			return a.ClaimedAt.Before(b.ClaimedAt)
		}
		return a.ID < b.ID
	})
	out := make([]Referral, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Referral)
	}
	return capped(out, limit), nil
}

func (m *memStore) MarkClaimedChecked(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.refs[id]; r != nil && r.State == StateClaimed {
		r.lastChecked = at
	}
	return nil
}

// MarkRedeemed replays 016's referral_mark_redeemed.
func (m *memStore) MarkRedeemed(_ context.Context, id int64, at time.Time, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateClaimed {
		return false, nil // referral_mark_redeemed: 'skipped'
	}
	if key != "" {
		dup := m.usedKeys[key]
		for _, o := range m.refs {
			if o.ID != id && o.subKey == key && o.State != StateVoid {
				dup = true
			}
		}
		if dup {
			r.State, r.voidReason, r.subKey = StateVoid, VoidDuplicate, key
			return true, nil
		}
	}
	r.State, r.RedeemedAt, r.subKey = StateRedeemed, at, key
	return false, nil
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

// fakeDC is an in-memory DeviceChecker: bits by (env, token). invalid tokens
// get ErrDeviceTokenInvalid; queryErr / setErr fail every call; block makes
// each call wait for its context (a hung Apple); delay makes each call take
// that long (widens race windows). setApplies: a failing set still sets the
// bit (Apple did it, the answer got lost). queryFails, when set, may fail
// query number n (1-based). afterSet runs once a set has succeeded.
type fakeDC struct {
	mu         sync.Mutex
	bits       map[string]bool // env + "|" + token → bit0
	invalid    map[string]bool
	queryErr   error
	setErr     error
	setApplies bool
	queryFails func(n int) error
	afterSet   func()
	block      bool
	delay      time.Duration
	queries    int
	sets       int
	envs       []string
}

func newFakeDC() *fakeDC { return &fakeDC{bits: map[string]bool{}, invalid: map[string]bool{}} }

func (f *fakeDC) wait(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !f.block {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeDC) QueryBit0(ctx context.Context, env, token string) (bool, error) {
	if err := f.wait(ctx); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	f.envs = append(f.envs, env)
	if f.queryErr != nil {
		return false, f.queryErr
	}
	if f.queryFails != nil {
		if err := f.queryFails(f.queries); err != nil {
			return false, err
		}
	}
	if f.invalid[token] {
		return false, ErrDeviceTokenInvalid
	}
	return f.bits[env+"|"+token], nil
}

func (f *fakeDC) SetBit0(ctx context.Context, env, token string) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.setErr != nil {
		if f.setApplies {
			f.bits[env+"|"+token] = true
		}
		return f.setErr
	}
	f.bits[env+"|"+token] = true
	if f.afterSet != nil {
		f.afterSet()
	}
	return nil
}

func (f *fakeDC) bit(env, token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bits[env+"|"+token]
}

// successAt is the gate's clock for a referral: redeemed_at (its theoretical
// success time minus the shared hold_days), qualified_at for a row without
// one — as 017's referral_qualify coalesces.
func (r *memRef) successAt() time.Time {
	if r.RedeemedAt.IsZero() {
		return r.qualifiedAt
	}
	return r.RedeemedAt
}

// inWindowBefore: o's success time is inside the window before at's (at
// included), as referral_qualify / HeldDigests count.
func (m *memStore) inWindowBefore(o *memRef, at time.Time) bool {
	t := o.successAt()
	return t.After(at.Add(-time.Duration(m.velocityWindow)*time.Hour)) && !t.After(at)
}

// MarkQualified replays 017's referral_qualify: the velocity gate on
// theoretical success time (redeemed_at), not on the pass's clock.
func (m *memStore) MarkQualified(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateRedeemed {
		return false, errors.New("not redeemed")
	}
	to := StateQualified
	if m.velocityLimit > 0 {
		recent := 0
		for _, o := range m.refs {
			if o.ID == id || o.Referrer != r.Referrer {
				continue
			}
			switch o.State {
			case StateQualified, StateHeld, StateSaved, StateRewarded:
				if m.inWindowBefore(o, r.successAt()) {
					recent++
				}
			}
		}
		if recent >= m.velocityLimit {
			to = StateHeld
		}
	}
	r.State, r.qualifiedAt = to, m.dueNow
	return to == StateHeld, nil
}

// approve / reject replay 016's referral_approve / referral_reject (the
// operator script's functions).
func (m *memStore) approve(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State != StateHeld {
		return false
	}
	r.State, r.Reviewed = StateQualified, true
	return true
}

func (m *memStore) reject(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[id]
	if r.State == StateVoid {
		return false
	}
	r.State, r.voidReason, r.Reviewed = StateVoid, VoidRejected, true
	return true
}

func (m *memStore) HeldDigests(_ context.Context, limit int) ([]HeldDigest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.digestErr != nil {
		return nil, m.digestErr
	}
	now := m.dueNow
	byReferrer := map[string]bool{}
	recentlyMailed := map[string]bool{}
	for _, r := range m.refs {
		if !r.heldNotifiedAt.IsZero() && r.heldNotifiedAt.After(now.Add(-24*time.Hour)) {
			recentlyMailed[r.Referrer] = true
		}
	}
	var referrers []string
	for _, r := range m.sortedLocked() {
		if r.State == StateHeld && r.heldNotifiedAt.IsZero() && !recentlyMailed[r.Referrer] && !byReferrer[r.Referrer] {
			byReferrer[r.Referrer] = true
			referrers = append(referrers, r.Referrer)
		}
	}
	var out []HeldDigest
	hold := time.Duration(m.holdDays) * 24 * time.Hour
	for _, u := range capped(referrers, limit) {
		d := HeldDigest{Referrer: u, Limit: m.velocityLimit, WindowHours: m.velocityWindow}
		var held []*memRef
		for _, r := range m.sortedLocked() {
			if r.Referrer == u && r.State == StateHeld {
				held = append(held, r)
			}
		}
		for _, r := range m.sortedLocked() {
			if r.Referrer != u {
				continue
			}
			in := r.State == StateHeld
			switch r.State {
			case StateQualified, StateSaved, StateRewarded:
				for _, h := range held {
					if m.inWindowBefore(r, h.successAt()) {
						in = true
					}
				}
			}
			if in {
				d.Referrals = append(d.Referrals, HeldReferral{ID: r.ID, Referee: r.Referee, State: r.State,
					ClaimedAt: r.ClaimedAt, QualifiedAt: r.qualifiedAt, DueAt: r.successAt().Add(hold),
					InstallID: r.installID, Notified: !r.heldNotifiedAt.IsZero()})
			}
		}
		sort.SliceStable(d.Referrals, func(i, j int) bool { return d.Referrals[i].DueAt.Before(d.Referrals[j].DueAt) })
		out = append(out, d)
	}
	return out, nil
}

func (m *memStore) MarkHeldNotified(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.notifyErr != nil {
		return m.notifyErr
	}
	for _, id := range ids {
		if r := m.refs[id]; r.State == StateHeld && r.heldNotifiedAt.IsZero() {
			r.heldNotifiedAt = m.dueNow
		}
	}
	return nil
}

// sortedLocked is every referral, id order (callers hold mu).
func (m *memStore) sortedLocked() []*memRef {
	out := make([]*memRef, 0, len(m.refs))
	for _, r := range m.refs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
	r.State, r.lastChecked = StateSaved, time.Time{}
	return nil
}

func (m *memStore) Ledger(_ context.Context, userID string) (Ledger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	led := m.ledgers[userID]
	led.UserID = userID
	return led, nil
}

// ListSavers replays pgstore.go's rotation order (017): per referrer the
// earliest last look (never = zero = first), then the earliest qualified_at.
func (m *memStore) ListSavers(_ context.Context, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type saver struct {
		user               string
		checked, qualified time.Time
	}
	byUser := map[string]*saver{}
	var all []*saver
	for _, r := range m.sortedLocked() {
		if r.State != StateSaved {
			continue
		}
		sv := byUser[r.Referrer]
		if sv == nil {
			sv = &saver{user: r.Referrer, checked: r.lastChecked, qualified: r.qualifiedAt}
			byUser[r.Referrer] = sv
			all = append(all, sv)
			continue
		}
		if r.lastChecked.Before(sv.checked) {
			sv.checked = r.lastChecked
		}
		if r.qualifiedAt.Before(sv.qualified) {
			sv.qualified = r.qualifiedAt
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.checked.Equal(b.checked) {
			return a.checked.Before(b.checked)
		}
		if !a.qualified.Equal(b.qualified) {
			return a.qualified.Before(b.qualified)
		}
		return a.user < b.user
	})
	out := make([]string, 0, len(all))
	for _, sv := range all {
		out = append(out, sv.user)
	}
	return capped(out, limit), nil
}

func (m *memStore) MarkSaverChecked(_ context.Context, userID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.refs {
		if r.Referrer == userID && r.State == StateSaved {
			r.lastChecked = at
		}
	}
	return nil
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
	asked  map[string]int // gets per app user id
	gerr   error
}

type grantCall struct {
	user, ent string
	until     time.Time
}

func newFakeRC() *fakeRC { return &fakeRC{subs: map[string]*Subscriber{}, asked: map[string]int{}} }

func (f *fakeRC) set(userID string, sub *Subscriber) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[AppUserID(userID)] = sub
}

func (f *fakeRC) Subscriber(_ context.Context, appUserID string) (*Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	f.asked[appUserID]++
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
