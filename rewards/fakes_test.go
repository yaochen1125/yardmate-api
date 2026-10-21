package rewards

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store. It keeps the same rules as pgstore.go /
// 015_referral_rewards.sql (event-id and transaction-id dedupe, void only
// pending, a refund with nothing pending leaves a tombstone that blocks the
// late charge, mature on the database clock, balance = available − requested
// − sent, one requested payout per user) so the handler tests exercise real
// bookkeeping.
type memStore struct {
	mu sync.Mutex

	clock   time.Time // the "database now()"
	users   map[string]bool
	refs    map[string]Referral // by referee
	ledger  []*ledgerRow
	voided  map[string]bool // reward_voided_txns
	payouts []*Payout
	nextID  int64

	// error knobs
	findErr, recordErr, voidErr, matureErr, balanceErr, payoutErr, reviewErr error

	// review knobs: referral rows beyond refs (by id) for ReviewStats —
	// state, void reason, claim device.
	reviewRefs []reviewRef
}

type reviewRef struct {
	id                    int64
	referrer              string
	state, reason, device string
}

type ledgerRow struct {
	Credit
	State      string
	VoidReason string
}

func newMemStore(now time.Time) *memStore {
	return &memStore{clock: now, users: map[string]bool{}, refs: map[string]Referral{}, voided: map[string]bool{}}
}

func (m *memStore) addReferral(referrer, referee, state string, claimedAt time.Time) Referral {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.users[referrer], m.users[referee] = true, true
	r := Referral{ID: m.nextID, Referrer: referrer, Referee: referee, State: state, ClaimedAt: claimedAt}
	m.refs[referee] = r
	return r
}

// credit seeds a ledger row directly.
func (m *memStore) credit(referrer, eventID, txn string, cents int64, state string, availableAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[referrer] = true
	m.ledger = append(m.ledger, &ledgerRow{Credit: Credit{
		Referrer: referrer, EventID: eventID, TransactionID: txn, CreditCents: cents, AvailableAt: availableAt,
	}, State: state})
}

// payout seeds a payout row directly.
func (m *memStore) payout(userID, state string, requestedAt, sentAt time.Time) *Payout {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	p := &Payout{ID: m.nextID, UserID: userID, AmountCents: PayoutAmountCents, Email: "seed@example.com",
		State: state, RequestedAt: requestedAt, SentAt: sentAt}
	m.payouts = append(m.payouts, p)
	return p
}

func (m *memStore) rows() []ledgerRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledgerRow, 0, len(m.ledger))
	for _, r := range m.ledger {
		out = append(out, *r)
	}
	return out
}

func (m *memStore) ReferralByReferee(_ context.Context, referee string) (Referral, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findErr != nil {
		return Referral{}, false, m.findErr
	}
	r, ok := m.refs[referee]
	return r, ok, nil
}

func (m *memStore) RecordCredit(_ context.Context, c Credit) (CreditResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recordErr != nil {
		return 0, m.recordErr
	}
	for _, r := range m.ledger {
		if r.EventID == c.EventID || r.TransactionID == c.TransactionID {
			return CreditDuplicate, nil
		}
	}
	if m.voided[c.TransactionID] {
		return CreditVoided, nil
	}
	m.ledger = append(m.ledger, &ledgerRow{Credit: c, State: LedgerPending})
	return CreditInserted, nil
}

// tombstoned reports whether a refund left txn in reward_voided_txns.
func (m *memStore) tombstoned(txn string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.voided[txn]
}

func (m *memStore) VoidPending(_ context.Context, txn string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.voidErr != nil {
		return 0, m.voidErr
	}
	var n int64
	for _, r := range m.ledger {
		if r.TransactionID == txn && r.State == LedgerPending {
			r.State, r.VoidReason = LedgerVoided, VoidRefunded
			n++
		}
	}
	if n == 0 {
		m.voided[txn] = true
	}
	return n, nil
}

func (m *memStore) MatureDue(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.matureErr != nil {
		return 0, m.matureErr
	}
	var n int64
	for _, r := range m.ledger {
		if r.State == LedgerPending && !r.AvailableAt.After(m.clock) {
			r.State = LedgerAvailable
			n++
		}
	}
	return n, nil
}

func (m *memStore) sumsLocked(userID string) (available, pending, spent int64) {
	for _, r := range m.ledger {
		if r.Referrer != userID {
			continue
		}
		switch r.State {
		case LedgerAvailable:
			available += r.CreditCents
		case LedgerPending:
			pending += r.CreditCents
		}
	}
	for _, p := range m.payouts {
		if p.UserID == userID && (p.State == PayoutRequested || p.State == PayoutSent) {
			spent += p.AmountCents
		}
	}
	return
}

func (m *memStore) Balance(_ context.Context, userID string) (Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.balanceErr != nil {
		return Balance{}, m.balanceErr
	}
	var b Balance
	b.AvailableLedgerCents, b.PendingCents, b.SpentCents = m.sumsLocked(userID)
	var mine []*Payout
	for _, p := range m.payouts {
		if p.UserID == userID {
			mine = append(mine, p)
		}
	}
	sort.Slice(mine, func(i, j int) bool {
		if !mine[i].RequestedAt.Equal(mine[j].RequestedAt) {
			return mine[i].RequestedAt.After(mine[j].RequestedAt)
		}
		return mine[i].ID > mine[j].ID
	})
	if len(mine) > 0 {
		p := *mine[0]
		b.Latest = &p
	}
	return b, nil
}

func (m *memStore) RequestPayout(_ context.Context, userID, email string, amount, threshold int64, yearStart time.Time) (Payout, YearTotals, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.payoutErr != nil {
		return Payout{}, YearTotals{}, m.payoutErr
	}
	if !m.users[userID] {
		return Payout{}, YearTotals{}, ErrBelowThreshold
	}
	for _, p := range m.payouts {
		if p.UserID == userID && p.State == PayoutRequested {
			return Payout{}, YearTotals{}, ErrPayoutInProgress
		}
	}
	available, _, spent := m.sumsLocked(userID)
	if available-spent < threshold {
		return Payout{}, YearTotals{}, ErrBelowThreshold
	}
	m.nextID++
	p := &Payout{ID: m.nextID, UserID: userID, AmountCents: amount, Email: email, State: PayoutRequested, RequestedAt: m.clock}
	m.payouts = append(m.payouts, p)
	var y YearTotals
	for _, q := range m.payouts {
		if q.UserID != userID {
			continue
		}
		switch {
		case q.State == PayoutSent && !q.SentAt.Before(yearStart):
			y.SentCents += q.AmountCents
		case q.State == PayoutRequested:
			y.RequestedCents += q.AmountCents
		}
	}
	return *p, y, nil
}

// ReviewStats replays the PG query over reviewRefs (and the ledger).
func (m *memStore) ReviewStats(_ context.Context, userID string) (Review, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rv := Review{VoidByReason: map[string]int{}}
	if m.reviewErr != nil {
		return rv, m.reviewErr
	}
	devices := map[string]bool{}
	live := map[int64]bool{}
	for _, r := range m.reviewRefs {
		if r.referrer != userID {
			continue
		}
		switch {
		case r.state == "void":
			reason := r.reason
			if reason == "" {
				reason = "unknown"
			}
			rv.VoidByReason[reason]++
			continue
		case r.state == "held":
			rv.Held++
		}
		rv.Friends++
		live[r.id] = true
		if r.device == "" {
			rv.NoDevice++
		} else {
			devices[r.device] = true
		}
	}
	rv.ClaimDevices = len(devices)
	paid := map[int64]bool{}
	for _, l := range m.ledger {
		if l.Referrer != userID {
			continue
		}
		if l.State == LedgerVoided {
			if l.VoidReason == "" || l.VoidReason == VoidRefunded {
				rv.Refunds++
			}
			continue
		}
		if live[l.ReferralID] {
			paid[l.ReferralID] = true
		}
	}
	rv.FriendsPaid = len(paid)
	return rv, nil
}

// fakeMailer records Alert calls.
type fakeMailer struct {
	mu       sync.Mutex
	subjects []string
	bodies   []string
	err      error
}

func (f *fakeMailer) Alert(subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)
	return f.err
}

const (
	device   = "4E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"
	referrer = "11111111-2222-3333-4444-555555555555"
	referee  = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	stranger = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	secret   = "whsec-test-0123456789"
)

var errBoom = errors.New("boom")

func authAs(user string) Authenticator {
	return func(*http.Request) (string, error) { return user, nil }
}

func badAuth(*http.Request) (string, error) { return "", errors.New("bad token") }

// newTestSvc builds a fully configured service on a memStore whose clock and
// the service clock agree, with the mail delivered synchronously.
func newTestSvc(now time.Time) (*Service, *memStore, *fakeMailer) {
	store := newMemStore(now)
	mailer := &fakeMailer{}
	svc := New(store, authAs(referrer), Options{WebhookSecret: secret, Mailer: mailer})
	svc.now = func() time.Time { return now }
	svc.async = func(f func()) { f() }
	return svc, store, mailer
}
