package rewards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

const (
	// payoutBodyCap: an email address and nothing else.
	payoutBodyCap = 4 << 10
	// maxEmailLen is RFC 5321's path limit.
	maxEmailLen = 254
)

// Configured reports whether POST /v1/referral/payout can work.
func (s *Service) Configured() bool { return s != nil && s.store != nil && s.auth != nil }

type payoutRequest struct {
	Email string `json:"email"`
}

// HandlePayout is POST /v1/referral/payout {email}: $100 of redeemable
// balance becomes one Amazon.com gift-card request (rewards.md ## 数据契约).
//
// Order: headers + JWT (as /claim) → body → email shape → the shared
// per-device try (/claim's 5 / 24 h bucket; a typo'd address does not spend
// one) → one transaction that refuses payout_in_progress / below_threshold or
// inserts the request → the operator mail, off the request path.
func (s *Service) HandlePayout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.caller(w, r)
		if !ok {
			return
		}
		var req payoutRequest
		r.Body = http.MaxBytesReader(w, r.Body, payoutBodyCap)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}
		email, ok := validEmail(req.Email)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_email")
			return
		}
		now := s.now().UTC()
		if allowed, retry := s.bucket.Allow(r.Header.Get("X-Device-Install-Id"), now); !allowed {
			ratelimit.Write429(w, retry, "rate_limit_device")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
		defer cancel()
		yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		po, year, err := s.store.RequestPayout(ctx, userID, email, PayoutAmountCents, ThresholdCents, yearStart)
		switch {
		case errors.Is(err, ErrPayoutInProgress):
			writeError(w, http.StatusConflict, "payout_in_progress")
			return
		case errors.Is(err, ErrBelowThreshold):
			writeError(w, http.StatusConflict, "below_threshold")
			return
		case err != nil:
			log.Printf("referral payout: store error: %v", err)
			writeError(w, http.StatusBadGateway, "store_failed")
			return
		}
		s.notifyOperator(po, year, now.Year())
		writeJSON(w, http.StatusCreated, map[string]any{"payout": viewOf(&po)})
	}
}

// caller: the same two headers, configuration and bearer-token checks as the
// referral endpoints, in the same order and with the same error codes.
func (s *Service) caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !isUUID(r.Header.Get("X-Device-Install-Id")) {
		writeError(w, http.StatusBadRequest, "missing_device_id")
		return "", false
	}
	if strings.TrimSpace(r.Header.Get("X-App-Version")) == "" {
		writeError(w, http.StatusBadRequest, "missing_app_version")
		return "", false
	}
	if !s.Configured() {
		writeError(w, http.StatusServiceUnavailable, "not_configured")
		return "", false
	}
	userID, err := s.auth(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return userID, true
}

// validEmail: net/mail must parse it as a bare address (no display name, no
// angle brackets, nothing it would rewrite) with an @. The server only checks
// the shape (rewards.md); whether it can receive mail is the user's word.
func validEmail(raw string) (string, bool) {
	e := strings.TrimSpace(raw)
	if e == "" || len(e) > maxEmailLen || !strings.Contains(e, "@") {
		return "", false
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Name != "" || addr.Address != e {
		return "", false
	}
	return addr.Address, true
}

// notifyOperator mails the request to the operator inbox (same account as
// /v1/feedback). Fire-and-forget: the row is already committed and
// deploy/referral-payout.sh list shows it either way, so a mail outage must
// not fail or slow the 201.
func (s *Service) notifyOperator(po Payout, year YearTotals, calendarYear int) {
	subject, body := composePayoutMail(po, year, calendarYear)
	if s.mailer == nil {
		log.Printf("referral payout #%d requested; operator mail disabled (FEEDBACK_SMTP_* unset) — deploy/referral-payout.sh list", po.ID)
		return
	}
	m := s.mailer
	s.async(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("referral payout #%d: operator mail panic recovered: %v", po.ID, r)
			}
		}()
		if err := m.Alert(subject, body); err != nil {
			log.Printf("referral payout #%d: operator mail failed (request stored; deploy/referral-payout.sh list): %v", po.ID, err)
		}
	})
}

// composePayoutMail renders the operator notice. Subject is plain ASCII
// (Alert does no RFC 2047 encoding); the user's email address only ever goes
// in the body.
func composePayoutMail(po Payout, year YearTotals, calendarYear int) (subject, body string) {
	subject = fmt.Sprintf("[YardMate] Referral gift card request #%d", po.ID)
	var b strings.Builder
	b.WriteString("A referrer asked for a gift card.\n\n")
	fmt.Fprintf(&b, "Request:      #%d\n", po.ID)
	fmt.Fprintf(&b, "User id:      %s\n", po.UserID)
	fmt.Fprintf(&b, "Send to:      %s\n", po.Email)
	fmt.Fprintf(&b, "Amount:       %s Amazon.com (US) gift card\n", usd(po.AmountCents))
	fmt.Fprintf(&b, "Requested at: %s\n\n", rfc3339(po.RequestedAt))
	fmt.Fprintf(&b, "This user, calendar year %d (UTC):\n", calendarYear)
	fmt.Fprintf(&b, "  Sent:                    %s\n", usd(year.SentCents))
	fmt.Fprintf(&b, "  Requested (incl. this):  %s\n", usd(year.RequestedCents))
	fmt.Fprintf(&b, "  Total:                   %s\n\n", usd(year.Total()))
	if year.Total() >= W9WarnCents {
		fmt.Fprintf(&b, "W-9 FIRST (先收 W-9 再发): this user is at %s or more in gift cards this year,\n", usd(W9WarnCents))
		b.WriteString("and 1099-NEC is required from $600. Collect a W-9 before sending this card.\n\n")
	}
	b.WriteString("Within 3 business days: buy the card, email the code to the address above, then\n")
	fmt.Fprintf(&b, "  deploy/referral-payout.sh mark-sent %d\n", po.ID)
	b.WriteString("To cancel instead (the amount goes back to their balance):\n")
	fmt.Fprintf(&b, "  deploy/referral-payout.sh cancel %d\n", po.ID)
	return subject, b.String()
}

// usd renders cents as "$1234.56" (no thousands grouping; operator text only).
func usd(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
