package referral

import (
	"context"
	"log"
	"time"
)

// Settlement — the server's own 10-minute loop. Nothing here is driven by a
// request; /redeemed only nudges it to run sooner.
//
//	① claimed   → redeemed   RevenueCat shows the referee's own store subscription
//	                         starting after the claim (offer code, or just paying).
//	② redeemed  → qualified  hold_days served and not refunded; else void.
//	③ qualified → rewarded   referrer not covered by a store subscription: grant
//	                         30 days on top of max(now, ledger, RC promo expiry).
//	            → saved      referrer covered (paid / trial / intro / their own
//	                         friend's month): park it.
//	④ saved     → rewarded   referrer's store coverage ended: grant ALL saved
//	                         months in one call (n × 30 days).
//
// No cap anywhere: the 11th qualified friend adds 30 days like the first.
const (
	defaultSettleInterval     = 10 * time.Minute
	defaultSettleStartupDelay = time.Minute
	settlePassTimeout         = 8 * time.Minute
	settleBatch               = 200
	nudgeDebounce             = time.Minute

	// RewardMonth is what one successful referral is worth on RevenueCat.
	RewardMonth = 30 * 24 * time.Hour
)

// Report counts what one pass did. Numbers only — never ids.
type Report struct {
	Expired   int64
	Redeemed  int
	Voided    int
	Qualified int
	Granted   int // referrals rewarded straight away (③)
	Saved     int // referrals parked (③)
	Flushed   int // saved referrals paid out (④)
	Usable    int // free pool codes beyond the issue margin (pool.go)
	Alerted   int // pool alert emails sent this pass (0 or 1)
	Errors    int
}

// Nudge asks the loop to run a pass soon (debounced). Non-blocking.
func (s *Service) Nudge() {
	if s == nil {
		return
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// StartSettler launches the loop on a background goroutine until ctx ends.
// A no-op (logged) when the feature is not fully configured.
func (s *Service) StartSettler(ctx context.Context) {
	if !s.Configured() {
		log.Printf("referral settle: disabled (missing %v)", s.Missing())
		return
	}
	log.Printf("referral settle: enabled interval=%s", s.interval)
	go s.loop(ctx)
}

func (s *Service) loop(ctx context.Context) {
	first := time.NewTimer(s.startupDelay)
	defer first.Stop()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var lastNudged time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			s.runPass(ctx)
		case <-ticker.C:
			s.runPass(ctx)
		case <-s.nudge:
			if s.now().Sub(lastNudged) < nudgeDebounce {
				continue // the regular tick picks it up
			}
			lastNudged = s.now()
			s.runPass(ctx)
		}
	}
}

func (s *Service) runPass(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, settlePassTimeout)
	defer cancel()
	rep := s.Settle(pctx)
	if rep.Errors > 0 || rep.Redeemed+rep.Voided+rep.Qualified+rep.Granted+rep.Saved+rep.Flushed > 0 || rep.Expired > 0 || rep.Alerted > 0 {
		log.Printf("referral settle: expired=%d redeemed=%d voided=%d qualified=%d granted=%d saved=%d flushed=%d usable=%d alerted=%d errors=%d",
			rep.Expired, rep.Redeemed, rep.Voided, rep.Qualified, rep.Granted, rep.Saved, rep.Flushed, rep.Usable, rep.Alerted, rep.Errors)
	}
	// The per-device buckets are this package's own; sweep them here rather
	// than threading them through ratelimit.Limiter.
	now := s.now()
	s.claimBucket.Sweep(now)
	s.readBucket.Sweep(now)
}

// Settle runs one pass. Exported for tests; the loop calls it on its timer.
// Every step is independent: an error in one row is counted and skipped, so a
// single bad RevenueCat answer never stalls the rest.
func (s *Service) Settle(ctx context.Context) Report {
	var rep Report
	now := s.now()

	if n, err := s.store.ExpireCodes(ctx); err != nil {
		log.Printf("referral settle: expire codes: %v", err)
		rep.Errors++
	} else {
		rep.Expired = n
	}
	// Stock check right after retiring the dead codes, so the numbers are
	// about what /claim can actually hand out.
	s.watchPool(ctx, now, &rep)
	hold, err := s.store.HoldDays(ctx)
	if err != nil || hold < 0 {
		log.Printf("referral settle: hold_days unavailable (%v); using 7", err)
		hold = 7
		rep.Errors++
	}

	// One RevenueCat lookup per customer per pass.
	cache := map[string]*Subscriber{}
	lookup := func(userID string) (*Subscriber, bool) {
		if sub, ok := cache[userID]; ok {
			return sub, true
		}
		sub, err := s.rc.Subscriber(ctx, AppUserID(userID))
		if err != nil {
			log.Printf("referral settle: revenuecat lookup: %v", err)
			rep.Errors++
			return nil, false
		}
		cache[userID] = sub
		return sub, true
	}

	// ① claimed → redeemed
	if claimed, err := s.store.ListClaimed(ctx, settleBatch); err != nil {
		log.Printf("referral settle: list claimed: %v", err)
		rep.Errors++
	} else {
		for _, ref := range claimed {
			sub, ok := lookup(ref.Referee)
			if !ok {
				continue
			}
			_, start, started := sub.StartedAfter(ref.ClaimedAt)
			if !started {
				continue
			}
			if err := s.store.MarkRedeemed(ctx, ref.ID, start); err != nil {
				log.Printf("referral settle: mark redeemed: %v", err)
				rep.Errors++
				continue
			}
			rep.Redeemed++
		}
	}

	// ② redeemed → qualified / void
	if due, err := s.store.ListDue(ctx, time.Duration(hold)*24*time.Hour, settleBatch); err != nil {
		log.Printf("referral settle: list due: %v", err)
		rep.Errors++
	} else {
		for _, ref := range due {
			sub, ok := lookup(ref.Referee)
			if !ok {
				continue
			}
			key, _, started := sub.StartedAfter(ref.ClaimedAt)
			reason := ""
			switch {
			case !started:
				reason = "no_subscription"
			case sub.Refunded(key):
				reason = "refunded"
			}
			if reason != "" {
				if err := s.store.MarkVoid(ctx, ref.ID, reason); err != nil {
					log.Printf("referral settle: mark void: %v", err)
					rep.Errors++
					continue
				}
				rep.Voided++
				continue
			}
			if err := s.store.MarkQualified(ctx, ref.ID); err != nil {
				log.Printf("referral settle: mark qualified: %v", err)
				rep.Errors++
				continue
			}
			rep.Qualified++
		}
	}

	// ③ qualified → rewarded / saved
	if qualified, err := s.store.ListQualified(ctx, settleBatch); err != nil {
		log.Printf("referral settle: list qualified: %v", err)
		rep.Errors++
	} else {
		for _, ref := range qualified {
			sub, ok := lookup(ref.Referrer)
			if !ok {
				continue
			}
			if sub.Covered(now) {
				if err := s.store.MarkSaved(ctx, ref.ID); err != nil {
					log.Printf("referral settle: mark saved: %v", err)
					rep.Errors++
					continue
				}
				rep.Saved++
				continue
			}
			if err := s.grant(ctx, ref.Referrer, []int64{ref.ID}, sub, now); err != nil {
				rep.Errors++
				continue
			}
			rep.Granted++
		}
	}

	// ④ saved → rewarded once the referrer's store coverage has ended
	if savers, err := s.store.ListSavers(ctx, settleBatch); err != nil {
		log.Printf("referral settle: list savers: %v", err)
		rep.Errors++
	} else {
		for _, userID := range savers {
			sub, ok := lookup(userID)
			if !ok || sub.Covered(now) {
				continue
			}
			ids, err := s.store.SavedIDs(ctx, userID)
			if err != nil {
				log.Printf("referral settle: saved ids: %v", err)
				rep.Errors++
				continue
			}
			if len(ids) == 0 {
				continue
			}
			if err := s.grant(ctx, userID, ids, sub, now); err != nil {
				rep.Errors++
				continue
			}
			rep.Flushed += len(ids)
		}
	}
	return rep
}

// grant pays len(ids) months to userID: RevenueCat first, then the ledger.
// The expiry stacks on the latest of now, what our ledger says we last
// granted, and what RevenueCat currently shows (so a grant made by hand in
// the dashboard is extended, never cut short). If RevenueCat accepts but the
// ledger write fails, the next pass grants again — the user gets an extra
// month rather than losing one, and the failure is logged loudly.
func (s *Service) grant(ctx context.Context, userID string, ids []int64, sub *Subscriber, now time.Time) error {
	led, err := s.store.Ledger(ctx, userID)
	if err != nil {
		log.Printf("referral settle: ledger: %v", err)
		return err
	}
	base := now
	if led.PromoUntil.After(base) {
		base = led.PromoUntil
	}
	if p := sub.PromoUntil(); p.After(base) {
		base = p
	}
	until := base.Add(time.Duration(len(ids)) * RewardMonth)
	if err := s.rc.GrantPromotional(ctx, AppUserID(userID), s.entitlement, until); err != nil {
		log.Printf("referral settle: revenuecat grant (%d month(s)): %v", len(ids), err)
		return err
	}
	if err := s.store.RecordGranted(ctx, userID, ids, until); err != nil {
		log.Printf("referral settle: GRANTED ON REVENUECAT BUT NOT RECORDED (%d month(s), %d referral(s)); will re-grant next pass: %v", len(ids), len(ids), err)
		return err
	}
	return nil
}
