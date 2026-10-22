package referral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Subscribers is the slice of the RevenueCat REST API v1 this package uses.
// An interface so the settlement tests run against a fake.
type Subscribers interface {
	// Subscriber fetches the customer's current info. NOTE: RevenueCat CREATES
	// the customer when the id does not exist (201), so callers must only pass
	// ids of real, authenticated users.
	Subscriber(ctx context.Context, appUserID string) (*Subscriber, error)
	// GrantPromotional grants entitlement to the customer until the given time
	// (POST .../entitlements/{entitlement}/promotional {"end_time_ms"}). RC
	// treats a request whose end is within 2 h of an existing promotional
	// expiry as a duplicate; ours always extend by ≥ 30 days.
	GrantPromotional(ctx context.Context, appUserID, entitlement string, until time.Time) error
}

// Subscriber is the part of RevenueCat's customer-info object we read.
type Subscriber struct {
	Subscriptions map[string]Subscription `json:"subscriptions"`
	Entitlements  map[string]Entitlement  `json:"entitlements"`
}

// Subscription is one entry of subscriber.subscriptions, keyed by product id.
// Dates are ISO 8601 UTC strings (or null) in the wire format.
type Subscription struct {
	Store                string     `json:"store"`          // app_store | play_store | promotional | ...
	PeriodType           string     `json:"period_type"`    // normal | trial | intro
	OwnershipType        string     `json:"ownership_type"` // PURCHASED | FAMILY_SHARED
	PurchaseDate         *time.Time `json:"purchase_date"`
	OriginalPurchaseDate *time.Time `json:"original_purchase_date"`
	ExpiresDate          *time.Time `json:"expires_date"`
	RefundedAt           *time.Time `json:"refunded_at"`
	// UnsubscribeDetectedAt is auto-renew turned off. Informational only: a
	// cancelled-but-not-refunded month still counts (docs ## 特殊情况).
	UnsubscribeDetectedAt *time.Time `json:"unsubscribe_detected_at"`
	IsSandbox             bool       `json:"is_sandbox"`
	// Price is kept raw and read leniently (free): RevenueCat documents no
	// schema for it on v1 subscriptions, and a shape we did not expect must
	// not fail the whole customer-info decode.
	Price json.RawMessage `json:"price"`
}

// Entitlement is one entry of subscriber.entitlements, keyed by identifier.
type Entitlement struct {
	ProductIdentifier string     `json:"product_identifier"`
	PurchaseDate      *time.Time `json:"purchase_date"`
	ExpiresDate       *time.Time `json:"expires_date"`
}

const (
	storePromotional    = "promotional"
	ownershipFamily     = "FAMILY_SHARED"
	revenueCatBase      = "https://api.revenuecat.com/v1"
	revenueCatTimeout   = 10 * time.Second
	revenueCatMaxBody   = 1 << 20
	revenueCatUserAgent = "yardmate-api referral"
)

// AppUserID maps a Supabase auth user id (the JWT sub, lowercase) to the
// RevenueCat app user id the iOS app logs in with. The app passes
// `session.userID.uuidString` to Purchases.logIn, and Swift's UUID.uuidString
// is UPPERCASE — RevenueCat ids are case-sensitive, so the server must match
// that exactly or it would be talking to a different (empty) customer.
func AppUserID(userID string) string {
	return strings.ToUpper(strings.TrimSpace(userID))
}

// isStorePurchase is a subscription bought from a store by this account —
// not a RevenueCat promotional grant (ours), not one shared into the account
// through Family Sharing.
func (s Subscription) isStorePurchase() bool {
	return s.Store != storePromotional && s.OwnershipType != ownershipFamily
}

// start is when the subscription chain began: original_purchase_date, or
// purchase_date for the odd entry without one.
func (s Subscription) start() (time.Time, bool) {
	switch {
	case s.OriginalPurchaseDate != nil:
		return *s.OriginalPurchaseDate, true
	case s.PurchaseDate != nil:
		return *s.PurchaseDate, true
	}
	return time.Time{}, false
}

// HasPaidHistory reports whether this customer has EVER had a store
// subscription of their own (any product, any state, trial included). That is
// what "has subscribed before" means for the friend's month: promotional
// grants and family-shared subscriptions do not count.
func (s *Subscriber) HasPaidHistory() bool {
	if s == nil {
		return false
	}
	for _, sub := range s.Subscriptions {
		if sub.isStorePurchase() {
			return true
		}
	}
	return false
}

// free reports a record nobody paid for: a free trial (period_type trial —
// the friend's Referral Friend Month is a FREE_TRIAL offer code) or a record
// whose price RevenueCat gives as 0. Anything else counts as paid, so a
// refund on a record we cannot place still voids (holdVerdict).
func (s Subscription) free() bool {
	if strings.EqualFold(s.PeriodType, "trial") {
		return true
	}
	var p struct {
		Amount *json.Number `json:"amount"`
	}
	if len(s.Price) == 0 || json.Unmarshal(s.Price, &p) != nil || p.Amount == nil {
		return false
	}
	f, err := p.Amount.Float64()
	return err == nil && f == 0
}

// StartedAfter finds a store subscription of this customer's own that began
// after t (the claim time) — the referee's redemption, or them simply
// subscribing after putting the code in, which also counts. Returns the
// product key and the start time. An upgrade leaves two records with the same
// start (the free month and the new plan); the smaller product id wins the
// tie, so the answer does not depend on map order.
func (s *Subscriber) StartedAfter(t time.Time) (key string, start time.Time, ok bool) {
	if s == nil {
		return "", time.Time{}, false
	}
	for k, sub := range s.Subscriptions {
		if !sub.isStorePurchase() {
			continue
		}
		st, has := sub.start()
		if !has || !st.After(t) {
			continue
		}
		if !ok || st.Before(start) || (st.Equal(start) && k < key) {
			key, start, ok = k, st, true
		}
	}
	return key, start, ok
}

// SubscriptionKey is anti-abuse gate 3's identity for the subscription under
// key (a product id from StartedAfter): "<store>:<original purchase time>",
// "" when there is no such entry or no start time.
//
// The product docs ask for Apple's original_transaction_id, but RevenueCat's
// REST API does not return it (v1 subscriptions carry store_transaction_id,
// which is the LATEST transaction and changes on every renewal; v2 has no
// original id either — only webhooks do, and a subscription transferred to a
// second account never sends that account an INITIAL_PURCHASE). What the REST
// record does carry is original_purchase_date: the purchase time of the
// original transaction, the same on every renewal, product change within the
// group, re-login or RevenueCat transfer — so one Apple ID's subscription
// chain shows the same key on every account it lands on. The product id is
// deliberately left out (an upgrade keeps the chain but changes the key it
// sits under). Two DIFFERENT people starting a subscription in the same
// second would collide; at YardMate's volume that is rare, and the cost is one
// referral voided as duplicate_apple_id that an operator can see.
func (s *Subscriber) SubscriptionKey(key string) string {
	if s == nil {
		return ""
	}
	sub, found := s.Subscriptions[key]
	if !found {
		return ""
	}
	st, ok := sub.start()
	if !ok {
		return ""
	}
	store := sub.Store
	if store == "" {
		store = "unknown"
	}
	return store + ":" + st.UTC().Format(time.RFC3339Nano)
}

// Refunded reports whether the subscription chain under key (a product id
// from StartedAfter) has a paid record that was refunded. All the customer's
// store records with the same SubscriptionKey are one chain: after an upgrade
// inside the free month RevenueCat holds the free month AND the new plan,
// both with the same start, and the refund lands on the plan the friend paid
// for — whichever record StartedAfter picked. A free record (the $0 free
// month, marked refunded / upgraded or not) never counts.
func (s *Subscriber) Refunded(key string) bool {
	if s == nil {
		return false
	}
	chain := s.SubscriptionKey(key)
	for k, sub := range s.Subscriptions {
		if sub.RefundedAt == nil || sub.free() {
			continue
		}
		if k == key || (chain != "" && sub.isStorePurchase() && s.SubscriptionKey(k) == chain) {
			return true
		}
	}
	return false
}

// Covered reports whether the customer currently holds Premium through a
// store subscription (their own purchase — paid, trial, intro, the friend's
// free month — or family-shared). While covered, a promotional grant would run
// concurrently with it and buy nothing, so earned months are saved instead.
// Promotional grants themselves do not count: they are what we hand out.
func (s *Subscriber) Covered(now time.Time) bool {
	if s == nil {
		return false
	}
	for _, sub := range s.Subscriptions {
		if sub.Store == storePromotional || sub.RefundedAt != nil {
			continue
		}
		if sub.ExpiresDate == nil || sub.ExpiresDate.After(now) {
			return true
		}
	}
	return false
}

// PromoUntil is the latest expiry among the customer's promotional grants, or
// zero. Used as a stacking base alongside our own ledger so a grant made by
// hand in the RevenueCat dashboard is extended rather than cut short.
func (s *Subscriber) PromoUntil() time.Time {
	var latest time.Time
	if s == nil {
		return latest
	}
	for _, sub := range s.Subscriptions {
		if sub.Store != storePromotional || sub.ExpiresDate == nil {
			continue
		}
		if sub.ExpiresDate.After(latest) {
			latest = *sub.ExpiresDate
		}
	}
	return latest
}

// Client talks to RevenueCat REST API v1 with a v1 secret key
// (REVENUECAT_SECRET_KEY). The key is read from the vault, lives only in this
// struct, and is never logged.
type Client struct {
	base string
	key  string
	http *http.Client
}

// NewClient returns a client, or nil when the key is blank (the feature then
// answers not_configured rather than failing every call).
func NewClient(secretKey string) *Client {
	secretKey = strings.TrimSpace(secretKey)
	if secretKey == "" {
		return nil
	}
	return &Client{base: revenueCatBase, key: secretKey, http: &http.Client{Timeout: revenueCatTimeout}}
}

// SetBase overrides the API base URL (tests).
func (c *Client) SetBase(base string) { c.base = strings.TrimRight(base, "/") }

func (c *Client) Subscriber(ctx context.Context, appUserID string) (*Subscriber, error) {
	if c == nil {
		return nil, errors.New("revenuecat: no client")
	}
	raw, status, err := c.do(ctx, "get subscriber", http.MethodGet, "/subscribers/"+url.PathEscape(appUserID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("revenuecat: get subscriber status %d", status)
	}
	var out struct {
		Subscriber Subscriber `json:"subscriber"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("revenuecat: decode subscriber: %w", err)
	}
	return &out.Subscriber, nil
}

func (c *Client) GrantPromotional(ctx context.Context, appUserID, entitlement string, until time.Time) error {
	if c == nil {
		return errors.New("revenuecat: no client")
	}
	if entitlement == "" {
		return errors.New("revenuecat: empty entitlement")
	}
	body, err := json.Marshal(map[string]any{"end_time_ms": until.UnixMilli()})
	if err != nil {
		return err
	}
	path := "/subscribers/" + url.PathEscape(appUserID) + "/entitlements/" + url.PathEscape(entitlement) + "/promotional"
	_, status, err := c.do(ctx, "grant promotional", http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("revenuecat: grant promotional status %d", status)
	}
	return nil
}

// do issues one authenticated request; op names it in errors ("get
// subscriber"). Only the status and body come back. The key and the app user
// id never appear in errors or logs: the path carries the user id, and
// net/http's *url.Error prints the whole URL ("Get
// \"https://…/subscribers/<APP_USER_ID>\": …"), so errors keep op + the
// underlying cause only (still wrapped: errors.Is(err,
// context.DeadlineExceeded) works).
func (c *Client) do(ctx context.Context, op, method, path string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("revenuecat: %s: cannot build request", op)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", revenueCatUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("revenuecat: %s: %w", op, withoutURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, revenueCatMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("revenuecat: %s: read body: %w", op, withoutURL(err))
	}
	return raw, resp.StatusCode, nil
}

// withoutURL unwraps net/http's *url.Error to its cause, dropping the URL
// (and with it any id in the path).
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
