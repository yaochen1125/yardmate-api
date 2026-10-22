package rewards

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const (
	// webhookBodyCap: a RevenueCat event is a few KB; 64 KB is generous.
	webhookBodyCap = 64 << 10
	// webhookTimeout covers a RevenueCat lookup (10 s client timeout) for a
	// RENEWAL that beats settlement (redeemCharge) plus the store calls;
	// RevenueCat itself waits 60 s.
	webhookTimeout = 20 * time.Second

	// fallbackTakehome is the payout rate when the event lacks
	// tax_percentage / commission_percentage (or they make no sense): Apple's
	// standard 30% commission with no tax — below a US small-business charge
	// (0.85), so the referrer is rather underpaid than overpaid (above it only
	// in high-VAT storefronts; rewards.md ### 记账来源).
	fallbackTakehome = "0.70"
)

// shareRate is the referrer's cut of what Apple pays out: 30%.
var shareRate = big.NewRat(30, 100)

// rcEvent is the part of a RevenueCat webhook event this package reads
// (rewards.md ## 数据契约). price and the percentages stay json.Number so the
// money arithmetic below is done on the exact decimal RevenueCat sent. The
// deprecated takehome_percentage is not read: it is before VAT.
type rcEvent struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Environment string `json:"environment"`
	AppUserID   string `json:"app_user_id"`
	ProductID   string `json:"product_id"`
	// NewProductID is PRODUCT_CHANGE's target product (logged only).
	NewProductID          string      `json:"new_product_id"`
	PeriodType            string      `json:"period_type"`
	Price                 json.Number `json:"price"`
	TaxPercentage         json.Number `json:"tax_percentage"`
	CommissionPercentage  json.Number `json:"commission_percentage"`
	TransactionID         string      `json:"transaction_id"`
	OriginalTransactionID string      `json:"original_transaction_id"`
	PurchasedAtMs         int64       `json:"purchased_at_ms"`
	CancelReason          string      `json:"cancel_reason"`
	// IsFamilyShare: the purchase is a family organiser's, shared to this
	// account — the friend did not pay, so it never counts.
	IsFamilyShare bool `json:"is_family_share"`
}

type webhookBody struct {
	Event      rcEvent `json:"event"`
	APIVersion string  `json:"api_version"`
}

// Outcomes of one event, returned in the 200 body (visible in RevenueCat's
// webhook log) and used by the tests. None of them carries an id.
const (
	outIgnoredEnvironment = "ignored_environment" // SANDBOX etc.
	outIgnoredType        = "ignored_type"        // not a purchase / renewal / refund
	outNotCounted         = "not_counted"         // TRIAL / PROMOTIONAL / $0 / family share
	outNotReferee         = "not_referee"         // app_user_id is nobody's friend
	outNotRedeemed        = "not_redeemed"        // their referral is void / not yet redeemed
	outRecorded           = "recorded"
	outDuplicate          = "duplicate" // event id already in the ledger
	outVoided             = "voided"
	outNothingToVoid      = "nothing_to_void"
	outProductChange      = "product_change" // a referee's PRODUCT_CHANGE: logged, never credited
)

// errBadEvent: a counted event without the fields we key on. 400, not 500 —
// a redelivery will not grow them.
var errBadEvent = errors.New("bad_event")

// errNotSettled: a referee's RENEWAL arrived while the referral is still
// claimed and could not be redeemed on the spot (redeemCharge). 503 so
// RevenueCat redelivers; the settlement loop redeems it within minutes.
var errNotSettled = errors.New("not_settled")

// HandleWebhook is POST /v1/revenuecat/webhook. RevenueCat retries anything
// but a 2xx, so: our own failures are 500 (retry), a request we will never
// accept is 4xx, and every event we look at and decline is a 200.
func (s *Service) HandleWebhook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.secret == "" {
			log.Printf("revenuecat webhook: REVENUECAT_WEBHOOK_SECRET not set; refusing (503)")
			writeError(w, http.StatusServiceUnavailable, "not_configured")
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if s.store == nil {
			log.Printf("revenuecat webhook: no database (SUPABASE_DB_URL); refusing (503)")
			writeError(w, http.StatusServiceUnavailable, "not_configured")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, webhookBodyCap)
		var body webhookBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
		defer cancel()
		out, err := s.handleEvent(ctx, body.Event)
		switch {
		case errors.Is(err, errNotSettled):
			log.Printf("revenuecat webhook: %s for a referral not settled yet (503, RevenueCat will retry): %v", safeType(body.Event.Type), err)
			writeError(w, http.StatusServiceUnavailable, "not_settled")
			return
		case errors.Is(err, errBadEvent):
			log.Printf("revenuecat webhook: %s event missing id / transaction_id / sane price", safeType(body.Event.Type))
			writeError(w, http.StatusBadRequest, "bad_event")
			return
		case err != nil:
			log.Printf("revenuecat webhook: %s: store error (500, RevenueCat will retry): %v", safeType(body.Event.Type), err)
			writeError(w, http.StatusInternalServerError, "store_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": out})
	}
}

// authorized compares the Authorization header with REVENUECAT_WEBHOOK_SECRET
// in constant time. RevenueCat sends back exactly what was typed into its
// dashboard, so a "Bearer " prefix on either side is ignored: the secret can be
// configured with or without it and the dashboard filled either way.
func (s *Service) authorized(header string) bool {
	got, want := stripBearer(header), stripBearer(s.secret)
	if got == "" || want == "" {
		return false
	}
	// Hash first so the comparison is constant-time in the length too.
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func stripBearer(v string) string {
	v = strings.TrimSpace(v)
	const prefix = "bearer "
	if len(v) >= len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		v = strings.TrimSpace(v[len(prefix):])
	}
	return v
}

// handleEvent applies the rules of rewards.md ## 实现方式 to one event.
func (s *Service) handleEvent(ctx context.Context, ev rcEvent) (string, error) {
	if ev.Environment != "PRODUCTION" && !(s.allowSandbox && ev.Environment == "SANDBOX") {
		return outIgnoredEnvironment, nil
	}
	switch ev.Type {
	case "INITIAL_PURCHASE", "RENEWAL":
		return s.credit(ctx, ev)
	case "PRODUCT_CHANGE":
		return s.productChange(ctx, ev)
	case "CANCELLATION":
		if ev.CancelReason != "CUSTOMER_SUPPORT" { // only an Apple refund
			return outIgnoredType, nil
		}
		return s.refund(ctx, ev)
	default:
		// TRANSFER included: afterwards the subscription's events carry the
		// other account's app_user_id and simply stop matching.
		return outIgnoredType, nil
	}
}

func (s *Service) credit(ctx context.Context, ev rcEvent) (string, error) {
	if ev.PeriodType != "NORMAL" && ev.PeriodType != "INTRO" {
		return outNotCounted, nil // TRIAL, PROMOTIONAL, PREPAID, …
	}
	if ev.IsFamilyShare {
		return outNotCounted, nil
	}
	price, ok := decimal(ev.Price)
	if !ok {
		// A real charge without a price would silently cost the referrer this
		// credit, so leave a trace (type + period only, never an id).
		log.Printf("revenuecat webhook: %s %s event has price null / missing; not counted", safeType(ev.Type), ev.PeriodType)
		return outNotCounted, nil
	}
	if price.Sign() <= 0 {
		return outNotCounted, nil
	}

	// RevenueCat's app user id is the Supabase id upper-cased (iOS passes
	// UUID.uuidString); our rows hold it lower-case. Anonymous ids
	// ($RCAnonymousID:…) are nobody's friend.
	referee := strings.ToLower(strings.TrimSpace(ev.AppUserID))
	if !isUUID(referee) {
		return outNotReferee, nil
	}
	ref, found, err := s.store.ReferralByReferee(ctx, referee)
	if err != nil {
		return "", err
	}
	if !found {
		return outNotReferee, nil
	}
	purchased := s.now()
	if ev.PurchasedAtMs > 0 {
		purchased = time.UnixMilli(ev.PurchasedAtMs).UTC()
	}
	if !eligible(ref, ev.Type, purchased) {
		if !chargeBeforeSettlement(ref, ev.Type, purchased) {
			return outNotRedeemed, nil
		}
		if ev.ID == "" || ev.TransactionID == "" {
			return "", errBadEvent
		}
		if out, err := s.redeemCharge(ctx, ref, ev); out != "" || err != nil {
			return out, err
		}
		// Redeemed: read the referral again — the settlement loop may have
		// moved it on, or voided it, meanwhile.
		if ref, found, err = s.store.ReferralByReferee(ctx, referee); err != nil {
			return "", err
		}
		if !found || !eligible(ref, ev.Type, purchased) {
			return outNotRedeemed, nil
		}
	}
	if ev.ID == "" || ev.TransactionID == "" {
		return "", errBadEvent
	}

	takehome := takehomeOf(ev)
	priceCents, okP := roundCents(new(big.Rat).Mul(price, big.NewRat(100, 1)))
	creditCents, okC := CreditCents(price, takehome)
	if !okP || !okC {
		return "", errBadEvent
	}
	if creditCents <= 0 {
		return outNotCounted, nil // a few cents of price rounds to nothing
	}

	res, err := s.store.RecordCredit(ctx, Credit{
		ReferralID:            ref.ID,
		Referrer:              ref.Referrer,
		Referee:               ref.Referee,
		EventID:               ev.ID,
		EventType:             ev.Type,
		TransactionID:         ev.TransactionID,
		OriginalTransactionID: ev.OriginalTransactionID,
		ProductID:             ev.ProductID,
		PeriodType:            ev.PeriodType,
		PriceUSDCents:         priceCents,
		Takehome:              takehome.FloatString(4),
		CreditCents:           creditCents,
		PurchasedAt:           purchased,
		AvailableAt:           purchased.Add(RefundWindow),
	})
	if err != nil {
		return "", err
	}
	switch res {
	case CreditDuplicate:
		return outDuplicate, nil
	case CreditVoided:
		log.Printf("revenuecat webhook: %s arrived after its refund; not credited", ev.Type)
		return outVoided, nil
	case CreditReferralVoid:
		log.Printf("revenuecat webhook: %s for a referral voided meanwhile; not credited", ev.Type)
		return outNotRedeemed, nil
	}
	log.Printf("revenuecat webhook: recorded %s %s credit_cents=%d", ev.Type, ev.PeriodType, creditCents)
	return outRecorded, nil
}

// refund voids the pending credit of the refunded transaction. Matched on
// transaction_id alone: only friends' charges are in the ledger, and a
// refund that arrives after a TRANSFER still finds its row. Available credits
// are not clawed back (rewards.md ## 特殊情况). When nothing is pending the
// store remembers the transaction id, so a charge event delivered after its
// refund (its first delivery failed and RevenueCat retried later) is voided
// on arrival instead of maturing in 90 days.
func (s *Service) refund(ctx context.Context, ev rcEvent) (string, error) {
	if ev.TransactionID == "" {
		return outNothingToVoid, nil
	}
	n, err := s.store.VoidPending(ctx, ev.TransactionID)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return outNothingToVoid, nil
	}
	log.Printf("revenuecat webhook: refund voided %d pending credit(s)", n)
	return outVoided, nil
}

// eligible: the friend has redeemed (013 referrals.state redeemed / qualified /
// saved / rewarded). A referral still 'claimed' counts only for a first
// purchase (INITIAL_PURCHASE) after the claim: that is exactly what the
// settlement loop's step ① turns into 'redeemed' within ten minutes, and a
// friend paying straight away would otherwise lose their first charge to the
// race. A RENEWAL while still claimed is a subscription older than the claim
// — e.g. a second account that claimed a code and then pulled an existing
// subscription over with Restore Purchases — and counting it would pay the
// referrer for every renewal of a code that was never redeemed.
func eligible(ref Referral, evType string, purchased time.Time) bool {
	switch ref.State {
	case refRedeemed, refQualified, refHeld, refSaved, refRewarded:
		return true
	case refClaimed:
		return evType == "INITIAL_PURCHASE" && purchased.After(ref.ClaimedAt)
	default: // void
		return false
	}
}

// chargeBeforeSettlement: a RENEWAL charged after the claim while the
// referral is still claimed — the friend upgraded out of the free month
// before settlement ① redeemed them. Whether its subscription began after
// the claim is RevenueCat's to say (redeemCharge).
func chargeBeforeSettlement(ref Referral, evType string, purchased time.Time) bool {
	return ref.State == refClaimed && evType == "RENEWAL" && purchased.After(ref.ClaimedAt)
}

// redeemCharge asks the referral package to run settlement ① for this
// referral now (Redeemer). "" + nil: redeemed, go on and credit. Otherwise
// the outcome to answer, or errNotSettled for a 503.
func (s *Service) redeemCharge(ctx context.Context, ref Referral, ev rcEvent) (string, error) {
	if s.redeemer == nil {
		return "", fmt.Errorf("%w: no referral settlement wired", errNotSettled)
	}
	v, err := s.redeemer.RedeemForCharge(ctx, ref.ID, ref.Referee, ref.ClaimedAt, ev.ProductID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNotSettled, err)
	}
	switch v {
	case ChargeRedeemed:
		return "", nil
	case ChargeVoided:
		log.Printf("revenuecat webhook: %s: referral voided on the spot (duplicate_apple_id); not credited", ev.Type)
		return outNotRedeemed, nil
	default: // ChargeNotCounted: the subscription began before the claim
		return outNotRedeemed, nil
	}
}

// takehomeOf is the share of the price Apple pays out:
// 1 − tax_percentage − commission_percentage. Either one missing, tax outside
// [0, 1) (0 is real: no US sales tax), commission outside (0, 1) (Apple always
// takes a cut; 0 means the estimate is missing), or nothing left →
// fallbackTakehome, logged every time.
func takehomeOf(ev rcEvent) *big.Rat {
	fallback := func(why string) *big.Rat {
		log.Printf("revenuecat webhook: %s %s: %s; payout rate %s", safeType(ev.Type), ev.PeriodType, why, fallbackTakehome)
		r, _ := decimal(fallbackTakehome)
		return r
	}
	tax, okT := decimal(ev.TaxPercentage)
	com, okC := decimal(ev.CommissionPercentage)
	if !okT || !okC {
		return fallback("tax_percentage / commission_percentage missing")
	}
	one := big.NewRat(1, 1)
	if tax.Sign() < 0 || tax.Cmp(one) >= 0 || com.Sign() <= 0 || com.Cmp(one) >= 0 {
		return fallback("tax_percentage / commission_percentage out of range")
	}
	r := new(big.Rat).Sub(one, tax)
	r.Sub(r, com)
	if r.Sign() <= 0 {
		return fallback("tax + commission leave nothing")
	}
	return r
}

// productChange logs a referee's PRODUCT_CHANGE with a running count, for
// reconciling against the RENEWAL that should follow an upgrade (some
// upgrades are reported to deliver only the PRODUCT_CHANGE). Never credited:
// its price is 0 and the change itself charges nothing.
func (s *Service) productChange(ctx context.Context, ev rcEvent) (string, error) {
	referee := strings.ToLower(strings.TrimSpace(ev.AppUserID))
	if !isUUID(referee) {
		return outNotReferee, nil
	}
	ref, found, err := s.store.ReferralByReferee(ctx, referee)
	if err != nil {
		return "", err
	}
	if !found {
		return outNotReferee, nil
	}
	n := s.productChanges.Add(1)
	log.Printf("revenuecat webhook: referee PRODUCT_CHANGE #%d since start (referral %s): %s -> %s",
		n, safeType(ref.State), safeProduct(ev.ProductID), safeProduct(ev.NewProductID))
	return outProductChange, nil
}

// CreditCents is round(price × takehome × 0.30 × 100), half away from zero,
// on exact decimals. ok=false when the result does not fit the ledger column.
func CreditCents(price, takehome *big.Rat) (int64, bool) {
	r := new(big.Rat).Mul(price, takehome)
	r.Mul(r, shareRate)
	r.Mul(r, big.NewRat(100, 1))
	return roundCents(r)
}

// roundCents rounds a non-negative amount of cents to an int, half up; false
// when negative or beyond int32 (the column type).
func roundCents(r *big.Rat) (int64, bool) {
	if r.Sign() < 0 {
		return 0, false
	}
	n := new(big.Rat).Add(r, big.NewRat(1, 2))
	q := new(big.Int).Quo(n.Num(), n.Denom()) // positive → truncation = floor
	if !q.IsInt64() || q.Int64() > math.MaxInt32 {
		return 0, false
	}
	return q.Int64(), true
}

// decimal parses a JSON number exactly. "" (absent / null) → false.
func decimal(n json.Number) (*big.Rat, bool) {
	if n == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(string(n))
	return r, ok
}

// safeProduct keeps a product id in a log line short and printable.
func safeProduct(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) > 64 {
		id = id[:64]
	}
	for _, c := range id {
		if !(c == '_' || c == '.' || c == '-' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return "?"
		}
	}
	return id
}

// safeType keeps a log line's event type to something short and printable.
func safeType(t string) string {
	if len(t) > 32 {
		t = t[:32]
	}
	for _, c := range t {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return "?"
		}
	}
	return t
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}

// isUUID accepts RFC 4122 canonical form, either hex case. Same 12 lines as
// referral/ratelimit/proxy (not worth a package).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
