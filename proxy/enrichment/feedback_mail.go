package enrichment

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// FeedbackMailer emails every stored feedback message to the operator inbox,
// so feedback is read without polling the Supabase table. Credentials come
// from the secrets Vault (FEEDBACK_SMTP_* / FEEDBACK_EMAIL_TO in
// /etc/yardmate-api/secrets.env) — same Gmail app-password account the
// yardmate-onfail systemd alert uses, but read from the service's own vault
// instead of the root-only /etc/yardmate-alert/smtp-pass.
//
// Delivery is fire-and-forget from the handler (goroutine, bounded by a
// connection deadline): a mail outage must never fail or slow the 200
// response — the row is already durably stored, mail is a convenience copy.
type FeedbackMailer struct {
	addr string // "host:port"
	host string // for STARTTLS SNI + auth
	from string
	to   string
	pass string
}

// NewFeedbackMailer returns nil (mail disabled) unless from/pass/to are all
// present. Host/port default to Gmail submission (smtp.gmail.com:587).
func NewFeedbackMailer(host, port, from, pass, to string) *FeedbackMailer {
	if from == "" || pass == "" || to == "" {
		return nil
	}
	if host == "" {
		host = "smtp.gmail.com"
	}
	if port == "" {
		port = "587"
	}
	return &FeedbackMailer{
		addr: net.JoinHostPort(host, port),
		host: host,
		from: from,
		to:   to,
		pass: pass,
	}
}

// notify composes and sends the feedback email. Runs on its own goroutine —
// errors are logged, never surfaced to the client.
func (m *FeedbackMailer) notify(id string, row feedbackRow) {
	if err := m.send(id, row); err != nil {
		log.Printf("feedback mail err: id=%s err=%v", id, err)
	}
}

func (m *FeedbackMailer) send(id string, row feedbackRow) error {
	msg := m.compose(id, row, time.Now().UTC())

	// smtp.SendMail has no deadline — a stalled SMTP server would leak the
	// goroutine. Dial + absolute connection deadline bounds the whole exchange.
	conn, err := net.DialTimeout("tcp", m.addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()
	if err := c.StartTLS(&tls.Config{ServerName: m.host}); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	if err := c.Auth(smtp.PlainAuth("", m.from, m.pass, m.host)); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(m.from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(m.to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return c.Quit()
}

// compose renders the RFC 5322 message. Every client-supplied value embedded
// in a HEADER goes through headerSafe (CR/LF stripped) — feedback fields are
// truncated but not newline-free, and header injection must be impossible.
// The body is plain UTF-8 text, so the raw message is fine there.
func (m *FeedbackMailer) compose(id string, row feedbackRow, now time.Time) string {
	// Subject 保持纯 ASCII 结构符（"-" 而非 "·"）：header 里的非 ASCII 字符按
	// RFC 2047 须编码，设备名/版本实际恒 ASCII，不引入编码依赖。
	subject := "[YardMate] New feedback - " + headerSafe(row.device) + " - " + headerSafe(row.appVersion)
	var b strings.Builder
	fmt.Fprintf(&b, "From: YardMate Feedback <%s>\r\n", m.from)
	fmt.Fprintf(&b, "To: %s\r\n", m.to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(row.message)
	b.WriteString("\r\n\r\n---\r\n")
	fmt.Fprintf(&b, "App version: %s\r\n", row.appVersion)
	fmt.Fprintf(&b, "Device: %s\r\n", row.device)
	fmt.Fprintf(&b, "System: %s\r\n", row.system)
	fmt.Fprintf(&b, "App language: %s\r\n", row.appLanguage)
	fmt.Fprintf(&b, "Region: %s\r\n", row.region)
	fmt.Fprintf(&b, "Subscriber: %s\r\n", yesNo(row.isSubscriber))
	fmt.Fprintf(&b, "Device install id: %s\r\n", row.deviceID)
	fmt.Fprintf(&b, "Feedback id: %s\r\n", id)
	fmt.Fprintf(&b, "Received at: %s\r\n", now.Format(time.RFC3339))
	return b.String()
}

// yesNo renders a bool for the human-readable email body.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// headerSafe strips CR/LF so a client-supplied value cannot inject headers.
func headerSafe(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}
