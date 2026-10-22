package referral

import (
	"context"
	"errors"
	"log"
	"time"
)

// ChargeVerdict is what RedeemForCharge made of a friend's charge.
type ChargeVerdict int

const (
	// ChargeRedeemed: the charged product's subscription began after the
	// claim and step ① ran for the referral — it is redeemed now (or was
	// already past claimed: the settlement loop got there first).
	ChargeRedeemed ChargeVerdict = iota
	// ChargeNotCounted: the charged product's subscription began before the
	// claim (or is not the friend's own store purchase) — e.g. a second
	// account that claimed a code and then pulled an older subscription over
	// with Restore Purchases. Its charges never count.
	ChargeNotCounted
	// ChargeVoided: step ① voided the referral (gate 3, duplicate_apple_id).
	ChargeVoided
)

// ErrChargeNotReady: RevenueCat does not show the charged product on the
// friend's customer yet (or this server cannot ask it). The webhook answers
// 5xx so RevenueCat redelivers; by then the settlement loop has usually
// redeemed the referral.
var ErrChargeNotReady = errors.New("referral: subscription not visible on revenuecat yet")

// RedeemForCharge runs settlement step ① now, for one claimed referral whose
// friend was just charged for productID — the rewards webhook's RENEWAL that
// can beat the 10-minute loop: the friend redeemed the free month and
// upgraded to a yearly plan (an upgrade charges at once, as a RENEWAL of the
// new product) before ① got to them (invite-friends.md ### 朋友在免费月里换套餐).
//
// The charged product's record decides whether its subscription is new: its
// original_purchase_date is the chain's start, unchanged by the upgrade.
// Started before the claim → ChargeNotCounted, the rule that keeps a restored
// old subscription from paying the referrer. Started after → exactly ①:
// StartedAfter + MarkRedeemed (gate 3 included). The record not there yet,
// or RevenueCat failing → ErrChargeNotReady (wrapped).
func (s *Service) RedeemForCharge(ctx context.Context, referralID int64, referee string, claimedAt time.Time, productID string) (ChargeVerdict, error) {
	if s.store == nil || s.rc == nil {
		return 0, ErrChargeNotReady
	}
	sub, err := s.rc.Subscriber(ctx, AppUserID(referee))
	if err != nil {
		return 0, errors.Join(ErrChargeNotReady, err)
	}
	rec, found := sub.Subscriptions[productID]
	if !found {
		return 0, ErrChargeNotReady
	}
	st, has := rec.start()
	if !has {
		return 0, ErrChargeNotReady
	}
	if !rec.isStorePurchase() || !st.After(claimedAt) {
		return ChargeNotCounted, nil
	}
	key, start, ok := sub.StartedAfter(claimedAt)
	if !ok { // cannot happen: rec itself qualifies
		return 0, ErrChargeNotReady
	}
	voided, err := s.store.MarkRedeemed(ctx, referralID, start, sub.SubscriptionKey(key))
	if err != nil {
		return 0, err
	}
	if voided {
		log.Printf("referral: charge before settlement: voided (duplicate_apple_id)")
		return ChargeVoided, nil
	}
	log.Printf("referral: charge before settlement: redeemed on the spot")
	return ChargeRedeemed, nil
}
