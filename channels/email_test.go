package channels

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stonesuite-notify/config"
)

func TestSendNotificationEmail_NoProviderConfigured_NoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", "", nil)
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}

func TestSendNotificationEmail_MissingRecipient_ReturnsError(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "", "title", "body", "", "", nil)
	if err == nil {
		t.Fatal("expected an error for an empty recipient address")
	}
}

func TestSendNotificationEmail_ConfiguredButNoEmailFrom_ReturnsNamedError(t *testing.T) {
	// RESEND_API_KEY present, EMAIL_FROM empty — the exact production
	// misconfiguration that made Resend 422 every send. Must fail before the
	// provider call, with a message that names EMAIL_FROM.
	cfg := config.Config{ResendAPIKey: "re_test_key"}
	err := SendNotificationEmail(cfg, "customer@example.com", "Invoice INV-1 sent", "body", "", "<p>branded</p>", nil)
	if err == nil {
		t.Fatal("expected an error when a provider is configured but EMAIL_FROM is unset")
	}
	if !strings.Contains(err.Error(), "EMAIL_FROM") {
		t.Fatalf("error must name the missing variable, got %q", err)
	}
}

func TestSendViaResend_ErrorIncludesResponseBody(t *testing.T) {
	const resendBody = `{"statusCode":422,"name":"validation_error","message":"The gmail.com domain is not verified."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(resendBody))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Invoice INV-1 sent", "<p>branded</p>", nil)
	if err == nil {
		t.Fatal("expected an error on a 422 response")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "domain is not verified") {
		t.Fatalf("error must carry Resend's status and response body, got %q", err)
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
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", "", nil)
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

func TestBuildSMTPMessage_UsesGivenHTMLVerbatim(t *testing.T) {
	brandedHTML := `<html><body><p>Please find your invoice attached.</p><p>Regards,<br>Acme</p></body></html>`
	msg := buildSMTPMessage("user@example.com", "from@example.com", "Invoice INV-1", brandedHTML, nil)
	s := string(msg)
	if !strings.Contains(s, "Please find your invoice attached.") {
		t.Fatalf("expected the branded HTML body verbatim, got:\n%s", s)
	}
	if strings.Contains(s, "<h2>Invoice INV-1</h2>") {
		t.Fatalf("expected the generic <h2>title</h2> template NOT to be used when explicit HTML is given, got:\n%s", s)
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
