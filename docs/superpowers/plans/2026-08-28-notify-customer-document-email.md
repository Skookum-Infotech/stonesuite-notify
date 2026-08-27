# Notify-Routed Customer Document Email Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route the customer-facing document-send email (`controllers/documents.go` `Send`, used by Sales Order/Estimate/Quote/Invoice/etc.) through `stonesuite-notify` instead of a direct Resend/SMTP call from the backend, so it gets Notify's queue/retry/audit reliability layer — the same layer the internal "owner notified" ping already gets.

**Architecture:** `stonesuite-notify`'s data model is relaxed so a notification can be addressed by `recipientEmail` alone (no `recipientUserId`) — an "external recipient." The backend's `documents.go` `Send` handler then sends the customer copy as one Notify create-request with one email-only recipient per `to`/`cc` address, carrying a new `emailBodyHtml` field so the branded template is preserved instead of falling back to Notify's generic one. The direct-Resend code path in the backend is deleted once nothing calls it.

**Tech Stack:** Go 1.24+, PostgreSQL (pgx/v5), `stonesuite-notify` and `StoneSuite-Backend` are separate Go modules/repos on the same machine.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-28-notify-customer-document-email-design.md` (committed to both repos) — every task below implements a decision (D1–D6) from that spec.
- D5: if the Notify HTTP call fails (unreachable or non-2xx), `documents.go` `Send` must fail the request (`502`) — never silently fall back to direct Resend.
- D6: `services.SendDocumentEmail`/`sendDocViaResend`/`buildMIME`/`writeQuotedPrintable`/`EmailAttachment` in the backend must be deleted once `documents.go` stops calling them (confirmed via grep to have no other caller).
- No live Postgres is assumed available in this environment — every step must be verifiable with `go build`, `go vet`, and `go test ./...` alone (no test in either repo currently requires a live DB; keep it that way). Where a change can only be truly verified against a real database (the SQL migration's idempotency, the nullable-column insert), say so explicitly rather than inventing a fake DB test.
- Repos: `stonesuite-notify` = `C:\Users\Lenovo\stonesuite-notify`, `StoneSuite-Backend` = `C:\Users\Lenovo\StoneSuite-Backend`. All paths below are relative to one of these two roots — each task states which.

---

## Part A — `stonesuite-notify`

### Task 1: Schema migration — nullable recipient, email-body-override column

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `database/migrations/schema.sql` (append at EOF)

**Interfaces:**
- Produces: `notifications.recipient_user_id` and `notification_deliveries.recipient_user_id` become nullable; `notifications` gets a `CHECK` requiring `recipient_user_id IS NOT NULL OR recipient_email <> ''`; `notification_attachments` gets a new `email_body_html TEXT NOT NULL DEFAULT ''` column. Task 3 and Task 6 depend on these existing.

- [ ] **Step 1: Append the migration**

Append this block to the end of `database/migrations/schema.sql`:

```sql

-- Email-only recipients (no StoneSuite users.id) — added so a notification
-- can address someone with no account yet (e.g. a document-send customer
-- email), routing that email through this service's queue/retry/audit
-- layer instead of a direct Resend/SMTP call from the caller. Existing
-- internal-recipient rows are unaffected: this only widens what the column
-- allows, nothing narrows it.
ALTER TABLE notifications ALTER COLUMN recipient_user_id DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'notifications_recipient_identified'
    ) THEN
        ALTER TABLE notifications ADD CONSTRAINT notifications_recipient_identified
            CHECK (recipient_user_id IS NOT NULL OR recipient_email <> '');
    END IF;
END $$;

ALTER TABLE notification_deliveries ALTER COLUMN recipient_user_id DROP NOT NULL;

-- Optional per-notification override of the generic email template
-- (<h2>title</h2><p>body</p>), used when a caller needs its own branded
-- HTML — e.g. the document-send customer email. Empty string means "use
-- the generic template"; see channels.SendNotificationEmail.
ALTER TABLE notification_attachments ADD COLUMN IF NOT EXISTS email_body_html TEXT NOT NULL DEFAULT '';
```

- [ ] **Step 2: Verify the package still compiles (schema.sql is `//go:embed`-ed as a string, so a syntax typo won't be caught by Go — only by Postgres. This step just confirms nothing else broke.)**

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add database/migrations/schema.sql
git commit -m "feat(database): allow email-only notification recipients"
```

**Manual verification (optional, needs a real Postgres — not part of this task's automated acceptance):** `scripts/dev-up.sh` then confirm the service boots without a schema error (`ApplySchema` runs this file on every boot and fails fast on any SQL error).

---

### Task 2: Relax `CreateInput.Validate()` for email-only recipients

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `notifications/types.go`
- Test: `notifications/types_test.go`

**Interfaces:**
- Produces: `CreateInput.Validate()` no longer requires `RecipientUserID` when `RecipientEmail` is set. Task 6 relies on this.

- [ ] **Step 1: Write the failing test — update the existing case and add a new one**

In `notifications/types_test.go`, replace the `"missing recipientUserId"` case:

```go
		{"missing recipientUserId", func(in *CreateInput) { in.RecipientUserID = "" }, "recipientUserId is required"},
```

with:

```go
		{"missing both recipientUserId and recipientEmail", func(in *CreateInput) { in.RecipientUserID = "" }, "recipientUserId or recipientEmail is required"},
```

Then add a new test function after `TestCreateInputValidate`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./notifications/... -run TestCreateInputValidate -v`
Expected: `TestCreateInputValidate/missing_both_recipientUserId_and_recipientEmail` FAILs (current message is `"recipientUserId is required"`, not `"recipientUserId or recipientEmail is required"`); `TestCreateInputValidate_RecipientEmailAloneIsSufficient` FAILs (current code rejects a missing `RecipientUserID` regardless of `RecipientEmail`).

- [ ] **Step 3: Implement**

In `notifications/types.go`, replace the `RecipientUserID` case in `Validate()`:

```go
	case in.RecipientUserID == "":
		return errRequired("recipientUserId")
```

with:

```go
	case in.RecipientUserID == "" && in.RecipientEmail == "":
		return errRequired("recipientUserId or recipientEmail")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./notifications/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Commit**

```bash
git add notifications/types.go notifications/types_test.go
git commit -m "feat(notifications): allow recipientEmail alone to satisfy CreateInput.Validate"
```

---

### Task 3: `AttachmentInput.EmailBodyHTML` + nullable-recipient store plumbing

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `notifications/types.go` (add field to `AttachmentInput`)
- Modify: `notifications/store.go` (`Create`, `scanNotification`, `SaveAttachment`, `GetAttachment`)
- Modify: `deliveries/store.go` (`Enqueue`, `scanDelivery`)

**Interfaces:**
- Consumes: schema from Task 1 (nullable `recipient_user_id` columns, `notification_attachments.email_body_html`).
- Produces: `notifications.AttachmentInput{FileName, ContentType, Content, EmailBodyHTML string}`. `notifications.PGStore.Create` and `deliveries.PGStore.Enqueue` insert SQL `NULL` (not an empty-string UUID, which Postgres would reject) when `RecipientUserID`/`recipientUserID` is `""`. `GetAttachment` returns `EmailBodyHTML` alongside the existing fields. Task 5 (workers) and Task 6 (controllers) depend on all of this.

No live-DB test is possible here (neither package has a `_test.go` today — see Global Constraints). Verification for this task is `go build`/`go vet` (catches every signature mismatch) plus the Task 6 controller-level tests, which exercise `Create`'s call into these methods through `notifications.Store`/`deliveries.Store` interfaces via fakes (the fakes don't touch real SQL, but they do prove the calling code — the part Task 6 changes — passes the right values through). The actual nullable-insert SQL is real, hand-verified Postgres syntax (`INSERT ... VALUES ($1, $2, ...)` with a Go `nil` interface value for `$2` is pgx's standard way to send `NULL` — the same pattern already used for `actor_user_id` two lines above).

- [ ] **Step 1: Add `EmailBodyHTML` to `AttachmentInput`**

In `notifications/types.go`, replace:

```go
type AttachmentInput struct {
	FileName    string
	ContentType string
	Content     []byte
}
```

with:

```go
// AttachmentInput is the optional per-notification content only the email
// delivery worker reads: an attached file, custom HTML to use in place of
// the generic template, or both.
type AttachmentInput struct {
	FileName      string
	ContentType   string
	Content       []byte
	EmailBodyHTML string
}
```

- [ ] **Step 2: `notifications/store.go` — nullable insert + scan for `recipient_user_id`**

Replace `Create`:

```go
func (s *PGStore) Create(ctx context.Context, in CreateInput) (*Notification, error) {
	var actorUserIDArg any
	if in.ActorUserID != "" {
		actorUserIDArg = in.ActorUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+notificationColumns,
		in.TenantID, in.RecipientUserID, in.RecipientEmail, actorUserIDArg, in.EventType, in.Resource, in.ResourceID, in.Title, in.Body, in.Link, in.VisibleInApp)

	n, err := scanNotification(row)
	if err != nil {
		return nil, fmt.Errorf("create notification: %w", err)
	}
	return n, nil
}
```

with:

```go
func (s *PGStore) Create(ctx context.Context, in CreateInput) (*Notification, error) {
	var actorUserIDArg any
	if in.ActorUserID != "" {
		actorUserIDArg = in.ActorUserID
	}
	var recipientUserIDArg any
	if in.RecipientUserID != "" {
		recipientUserIDArg = in.RecipientUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+notificationColumns,
		in.TenantID, recipientUserIDArg, in.RecipientEmail, actorUserIDArg, in.EventType, in.Resource, in.ResourceID, in.Title, in.Body, in.Link, in.VisibleInApp)

	n, err := scanNotification(row)
	if err != nil {
		return nil, fmt.Errorf("create notification: %w", err)
	}
	return n, nil
}
```

Replace `scanNotification`:

```go
func scanNotification(row pgx.Row) (*Notification, error) {
	var n Notification
	var actorUserID *string
	if err := row.Scan(
		&n.ID, &n.TenantID, &n.RecipientUserID, &n.RecipientEmail, &actorUserID,
		&n.EventType, &n.Resource, &n.ResourceID, &n.Title, &n.Body, &n.Link, &n.VisibleInApp,
		&n.ReadAt, &n.CreatedAt,
	); err != nil {
		return nil, err
	}
	if actorUserID != nil {
		n.ActorUserID = *actorUserID
	}
	return &n, nil
}
```

with:

```go
func scanNotification(row pgx.Row) (*Notification, error) {
	var n Notification
	var recipientUserID *string
	var actorUserID *string
	if err := row.Scan(
		&n.ID, &n.TenantID, &recipientUserID, &n.RecipientEmail, &actorUserID,
		&n.EventType, &n.Resource, &n.ResourceID, &n.Title, &n.Body, &n.Link, &n.VisibleInApp,
		&n.ReadAt, &n.CreatedAt,
	); err != nil {
		return nil, err
	}
	if recipientUserID != nil {
		n.RecipientUserID = *recipientUserID
	}
	if actorUserID != nil {
		n.ActorUserID = *actorUserID
	}
	return &n, nil
}
```

- [ ] **Step 3: `notifications/store.go` — `SaveAttachment`/`GetAttachment` carry `email_body_html`**

Replace both functions:

```go
func (s *PGStore) SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_attachments (notification_id, file_name, content_type, content)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (notification_id) DO UPDATE
			SET file_name = EXCLUDED.file_name,
			    content_type = EXCLUDED.content_type,
			    content = EXCLUDED.content`,
		notificationID, a.FileName, a.ContentType, a.Content)
	if err != nil {
		return fmt.Errorf("save notification attachment %s: %w", notificationID, err)
	}
	return nil
}

func (s *PGStore) GetAttachment(ctx context.Context, tenantID, notificationID string) (*AttachmentInput, error) {
	var a AttachmentInput
	err := s.pool.QueryRow(ctx, `
		SELECT na.file_name, na.content_type, na.content
		FROM notification_attachments na
		JOIN notifications n ON n.id = na.notification_id
		WHERE na.notification_id = $1 AND n.tenant_id = $2`,
		notificationID, tenantID).Scan(&a.FileName, &a.ContentType, &a.Content)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification attachment %s: %w", notificationID, err)
	}
	return &a, nil
}
```

with:

```go
// SaveAttachment inserts (or replaces, on the rare case of a retry)
// notification_id's attachment row. content defaults to an empty (not
// NULL) byte slice when only a.EmailBodyHTML is set and there is no actual
// file, since the column is NOT NULL.
func (s *PGStore) SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error {
	content := a.Content
	if content == nil {
		content = []byte{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_attachments (notification_id, file_name, content_type, content, email_body_html)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (notification_id) DO UPDATE
			SET file_name = EXCLUDED.file_name,
			    content_type = EXCLUDED.content_type,
			    content = EXCLUDED.content,
			    email_body_html = EXCLUDED.email_body_html`,
		notificationID, a.FileName, a.ContentType, content, a.EmailBodyHTML)
	if err != nil {
		return fmt.Errorf("save notification attachment %s: %w", notificationID, err)
	}
	return nil
}

func (s *PGStore) GetAttachment(ctx context.Context, tenantID, notificationID string) (*AttachmentInput, error) {
	var a AttachmentInput
	err := s.pool.QueryRow(ctx, `
		SELECT na.file_name, na.content_type, na.content, na.email_body_html
		FROM notification_attachments na
		JOIN notifications n ON n.id = na.notification_id
		WHERE na.notification_id = $1 AND n.tenant_id = $2`,
		notificationID, tenantID).Scan(&a.FileName, &a.ContentType, &a.Content, &a.EmailBodyHTML)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification attachment %s: %w", notificationID, err)
	}
	return &a, nil
}
```

- [ ] **Step 4: `deliveries/store.go` — nullable insert + scan for `recipient_user_id`**

Replace `Enqueue`:

```go
func (s *PGStore) Enqueue(ctx context.Context, notificationID, tenantID, recipientUserID, channel, status string) (*Delivery, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries (notification_id, tenant_id, recipient_user_id, channel, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+deliveryColumns,
		notificationID, tenantID, recipientUserID, channel, status)

	d, err := scanDelivery(row)
	if err != nil {
		return nil, fmt.Errorf("enqueue delivery: %w", err)
	}
	return d, nil
}
```

with:

```go
func (s *PGStore) Enqueue(ctx context.Context, notificationID, tenantID, recipientUserID, channel, status string) (*Delivery, error) {
	var recipientUserIDArg any
	if recipientUserID != "" {
		recipientUserIDArg = recipientUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries (notification_id, tenant_id, recipient_user_id, channel, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+deliveryColumns,
		notificationID, tenantID, recipientUserIDArg, channel, status)

	d, err := scanDelivery(row)
	if err != nil {
		return nil, fmt.Errorf("enqueue delivery: %w", err)
	}
	return d, nil
}
```

Replace `scanDelivery`:

```go
func scanDelivery(row pgx.Row) (*Delivery, error) {
	var d Delivery
	var lastError *string
	if err := row.Scan(
		&d.ID, &d.NotificationID, &d.TenantID, &d.RecipientUserID, &d.Channel, &d.Status,
		&d.Attempts, &d.MaxAttempts, &d.NextAttemptAt, &lastError, &d.ProviderResponse,
		&d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if lastError != nil {
		d.LastError = *lastError
	}
	return &d, nil
}
```

with:

```go
func scanDelivery(row pgx.Row) (*Delivery, error) {
	var d Delivery
	var recipientUserID *string
	var lastError *string
	if err := row.Scan(
		&d.ID, &d.NotificationID, &d.TenantID, &recipientUserID, &d.Channel, &d.Status,
		&d.Attempts, &d.MaxAttempts, &d.NextAttemptAt, &lastError, &d.ProviderResponse,
		&d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if recipientUserID != nil {
		d.RecipientUserID = *recipientUserID
	}
	if lastError != nil {
		d.LastError = *lastError
	}
	return &d, nil
}
```

- [ ] **Step 5: Verify it compiles**

Run: `go build ./... && go vet ./...`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add notifications/types.go notifications/store.go deliveries/store.go
git commit -m "feat(notifications,deliveries): support NULL recipient_user_id and email-body override"
```

---

### Task 4: `channels.SendNotificationEmail` — branded HTML override

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `channels/email.go`
- Test: `channels/email_test.go`

**Interfaces:**
- Produces: `SendNotificationEmail(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *EmailAttachment) error` — one new `string` parameter, inserted before `attachment`. Task 5 (`workers/dispatch.go`) depends on this exact signature.

- [ ] **Step 1: Write the failing tests**

In `channels/email_test.go`, every existing call to `SendNotificationEmail` needs a new `""` argument inserted before the trailing `nil`/attachment argument. Replace:

```go
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
```

with:

```go
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
```

And replace:

```go
func TestSendNotificationEmail_NoAttachment_StillNoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", nil)
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}
```

with:

```go
func TestSendNotificationEmail_NoAttachment_StillNoOp(t *testing.T) {
	err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", "", nil)
	if err != nil {
		t.Fatalf("expected no-op (nil error) when no provider is configured, got %v", err)
	}
}
```

Then add one new test function at the end of the file — `buildSMTPMessage` already takes the final `html` string directly, so it's the layer that pins down "an explicit body wins over the generic template" (the branching that decides which HTML `SendNotificationEmail` passes down):

```go
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
```

- [ ] **Step 2: Run tests to verify the new/changed ones fail**

Run: `go test ./channels/... -v`
Expected: compile error (`SendNotificationEmail` still takes 5 positional args before `attachment`, calls now pass 6) — this "step fails to even compile" is the correct RED state for a signature change; proceed to Step 3.

- [ ] **Step 3: Implement**

In `channels/email.go`, replace:

```go
func SendNotificationEmail(cfg config.Config, to, title, body, link string, attachment *EmailAttachment) error {
	if to == "" {
		return fmt.Errorf("email channel: recipient address is empty")
	}

	html := renderEmailHTML(title, body, link)

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
```

with:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./channels/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Commit**

```bash
git add channels/email.go channels/email_test.go
git commit -m "feat(channels): let a caller override the generic email template with its own HTML"
```

---

### Task 5: `workers/dispatch.go` — wire the email-body override through

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `workers/dispatch.go`
- Test: `workers/dispatch_test.go`

**Interfaces:**
- Consumes: `channels.SendNotificationEmail`'s new signature (Task 4), `notifications.AttachmentInput.EmailBodyHTML` (Task 3).
- Produces: `Deps.SendEmail` field type updated to match; `attemptEmail` now passes `att.EmailBodyHTML` through and only builds a `*channels.EmailAttachment` when there's an actual file (`att.FileName != ""`), since an email-body-only attachment row (no PDF) is now possible.

- [ ] **Step 1: Update every `SendEmail` fake signature in the test file**

`workers/dispatch_test.go` has 8 occurrences of a `SendEmail:` closure with the old 5-string-param signature. Every one of the form:

```go
		SendEmail:     func(_ config.Config, _, _, _, _ string, _ *channels.EmailAttachment) error { return nil },
```

becomes (one more `_` before `string`):

```go
		SendEmail:     func(_ config.Config, _, _, _, _, _ string, _ *channels.EmailAttachment) error { return nil },
```

The two occurrences that name the attachment parameter (`attachment *channels.EmailAttachment` instead of `_`) get the same treatment — only the run of `string`-typed underscore parameters grows by one; the named `attachment` parameter is untouched. There is no behavioral change to make in any of these closures — this step is purely updating them to match the new signature so the package compiles. Grep for `SendEmail:` in this file to find all 8 and apply the same one-underscore insertion to each.

- [ ] **Step 2: Write one new failing test proving `emailBodyHTML` is read from the attachment row and passed through**

Add to `workers/dispatch_test.go`, matching the exact pattern `TestAttemptEmail_WithAttachment_PassesItToSendEmail` (already in this file, just above where this goes) already uses — same `fakeNotifications{byID, attachments}` / `fakeDeliveries{}` construction, driven through `attemptDelivery` rather than calling `attemptEmail` directly:

```go
func TestAttemptEmail_PassesEmailBodyHTMLFromAttachment(t *testing.T) {
	n := notifications.Notification{
		ID: "n-3", TenantID: "t-1",
		RecipientEmail: "customer@example.com", Title: "Sales Order SO-1", Body: "Document sent.",
	}
	notifStore := &fakeNotifications{
		byID:        map[string]notifications.Notification{"n-3": n},
		attachments: map[string]notifications.AttachmentInput{"n-3": {EmailBodyHTML: "<p>Branded body.</p>"}},
	}
	deliveryStore := &fakeDeliveries{}

	var gotEmailBodyHTML string
	var gotAttachment *channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, emailBodyHTML string, attachment *channels.EmailAttachment) error {
			gotEmailBodyHTML = emailBodyHTML
			gotAttachment = attachment
			return nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-3", NotificationID: "n-3", Channel: deliveries.ChannelEmail,
	})

	if gotEmailBodyHTML != "<p>Branded body.</p>" {
		t.Fatalf("emailBodyHTML = %q, want %q", gotEmailBodyHTML, "<p>Branded body.</p>")
	}
	if gotAttachment != nil {
		t.Fatalf("expected no file attachment (attachment row has no FileName), got %+v", gotAttachment)
	}
}
```

The last assertion is deliberate: this attachment row carries only `EmailBodyHTML` (no `FileName`), which is exactly the case `attemptEmail`'s new `if att.FileName != ""` guard (Step 4 below) needs to handle — an email-body-only override must not synthesize a fake empty-file attachment.

- [ ] **Step 3: Run tests to verify state**

Run: `go test ./workers/... -v`
Expected: compile errors from Step 1 not yet applied consistently, or the new test FAILing on the assertion — resolve compile errors first (Step 1), then confirm the new test fails against the current `attemptEmail` (which doesn't yet read/pass `EmailBodyHTML`).

- [ ] **Step 4: Implement**

In `workers/dispatch.go`, replace the `Deps` struct's `SendEmail` field:

```go
	SendEmail     func(cfg config.Config, to, title, body, link string, attachment *channels.EmailAttachment) error
```

with:

```go
	SendEmail     func(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *channels.EmailAttachment) error
```

Replace `attemptEmail`:

```go
func attemptEmail(ctx context.Context, deps Deps, d deliveries.Delivery, n notifications.Notification) {
	var attachment *channels.EmailAttachment
	att, err := deps.Notifications.GetAttachment(ctx, n.TenantID, n.ID)
	if err != nil {
		log.Printf("workers: load attachment for notification %s: %v", n.ID, err)
	} else if att != nil {
		attachment = &channels.EmailAttachment{FileName: att.FileName, ContentType: att.ContentType, Content: att.Content}
	}

	if err := deps.SendEmail(deps.Config, n.RecipientEmail, n.Title, n.Body, n.Link, attachment); err != nil {
		log.Printf("workers: email delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if err := deps.Deliveries.MarkSent(ctx, d.ID, nil); err != nil {
		log.Printf("workers: mark email delivery %s sent: %v", d.ID, err)
	}
	recordOutcome(ctx, deps, d, audit.ActionDeliverySent, map[string]any{})
}
```

with:

```go
func attemptEmail(ctx context.Context, deps Deps, d deliveries.Delivery, n notifications.Notification) {
	var attachment *channels.EmailAttachment
	var emailBodyHTML string
	att, err := deps.Notifications.GetAttachment(ctx, n.TenantID, n.ID)
	if err != nil {
		log.Printf("workers: load attachment for notification %s: %v", n.ID, err)
	} else if att != nil {
		emailBodyHTML = att.EmailBodyHTML
		if att.FileName != "" {
			attachment = &channels.EmailAttachment{FileName: att.FileName, ContentType: att.ContentType, Content: att.Content}
		}
	}

	if err := deps.SendEmail(deps.Config, n.RecipientEmail, n.Title, n.Body, n.Link, emailBodyHTML, attachment); err != nil {
		log.Printf("workers: email delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if err := deps.Deliveries.MarkSent(ctx, d.ID, nil); err != nil {
		log.Printf("workers: mark email delivery %s sent: %v", d.ID, err)
	}
	recordOutcome(ctx, deps, d, audit.ActionDeliverySent, map[string]any{})
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./workers/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 6: Commit**

```bash
git add workers/dispatch.go workers/dispatch_test.go
git commit -m "feat(workers): pass a notification's email-body override through to the send"
```

---

### Task 6: `controllers.Handler.Create` — accept email-only recipients, skip preference resolution, thread `emailBodyHtml`

**Repo root:** `C:\Users\Lenovo\stonesuite-notify`

**Files:**
- Modify: `controllers/notifications.go`
- Test: `controllers/notifications_test.go`

**Interfaces:**
- Consumes: `CreateInput.Validate()` (Task 2), `AttachmentInput.EmailBodyHTML` (Task 3).
- Produces: `createNotificationRequest.EmailBodyHTML string` (JSON `emailBodyHtml`) — this is the exact wire field `StoneSuite-Backend`'s Task 7/8 must send.

- [ ] **Step 1: Update the existing test that currently asserts rejection**

In `controllers/notifications_test.go`, `TestCreate_RejectsRecipientMissingUserID` currently asserts a recipient with no `UserID` is rejected — that's no longer universally true (it's fine if `Email` is set). Rename it and tighten what it actually asserts:

Replace:

```go
func TestCreate_RejectsRecipientMissingUserID(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "no-id@example.com"}},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 0 {
		t.Fatalf("store has %d rows, want 0 (invalid recipient must reject whole request)", len(store.rows))
	}
}
```

with:

```go
func TestCreate_RejectsRecipientMissingBothUserIDAndEmail(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{}},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 0 {
		t.Fatalf("store has %d rows, want 0 (invalid recipient must reject whole request)", len(store.rows))
	}
}

func TestCreate_AcceptsRecipientWithEmailOnly(t *testing.T) {
	store := &fakeStore{}
	delivStore := &fakeDeliveriesStore{}
	h := newTestHandler(store, newFakePreferencesStore(), delivStore, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "customer@example.com"}},
		EventType:  "document.sent",
		Resource:   "salesorder",
		ResourceID: "so-1",
		Title:      "Sales Order SO-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
	n := store.rows[0]
	if n.RecipientUserID != "" {
		t.Fatalf("RecipientUserID = %q, want empty for an email-only recipient", n.RecipientUserID)
	}
	if n.RecipientEmail != "customer@example.com" {
		t.Fatalf("RecipientEmail = %q, want customer@example.com", n.RecipientEmail)
	}
	if n.VisibleInApp {
		t.Fatalf("VisibleInApp = true, want false (no user to show a bell to)")
	}
	emailDelivery, ok := delivStore.find(deliveries.ChannelEmail)
	if !ok {
		t.Fatalf("no email delivery row enqueued for the email-only recipient")
	}
	if emailDelivery.Status != deliveries.StatusPending {
		t.Fatalf("email delivery status = %q, want %q", emailDelivery.Status, deliveries.StatusPending)
	}
}

func TestCreate_EmailOnlyRecipient_SkipsPreferenceResolve(t *testing.T) {
	store := &fakeStore{}
	prefs := newFakePreferencesStore()
	h := newTestHandler(store, prefs, &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "customer@example.com"}},
		EventType:  "document.sent",
		Resource:   "salesorder",
		ResourceID: "so-1",
		Title:      "Sales Order SO-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if prefs.resolveCalls != 0 {
		t.Fatalf("PreferenceStore.Resolve was called %d times for an email-only recipient, want 0", prefs.resolveCalls)
	}
}

func TestCreate_UserRecipient_StillResolvesPreferences(t *testing.T) {
	store := &fakeStore{}
	prefs := newFakePreferencesStore()
	h := newTestHandler(store, prefs, &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if prefs.resolveCalls != 1 {
		t.Fatalf("PreferenceStore.Resolve was called %d times for a real user, want 1", prefs.resolveCalls)
	}
}

func TestCreate_EmailBodyHTML_PersistedAsAttachment(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:      "t1",
		Recipients:    []recipientTarget{{Email: "customer@example.com"}},
		EventType:     "document.sent",
		Resource:      "salesorder",
		ResourceID:    "so-1",
		Title:         "Sales Order SO-1 sent",
		Channels:      []string{"email"},
		EmailBodyHTML: "<p>Branded body.</p>",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
	saved, ok := store.savedAttachments[store.rows[0].ID]
	if !ok {
		t.Fatalf("no attachment saved for notification %s", store.rows[0].ID)
	}
	if saved.EmailBodyHTML != "<p>Branded body.</p>" {
		t.Fatalf("saved EmailBodyHTML = %q, want %q", saved.EmailBodyHTML, "<p>Branded body.</p>")
	}
}
```

Also add a call counter to `fakePreferencesStore` — replace:

```go
type fakePreferencesStore struct {
	resolved             preferences.Preferences
	tenantDefaults       preferences.Preferences
	resolveErr           error
	lastOverride         preferences.OverrideInput
	lastTenantDefaults   preferences.TenantDefaultsInput
	lastTenantDefaultsID string
	setOverrideHook      func(tenantID, userID string)
}

func newFakePreferencesStore() *fakePreferencesStore {
	return &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}}
}

func (f *fakePreferencesStore) Resolve(_ context.Context, _, _ string) (preferences.Preferences, error) {
	if f.resolveErr != nil {
		return preferences.Preferences{}, f.resolveErr
	}
	return f.resolved, nil
}
```

with:

```go
type fakePreferencesStore struct {
	resolved             preferences.Preferences
	tenantDefaults       preferences.Preferences
	resolveErr           error
	resolveCalls         int
	lastOverride         preferences.OverrideInput
	lastTenantDefaults   preferences.TenantDefaultsInput
	lastTenantDefaultsID string
	setOverrideHook      func(tenantID, userID string)
}

func newFakePreferencesStore() *fakePreferencesStore {
	return &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}}
}

func (f *fakePreferencesStore) Resolve(_ context.Context, _, _ string) (preferences.Preferences, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return preferences.Preferences{}, f.resolveErr
	}
	return f.resolved, nil
}
```

- [ ] **Step 2: Run tests to verify the new/changed ones fail**

Run: `go test ./controllers/... -v`
Expected: `TestCreate_AcceptsRecipientWithEmailOnly` FAILs with 400 (current `Validate()` still requires `RecipientUserID` — wait, Task 2 already fixed `Validate()`; if Task 2 landed first, this instead FAILs because `enqueueDeliveries`/`PreferenceStore.Resolve` is called with an empty `userID`, which the fake tolerates but the real behavior we're adding — `VisibleInApp=false` forced — isn't implemented yet, so `VisibleInApp` assertion fails). `TestCreate_EmailOnlyRecipient_SkipsPreferenceResolve` FAILs (`resolveCalls` is 1, not 0 — current code always calls `Resolve`). `TestCreate_EmailBodyHTML_PersistedAsAttachment` FAILs to compile (`createNotificationRequest` has no `EmailBodyHTML` field yet).

- [ ] **Step 3: Implement**

In `controllers/notifications.go`, add the field to `createNotificationRequest`:

```go
type createNotificationRequest struct {
	TenantID    string            `json:"tenantId"`
	Recipients  []recipientTarget `json:"recipients"`
	ActorUserID string            `json:"actorUserId,omitempty"`
	EventType   string            `json:"eventType"`
	Resource    string            `json:"resource"`
	ResourceID  string            `json:"resourceId"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Link        string            `json:"link,omitempty"`
	Channels    []string          `json:"channels,omitempty"`
	Attachment  *attachmentInput  `json:"attachment,omitempty"`
}
```

becomes:

```go
type createNotificationRequest struct {
	TenantID      string            `json:"tenantId"`
	Recipients    []recipientTarget `json:"recipients"`
	ActorUserID   string            `json:"actorUserId,omitempty"`
	EventType     string            `json:"eventType"`
	Resource      string            `json:"resource"`
	ResourceID    string            `json:"resourceId"`
	Title         string            `json:"title"`
	Body          string            `json:"body,omitempty"`
	Link          string            `json:"link,omitempty"`
	Channels      []string          `json:"channels,omitempty"`
	// EmailBodyHTML, when set, is used verbatim as the email's HTML body
	// instead of the generic <h2>title</h2><p>body</p> template — see
	// channels.SendNotificationEmail.
	EmailBodyHTML string           `json:"emailBodyHtml,omitempty"`
	Attachment    *attachmentInput `json:"attachment,omitempty"`
}
```

Replace the attachment-decoding block in `Create`:

```go
	var attachment *notifications.AttachmentInput
	if req.Attachment != nil {
		content, decErr := base64.StdEncoding.DecodeString(req.Attachment.ContentBase64)
		if decErr != nil {
			fail(w, http.StatusBadRequest, "attachment.contentBase64 is not valid base64.")
			return
		}
		attachment = &notifications.AttachmentInput{
			FileName:    req.Attachment.FileName,
			ContentType: req.Attachment.ContentType,
			Content:     content,
		}
	}
```

with:

```go
	var attachment *notifications.AttachmentInput
	if req.Attachment != nil || req.EmailBodyHTML != "" {
		attachment = &notifications.AttachmentInput{EmailBodyHTML: req.EmailBodyHTML}
		if req.Attachment != nil {
			content, decErr := base64.StdEncoding.DecodeString(req.Attachment.ContentBase64)
			if decErr != nil {
				fail(w, http.StatusBadRequest, "attachment.contentBase64 is not valid base64.")
				return
			}
			attachment.FileName = req.Attachment.FileName
			attachment.ContentType = req.Attachment.ContentType
			attachment.Content = content
		}
	}
```

Replace the per-recipient preference-resolution loop:

```go
	for _, recipient := range req.Recipients {
		in := notifications.CreateInput{
			TenantID:        req.TenantID,
			RecipientUserID: recipient.UserID,
			RecipientEmail:  recipient.Email,
			ActorUserID:     req.ActorUserID,
			EventType:       req.EventType,
			Resource:        req.Resource,
			ResourceID:      req.ResourceID,
			Title:           req.Title,
			Body:            req.Body,
			Link:            req.Link,
		}
		if err := in.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}

		prefs, err := h.PreferenceStore.Resolve(r.Context(), req.TenantID, recipient.UserID)
		if err != nil {
			// Fail open: an outage in the preferences table must never
			// block the in-app row, which remains the source of truth.
			log.Printf("notifications: resolve preferences for %s: %v", recipient.UserID, err)
			prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}
		}
		in.VisibleInApp = prefs.InAppEnabled

		planned = append(planned, plannedCreate{input: in, prefs: prefs})
	}
```

with:

```go
	for _, recipient := range req.Recipients {
		in := notifications.CreateInput{
			TenantID:        req.TenantID,
			RecipientUserID: recipient.UserID,
			RecipientEmail:  recipient.Email,
			ActorUserID:     req.ActorUserID,
			EventType:       req.EventType,
			Resource:        req.Resource,
			ResourceID:      req.ResourceID,
			Title:           req.Title,
			Body:            req.Body,
			Link:            req.Link,
		}
		if err := in.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}

		var prefs preferences.Preferences
		if recipient.UserID == "" {
			// No StoneSuite account to resolve tenant/user preference
			// overrides against — an external recipient always gets
			// email (the only channel they can receive), never in-app
			// (no bell to show it in) or push (no device subscription
			// possible without a user).
			prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: false, PushEnabled: false}
		} else {
			var err error
			prefs, err = h.PreferenceStore.Resolve(r.Context(), req.TenantID, recipient.UserID)
			if err != nil {
				// Fail open: an outage in the preferences table must never
				// block the in-app row, which remains the source of truth.
				log.Printf("notifications: resolve preferences for %s: %v", recipient.UserID, err)
				prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}
			}
		}
		in.VisibleInApp = prefs.InAppEnabled

		planned = append(planned, plannedCreate{input: in, prefs: prefs})
	}
```

Finally, extend the audit metadata so an external recipient's audit entry is identifiable (it has no `recipientUserId`) — replace:

```go
			Metadata: audit.Metadata(map[string]any{
				"recipientUserId": n.RecipientUserID,
				"eventType":       n.EventType,
				"resource":        n.Resource,
				"resourceId":      n.ResourceID,
				"visibleInApp":    n.VisibleInApp,
				"emailEnabled":    p.prefs.EmailEnabled,
				"pushEnabled":     p.prefs.PushEnabled,
				"emailRequested":  wantsEmail,
			}),
```

with:

```go
			Metadata: audit.Metadata(map[string]any{
				"recipientUserId": n.RecipientUserID,
				"recipientEmail":  n.RecipientEmail,
				"eventType":       n.EventType,
				"resource":        n.Resource,
				"resourceId":      n.ResourceID,
				"visibleInApp":    n.VisibleInApp,
				"emailEnabled":    p.prefs.EmailEnabled,
				"pushEnabled":     p.prefs.PushEnabled,
				"emailRequested":  wantsEmail,
			}),
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controllers/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Full-repo verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: no output from build/vet; every package `ok` from test.

- [ ] **Step 6: Commit**

```bash
git add controllers/notifications.go controllers/notifications_test.go
git commit -m "feat(controllers): accept email-only recipients and an emailBodyHtml override on Create"
```

---

## Part B — `StoneSuite-Backend`

### Task 7: `services.NotificationRequest` gains `EmailBodyHTML`

**Repo root:** `C:\Users\Lenovo\StoneSuite-Backend`

**Files:**
- Modify: `services/notify.go`
- Test: `services/notify_test.go`

**Interfaces:**
- Produces: `NotificationRequest.EmailBodyHTML string` (JSON `emailBodyHtml`), matching Notify's Task 6 field exactly. Task 8 depends on this.

- [ ] **Step 1: Write the failing test**

Add to `services/notify_test.go`:

```go
func TestSendNotification_IncludesEmailBodyHTML(t *testing.T) {
	var gotBody NotificationRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	config.AppConfig = config.Config{NotifyURL: server.URL, NotifyAPIKey: "nk_dev_test_secret"}

	err := SendNotification(context.Background(), NotificationRequest{
		TenantID:      "tenant-1",
		Recipients:    []RecipientTarget{{Email: "customer@example.com"}},
		EventType:     "document.sent",
		Resource:      "salesorder",
		ResourceID:    "so-1",
		Title:         "Sales Order SO-1 sent",
		EmailBodyHTML: "<p>Branded body.</p>",
	})
	require.NoError(t, err)

	assert.Equal(t, "<p>Branded body.</p>", gotBody.EmailBodyHTML)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./services/... -run TestSendNotification_IncludesEmailBodyHTML -v`
Expected: compile error — `NotificationRequest` has no field `EmailBodyHTML`.

- [ ] **Step 3: Implement**

In `services/notify.go`, replace:

```go
// NotificationRequest is the payload sent to the notify service.
type NotificationRequest struct {
	TenantID    string             `json:"tenantId"`
	Recipients  []RecipientTarget  `json:"recipients"`
	ActorUserID string             `json:"actorUserId,omitempty"`
	EventType   string             `json:"eventType"`
	Resource    string             `json:"resource"`
	ResourceID  string             `json:"resourceId"`
	Title       string             `json:"title"`
	Body        string             `json:"body,omitempty"`
	Link        string             `json:"link,omitempty"`
	Channels    []string           `json:"channels,omitempty"`
	Attachments []NotifyAttachment `json:"attachments,omitempty"`
}
```

with:

```go
// NotificationRequest is the payload sent to the notify service.
type NotificationRequest struct {
	TenantID    string            `json:"tenantId"`
	Recipients  []RecipientTarget `json:"recipients"`
	ActorUserID string            `json:"actorUserId,omitempty"`
	EventType   string            `json:"eventType"`
	Resource    string            `json:"resource"`
	ResourceID  string            `json:"resourceId"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Link        string            `json:"link,omitempty"`
	Channels    []string          `json:"channels,omitempty"`
	// EmailBodyHTML, when set, is used by stonesuite-notify verbatim as the
	// email's HTML body instead of its generic title/body template — for
	// callers (e.g. a document-send customer email) that need their own
	// branding.
	EmailBodyHTML string             `json:"emailBodyHtml,omitempty"`
	Attachments   []NotifyAttachment `json:"attachments,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./services/... -v`
Expected: PASS (all tests in the package).

- [ ] **Step 5: Commit**

```bash
git add services/notify.go services/notify_test.go
git commit -m "feat(services): add EmailBodyHTML to the notify create request"
```

---

### Task 8: `documents.go` `Send` routes the customer email through Notify

**Repo root:** `C:\Users\Lenovo\StoneSuite-Backend`

**Files:**
- Modify: `controllers/documents.go`
- Test: `controllers/documents_test.go`

**Interfaces:**
- Consumes: `services.NotificationRequest.EmailBodyHTML` (Task 7), `services.SendNotification` (unchanged signature).
- Produces: `customerSendRequest(tenantID string, meta DocMeta, recordID, subject string, doc docpdf.PrintableDoc, message string, to, cc []string, fileName string, pdf []byte) services.NotificationRequest` — a new, independently-testable pure function `Send` calls.

- [ ] **Step 1: Write the failing test**

Add to `controllers/documents_test.go`:

```go
func TestCustomerSendRequest_OneRecipientPerToAndCCAddress(t *testing.T) {
	req := customerSendRequest("tenant-1", DocMeta{WorkflowKey: "salesorder"}, "rec-1", "Sales Order SO-1",
		docpdf.PrintableDoc{Kind: "SALES ORDER", Number: "SO-1", Seller: docpdf.Seller{Name: "Acme"}},
		"Please review.", []string{"buyer@example.com"}, []string{"ap@example.com"},
		"SO-1.pdf", []byte("%PDF-1.4"))

	require.Len(t, req.Recipients, 2)
	assert.Equal(t, "buyer@example.com", req.Recipients[0].Email)
	assert.Empty(t, req.Recipients[0].UserID)
	assert.Equal(t, "ap@example.com", req.Recipients[1].Email)
	assert.Equal(t, "tenant-1", req.TenantID)
	assert.Equal(t, "document.sent", req.EventType)
	assert.Equal(t, "salesorder", req.Resource)
	assert.Equal(t, "rec-1", req.ResourceID)
	assert.Equal(t, "Sales Order SO-1", req.Title)
	assert.Contains(t, req.EmailBodyHTML, "Please review.")
	assert.Contains(t, req.EmailBodyHTML, "Acme")
	assert.Equal(t, []string{"email"}, req.Channels)
	require.Len(t, req.Attachments, 1)
	assert.Equal(t, "SO-1.pdf", req.Attachments[0].FileName)
	assert.Equal(t, "application/pdf", req.Attachments[0].ContentType)
}

func TestCustomerSendRequest_NoCC_OneRecipient(t *testing.T) {
	req := customerSendRequest("tenant-1", DocMeta{WorkflowKey: "invoice"}, "rec-1", "Invoice INV-1",
		docpdf.PrintableDoc{Kind: "INVOICE", Number: "INV-1", Seller: docpdf.Seller{Name: "Acme"}},
		"", []string{"buyer@example.com"}, nil, "INV-1.pdf", []byte("%PDF-1.4"))

	require.Len(t, req.Recipients, 1)
	assert.Equal(t, "buyer@example.com", req.Recipients[0].Email)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./controllers/... -run TestCustomerSendRequest -v`
Expected: compile error — `customerSendRequest` does not exist yet.

- [ ] **Step 3: Implement**

In `controllers/documents.go`, replace the `Send` method body:

```go
// Send renders the document in memory and emails it to the customer, then
// records the send. RBAC: <type>:update.
func (h *DocumentOps) Send(w http.ResponseWriter, r *http.Request) {
	recordID := r.PathValue("id")
	pool, doc, meta, identityID, ownerUserID, ok := h.loadForRender(w, r, recordID, authz.ActionUpdate)
	if !ok {
		return
	}

	var req sendDocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	to := normalizeRecipients(req.To)
	if len(to) == 0 && meta.DefaultRecipientEmail != "" {
		to = []string{meta.DefaultRecipientEmail}
	}
	if len(to) == 0 {
		fail(w, http.StatusBadRequest, "At least one recipient is required.")
		return
	}
	for _, addr := range append(append([]string{}, to...), normalizeRecipients(req.CC)...) {
		if !looksLikeEmail(addr) {
			fail(w, http.StatusBadRequest, "Invalid recipient email: "+addr)
			return
		}
	}
	subject := req.Subject
	if subject == "" {
		subject = meta.DefaultSubject
	}
	if hasHeaderInjection(subject) {
		fail(w, http.StatusBadRequest, "Invalid subject.")
		return
	}

	// 1. Render.
	pdf, err := h.renderPDF(doc)
	if err != nil {
		fail(w, http.StatusInternalServerError, "Failed to render PDF.")
		return
	}
	fileName := workflow.SanitizeFileName(meta.Number + ".pdf")
	actorUserID, _ := workflow.UserIDByIdentity(r.Context(), pool, identityID)

	// 2. Email with the PDF attached.
	if err := services.SendDocumentEmail(to, normalizeRecipients(req.CC), subject,
		documentEmailHTML(doc, req.Message),
		[]services.EmailAttachment{{FileName: fileName, ContentType: "application/pdf", Content: pdf}},
	); err != nil {
		fail(w, http.StatusBadGateway, "Failed to send email.")
		return
	}

	// 3. Record the send + audit (best-effort audit).
	sendID, err := workflow.InsertDocumentSend(r.Context(), pool, workflow.DocumentSend{
		RecordID: recordID, WorkflowKey: meta.WorkflowKey,
		SentTo: joinRecipients(to), CC: joinRecipients(normalizeRecipients(req.CC)),
		Subject: subject, SentByUserID: actorUserID,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "Failed to record send.")
		return
	}
	_ = workflow.LogAudit(r.Context(), pool, actorUserID, "document.sent", "document_send", sendID,
		map[string]any{"recordId": recordID, "workflowKey": meta.WorkflowKey, "to": to})

	tenant, tErr := tenancy.TenantFromContext(r.Context())
	if tErr == nil {
		notifyOwnerOfSend(r.Context(), services.SendNotification, tenant.ID, ownerUserID,
			doc, meta.Number, meta.WorkflowKey, recordID, to, pdf, fileName)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "sendId": sendID, "sentTo": to,
	})
}
```

with:

```go
// Send renders the document in memory and emails it to the customer, then
// records the send. RBAC: <type>:update.
func (h *DocumentOps) Send(w http.ResponseWriter, r *http.Request) {
	recordID := r.PathValue("id")
	pool, doc, meta, identityID, ownerUserID, ok := h.loadForRender(w, r, recordID, authz.ActionUpdate)
	if !ok {
		return
	}

	// Needed up front (not just for the owner ping, as before): the
	// customer email itself is now a Notify create-request, which requires
	// a real tenantId.
	tenant, tErr := tenancy.TenantFromContext(r.Context())
	if tErr != nil {
		fail(w, http.StatusInternalServerError, "Tenant not resolved.")
		return
	}

	var req sendDocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	to := normalizeRecipients(req.To)
	if len(to) == 0 && meta.DefaultRecipientEmail != "" {
		to = []string{meta.DefaultRecipientEmail}
	}
	if len(to) == 0 {
		fail(w, http.StatusBadRequest, "At least one recipient is required.")
		return
	}
	cc := normalizeRecipients(req.CC)
	for _, addr := range append(append([]string{}, to...), cc...) {
		if !looksLikeEmail(addr) {
			fail(w, http.StatusBadRequest, "Invalid recipient email: "+addr)
			return
		}
	}
	subject := req.Subject
	if subject == "" {
		subject = meta.DefaultSubject
	}
	if hasHeaderInjection(subject) {
		fail(w, http.StatusBadRequest, "Invalid subject.")
		return
	}

	// 1. Render.
	pdf, err := h.renderPDF(doc)
	if err != nil {
		fail(w, http.StatusInternalServerError, "Failed to render PDF.")
		return
	}
	fileName := workflow.SanitizeFileName(meta.Number + ".pdf")
	actorUserID, _ := workflow.UserIDByIdentity(r.Context(), pool, identityID)

	// 2. Email with the PDF attached, via Notify — gets the same
	// queue/retry/audit reliability layer the owner ping below already
	// uses, instead of a direct, unretried Resend/SMTP call.
	if err := services.SendNotification(r.Context(),
		customerSendRequest(tenant.ID, meta, recordID, subject, doc, req.Message, to, cc, fileName, pdf),
	); err != nil {
		fail(w, http.StatusBadGateway, "Failed to send email.")
		return
	}

	// 3. Record the send + audit (best-effort audit).
	sendID, err := workflow.InsertDocumentSend(r.Context(), pool, workflow.DocumentSend{
		RecordID: recordID, WorkflowKey: meta.WorkflowKey,
		SentTo: joinRecipients(to), CC: joinRecipients(cc),
		Subject: subject, SentByUserID: actorUserID,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "Failed to record send.")
		return
	}
	_ = workflow.LogAudit(r.Context(), pool, actorUserID, "document.sent", "document_send", sendID,
		map[string]any{"recordId": recordID, "workflowKey": meta.WorkflowKey, "to": to})

	notifyOwnerOfSend(r.Context(), services.SendNotification, tenant.ID, ownerUserID,
		doc, meta.Number, meta.WorkflowKey, recordID, to, pdf, fileName)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "sendId": sendID, "sentTo": to,
	})
}

// customerSendRequest builds the Notify create request for a document-send's
// customer copy: one email-only recipient per to/cc address (Notify has no
// CC concept — each address becomes its own notification, delivery, and
// audit entry, individually addressed rather than sharing a To/Cc header),
// sharing the same branded HTML body and PDF attachment.
func customerSendRequest(
	tenantID string, meta DocMeta, recordID, subject string,
	doc docpdf.PrintableDoc, message string, to, cc []string, fileName string, pdf []byte,
) services.NotificationRequest {
	recipients := make([]services.RecipientTarget, 0, len(to)+len(cc))
	for _, addr := range append(append([]string{}, to...), cc...) {
		recipients = append(recipients, services.RecipientTarget{Email: addr})
	}
	return services.NotificationRequest{
		TenantID:      tenantID,
		Recipients:    recipients,
		EventType:     "document.sent",
		Resource:      meta.WorkflowKey,
		ResourceID:    recordID,
		Title:         subject,
		Body:          "Document sent.",
		EmailBodyHTML: documentEmailHTML(doc, message),
		Channels:      []string{"email"},
		Attachments:   []services.NotifyAttachment{{FileName: fileName, ContentType: "application/pdf", Content: pdf}},
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./controllers/... -v`
Expected: PASS (all tests in the package, including the pre-existing `TestNotifyOwnerOfSend_*` tests, which are unaffected by this change).

- [ ] **Step 5: Commit**

```bash
git add controllers/documents.go controllers/documents_test.go
git commit -m "feat(controllers): send the document-send customer email through Notify"
```

---

### Task 9: Delete the now-dead direct-Resend document-email code

**Repo root:** `C:\Users\Lenovo\StoneSuite-Backend`

**Files:**
- Modify: `services/email.go` (delete `EmailAttachment`, `SendDocumentEmail`, `buildMIME`, `writeQuotedPrintable`, `sendDocViaResend`, and their now-unused imports)
- Delete: `services/email_attach_test.go` (tests only `buildMIME`, which no longer exists)

**Interfaces:**
- Consumes: Task 8 must have already landed (nothing calls `services.SendDocumentEmail` anymore) — confirmed by `go build` failing loudly if this task runs first.

- [ ] **Step 1: Confirm nothing still calls the functions being deleted**

Run: `grep -rn "SendDocumentEmail\|sendDocViaResend\|buildMIME\|writeQuotedPrintable" --include="*.go" .`
Expected: only matches inside `services/email.go` itself and `services/email_attach_test.go` (Task 8 already removed the call in `controllers/documents.go`).

- [ ] **Step 2: Delete the dead code from `services/email.go`**

Delete everything from the `// EmailAttachment is a single file attached to a document email.` comment through the end of the file (the `EmailAttachment` type, `SendDocumentEmail`, `buildMIME`, `writeQuotedPrintable`, and `sendDocViaResend` — these five are contiguous at the tail of the file).

Then update the import block at the top of `services/email.go` — `encoding/base64`, `mime/multipart`, `mime/quotedprintable`, and `net/textproto` are no longer used by anything left in the file. Replace:

```go
import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/smtp"
	"net/textproto"
	"strings"

	"stonesuite-backend/config"
)
```

with:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"strings"

	"stonesuite-backend/config"
)
```

- [ ] **Step 3: Delete the now-orphaned test file**

Delete `services/email_attach_test.go` entirely — every test in it exercises `buildMIME`, which no longer exists.

- [ ] **Step 4: Verify the whole repo still builds and tests pass**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: no output from build/vet; every package `ok` from test.

- [ ] **Step 5: Commit**

```bash
git add services/email.go
git rm services/email_attach_test.go
git commit -m "chore(services): remove the direct-Resend document-email path, superseded by Notify"
```

---

## Final verification (both repos)

- [ ] In `stonesuite-notify`: `go build ./... && go vet ./... && go test ./...` — all green.
- [ ] In `StoneSuite-Backend`: `go build ./... && go vet ./... && go test ./...` — all green.
- [ ] Grep both repos once more for the deleted symbols (`SendDocumentEmail`, `sendDocViaResend`, `buildMIME`, `writeQuotedPrintable`) to confirm zero remaining references.
- [ ] Re-read the spec's §5 "Known trade-off" — confirm the team building the frontend delivery-log UI (out of scope here) is aware a bad customer address now fails silently from the Send-to-Customer button's point of view.
