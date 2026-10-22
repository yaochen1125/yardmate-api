package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Auto-restock bookkeeping (migration 021_referral_auto_restock.sql): the
// referral_settings columns and code_restocks, one row per restock. "At most
// one unfinished" is the partial unique index code_restocks_one_open_uidx;
// asc_batch_id is unique, so one Apple batch is never claimed twice.

const pgUniqueViolation = "23505"

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func (p *PGStore) RestockStatus(ctx context.Context, dayStart time.Time) (RestockStatus, error) {
	var st RestockStatus
	s := &st.Settings
	if err := p.pool.QueryRow(ctx,
		`select auto_restock_enabled, restock_below, restock_batch_size, restock_daily_max,
		        restock_lead_days, coalesce(restock_environment, '')
		   from referral_settings where id = 1`).
		Scan(&s.Enabled, &s.Below, &s.BatchSize, &s.DailyMax, &s.LeadDays, &s.Environment); err != nil {
		if undefinedObject(err) {
			return RestockStatus{}, ErrRestockNotMigrated
		}
		return RestockStatus{}, fmt.Errorf("restock settings: %w", err)
	}
	var open Restock
	err := p.pool.QueryRow(ctx,
		`select id, environment, number_of_codes, expires_at, coalesce(asc_batch_id, ''), state, started_at
		   from code_restocks
		  where state in ('creating','created')
		  order by started_at limit 1`).
		Scan(&open.ID, &open.Environment, &open.Codes, &open.ExpiresAt, &open.BatchID, &open.State, &open.StartedAt)
	switch {
	case err == nil:
		st.Open = &open
	case errors.Is(err, pgx.ErrNoRows):
	case undefinedObject(err):
		return RestockStatus{}, ErrRestockNotMigrated
	default:
		return RestockStatus{}, fmt.Errorf("open restock: %w", err)
	}
	var last *time.Time
	if err := p.pool.QueryRow(ctx,
		`select count(*) filter (where started_at >= $1), max(started_at) from code_restocks`, dayStart).
		Scan(&st.StartedToday, &last); err != nil {
		return RestockStatus{}, fmt.Errorf("restock counts: %w", err)
	}
	if last != nil {
		st.LastStarted = *last
	}
	return st, nil
}

// BeginRestock is a conditional insert: the daily max and the minimum gap
// are re-checked in the same statement (each statement sees what committed
// before it), and two processes inserting at once collide on the partial
// unique index — the second gets 23505.
func (p *PGStore) BeginRestock(ctx context.Context, r RestockStart) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx,
		`insert into code_restocks (environment, number_of_codes, expires_at, state, started_at)
		 select $1::text, $2::int, $3::timestamptz, 'creating', $4::timestamptz
		  where (select count(*) from code_restocks where started_at >= $5::timestamptz) < $6::int
		    and not exists (select 1 from code_restocks
		                     where started_at > $4::timestamptz - make_interval(secs => $7::float8))
		 returning id`,
		r.Environment, r.Codes, r.ExpiresAt, r.StartedAt, r.DayStart, r.DailyMax, r.MinGap.Seconds()).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case uniqueViolation(err):
		return 0, errRestockOpen
	case errors.Is(err, pgx.ErrNoRows):
		var open bool
		if err := p.pool.QueryRow(ctx,
			`select exists (select 1 from code_restocks where state in ('creating','created'))`).Scan(&open); err == nil && open {
			return 0, errRestockOpen
		}
		return 0, errRestockLimited
	}
	return 0, err
}

func (p *PGStore) RestockBatchesTaken(ctx context.Context, batchIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(batchIDs) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `select asc_batch_id from code_restocks where asc_batch_id = any($1::text[])`, batchIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (p *PGStore) MarkRestockCreated(ctx context.Context, id int64, batchID string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`update code_restocks set state = 'created', asc_batch_id = $2
		  where id = $1 and state = 'creating'`, id, batchID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ImportRestock inserts like import-codes.sh (on conflict (code) do nothing:
// a code already in the pool — issued ones included — is left alone) and
// closes the restock in the same transaction.
func (p *PGStore) ImportRestock(ctx context.Context, id int64, codes []string, offerRefName string, expiresAt, at time.Time) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx,
		`insert into code_pool (code, offer_ref_name, expires_at)
		 select unnest($1::text[]), $2::text, $3::timestamptz
		 on conflict (code) do nothing`, codes, offerRefName, expiresAt)
	if err != nil {
		return 0, err
	}
	inserted := int(tag.RowsAffected())
	tag, err = tx.Exec(ctx,
		`update code_restocks set state = 'imported', imported_count = $2, finished_at = $3
		  where id = $1 and state = 'created'`, id, inserted, at)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != 1 {
		return 0, errRestockNotCreated
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (p *PGStore) FailRestock(ctx context.Context, id int64, reason string, at time.Time) error {
	_, err := p.pool.Exec(ctx,
		`update code_restocks set state = 'failed', error = left($2, 1000), finished_at = $3
		  where id = $1 and state in ('creating','created')`, id, reason, at)
	return err
}
