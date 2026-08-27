package notifications

import "testing"

func TestCreateInputValidate(t *testing.T) {
	base := CreateInput{
		TenantID:        "tenant-1",
		RecipientUserID: "user-1",
		EventType:       "invoice.approved",
		Resource:        "invoice",
		ResourceID:      "inv-1",
		Title:           "Invoice INV-1042 approved",
	}

	t.Run("valid input passes", func(t *testing.T) {
		if err := base.Validate(); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	cases := []struct {
		name    string
		mutate  func(in *CreateInput)
		wantErr string
	}{
		{"missing tenantId", func(in *CreateInput) { in.TenantID = "" }, "tenantId is required"},
		{"missing both recipientUserId and recipientEmail", func(in *CreateInput) { in.RecipientUserID = "" }, "recipientUserId or recipientEmail is required"},
		{"missing eventType", func(in *CreateInput) { in.EventType = "" }, "eventType is required"},
		{"missing resource", func(in *CreateInput) { in.Resource = "" }, "resource is required"},
		{"missing resourceId", func(in *CreateInput) { in.ResourceID = "" }, "resourceId is required"},
		{"missing title", func(in *CreateInput) { in.Title = "" }, "title is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			err := in.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("got error %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCreateInputValidate_RecipientEmailAloneIsSufficient(t *testing.T) {
	in := CreateInput{
		TenantID:       "tenant-1",
		RecipientEmail: "customer@example.com",
		EventType:      "document.sent",
		Resource:       "salesorder",
		ResourceID:     "so-1",
		Title:          "Sales Order SO-1 sent",
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("expected no error for an email-only recipient, got %v", err)
	}
}
