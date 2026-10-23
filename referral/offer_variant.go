package referral

import (
	"context"
	"strings"
)

// The two experiment groups for the friend's free month (023,
// invite-friends.md ### 兑换码分组实验（月付组 / 年付组）): the Apple code
// the friend gets renews monthly (premium.month) or yearly
// (premium.yearly.3d) after the free month.
const (
	VariantMonth = "month"
	VariantYear  = "year"
)

// NormaliseVariant maps a stored offer_variant to "month" / "year"; null
// (a referral from before the experiment) and anything unknown is "month".
func NormaliseVariant(v *string) string {
	if v != nil && strings.TrimSpace(*v) == VariantYear {
		return VariantYear
	}
	return VariantMonth
}

// SplitByVariant splits the pool into the monthly group's batches (every
// offer_ref_name except the yearly one) and the yearly group's. Settings are
// copied to both. Without 023 (OfferRefYear empty) everything is monthly.
func (ps PoolStatus) SplitByVariant() (month, year PoolStatus) {
	month.Settings, year.Settings = ps.Settings, ps.Settings
	for _, b := range ps.Batches {
		if ps.Settings.OfferRefYear != "" && b.Name == ps.Settings.OfferRefYear {
			year.Batches = append(year.Batches, b)
		} else {
			month.Batches = append(month.Batches, b)
		}
	}
	return month, year
}

// UnknownOfferNames lists code_pool.offer_ref_name values that are neither the
// monthly nor the yearly group's configured name (023). They are handed out
// as monthly codes; listed so a mistyped yearly import is noticed. Empty
// without 023.
func (ps PoolStatus) UnknownOfferNames() []string {
	if ps.Settings.OfferRefYear == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range ps.Batches {
		if b.Name == ps.Settings.OfferRefYear || b.Name == ps.Settings.OfferRefMonth || seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		out = append(out, b.Name)
	}
	return out
}

type forceMonthKey struct{}

// WithForceMonth marks a claim from an app build that does not know the
// experiment groups (no offer_variant_aware): a new referral it makes always
// lands in the monthly group (PGStore.Claim sets referral.force_month for
// the transaction; 023's referral_offer_variant reads it).
func WithForceMonth(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceMonthKey{}, true)
}

// ForceMonth reports whether WithForceMonth marked ctx.
func ForceMonth(ctx context.Context) bool {
	v, _ := ctx.Value(forceMonthKey{}).(bool)
	return v
}
