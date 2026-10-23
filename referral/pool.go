package referral

import (
	"context"
	"fmt"
	"log"
	"strings"
	"text/tabwriter"
	"time"
)

// Pool watch — once per settlement pass, take stock of code_pool and shout
// before /claim starts answering pool_empty.
//
// The pool is Apple one-time offer codes imported in batches (import-codes.sh).
// Each batch dies on its own date (Apple allows at most six months), and
// /claim only hands out codes that still have issue_margin_days left, so the
// stock that matters is "free AND beyond the margin", not "free". Two
// conditions raise an alert, each read from referral_settings:
//
//	low stock   usable < low_stock_threshold
//	expiring    the earliest usable batch expires within expiry_warn_days AND
//	            what is left once it is gone would be below the threshold —
//	            a projected low stock, so a fresh batch waiting behind the
//	            old one keeps the watch quiet.
//
// An alert is a WARN log line every pass plus at most ONE email per day for
// the same set of conditions (in memory; a restart may repeat one, which is
// fine). A failed send is not counted as sent, so the next pass retries.
//
// Auto-restock (restock.go, default off) runs inside the watch, before the
// verdict; while one of its restocks is under way the alert only logs.

// restockCommand is the exact line the operator runs after exporting a new
// batch from App Store Connect (deploy/supabase/import-codes.sh).
const restockCommand = `deploy/supabase/import-codes.sh codes.csv "Referral Friend Month" YYYY-MM-DD`

const poolDay = 24 * time.Hour

// Alerter delivers an operator notice: a plain-ASCII subject and a UTF-8
// plain-text body. main passes the feedback mailer (FEEDBACK_SMTP_* /
// FEEDBACK_EMAIL_TO); nil = log only.
type Alerter interface {
	Alert(subject, body string) error
}

// Stock is what the watch concludes from one PoolStatus at one moment.
type Stock struct {
	// Usable is the free codes beyond the issue margin: what /claim can hand
	// out right now.
	Usable int
	// Short is the free codes inside the margin: never issued again, waiting
	// for ExpireCodes to retire them.
	Short int
	// Earliest is the expiry of the earliest usable batch; zero when none.
	Earliest time.Time
	// AfterEarliest is what is still issuable on the day the earliest batch
	// expires: batches that outlive it by more than the issue margin. A batch
	// expiring a day later is no stand-in — the margin retires it at almost
	// the same time (Codex P2).
	AfterEarliest int
	LowStock      bool
	Expiring      bool
}

// Alerting reports whether either condition holds.
func (st Stock) Alerting() bool { return st.LowStock || st.Expiring }

// conditions names what is wrong, stable and ASCII, for the dedup key and
// the subject line.
func (st Stock) conditions() string {
	var c []string
	if st.LowStock {
		c = append(c, "low_stock")
	}
	if st.Expiring {
		c = append(c, "expiring")
	}
	return strings.Join(c, "+")
}

// TakeStock applies the settings to the batches as of now. Batches arrive
// earliest expiry first (pgstore orders them), but nothing here relies on it.
func TakeStock(ps PoolStatus, now time.Time) Stock {
	var st Stock
	margin := time.Duration(ps.Settings.IssueMarginDays) * poolDay
	cutoff := now.Add(margin)
	for _, b := range ps.Batches {
		if !b.ExpiresAt.After(cutoff) {
			st.Short += b.Free
			continue
		}
		if b.Free == 0 {
			continue
		}
		st.Usable += b.Free
		if st.Earliest.IsZero() || b.ExpiresAt.Before(st.Earliest) {
			st.Earliest = b.ExpiresAt
		}
	}
	if !st.Earliest.IsZero() {
		horizon := st.Earliest.Add(margin)
		for _, b := range ps.Batches {
			if b.ExpiresAt.After(horizon) {
				st.AfterEarliest += b.Free
			}
		}
	}
	st.LowStock = st.Usable < ps.Settings.LowStockThreshold
	if !st.Earliest.IsZero() && ps.Settings.ExpiryWarnDays > 0 {
		soon := !st.Earliest.After(now.Add(time.Duration(ps.Settings.ExpiryWarnDays) * poolDay))
		st.Expiring = soon && st.AfterEarliest < ps.Settings.LowStockThreshold
	}
	return st
}

// SettingsProblem names a referral_settings combination that cannot do what
// it looks like it does; "" when the row is fine. Logged once per
// misconfiguration by the watch, never fatal.
func SettingsProblem(s PoolSettings) string {
	switch {
	case s.ExpiryWarnDays > 0 && s.ExpiryWarnDays <= s.IssueMarginDays:
		return fmt.Sprintf("expiry_warn_days=%d <= issue_margin_days=%d: a usable batch always has more than the margin left, so the expiring alert can never fire",
			s.ExpiryWarnDays, s.IssueMarginDays)
	case s.SwapGraceDays > s.IssueMarginDays:
		return fmt.Sprintf("swap_grace_days=%d > issue_margin_days=%d: a code can be swapped the moment it is issued, burning a second one on every re-claim",
			s.SwapGraceDays, s.IssueMarginDays)
	}
	return ""
}

// watchPool is the per-pass step: read, run auto-restock (restock.go), then
// assess, log, and email at most once a day. Restock goes first so the
// verdict is about the pool after it; while a restock is under way (busy) the
// low-stock / expiring alert is only logged — a mail would send the operator
// to restock by hand on top of it. Errors are counted on the report and never
// stop the pass.
func (s *Service) watchPool(ctx context.Context, now time.Time, rep *Report) {
	ps, err := s.store.PoolStatus(ctx)
	if err != nil {
		log.Printf("referral settle: pool status: %v", err)
		rep.Errors++
		return
	}
	if p := SettingsProblem(ps.Settings); p != "" {
		if !s.poolConfigWarned {
			log.Printf("WARN referral pool settings: %s", p)
			s.poolConfigWarned = true
		}
	} else {
		s.poolConfigWarned = false
	}
	// 023: the monthly and yearly groups never lend each other codes, so each
	// is its own stock. Auto-restock only refills the monthly group (the
	// yearly offer's codes are imported by hand for now).
	month, year := ps.SplitByVariant()
	busy, refreshed := s.restockStep(ctx, now, month, rep)
	if refreshed {
		if fresh, err := s.store.PoolStatus(ctx); err != nil {
			log.Printf("referral settle: pool status after restock: %v", err)
			rep.Errors++
		} else {
			ps = fresh
			month, year = ps.SplitByVariant()
		}
	}
	if names := ps.UnknownOfferNames(); len(names) > 0 {
		// Codes under a name that is neither group's are handed out as
		// monthly; a typo in a yearly import would mislabel the friend's terms.
		log.Printf("WARN referral pool: code_pool names that are neither %q nor %q are treated as the monthly group: %v",
			ps.Settings.OfferRefMonth, ps.Settings.OfferRefYear, names)
	}
	if ps.Settings.OfferYearPct > 0 || ps.UnredeemedYear > 0 {
		s.watchVariant(now, year, VariantYear, false, rep)
	}
	s.watchVariant(now, month, VariantMonth, busy, rep)
}

// watchVariant logs and mails the low-stock / expiring alert for one group's
// stock. The monthly group keeps the original alert key and subject; the
// yearly one is tagged so both can go out the same day.
func (s *Service) watchVariant(now time.Time, ps PoolStatus, variant string, busy bool, rep *Report) {
	st := TakeStock(ps, now)
	if variant == VariantMonth {
		rep.Usable = st.Usable
	} else {
		rep.UsableYear = st.Usable
	}
	if !st.Alerting() {
		return
	}
	earliest := "none"
	if !st.Earliest.IsZero() {
		earliest = st.Earliest.UTC().Format("2006-01-02")
	}
	log.Printf("WARN referral pool (%s): %s usable=%d short=%d earliest=%s after_earliest=%d threshold=%d warn_days=%d margin_days=%d",
		variant, st.conditions(), st.Usable, st.Short, earliest, st.AfterEarliest,
		ps.Settings.LowStockThreshold, ps.Settings.ExpiryWarnDays, ps.Settings.IssueMarginDays)
	if busy {
		log.Printf("referral pool: alert mail held back — an auto-restock is under way")
		return
	}
	if s.alerter == nil {
		return
	}
	subject, body := alertMail(ps, st, now, s.restock.Environment)
	key := st.conditions()
	if variant == VariantYear {
		subject = strings.Replace(subject, "Referral code pool", "Referral code pool (yearly group)", 1)
		body = "年付组（offer " + ps.Settings.OfferRefYear + "）。补货：import-codes.sh codes.csv \"" + ps.Settings.OfferRefYear + "\" YYYY-MM-DD\n\n" + body
		key = "year:" + key
	}
	sent, err := s.alertOnce(key, now, subject, body)
	if err != nil {
		log.Printf("referral pool alert mail: %v", err)
		rep.Errors++
		return
	}
	if sent {
		rep.Alerted++
	}
}

// alertOnce mails subject / body unless key already went out today: one mail
// per key per UTC day, flapping included — the keys sent today are
// remembered (in memory; a restart may repeat one, which is fine) and
// forgotten when the date changes. A failed send is not remembered, so the
// next pass retries. No alerter → false, nil (the caller has logged).
func (s *Service) alertOnce(key string, now time.Time, subject, body string) (bool, error) {
	if s.alerter == nil {
		return false, nil
	}
	if today := now.UTC().Format("2006-01-02"); today != s.alertDay {
		s.alertDay, s.alertsSent = today, map[string]bool{}
	}
	if s.alertsSent[key] {
		return false, nil
	}
	if err := s.alerter.Alert(subject, body); err != nil {
		return false, err
	}
	s.alertsSent[key] = true
	return true, nil
}

// alertMail renders the operator email: what is left per batch, when the
// first batch dies, and the exact restock command. Subject stays ASCII (a
// non-ASCII header would need RFC 2047 encoding); the body is UTF-8 Chinese
// like every other ops note in this repo. Dates are UTC, like the dedup key.
// env is ASC_OFFER_CODE_ENVIRONMENT (normalised) when set: the subject then
// says [PRODUCTION] / [SANDBOX], so staging and prod mailing the same inbox
// can be told apart; "" leaves the subject as it always was.
func alertMail(ps PoolStatus, st Stock, now time.Time, env string) (subject, body string) {
	var why []string
	if st.LowStock {
		why = append(why, fmt.Sprintf("low stock (%d usable)", st.Usable))
	}
	if st.Expiring {
		why = append(why, "expiring "+st.Earliest.UTC().Format("2006-01-02"))
	}
	tag := ""
	if env != "" {
		tag = "[" + env + "] "
	}
	subject = "[YardMate] " + tag + "Referral code pool: " + strings.Join(why, ", ")

	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友码池告警（%s）\n\n", now.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "可发（free 且到期日在 %d 天余量之外）：%d 张", ps.Settings.IssueMarginDays, st.Usable)
	if st.LowStock {
		fmt.Fprintf(&b, "，低于阈值 %d", ps.Settings.LowStockThreshold)
	}
	b.WriteString("\n")
	if st.Earliest.IsZero() {
		b.WriteString("最早到期：没有可发的码\n")
	} else {
		left := int(st.Earliest.Sub(now).Hours() / 24)
		fmt.Fprintf(&b, "最早到期：%s（还有 %d 天", st.Earliest.UTC().Format("2006-01-02"), left)
		if st.Expiring {
			fmt.Fprintf(&b, "，预警线 %d 天；到期后可发只剩 %d 张，低于阈值 %d", ps.Settings.ExpiryWarnDays, st.AfterEarliest, ps.Settings.LowStockThreshold)
		} else {
			fmt.Fprintf(&b, "；到期后可发还剩 %d 张", st.AfterEarliest)
		}
		b.WriteString("）\n")
	}
	if st.Short > 0 {
		fmt.Fprintf(&b, "余量不足、不再发出的 free 码：%d 张（到期后自动划 expired）\n", st.Short)
	}

	b.WriteString("\n各批次：\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  批次\t到期日\tfree\tissued\tredeemed\texpired")
	for _, bt := range ps.Batches {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%d\n",
			bt.Name, bt.ExpiresAt.UTC().Format("2006-01-02"), bt.Free, bt.Issued, bt.Redeemed, bt.Expired)
	}
	if len(ps.Batches) == 0 {
		fmt.Fprintln(tw, "  （码池是空的）")
	}
	_ = tw.Flush()

	b.WriteString("\n补货：App Store Connect → offer「Referral Friend Month」再生成一批一次性码\n")
	b.WriteString("（每批 500–25,000 张、最长 6 个月；每 App 每季度上限 100 万张，所有订阅共用），导出 CSV 后在 secrets 环境里跑：\n")
	b.WriteString("  SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… " + restockCommand + "\n")
	b.WriteString("先 staging 后 prod。看现状：deploy/referral-pool.sh\n\n")
	b.WriteString("阈值在 referral_settings（issue_margin_days / swap_grace_days / low_stock_threshold / expiry_warn_days），改库即生效。\n")
	b.WriteString("自动补货（默认关）：referral_settings.auto_restock_enabled，见 deploy/README.md §10。\n")
	return subject, b.String()
}
