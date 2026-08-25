package channels

import (
	"strings"
	"testing"

	"stonesuite-notify/config"
)

func TestSendNotificationEmail_NoProviderConfigured_NoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", nil)
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}

func TestSendNotificationEmail_MissingRecipient_ReturnsError(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "", "title", "body", "", nil)
	if err == nil {
		t.Fatal("expected an error for an empty recipient address")
	}
}

func TestRenderEmailHTML_IncludesLinkWhenPresent(t *testing.T) {
	html := renderEmailHTML("Invoice approved", "Invoice INV-1 was approved.", "https://app.example.com/invoices/1")
	if !strings.Contains(html, "https://app.example.com/invoices/1") {
		t.Fatalf("expected rendered HTML to include the link, got %q", html)
	}
}

func TestRenderEmailHTML_OmitsLinkWhenAbsent(t *testing.T) {
	html := renderEmailHTML("Invoice approved", "Invoice INV-1 was approved.", "")
	if strings.Contains(html, "<a href=") {
		t.Fatalf("expected no link markup when link is empty, got %q", html)
	}
}

func TestSendNotificationEmail_NoAttachment_StillNoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", nil)
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}

func TestBuildSMTPMessage_IncludesAttachment(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "Invoice sent", "<p>See attached.</p>",
		&EmailAttachment{FileName: "INV-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")})
	s := string(msg)
	if !strings.Contains(s, "multipart/mixed") {
		t.Fatalf("expected a multipart/mixed message when an attachment is present, got:\n%s", s)
	}
	if !strings.Contains(s, `filename="INV-1.pdf"`) {
		t.Fatalf("expected the attachment filename in the message, got:\n%s", s)
	}
	if !strings.Contains(s, "JVBERi0xLjQ") {
		t.Fatalf("expected base64-encoded attachment content in the message, got:\n%s", s)
	}
}

func TestBuildSMTPMessage_NoAttachment_PlainHTML(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "Invoice sent", "<p>Hi.</p>", nil)
	s := string(msg)
	if strings.Contains(s, "multipart/mixed") {
		t.Fatalf("expected a plain (non-multipart) message with no attachment, got:\n%s", s)
	}
	if !strings.Contains(s, "<p>Hi.</p>") {
		t.Fatalf("expected the HTML body present, got:\n%s", s)
	}
}
