package main

// mail_wiring.go — the one operator mailbox. Built from FEEDBACK_SMTP_* /
// FEEDBACK_EMAIL_TO in the vault (deploy/secrets.env.example); nil when any
// of from / pass / to is missing, and every user of it then falls back to
// logging. Shared by /v1/feedback (each message forwarded) and the referral
// code-pool watch (low-stock / expiry alert), so there is one account to
// rotate and one inbox to read.

import (
	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/secrets"
)

func operatorMailer(vault *secrets.Vault) *enrichment.FeedbackMailer {
	return enrichment.NewFeedbackMailer(
		vault.Get("FEEDBACK_SMTP_HOST"), vault.Get("FEEDBACK_SMTP_PORT"),
		vault.Get("FEEDBACK_SMTP_FROM"), vault.Get("FEEDBACK_SMTP_PASS"),
		vault.Get("FEEDBACK_EMAIL_TO"))
}
