package referral

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// memRestocks is memStore's copy of migration 021: the auto-restock settings,
// code_restocks with its two uniqueness rules (one unfinished row; one row
// per asc_batch_id) and the codes in code_pool (for on-conflict-do-nothing).
// Guarded by memStore.mu.
type memRestocks struct {
	settings  RestockSettings
	rows      []*memRestockRow
	nextID    int64
	codes     map[string]bool
	statusErr error
	markErr   error // one-shot: the next MarkRestockCreated fails
}

type memRestockRow struct {
	Restock
	ImportedCount int
	Error         string
	FinishedAt    time.Time
}

func (m *memStore) restockRow(id int64) *memRestockRow {
	for _, r := range m.rs.rows {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// restockRows is a snapshot for assertions.
func (m *memStore) restockRows() []memRestockRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memRestockRow, len(m.rs.rows))
	for i, r := range m.rs.rows {
		out[i] = *r
	}
	return out
}

// seedRestock inserts a row as an earlier process left it.
func (m *memStore) seedRestock(r Restock) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rs.nextID++
	r.ID = m.rs.nextID
	m.rs.rows = append(m.rs.rows, &memRestockRow{Restock: r})
	return r.ID
}

// seedPoolCodes puts codes into the fake code_pool (duplicates for imports).
func (m *memStore) seedPoolCodes(codes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rs.codes == nil {
		m.rs.codes = map[string]bool{}
	}
	for _, c := range codes {
		m.rs.codes[c] = true
	}
}

func openState(s string) bool { return s == RestockCreating || s == RestockCreated }

func (m *memStore) RestockStatus(_ context.Context, dayStart time.Time) (RestockStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rs.statusErr != nil {
		return RestockStatus{}, m.rs.statusErr
	}
	st := RestockStatus{Settings: m.rs.settings}
	for _, r := range m.rs.rows {
		if openState(r.State) && st.Open == nil {
			cp := r.Restock
			st.Open = &cp
		}
		if !r.StartedAt.Before(dayStart) {
			st.StartedToday++
		}
		if r.StartedAt.After(st.LastStarted) {
			st.LastStarted = r.StartedAt
		}
	}
	return st, nil
}

func (m *memStore) BeginRestock(_ context.Context, in RestockStart) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	today := 0
	for _, r := range m.rs.rows {
		if openState(r.State) {
			return 0, errRestockOpen // the partial unique index
		}
		if !r.StartedAt.Before(in.DayStart) {
			today++
		}
		if r.StartedAt.After(in.StartedAt.Add(-in.MinGap)) {
			return 0, errRestockLimited
		}
	}
	if today >= in.DailyMax {
		return 0, errRestockLimited
	}
	m.rs.nextID++
	m.rs.rows = append(m.rs.rows, &memRestockRow{Restock: Restock{
		ID: m.rs.nextID, Environment: in.Environment, Codes: in.Codes, ExpiresAt: in.ExpiresAt,
		State: RestockCreating, StartedAt: in.StartedAt,
	}})
	return m.rs.nextID, nil
}

func (m *memStore) RestockBatchesTaken(_ context.Context, ids []string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, id := range ids {
		for _, r := range m.rs.rows {
			if r.BatchID == id {
				out[id] = true
			}
		}
	}
	return out, nil
}

func (m *memStore) MarkRestockCreated(_ context.Context, id int64, batchID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rs.markErr; err != nil {
		m.rs.markErr = nil
		return false, err
	}
	for _, r := range m.rs.rows {
		if r.BatchID == batchID && r.ID != id {
			return false, errors.New("duplicate key value violates unique constraint code_restocks_asc_batch_id_key")
		}
	}
	r := m.restockRow(id)
	if r == nil || r.State != RestockCreating {
		return false, nil
	}
	r.State, r.BatchID = RestockCreated, batchID
	return true, nil
}

func (m *memStore) ImportRestock(_ context.Context, id int64, codes []string, ref string, expiresAt, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.restockRow(id)
	if r == nil || r.State != RestockCreated {
		return 0, errRestockNotCreated
	}
	if m.rs.codes == nil {
		m.rs.codes = map[string]bool{}
	}
	inserted := 0
	for _, c := range codes {
		if !m.rs.codes[c] {
			m.rs.codes[c] = true
			inserted++
		}
	}
	// PoolStatus groups by (offer_ref_name, expires_at).
	merged := false
	for i, b := range m.pool.Batches {
		if b.Name == ref && b.ExpiresAt.Equal(expiresAt) {
			m.pool.Batches[i].Free += inserted
			merged = true
		}
	}
	if !merged && inserted > 0 {
		m.pool.Batches = append(m.pool.Batches, PoolBatch{Name: ref, ExpiresAt: expiresAt, Free: inserted})
	}
	r.State, r.ImportedCount, r.FinishedAt = RestockImported, inserted, at
	return inserted, nil
}

func (m *memStore) FailRestock(_ context.Context, id int64, reason string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.restockRow(id); r != nil && openState(r.State) {
		r.State, r.Error, r.FinishedAt = RestockFailed, reason, at
	}
	return nil
}

// fakeASC is App Store Connect for the restock tests: batches it "has", the
// values of each, and knobs for every way a call can go.
type fakeASC struct {
	mu sync.Mutex
	at time.Time // createdDate of the next batch (the test's clock)

	batches []OneTimeCodeBatch
	values  map[string][]string
	nextID  int

	creates    []CreateCodesRequest
	lists      int
	valueCalls int

	// createErr is returned by every create; with createdAnyway the batch
	// is still made (the answer was lost, Apple acted).
	createErr     error
	createdAnyway bool
	listErr       error
	valuesErr     error
	// valuesShort hands out this many values fewer than the batch holds.
	valuesShort int
	// unrecognised: the CSV has every line, but none reads as a code.
	unrecognised bool
	// listHadDeadline: some list call came with a context deadline (the
	// restock must not put one shared budget over the whole list).
	listHadDeadline bool
}

func newFakeASC() *fakeASC { return &fakeASC{values: map[string][]string{}, nextID: 622600} }

// addBatch puts a batch on Apple's side with n generated codes.
func (f *fakeASC) addBatch(n int, expires, env string, created time.Time) string {
	f.nextID++
	id := fmt.Sprintf("%d", f.nextID)
	f.batches = append(f.batches, OneTimeCodeBatch{ID: id, NumberOfCodes: n, CreatedDate: created, ExpirationDate: expires, Environment: env, Active: true})
	codes := make([]string, n)
	for i := range codes {
		codes[i] = fmt.Sprintf("C%s%06d", id, i)
	}
	f.values[id] = codes
	return id
}

func (f *fakeASC) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates) + f.lists + f.valueCalls
}

func (f *fakeASC) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeASC) CreateOneTimeCodes(_ context.Context, req CreateCodesRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		if f.createdAnyway {
			f.addBatch(req.NumberOfCodes, req.ExpirationDate, req.Environment, f.at)
		}
		return "", f.createErr
	}
	return f.addBatch(req.NumberOfCodes, req.ExpirationDate, req.Environment, f.at), nil
}

func (f *fakeASC) ListOneTimeCodeBatches(ctx context.Context, offerID string) ([]OneTimeCodeBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if _, ok := ctx.Deadline(); ok {
		f.listHadDeadline = true
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]OneTimeCodeBatch(nil), f.batches...), nil
}

func (f *fakeASC) OneTimeCodeValues(_ context.Context, batchID string) ([]string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCalls++
	if f.valuesErr != nil {
		return nil, 0, f.valuesErr
	}
	v, ok := f.values[batchID]
	if !ok {
		return nil, 0, &ASCError{Op: "values", Status: 404}
	}
	if f.unrecognised {
		return nil, len(v), nil
	}
	if f.valuesShort > 0 && f.valuesShort < len(v) {
		v = v[:len(v)-f.valuesShort]
	}
	return append([]string(nil), v...), len(v), nil
}
