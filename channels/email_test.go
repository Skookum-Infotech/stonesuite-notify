package channels

import (
	"strings"
	"testing"

	"stonesuite-notify/config"
)

func TestSendNotificationEmail_NoProviderConfigured_NoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "")
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}

func TestSendNotificationEmail_MissingRecipient_ReturnsError(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "", "title", "body", "")
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
