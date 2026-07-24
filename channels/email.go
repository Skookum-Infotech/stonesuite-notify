// Package channels delivers a persisted notification over side channels
// beyond the in-app feed: email and web push. Both senders are
// fire-and-forget from the caller's perspective — a delivery failure here
// must never fail the notification-create request, since the in-app row
// already exists and is the source of truth.
package channels

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"time"

	"stonesuite-notify/config"
)

const emailTimeout = 10 * time.Second

// SendNotificationEmail delivers a notification over email, choosing a
// provider the same way StoneSuite-Backend's services/email.go does:
// Resend if configured, else SMTP, else a logged no-op. Errors are
// returned for logging by the caller but are never fatal to the caller's
// own request.
func SendNotificationEmail(cfg config.Config, to, title, body, link string) error {
	if to == "" {
		return fmt.Errorf("email channel: recipient address is empty")
	}

	html := renderEmailHTML(title, body, link)

	switch {
	case cfg.ResendAPIKey != "":
		return sendViaResend(cfg, to, title, html)
	case cfg.SMTPHost != "":
		return sendViaSMTP(cfg, to, title, html)
	default:
		log.Printf("channels: email not configured, skipping send to %s (%q)", to, title)
		return nil
	}
}

func renderEmailHTML(title, body, link string) string {
	linkHTML := ""
	if link != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s">View in StoneSuite</a></p>`, link)
	}
	return fmt.Sprintf(`<div><h2>%s</h2><p>%s</p>%s</div>`, title, body, linkHTML)
}

type resendRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	HTML    string `json:"html"`
}

func sendViaResend(cfg config.Config, to, subject, html string) error {
	payload, err := json.Marshal(resendRequest{
		From:    cfg.EmailFrom,
		To:      to,
		Subject: subject,
		HTML:    html,
	})
	if err != nil {
		return fmt.Errorf("resend: encode request: %w", err)
	}

	client := &http.Client{Timeout: emailTimeout}
	req, err := http.NewRequest(http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.ResendAPIKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("resend: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func sendViaSMTP(cfg config.Config, to, subject, html string) error {
	addr := cfg.SMTPHost + ":" + cfg.SMTPPort
	msg := []byte("To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" + html + "\r\n")

	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}

	if err := smtp.SendMail(addr, auth, cfg.EmailFrom, []string{to}, msg); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return nil
}
