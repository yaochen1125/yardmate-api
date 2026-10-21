package referral

import (
	"context"
	"log"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

// RewardsReader supplies the "rewards" block of GET /v1/referral/me — the
// gift-card balance kept by the rewards/ package (rewards/SPEC.md). Rewards
// returns any JSON-encodable value; this package never looks inside it.
type RewardsReader interface {
	Rewards(ctx context.Context, userID string) (any, error)
}

// SetRewards injects the /me rewards block. Call before serving; nil (the
// default) leaves the field out of /me.
func (s *Service) SetRewards(r RewardsReader) { s.rewards = r }

// ClaimBucket is /claim's per-device bucket (5 / 24 h). POST
// /v1/referral/payout (rewards/) spends from the same bucket; the settlement
// pass sweeps it.
func (s *Service) ClaimBucket() *ratelimit.Bucket { return s.claimBucket }

// attachRewards fills standing.Rewards. A failure leaves the field out (the
// invite page still draws; the app treats a missing block as zeros) and is
// logged without the user id.
func (s *Service) attachRewards(ctx context.Context, userID string, standing *Standing) {
	if s.rewards == nil {
		return
	}
	v, err := s.rewards.Rewards(ctx, userID)
	if err != nil {
		log.Printf("referral me: rewards unavailable (field omitted): %v", err)
		return
	}
	standing.Rewards = v
}
