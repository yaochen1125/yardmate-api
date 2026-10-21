package referral

import (
	"context"
	"fmt"
	"log"
	"strings"
	"text/tabwriter"
	"time"
)

// Gate 4's operator mail (invite-friends.md ### 防刷 第 4 道). Settlement ②
// parks a referral as held when its referrer already had velocity_limit
// successes inside velocity_window_hours of its theoretical success time
// (redeemed_at + hold_days, 017); each pass then mails the operator
// about new held ones — at most one mail per referrer per 24 hours, kept in
// the database (referrals.held_notified_at), so a restart does not repeat it
// and a failed send is retried on the next pass. The operator answers with
// deploy/referral-review.sh approve / reject.

const reviewDigestBatch = 50

// reviewCommand is the operator script (deploy/referral-review.sh).
const reviewCommand = "deploy/referral-review.sh"

// notifyHeld is the per-pass step after ②. Errors are counted on the report
// and never stop the pass.
func (s *Service) notifyHeld(ctx context.Context, now time.Time, rep *Report) {
	digests, err := s.store.HeldDigests(ctx, reviewDigestBatch)
	if err != nil {
		log.Printf("referral settle: held digests: %v", err)
		rep.Errors++
		return
	}
	for _, d := range digests {
		ids := d.NewHeld()
		if len(ids) == 0 {
			continue
		}
		if s.alerter == nil {
			log.Printf("WARN referral held: %d new referral(s) awaiting review; operator mail disabled — %s list", len(ids), reviewCommand)
		} else {
			subject, body := reviewMail(d, now)
			if err := s.alerter.Alert(subject, body); err != nil {
				log.Printf("referral review mail: %v", err)
				rep.Errors++
				continue // not marked: the next pass tries again
			}
		}
		if err := s.store.MarkHeldNotified(ctx, ids); err != nil {
			// Mailed but not recorded: the next pass mails again. A duplicate
			// beats a held referral nobody hears about.
			log.Printf("referral settle: mark held notified: %v", err)
			rep.Errors++
			continue
		}
		rep.Reviewed++
	}
}

// deviceSpread counts the claim devices among the digest's referrals:
// distinct recorded install ids, how many referrals have one, how many do not.
func deviceSpread(refs []HeldReferral) (distinct, recorded, missing int) {
	seen := map[string]bool{}
	for _, r := range refs {
		if r.InstallID == "" {
			missing++
			continue
		}
		recorded++
		seen[strings.ToLower(r.InstallID)] = true
	}
	return len(seen), recorded, missing
}

// reviewMail renders the operator email. Subject ASCII (no RFC 2047 in
// Alert); body UTF-8 Chinese like the pool alert. Times are UTC.
func reviewMail(d HeldDigest, now time.Time) (subject, body string) {
	newIDs := d.NewHeld()
	held := 0
	for _, r := range d.Referrals {
		if r.State == StateHeld {
			held++
		}
	}
	subject = fmt.Sprintf("[YardMate] Referral review: %d held for one referrer", len(newIDs))

	var b strings.Builder
	fmt.Fprintf(&b, "推荐好友速度闸：有推荐进了「待审核」（%s）\n\n", now.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "推荐人：%s\n", d.Referrer)
	fmt.Fprintf(&b, "规则：%d 小时内成功超过 %d 个，第 %d 个起待审核（按理论成功时间 = 兑换时间 + hold_days 数，不按结算跑到的时刻；referral_settings.velocity_limit / velocity_window_hours）\n",
		d.WindowHours, d.Limit, d.Limit+1)
	fmt.Fprintf(&b, "待审核：%d 条（这封新增 %d 条）\n", held, len(newIDs))
	distinct, recorded, missing := deviceSpread(d.Referrals)
	verdict := "都不同"
	if distinct < recorded {
		verdict = "有重复（同一台设备填了不止一次）"
	}
	fmt.Fprintf(&b, "朋友填码设备：下表 %d 条里记了设备的 %d 条，%d 台不同设备 → %s", len(d.Referrals), recorded, distinct, verdict)
	if missing > 0 {
		fmt.Fprintf(&b, "（另有 %d 条是 016 之前填的码，没记设备）", missing)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "\n待审核 + 算进它们那 %d 小时窗口的成功（按理论成功时间）：\n", d.WindowHours)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  推荐 id\t状态\t填码（UTC）\t理论成功（UTC）\t结算认定（UTC）\t填码设备\t朋友")
	for _, r := range d.Referrals {
		state := r.State
		if r.State == StateHeld && !r.Notified {
			state += " (新)"
		}
		dev := r.InstallID
		if dev == "" {
			dev = "-"
		}
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, state,
			r.ClaimedAt.UTC().Format("2006-01-02 15:04"), r.DueAt.UTC().Format("2006-01-02 15:04"),
			r.QualifiedAt.UTC().Format("2006-01-02 15:04"), dev, r.Referee)
	}
	_ = tw.Flush()

	b.WriteString("\n审核（先 staging 后 prod；凭据从 secrets 环境读）：\n")
	fmt.Fprintf(&b, "  SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… %s list\n", reviewCommand)
	fmt.Fprintf(&b, "  … %s approve <推荐 id>   放行：下一轮结算（≤ 10 分钟）照常发月份 / 攒着\n", reviewCommand)
	fmt.Fprintf(&b, "  … %s reject <推荐 id>    作废（rejected）：不发月份，名下的礼品卡钱一起作废\n", reviewCommand)
	b.WriteString("不处理的话这些推荐一直停在待审核；推荐人那边只显示「在路上」，看不出审核。\n")
	b.WriteString("同一推荐人 24 小时内最多一封；之后再有新的待审核会再来一封。\n")
	return subject, b.String()
}
