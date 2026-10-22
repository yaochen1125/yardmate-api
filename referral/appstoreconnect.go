package referral

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// App Store Connect API — the three calls auto-restock (restock.go) makes on
// the referral offer: generate a batch of one-time offer codes, list the
// offer's batches (to reconcile a request whose outcome is unknown), and fetch
// one batch's code values (CSV). Used 2026-10-19 to create the first two
// batches by hand, read-only verified 2026-10-22.
//
//	POST /v1/subscriptionOfferCodeOneTimeUseCodes              → 201, data.id = batch id
//	GET  /v1/subscriptionOfferCodes/{offer}/oneTimeUseCodes    → data[] (batches)
//	GET  /v1/subscriptionOfferCodeOneTimeUseCodes/{batch}/values (Accept: text/csv)
//	                                                          → one code per line, no header
//
// Auth is an ES256 JWT (kid = ASC_KEY_ID, iss = ASC_ISSUER_ID,
// aud = appstoreconnect-v1, exp ≤ 20 min) signed with the API key's .p8 —
// with the standard library only, a fresh token per call. Neither the key,
// the token nor a code value ever reaches a log line or an error.

const (
	ascDefaultBase = "https://api.appstoreconnect.apple.com"
	ascAudience    = "appstoreconnect-v1"
	// ascTokenTTL: Apple refuses tokens that live longer than 20 minutes.
	ascTokenTTL = 15 * time.Minute
	// DefaultASCTimeout bounds each App Store Connect call (docs: 每次调苹果单独限时 60 秒).
	DefaultASCTimeout = 60 * time.Second
	ascMaxJSON        = 4 << 20
	// ascMaxCSV: 25,000 codes (Apple's batch cap) × a few dozen bytes, with room.
	ascMaxCSV = 8 << 20
	// ascListPages caps how many pages of 200 batches the reconcile reads.
	ascListPages = 10
	ascUserAgent = "yardmate-api referral-restock"
)

// Offer code environments (App Store Connect's own spelling, and the values
// of ASC_OFFER_CODE_ENVIRONMENT / referral_settings.restock_environment /
// code_restocks.environment).
const (
	EnvProduction = "PRODUCTION"
	EnvSandbox    = "SANDBOX"
)

// NormaliseOfferEnvironment returns PRODUCTION / SANDBOX for a value that is
// one of them (case and surrounding space ignored), else "".
func NormaliseOfferEnvironment(s string) string {
	switch v := strings.ToUpper(strings.TrimSpace(s)); v {
	case EnvProduction, EnvSandbox:
		return v
	}
	return ""
}

// OneTimeCodeBatch is one batch of the offer's one-time codes as App Store
// Connect lists it.
type OneTimeCodeBatch struct {
	ID             string
	NumberOfCodes  int
	CreatedDate    time.Time // Apple gives it to the second
	ExpirationDate string    // YYYY-MM-DD
	Environment    string    // PRODUCTION | SANDBOX
	Active         bool
}

// CreateCodesRequest is one batch to generate.
type CreateCodesRequest struct {
	OfferID        string
	NumberOfCodes  int
	ExpirationDate string // YYYY-MM-DD
	Environment    string // PRODUCTION | SANDBOX
}

// OfferCodeAPI is the slice of App Store Connect auto-restock uses. An
// interface so the restock tests run against a fake; ASCClient is the real
// one. Errors are *ASCError (Uncertain says whether Apple may have acted).
type OfferCodeAPI interface {
	CreateOneTimeCodes(ctx context.Context, req CreateCodesRequest) (batchID string, err error)
	ListOneTimeCodeBatches(ctx context.Context, offerID string) ([]OneTimeCodeBatch, error)
	OneTimeCodeValues(ctx context.Context, batchID string) ([]string, error)
}

// ASCError is a failed App Store Connect call.
//
// Uncertain is what decides a generate request's fate (docs: 结果不确定时先对账):
// true when the request went out and the answer is unknown — a timeout or a
// dropped connection after the request was written, a 5xx, or a 2xx whose body
// does not name the batch. Apple may have generated the codes, so the restock
// stays creating and is reconciled against the batch list. False when Apple
// plainly said no (401 / 403 / any other 4xx) or the request never left
// (DNS failure, connection refused, TLS handshake) — the restock fails at once.
type ASCError struct {
	Op        string // create / list / values
	Status    int    // HTTP status; 0 when no response
	Uncertain bool
	// Detail is Apple's own error code / title / detail (never secret), cut short.
	Detail string
	Err    error
}

func (e *ASCError) Error() string {
	var b strings.Builder
	b.WriteString("appstoreconnect " + e.Op)
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

func (e *ASCError) Unwrap() error { return e.Err }

// Kind names the failure for the one-mail-a-day dedup: http_<status> for an
// answer, unreachable for a request that never got out.
func (e *ASCError) Kind() string {
	if e.Status != 0 {
		return fmt.Sprintf("http_%d", e.Status)
	}
	if e.Uncertain {
		return "no_answer"
	}
	return "unreachable"
}

// ascOutcome reads a generate error: uncertain (keep creating, reconcile) or
// not, and the dedup kind. An error that is not an *ASCError (a fake's, or a
// bug) is uncertain — the safe side: a missed failure costs 30 minutes, a
// missed success costs a duplicate batch.
func ascOutcome(err error) (uncertain bool, kind string) {
	var e *ASCError
	if errors.As(err, &e) {
		return e.Uncertain, e.Kind()
	}
	return true, "unknown"
}

// ASCClient calls App Store Connect with one API key. The key lives only in
// this struct.
type ASCClient struct {
	keyID, issuerID string
	key             *ecdsa.PrivateKey
	base            string
	http            *http.Client
	now             func() time.Time
}

// NewASCClient parses the .p8 (PKCS#8 PEM, P-256) and returns a client. The
// error never contains key material.
func NewASCClient(keyID, issuerID string, p8 []byte) (*ASCClient, error) {
	keyID, issuerID = strings.TrimSpace(keyID), strings.TrimSpace(issuerID)
	if keyID == "" || issuerID == "" {
		return nil, errors.New("appstoreconnect: key id and issuer id are required")
	}
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("appstoreconnect: private key is not a PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("appstoreconnect: private key is not a PKCS#8 .p8")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("appstoreconnect: private key is not a P-256 EC key")
	}
	return &ASCClient{
		keyID: keyID, issuerID: issuerID, key: key,
		base: ascDefaultBase,
		http: &http.Client{
			Timeout: DefaultASCTimeout,
			// Never follow a redirect with the token on it; a 3xx is an
			// answer like any other non-2xx.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}, nil
}

// SetBaseURL overrides https://api.appstoreconnect.apple.com (tests).
func (c *ASCClient) SetBaseURL(u string) { c.base = strings.TrimRight(u, "/") }

// SetTimeout overrides the per-call HTTP timeout (tests).
func (c *ASCClient) SetTimeout(d time.Duration) { c.http.Timeout = d }

// token signs a fresh ES256 JWT: header {alg, kid, typ}, claims {iss, iat,
// exp, aud}, signature r||s (32 bytes each, not DER).
func (c *ASCClient) token() (string, error) {
	now := c.now()
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": c.keyID, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": c.issuerID,
		"iat": now.Unix(),
		"exp": now.Add(ascTokenTTL).Unix(),
		"aud": ascAudience,
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, digest[:])
	if err != nil {
		return "", errors.New("appstoreconnect: sign jwt failed")
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}

// do sends one request and reads at most limit bytes of the answer. A
// transport error is Uncertain exactly when the request had been written in
// full (httptrace WroteRequest without error) — Apple may have it.
func (c *ASCClient) do(ctx context.Context, op, method, target string, body []byte, accept string, limit int64) ([]byte, int, error) {
	if c == nil {
		return nil, 0, &ASCError{Op: op, Err: errors.New("no client")}
	}
	tok, err := c.token()
	if err != nil {
		return nil, 0, &ASCError{Op: op, Err: err}
	}
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	})
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, 0, &ASCError{Op: op, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", ascUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &ASCError{Op: op, Uncertain: wrote.Load(), Err: stripURLError(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		// The status line came back; the body broke off. Apple acted (or
		// refused) — we just cannot read which batch.
		return nil, resp.StatusCode, &ASCError{Op: op, Status: resp.StatusCode, Uncertain: true, Err: stripURLError(err)}
	}
	if int64(len(raw)) > limit {
		return nil, resp.StatusCode, &ASCError{Op: op, Status: resp.StatusCode, Uncertain: true, Detail: "response too large"}
	}
	return raw, resp.StatusCode, nil
}

// stripURLError drops net/http's "Post \"https://…\": " wrapper: the path is
// not secret here, but the reason is what matters (kept %w-able, so a
// timeout is still recognisable).
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// statusError turns a non-2xx answer into an *ASCError: 5xx is uncertain,
// everything else (4xx, an unexpected 3xx / 1xx) is Apple saying no.
func statusError(op string, status int, raw []byte) *ASCError {
	return &ASCError{Op: op, Status: status, Uncertain: status >= 500, Detail: appleErrorDetail(raw)}
}

// appleErrorDetail pulls the first {code, title, detail} out of App Store
// Connect's error document, cut to 300 characters; "" when there is none.
func appleErrorDetail(raw []byte) string {
	var doc struct {
		Errors []struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Errors) == 0 {
		return ""
	}
	e := doc.Errors[0]
	var parts []string
	for _, p := range []string{e.Code, e.Title, e.Detail} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	out := strings.Join(parts, " — ")
	if r := []rune(out); len(r) > 300 {
		out = string(r[:300]) + "…"
	}
	return out
}

// CreateOneTimeCodes asks Apple to generate one batch of one-time codes for
// the offer and returns the batch id.
func (c *ASCClient) CreateOneTimeCodes(ctx context.Context, in CreateCodesRequest) (string, error) {
	const op = "create"
	type ref struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	var body struct {
		Data struct {
			Type       string `json:"type"`
			Attributes struct {
				NumberOfCodes  int    `json:"numberOfCodes"`
				ExpirationDate string `json:"expirationDate"`
				Environment    string `json:"environment"`
			} `json:"attributes"`
			Relationships struct {
				OfferCode struct {
					Data ref `json:"data"`
				} `json:"offerCode"`
			} `json:"relationships"`
		} `json:"data"`
	}
	body.Data.Type = "subscriptionOfferCodeOneTimeUseCodes"
	body.Data.Attributes.NumberOfCodes = in.NumberOfCodes
	body.Data.Attributes.ExpirationDate = in.ExpirationDate
	body.Data.Attributes.Environment = in.Environment
	body.Data.Relationships.OfferCode.Data = ref{Type: "subscriptionOfferCodes", ID: in.OfferID}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", &ASCError{Op: op, Err: err}
	}
	raw, status, err := c.do(ctx, op, http.MethodPost, c.base+"/v1/subscriptionOfferCodeOneTimeUseCodes", payload, "application/json", ascMaxJSON)
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", statusError(op, status, raw)
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || strings.TrimSpace(out.Data.ID) == "" {
		// Apple said yes but we cannot read the batch: it exists, unnamed.
		return "", &ASCError{Op: op, Status: status, Uncertain: true, Detail: "no batch id in the response"}
	}
	return strings.TrimSpace(out.Data.ID), nil
}

// ListOneTimeCodeBatches lists the offer's one-time code batches, following
// links.next (same host only) for up to ascListPages pages of 200.
func (c *ASCClient) ListOneTimeCodeBatches(ctx context.Context, offerID string) ([]OneTimeCodeBatch, error) {
	const op = "list"
	next := c.base + "/v1/subscriptionOfferCodes/" + url.PathEscape(offerID) + "/oneTimeUseCodes?limit=200"
	var out []OneTimeCodeBatch
	for page := 0; next != "" && page < ascListPages; page++ {
		raw, status, err := c.do(ctx, op, http.MethodGet, next, nil, "application/json", ascMaxJSON)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, statusError(op, status, raw)
		}
		var doc struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					NumberOfCodes  int    `json:"numberOfCodes"`
					CreatedDate    string `json:"createdDate"`
					ExpirationDate string `json:"expirationDate"`
					Active         bool   `json:"active"`
					Environment    string `json:"environment"`
				} `json:"attributes"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, &ASCError{Op: op, Status: status, Detail: "undecodable batch list"}
		}
		for _, d := range doc.Data {
			b := OneTimeCodeBatch{
				ID:             d.ID,
				NumberOfCodes:  d.Attributes.NumberOfCodes,
				ExpirationDate: d.Attributes.ExpirationDate,
				Environment:    d.Attributes.Environment,
				Active:         d.Attributes.Active,
			}
			if t, err := time.Parse(time.RFC3339, d.Attributes.CreatedDate); err == nil {
				b.CreatedDate = t
			}
			out = append(out, b)
		}
		next = ""
		if n := doc.Links.Next; n != "" && strings.HasPrefix(n, c.base+"/") {
			next = n
		}
	}
	return out, nil
}

// OneTimeCodeValues fetches the batch's code values. Apple answers CSV with
// one code per line and no header; like import-codes.sh, every line yields
// the first token that looks like a code (^[A-Z0-9]{6,20}$), anything else is
// dropped, and duplicates are folded.
func (c *ASCClient) OneTimeCodeValues(ctx context.Context, batchID string) ([]string, error) {
	const op = "values"
	raw, status, err := c.do(ctx, op, http.MethodGet,
		c.base+"/v1/subscriptionOfferCodeOneTimeUseCodes/"+url.PathEscape(batchID)+"/values", nil, "text/csv", ascMaxCSV)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(op, status, raw)
	}
	return ParseOfferCodeCSV(raw), nil
}

var offerCodeShape = regexp.MustCompile(`^[A-Z0-9]{6,20}$`)

// ParseOfferCodeCSV picks the codes out of an App Store Connect export, the
// way deploy/supabase/import-codes.sh does (BOM, quotes, CRLF tolerated).
func ParseOfferCodeCSV(raw []byte) []string {
	text := strings.TrimPrefix(string(raw), "\uFEFF")
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.NewReplacer("\r", "", `"`, "").Replace(line)
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' || r == ' ' }) {
			if offerCodeShape.MatchString(tok) {
				if !seen[tok] {
					seen[tok] = true
					out = append(out, tok)
				}
				break
			}
		}
	}
	return out
}
