// Package channels delivers a persisted notification over side channels
// beyond the in-app feed: email and web push. Both senders are
// fire-and-forget from the caller's perspective — a delivery failure here
// must never fail the notification-create request, since the in-app row
// already exists and is the source of truth.
package channels

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/smtp"
	"net/textproto"
	"time"

	"stonesuite-notify/config"
)

const emailTimeout = 10 * time.Second

// EmailAttachment is the single file this channel can attach to a
// notification email — currently only ever the PDF from a "document sent"
// event. Defined here (not imported from notifications) so this package
// stays dependency-free of the domain layer, matching its existing
// config-only import list.
type EmailAttachment struct {
	FileName    string
	ContentType string
	Content     []byte
}

// SendNotificationEmail delivers a notification over email, choosing a
// provider the same way StoneSuite-Backend's services/email.go does:
// Resend if configured, else SMTP, else a logged no-op. attachment is
// optional (nil for the overwhelming majority of notifications). When
// emailBodyHTML is non-empty, it is used verbatim as the message body
// instead of the generic <h2>title</h2><p>body</p> template — used by
// callers (e.g. a document-send customer email) that need their own
// branding. Errors are returned for logging by the caller but are never
// fatal to the caller's own request.
func SendNotificationEmail(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *EmailAttachment) error {
	if to == "" {
		return fmt.Errorf("email channel: recipient address is empty")
	}

	html := emailBodyHTML
	if html == "" {
		html = renderEmailHTML(title, body, link)
	}

	switch {
	case cfg.ResendAPIKey != "":
		return sendViaResend(cfg, to, title, html, attachment)
	case cfg.SMTPHost != "":
		return sendViaSMTP(cfg, to, title, html, attachment)
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

type resendAttachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type resendRequest struct {
	From        string             `json:"from"`
	To          string             `json:"to"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

func sendViaResend(cfg config.Config, to, subject, html string, attachment *EmailAttachment) error {
	req := resendRequest{From: cfg.EmailFrom, To: to, Subject: subject, HTML: html}
	if attachment != nil {
		req.Attachments = []resendAttachment{{
			Filename: attachment.FileName,
			Content:  base64.StdEncoding.EncodeToString(attachment.Content),
		}}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("resend: encode request: %w", err)
	}

	client := &http.Client{Timeout: emailTimeout}
	httpReq, err := http.NewRequest(http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("resend: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.ResendAPIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("resend: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("resend: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func sendViaSMTP(cfg config.Config, to, subject, html string, attachment *EmailAttachment) error {
	addr := cfg.SMTPHost + ":" + cfg.SMTPPort
	msg := buildSMTPMessage(to, cfg.EmailFrom, subject, html, attachment)

	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}

	if err := smtp.SendMail(addr, auth, cfg.EmailFrom, []string{to}, msg); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return nil
}

// buildSMTPMessage assembles the raw RFC 5322 message. With no attachment
// it's the plain HTML body this function always sent before; with one, it
// becomes a multipart/mixed message (HTML part + base64 attachment part) —
// same approach as StoneSuite-Backend's services/email.go buildMIME, so
// both services carry outbound attachments the same way.
func buildSMTPMessage(to, from, subject, html string, attachment *EmailAttachment) []byte {
	if attachment == nil {
		return []byte("To: " + to + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n" +
			"\r\n" + html + "\r\n")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	buf.WriteString("To: " + to + "\r\n")
	buf.WriteString("Subject: " + subject + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: multipart/mixed; boundary=" + mw.Boundary() + "\r\n\r\n")

	htmlHdr := textproto.MIMEHeader{}
	htmlHdr.Set("Content-Type", "text/html; charset=UTF-8")
	htmlHdr.Set("Content-Transfer-Encoding", "8bit")
	if pw, err := mw.CreatePart(htmlHdr); err == nil {
		_, _ = pw.Write([]byte(html))
	}

	ct := attachment.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	attHdr := textproto.MIMEHeader{}
	attHdr.Set("Content-Type", ct)
	attHdr.Set("Content-Transfer-Encoding", "base64")
	attHdr.Set("Content-Disposition", `attachment; filename="`+attachment.FileName+`"`)
	if pw, err := mw.CreatePart(attHdr); err == nil {
		b64 := base64.StdEncoding.EncodeToString(attachment.Content)
		for i := 0; i < len(b64); i += 76 {
			end := i + 76
			if end > len(b64) {
				end = len(b64)
			}
			pw.Write([]byte(b64[i:end] + "\r\n"))
		}
	}
	_ = mw.Close()
	return buf.Bytes()
}
