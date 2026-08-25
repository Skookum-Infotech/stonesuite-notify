# Document-Send Owner Notification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the broken backend→notify wiring, and notify a document's internal owner (with the PDF attached) when it's sent to a customer.

**Architecture:** Two repos, two tracks. **Track A** (`stonesuite-notify`, Tasks 1–5) adds attachment support to its email delivery pipeline: a new 1:1 `notification_attachments` table, `Store.SaveAttachment`/`GetAttachment`, the internal `Create` endpoint persisting an optional attachment, and the async email worker fetching + forwarding it to a newly attachment-aware `channels.SendNotificationEmail`. **Track B** (`StoneSuite-Backend`, Tasks 6–7) reverts a broken, architecturally-mismatched WIP (`sendEmail()` routing through Notify with placeholder ids — this currently fails to even compile), fixes the one real bug in the existing `services/notify.go` helper (wrong URL), and wires `DocumentOps.Send` to best-effort-notify the record's real owner with the same PDF bytes already used for the customer email. Track A must land before Track B's Task 7, since Task 7's notification carries an attachment field Track A creates.

**Tech Stack:** Go, `net/http`, `pgx/v5` (both repos' Postgres access), stdlib `testing` (notify service — no testify there), `testify` (backend). Two separate git repositories/working directories.

## Global Constraints

**Repo roots (always specify which one a task's file paths are relative to):**
- Backend: `C:\Users\Lenovo\StoneSuite-Backend` (branch `feat/phase-1b-documents`)
- Notify: `C:\Users\Lenovo\stonesuite-notify` (branch `feat/notify-platform`)

**stonesuite-notify conventions (Tasks 1–5):**
- Tests use plain stdlib `testing` (`t.Fatalf`, no testify) — matches every existing test in this repo.
- Schema changes go in `database/migrations/schema.sql`, appended at EOF, every statement `IF NOT EXISTS` — "editing that file *is* the migration," no separate migration runner.
- Layering is store → handler per package; handlers depend on the `Store` interface, never `*PGStore` directly, so they stay fake-testable.
- `channels/` sends are synchronous inside a worker's claimed delivery attempt — a slow provider delays that delivery's own retry timer, never the original `POST /api/notifications/internal` response.
- No existing package has a DB-backed (`dbtest`) test for its `PGStore` methods — only fakes are used to test consumers. Match this: new `PGStore` methods get no direct DB test.
- Errors wrapped `fmt.Errorf("context: %w", err)`. `context.Context` first parameter throughout.

**StoneSuite-Backend conventions (Tasks 6–7):**
- Tests use `testify/assert`/`require`.
- Errors wrapped `fmt.Errorf("context: %w", err)`. No panic. `context.Context` first parameter on store/service functions.
- A Notify-service call must never fail the HTTP response it's attached to — log and continue, exactly like this codebase's existing best-effort `workflow.LogAudit` calls.
- **This repo currently does not compile** (`go build ./...` fails: `services\email.go:133:30: undefined: context`) because of the pre-existing WIP Task 6 reverts. Task 6 is the fix — no other task should be attempted first.

---

## Track A — stonesuite-notify

### Task 1: `notification_attachments` schema

**Files:**
- Modify: `C:\Users\Lenovo\stonesuite-notify\database\migrations\schema.sql` (append at EOF)

**Interfaces:**
- Consumes: nothing (pure schema).
- Produces: table `notification_attachments(notification_id UUID PK/FK, file_name, content_type, content BYTEA, created_at)`, consumed by Task 2's store methods.

- [ ] **Step 1: Append the table to `schema.sql`**

At the end of the file (after the existing `service_api_keys` block), add:
```sql
-- notification_attachments holds at most one binary attachment per
-- notification — currently used only for the "document sent" owner
-- notification, which carries the same PDF the customer received. This is
-- deliberately its own table, not a column on notifications: notifications
-- is read in bulk by the feed/list/summary endpoints backing the in-app
-- bell UI, and this table is read only by the email delivery worker, one
-- row at a time, by notification_id.
CREATE TABLE IF NOT EXISTS notification_attachments (
    notification_id UUID PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
    file_name        TEXT NOT NULL,
    content_type     TEXT NOT NULL,
    content          BYTEA NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

- [ ] **Step 2: Verify the service still builds**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go build ./...`
Expected: success (schema.sql is embedded via `//go:embed` and only applied at runtime against a live DB — this step just confirms the Go build isn't otherwise broken before continuing).

- [ ] **Step 3: Commit**

```bash
cd C:\Users\Lenovo\stonesuite-notify
git add database/migrations/schema.sql
git commit -m "feat(notifications): add notification_attachments table"
```

---

### Task 2: `AttachmentInput` type + `Store.SaveAttachment`/`GetAttachment`

**Files:**
- Modify: `C:\Users\Lenovo\stonesuite-notify\notifications\types.go` (add `AttachmentInput`)
- Modify: `C:\Users\Lenovo\stonesuite-notify\notifications\store.go` (extend `Store` interface + `PGStore`)
- Modify: `C:\Users\Lenovo\stonesuite-notify\notifications\types_test.go` (add a table test for the new type, if any validation is added — see Step 1)
- Modify: `C:\Users\Lenovo\stonesuite-notify\workers\dispatch_test.go` (add no-op stub methods to `fakeNotifications` so the package keeps compiling)
- Modify: `C:\Users\Lenovo\stonesuite-notify\controllers\notifications_test.go` (add no-op stub methods to `fakeStore` so the package keeps compiling)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `type AttachmentInput struct { FileName, ContentType string; Content []byte }` (package `notifications`)
  - `notifications.Store` interface gains:
    `SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error`
    `GetAttachment(ctx context.Context, tenantID, environment, notificationID string) (*AttachmentInput, error)` — returns `(nil, nil)` when no attachment exists (not an error case).

- [ ] **Step 1: Add `AttachmentInput` to `notifications/types.go`**

Add after the `CreateInput` type block:
```go
// AttachmentInput is the optional single file attached to a notification's
// email delivery. Persisted in notification_attachments, not on the
// notification row itself — see that table's doc comment in schema.sql.
type AttachmentInput struct {
	FileName    string
	ContentType string
	Content     []byte
}
```

- [ ] **Step 2: Extend the `Store` interface in `notifications/store.go`**

Add to the `Store` interface, after `MarkAllRead`:
```go
	// SaveAttachment persists the single attachment for a notification's
	// email delivery. Called at most once per notification, from
	// controllers.Handler.Create, only when email delivery was actually
	// requested for that recipient.
	SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error
	// GetAttachment loads a notification's attachment, if any. Returns
	// (nil, nil) — not an error — when the notification has none, since
	// the overwhelming majority of notifications never do.
	GetAttachment(ctx context.Context, tenantID, environment, notificationID string) (*AttachmentInput, error)
```

- [ ] **Step 3: Implement both on `PGStore`**

Add to `notifications/store.go`, after `MarkAllRead`'s implementation:
```go
// SaveAttachment inserts (or replaces, on the rare case of a retry)
// notification_id's attachment row.
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

// GetAttachment loads notificationID's attachment, scoped to tenant and
// environment like Get — defense in depth even though notification_id is
// already a globally-unique UUID, matching this store's existing scoping
// discipline. Returns (nil, nil) when the notification has no attachment
// row, which is the common case.
func (s *PGStore) GetAttachment(ctx context.Context, tenantID, environment, notificationID string) (*AttachmentInput, error) {
	var a AttachmentInput
	err := s.pool.QueryRow(ctx, `
		SELECT na.file_name, na.content_type, na.content
		FROM notification_attachments na
		JOIN notifications n ON n.id = na.notification_id
		WHERE na.notification_id = $1 AND n.tenant_id = $2 AND n.environment = $3`,
		notificationID, tenantID, environment).Scan(&a.FileName, &a.ContentType, &a.Content)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification attachment %s: %w", notificationID, err)
	}
	return &a, nil
}
```

- [ ] **Step 4: Add no-op stubs to `workers/dispatch_test.go`'s `fakeNotifications`**

So `workers` package tests keep compiling — Task 5 will give `GetAttachment` real behavior for its own test; this step just restores compilation. Add after the existing `MarkAllRead` stub on `fakeNotifications`:
```go
func (f *fakeNotifications) SaveAttachment(_ context.Context, _ string, _ notifications.AttachmentInput) error {
	return nil
}
func (f *fakeNotifications) GetAttachment(_ context.Context, _, _, _ string) (*notifications.AttachmentInput, error) {
	return nil, nil
}
```

- [ ] **Step 5: Add no-op stubs to `controllers/notifications_test.go`'s `fakeStore`**

Task 3 will give `SaveAttachment` real recording behavior for its own test; this step restores compilation. Add after the existing `MarkAllRead` stub on `fakeStore`:
```go
func (f *fakeStore) SaveAttachment(_ context.Context, _ string, _ notifications.AttachmentInput) error {
	return nil
}
func (f *fakeStore) GetAttachment(_ context.Context, _, _, _ string) (*notifications.AttachmentInput, error) {
	return nil, nil
}
```

- [ ] **Step 6: Verify build and existing tests**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go build ./... && go vet ./... && go test ./...`
Expected: all pass — this is a pure additive interface extension, so nothing existing should change behavior.

- [ ] **Step 7: Commit**

```bash
cd C:\Users\Lenovo\stonesuite-notify
git add notifications/types.go notifications/store.go workers/dispatch_test.go controllers/notifications_test.go
git commit -m "feat(notifications): add AttachmentInput and Store.SaveAttachment/GetAttachment"
```

---

### Task 3: `Create` handler accepts and persists an optional attachment

**Files:**
- Modify: `C:\Users\Lenovo\stonesuite-notify\controllers\notifications.go` (`createNotificationRequest`, `Create`)
- Modify: `C:\Users\Lenovo\stonesuite-notify\controllers\notifications_test.go` (real test using `fakeStore.SaveAttachment`)

**Interfaces:**
- Consumes: `notifications.AttachmentInput` (Task 2).
- Produces: `POST /api/notifications/internal` accepts an optional `"attachment"` field, base64-encoded on the wire.

- [ ] **Step 1: Give `fakeStore.SaveAttachment` real recording behavior**

Replace the no-op stub added in Task 2 Step 5 with (same file, `controllers/notifications_test.go`):
```go
type fakeStore struct {
	// ... existing fields unchanged ...
	savedAttachments map[string]notifications.AttachmentInput // keyed by notificationID
}

func (f *fakeStore) SaveAttachment(_ context.Context, notificationID string, a notifications.AttachmentInput) error {
	if f.savedAttachments == nil {
		f.savedAttachments = map[string]notifications.AttachmentInput{}
	}
	f.savedAttachments[notificationID] = a
	return nil
}
```
Leave `GetAttachment`'s stub as-is (unused by this task's test). Initialize `savedAttachments` in whatever constructor/literal this test file uses to build a `fakeStore{}` if it isn't already a zero-value-safe map (a nil map read is fine; only writes need the nil-check above, which the code already has).

- [ ] **Step 2: Write the failing test**

Add to `controllers/notifications_test.go` (find the existing `TestHandler_Create...`-style test for the pattern to match — request construction, service-key header, handler dispatch — and mirror it):
```go
func TestHandler_Create_WithAttachment_SavesIt(t *testing.T) {
	store := &fakeStore{}
	prefStore := &fakePreferencesStore{}
	deliveryStore := &fakeDeliveriesStore{}
	recorder := &fakeRecorder{}
	h := NewHandler(store, prefStore, deliveryStore, recorder, config.Config{})

	body := `{
		"tenantId": "tenant-1",
		"recipients": [{"userId": "user-1"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent",
		"channels": ["email"],
		"attachment": {"fileName": "INV-1.pdf", "contentType": "application/pdf", "contentBase64": "JVBERi0xLjQ="}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", strings.NewReader(body))
	req = req.WithContext(middleware.WithServiceKeyContext(req.Context(), middleware.ServiceKeyContext{
		KeyID: "key-1", Environment: "dev",
	}))
	rr := httptest.NewRecorder()

	h.Create(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.savedAttachments) != 1 {
		t.Fatalf("expected exactly 1 saved attachment, got %d", len(store.savedAttachments))
	}
	for _, a := range store.savedAttachments {
		if a.FileName != "INV-1.pdf" {
			t.Fatalf("expected fileName INV-1.pdf, got %q", a.FileName)
		}
		if string(a.Content) != "%PDF-1.4" {
			t.Fatalf("expected decoded content %q, got %q", "%PDF-1.4", a.Content)
		}
	}
}

func TestHandler_Create_NoAttachment_SavesNothing(t *testing.T) {
	store := &fakeStore{}
	prefStore := &fakePreferencesStore{}
	deliveryStore := &fakeDeliveriesStore{}
	recorder := &fakeRecorder{}
	h := NewHandler(store, prefStore, deliveryStore, recorder, config.Config{})

	body := `{
		"tenantId": "tenant-1",
		"recipients": [{"userId": "user-1"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", strings.NewReader(body))
	req = req.WithContext(middleware.WithServiceKeyContext(req.Context(), middleware.ServiceKeyContext{
		KeyID: "key-1", Environment: "dev",
	}))
	rr := httptest.NewRecorder()

	h.Create(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.savedAttachments) != 0 {
		t.Fatalf("expected no saved attachments, got %d", len(store.savedAttachments))
	}
}
```
**Before running this**, check the actual existing test file for: (a) the real constructor name and arg order for building a `*Handler` (`NewHandler(...)` — confirm signature matches `main.go`'s `controllers.NewHandler(store, prefStore, deliveryStore, auditRecorder, cfg)`), (b) how an existing `Create`-handler test builds the service-key-authenticated request context (the `middleware.WithServiceKeyContext`/`ServiceKeyContext` call above is a best guess at the real helper name — grep `middleware/*.go` for `ServiceKeyContext`/`WithServiceKeyContext`/`GetServiceKeyFromContext` and use the actual helper the existing tests use), and (c) whether `config` is already imported in this test file. Adjust the two tests to match the real helpers before running them — do not guess if the brief's guess is wrong, read the actual middleware package.

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./controllers/ -run TestHandler_Create_WithAttachment -v`
Expected: FAIL — `createNotificationRequest` has no `Attachment` field yet, so it's silently ignored by `json.Decode` today (the test would currently pass 201 but `savedAttachments` would be empty) — confirm the test fails on the attachment-count assertion, not on a compile error.

- [ ] **Step 4: Add the wire type and decode/save logic to `controllers/notifications.go`**

Add near `recipientTarget`:
```go
// attachmentInput is the wire shape of createNotificationRequest.Attachment
// — content travels as base64 over JSON, decoded to bytes before building
// notifications.AttachmentInput.
type attachmentInput struct {
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
}
```

Add a field to `createNotificationRequest`:
```go
	Attachment  *attachmentInput  `json:"attachment,omitempty"`
```

In `Create`, after decoding `req` and before the validation block, decode the attachment once (shared across every recipient the request fans out to):
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
Add `"encoding/base64"` to the import block if not already present.

In the creation loop (`for _, p := range planned { n, err := h.Store.Create(...); ...; h.enqueueDeliveries(...) }`), immediately after `created = append(created, n)` and before `h.enqueueDeliveries(...)`, add:
```go
		if attachment != nil && wantsEmail && p.prefs.EmailEnabled {
			if err := h.Store.SaveAttachment(r.Context(), n.ID, *attachment); err != nil {
				log.Printf("notifications: save attachment for %s: %v", n.ID, err)
			}
		}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./controllers/ -run 'TestHandler_Create_WithAttachment|TestHandler_Create_NoAttachment' -v`
Expected: PASS.

- [ ] **Step 6: Run the full package + build**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go build ./... && go vet ./... && go test ./...`
Expected: all pass, no regressions to the existing `Create` tests.

- [ ] **Step 7: Commit**

```bash
cd C:\Users\Lenovo\stonesuite-notify
git add controllers/notifications.go controllers/notifications_test.go
git commit -m "feat(notifications): accept and persist an optional attachment on create"
```

---

### Task 4: Attachment-aware `channels.SendNotificationEmail`

**Files:**
- Modify: `C:\Users\Lenovo\stonesuite-notify\channels\email.go`
- Modify: `C:\Users\Lenovo\stonesuite-notify\channels\email_test.go`

**Interfaces:**
- Consumes: nothing new (this package stays dependency-free of `notifications` — see Task 5 for why).
- Produces:
  - `type EmailAttachment struct { FileName, ContentType string; Content []byte }` (package `channels`)
  - `func SendNotificationEmail(cfg config.Config, to, title, body, link string, attachment *EmailAttachment) error` (signature extended with a trailing param)

- [ ] **Step 1: Write the failing tests**

Add to `channels/email_test.go`:
```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./channels/ -run 'TestSendNotificationEmail_NoAttachment_StillNoOp|TestBuildSMTPMessage' -v`
Expected: FAIL to compile — `SendNotificationEmail` still takes 4 params, `buildSMTPMessage`/`EmailAttachment` don't exist yet.

- [ ] **Step 3: Implement in `channels/email.go`**

Add `"encoding/base64"`, `"mime/multipart"`, `"net/textproto"` to the import block.

Add the type and change `SendNotificationEmail`'s signature:
```go
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
// optional (nil for the overwhelming majority of notifications). Errors are
// returned for logging by the caller but are never fatal to the caller's
// own request.
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

Update `resendRequest`/`sendViaResend` to include attachments:
```go
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
```

Replace `sendViaSMTP`'s manual message-building with the new `buildSMTPMessage` helper:
```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./channels/ -v`
Expected: all `channels` package tests pass, including the existing `TestSendNotificationEmail_MissingRecipient_ReturnsError` (called with a trailing `nil` now — check that pre-existing test's call site and add the `nil` argument so it still compiles: `SendNotificationEmail(config.Config{}, "", "title", "body", "", nil)`).

- [ ] **Step 5: Full package build/vet/test**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go build ./... && go vet ./...`
Expected: FAIL at this point — `workers/dispatch.go`'s `Deps.SendEmail` field and `attemptEmail` still call the old 5-arg signature. This is expected; Task 5 fixes it. Do not attempt to fix `workers/` in this task — confirm the *only* compile errors are in `workers/` (i.e., `channels` and `controllers` build clean) before moving on.

- [ ] **Step 6: Commit**

```bash
cd C:\Users\Lenovo\stonesuite-notify
git add channels/email.go channels/email_test.go
git commit -m "feat(channels): add attachment support to SendNotificationEmail"
```

---

### Task 5: `attemptEmail` fetches and forwards the attachment

**Files:**
- Modify: `C:\Users\Lenovo\stonesuite-notify\workers\dispatch.go`
- Modify: `C:\Users\Lenovo\stonesuite-notify\workers\dispatch_test.go`

**Interfaces:**
- Consumes: `notifications.Store.GetAttachment` (Task 2), `channels.EmailAttachment`/`SendNotificationEmail` (Task 4).
- Produces: `Deps.SendEmail func(cfg config.Config, to, title, body, link string, attachment *channels.EmailAttachment) error` (extended signature); `attemptEmail` now attachment-aware.

- [ ] **Step 1: Give `fakeNotifications.GetAttachment` real behavior for this test**

Replace the no-op stub added in Task 2 Step 4 (`workers/dispatch_test.go`) with:
```go
type fakeNotifications struct {
	byID        map[string]notifications.Notification
	attachments map[string]notifications.AttachmentInput // keyed by notification id
}

func (f *fakeNotifications) GetAttachment(_ context.Context, _, _, id string) (*notifications.AttachmentInput, error) {
	a, ok := f.attachments[id]
	if !ok {
		return nil, nil
	}
	return &a, nil
}
```
Leave `SaveAttachment`'s no-op stub as-is (unused by this task's test — the worker only reads attachments, never writes them).

- [ ] **Step 2: Write the failing test**

Add to `workers/dispatch_test.go` (find the existing `TestAttemptEmail...`-style test to mirror its `Deps`/fake-construction pattern):
```go
func TestAttemptEmail_WithAttachment_PassesItToSendEmail(t *testing.T) {
	n := notifications.Notification{
		ID: "n-1", TenantID: "t-1", Environment: "dev",
		RecipientEmail: "user@example.com", Title: "Invoice sent",
	}
	notifStore := &fakeNotifications{
		byID:        map[string]notifications.Notification{"n-1": n},
		attachments: map[string]notifications.AttachmentInput{"n-1": {FileName: "INV-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")}},
	}
	deliveryStore := &fakeDeliveries{}

	var gotAttachment *channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Audit:         nil,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _ string, attachment *channels.EmailAttachment) error {
			gotAttachment = attachment
			return nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-1", NotificationID: "n-1", Channel: deliveries.ChannelEmail,
	})

	if gotAttachment == nil {
		t.Fatal("expected an attachment to be passed to SendEmail, got nil")
	}
	if gotAttachment.FileName != "INV-1.pdf" {
		t.Fatalf("expected fileName INV-1.pdf, got %q", gotAttachment.FileName)
	}
	if deliveryStore.sentID != "d-1" {
		t.Fatalf("expected delivery d-1 to be marked sent, got %q", deliveryStore.sentID)
	}
}

func TestAttemptEmail_NoAttachment_PassesNil(t *testing.T) {
	n := notifications.Notification{ID: "n-2", TenantID: "t-1", Environment: "dev", RecipientEmail: "user@example.com"}
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n-2": n}}
	deliveryStore := &fakeDeliveries{}

	var gotAttachment *channels.EmailAttachment
	called := false
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _ string, attachment *channels.EmailAttachment) error {
			called = true
			gotAttachment = attachment
			return nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-2", NotificationID: "n-2", Channel: deliveries.ChannelEmail,
	})

	if !called {
		t.Fatal("expected SendEmail to be called")
	}
	if gotAttachment != nil {
		t.Fatalf("expected nil attachment, got %+v", gotAttachment)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./workers/ -run TestAttemptEmail -v`
Expected: FAIL to compile — `Deps.SendEmail`'s field type and `attemptEmail` still use the 5-arg signature.

- [ ] **Step 4: Update `workers/dispatch.go`**

Add `"stonesuite-notify/channels"` to the imports if not already present (it already is, per the existing `channels.PushSubscription` usage).

Change the `Deps.SendEmail` field type:
```go
	SendEmail     func(cfg config.Config, to, title, body, link string, attachment *channels.EmailAttachment) error
```

`NewDeps`'s assignment (`SendEmail: channels.SendNotificationEmail`) needs no code change — Go resolves it against the new signature automatically since `channels.SendNotificationEmail` was updated to match in Task 4.

Update `attemptEmail`:
```go
func attemptEmail(ctx context.Context, deps Deps, d deliveries.Delivery, n notifications.Notification) {
	var attachment *channels.EmailAttachment
	att, err := deps.Notifications.GetAttachment(ctx, n.TenantID, n.Environment, n.ID)
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
(This replaces the existing single-line body — the rest of the function, `MarkSent`/`recordOutcome`, is unchanged, just now reached after the attachment lookup.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go test ./workers/ -v`
Expected: all `workers` package tests pass, including pre-existing ones (their fake `SendEmail` closures need a 6th parameter added if they don't already accept it via `_` — check `workers/consumer_test.go`/`workers/retry_test.go` for any inline `SendEmail: func(...) error { ... }` literals and add the trailing `_ *channels.EmailAttachment` parameter to each so they keep compiling).

- [ ] **Step 6: Full repo build/vet/test**

Run: `cd C:\Users\Lenovo\stonesuite-notify && go build ./... && go vet ./... && go test ./...`
Expected: all green — Track A is now complete and self-consistent.

- [ ] **Step 7: Commit**

```bash
cd C:\Users\Lenovo\stonesuite-notify
git add workers/dispatch.go workers/dispatch_test.go workers/consumer_test.go workers/retry_test.go
git commit -m "feat(workers): forward a notification's attachment to the email channel"
```

---

## Track B — StoneSuite-Backend

### Task 6: Revert broken `sendEmail()` WIP; fix `services/notify.go`'s URL; add attachment support

**Files:**
- Modify: `C:\Users\Lenovo\StoneSuite-Backend\services\email.go`
- Modify: `C:\Users\Lenovo\StoneSuite-Backend\services\notify.go`
- Create: `C:\Users\Lenovo\StoneSuite-Backend\services\notify_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `sendEmail(to, subject, body string) error` — reverted to Resend → SMTP → no-op only (no behavior change to any of the exported `Send*Email` functions).
  - `type NotifyAttachment struct { FileName, ContentType string; Content []byte }` (package `services`)
  - `NotificationRequest` gains `Attachments []NotifyAttachment`.
  - `SendNotification` posts to the corrected URL and includes attachments as base64 in the JSON body.

- [ ] **Step 1: Revert the broken WIP in `services/email.go`**

Replace `sendEmail`'s current body (currently lines 118–150) with:
```go
func sendEmail(to, subject, body string) error {
	cfg := config.AppConfig

	if cfg.ResendAPIKey != "" {
		return sendViaResend(cfg.ResendAPIKey, cfg.SenderEmail, to, subject, body)
	}
	if cfg.SMTPHost != "" && cfg.SenderEmail != "" {
		return sendViaSMTP(cfg, to, subject, body)
	}

	log.Printf("INFO: no email provider configured (set RESEND_API_KEY or SMTP_HOST+SENDER_EMAIL) — skipping email to %s", to)
	return nil
}
```
This removes the `NotifyURL`/`NotifyAPIKey` branch entirely, including the `context.Background()` call that currently fails to compile (`"context"` is not imported in this file, and should not be added back — nothing else in this file needs it).

- [ ] **Step 2: Verify the build is fixed**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go build ./...`
Expected: success. (Before this step, `go build ./...` fails with `services\email.go:133:30: undefined: context` — confirm that specific error is gone.)

- [ ] **Step 3: Write the failing test for the URL fix**

Create `services/notify_test.go`:
```go
package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"stonesuite-backend/config"
)

func TestSendNotification_PostsToCorrectPath(t *testing.T) {
	var gotPath string
	var gotBody NotificationRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	config.AppConfig = config.Config{NotifyURL: server.URL, NotifyAPIKey: "nk_dev_test_secret"}

	err := SendNotification(context.Background(), NotificationRequest{
		TenantID:   "tenant-1",
		Recipients: []RecipientTarget{{UserID: "user-1"}},
		EventType:  "document.sent",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1 sent",
		Attachments: []NotifyAttachment{
			{FileName: "INV-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/api/notifications/internal", gotPath)
	assert.Equal(t, "tenant-1", gotBody.TenantID)
	require.Len(t, gotBody.Attachments, 1)
	assert.Equal(t, "INV-1.pdf", gotBody.Attachments[0].FileName)
}

func TestSendNotification_NotConfigured_ReturnsError(t *testing.T) {
	config.AppConfig = config.Config{}
	err := SendNotification(context.Background(), NotificationRequest{})
	assert.Error(t, err)
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go test ./services/ -run TestSendNotification_PostsToCorrectPath -v`
Expected: FAIL — asserts path `/api/notifications/internal` but the current code posts to `/api/internal/notifications`; also `NotifyAttachment`/`NotificationRequest.Attachments` don't exist yet (compile failure until Step 5).

- [ ] **Step 5: Fix `services/notify.go`**

Add `Attachments []NotifyAttachment` to `NotificationRequest`, add the `NotifyAttachment` type, and fix the URL:
```go
// NotifyAttachment is a single file attached to a notification's email
// delivery, base64-encoded on the wire by SendNotification.
type NotifyAttachment struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	Content     []byte `json:"-"`
}

// MarshalJSON encodes Content as base64 under "contentBase64", matching
// what stonesuite-notify's POST /api/notifications/internal decodes.
func (a NotifyAttachment) MarshalJSON() ([]byte, error) {
	type wire struct {
		FileName      string `json:"fileName"`
		ContentType   string `json:"contentType"`
		ContentBase64 string `json:"contentBase64"`
	}
	return json.Marshal(wire{
		FileName:      a.FileName,
		ContentType:   a.ContentType,
		ContentBase64: base64.StdEncoding.EncodeToString(a.Content),
	})
}
```
Add `"encoding/base64"` to the import block.

Add the field to `NotificationRequest`:
```go
	Attachments []NotifyAttachment `json:"attachments,omitempty"`
```

Fix the URL in `SendNotification`:
```go
	url := fmt.Sprintf("%s/api/notifications/internal", strings.TrimRight(cfg.NotifyURL, "/"))
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go test ./services/ -run TestSendNotification -v`
Expected: PASS.

- [ ] **Step 7: Full build/vet/test**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go build ./... && go vet ./... && go test ./...`
Expected: all pass — this confirms the repo-wide build breakage is fully resolved and nothing else regressed.

- [ ] **Step 8: Commit**

```bash
cd C:\Users\Lenovo\StoneSuite-Backend
git add services/email.go services/notify.go services/notify_test.go
git commit -m "fix(services): revert broken sendEmail Notify routing, fix notify URL, add attachments"
```

---

### Task 7: `DocumentOps.Send` notifies the record owner with the PDF attached

**Files:**
- Modify: `C:\Users\Lenovo\StoneSuite-Backend\controllers\documents.go`
- Modify: `C:\Users\Lenovo\StoneSuite-Backend\controllers\documents_test.go`

**Interfaces:**
- Consumes: `services.SendNotification`, `services.NotificationRequest`, `services.RecipientTarget`, `services.NotifyAttachment` (Task 6).
- Produces: `loadForRender` return signature gains the record's owner user id; `Send` best-effort-notifies that owner.

- [ ] **Step 1: Extend `loadForRender`'s return signature**

In `controllers/documents.go`, `authRecordAccess` already returns `workflow.RecordAccessInfo` (which has an `OwnerUserID` field) as its second value — `loadForRender` currently discards it. Change:
```go
func (h *DocumentOps) loadForRender(
	w http.ResponseWriter, r *http.Request, recordID string, action authz.Action,
) (*pgxpool.Pool, docpdf.PrintableDoc, DocMeta, string, string, bool) {
	pool, info, identityID, ok := authRecordAccess(w, r, recordID, action)
	if !ok {
		return nil, docpdf.PrintableDoc{}, DocMeta{}, "", "", false
	}
	loader, ok := h.loaders[info.WorkflowKey]
	if !ok {
		fail(w, http.StatusNotFound, "This record type has no printable document.")
		return nil, docpdf.PrintableDoc{}, DocMeta{}, "", "", false
	}
	tenant, err := tenancy.TenantFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "Tenant not resolved.")
		return nil, docpdf.PrintableDoc{}, DocMeta{}, "", "", false
	}
	seller := sellerFromTenant(tenant)
	doc, meta, err := loader(r.Context(), pool, recordID, seller)
	if err != nil {
		fail(w, http.StatusInternalServerError, "Failed to load document.")
		return nil, docpdf.PrintableDoc{}, DocMeta{}, "", "", false
	}
	return pool, doc, meta, identityID, info.OwnerUserID, true
}
```
(Inserted `info.OwnerUserID` as a new 5th return value, before the existing `bool`.)

Update `GetPDF`'s call site (the extra return value is simply unused there):
```go
	_, doc, meta, _, _, ok := h.loadForRender(w, r, recordID, authz.ActionRead)
```

- [ ] **Step 2: Write the failing test**

Add to `controllers/documents_test.go` — this tests the pure decision logic ("call SendNotification with the owner id when one exists, don't when it doesn't, and a failure from it doesn't fail the response") by extracting that logic into a small, directly-testable function rather than exercising it through the full HTTP handler (which needs a real pool/tenant context this package's existing tests don't set up):
```go
func TestNotifyOwnerOfSend_CallsWithOwnerID(t *testing.T) {
	var gotReq services.NotificationRequest
	called := false
	notify := func(_ context.Context, req services.NotificationRequest) error {
		called = true
		gotReq = req
		return nil
	}

	notifyOwnerOfSend(context.Background(), notify, "tenant-1", "owner-1",
		docpdf.PrintableDoc{Kind: "INVOICE", Seller: docpdf.Seller{Name: "Acme"}},
		"INV-1", "invoice", "rec-1", []string{"bob@buyer.example"},
		[]byte("%PDF-1.4"), "INV-1.pdf")

	assert.True(t, called)
	assert.Equal(t, "tenant-1", gotReq.TenantID)
	require.Len(t, gotReq.Recipients, 1)
	assert.Equal(t, "owner-1", gotReq.Recipients[0].UserID)
	assert.Equal(t, "document.sent", gotReq.EventType)
	assert.Equal(t, "invoice", gotReq.Resource)
	assert.Equal(t, "rec-1", gotReq.ResourceID)
	assert.Equal(t, []string{"email"}, gotReq.Channels)
	require.Len(t, gotReq.Attachments, 1)
	assert.Equal(t, "INV-1.pdf", gotReq.Attachments[0].FileName)
}

func TestNotifyOwnerOfSend_NoOwnerID_DoesNotCall(t *testing.T) {
	called := false
	notify := func(_ context.Context, _ services.NotificationRequest) error {
		called = true
		return nil
	}

	notifyOwnerOfSend(context.Background(), notify, "tenant-1", "",
		docpdf.PrintableDoc{Kind: "INVOICE"}, "INV-1", "invoice", "rec-1",
		[]string{"bob@buyer.example"}, []byte("%PDF-1.4"), "INV-1.pdf")

	assert.False(t, called)
}

func TestNotifyOwnerOfSend_NotifyErrors_DoesNotPanicOrReturnError(t *testing.T) {
	notify := func(_ context.Context, _ services.NotificationRequest) error {
		return assert.AnError
	}
	// Must not panic; notifyOwnerOfSend has no return value to check —
	// reaching this line without panicking is the assertion.
	notifyOwnerOfSend(context.Background(), notify, "tenant-1", "owner-1",
		docpdf.PrintableDoc{Kind: "INVOICE"}, "INV-1", "invoice", "rec-1",
		[]string{"bob@buyer.example"}, []byte("%PDF-1.4"), "INV-1.pdf")
}
```
Add `"context"` and `"stonesuite-backend/services"` and `"github.com/stretchr/testify/require"` to this test file's imports if not already present.

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go test ./controllers/ -run TestNotifyOwnerOfSend -v`
Expected: FAIL — `notifyOwnerOfSend` doesn't exist yet.

- [ ] **Step 4: Implement `notifyOwnerOfSend` and wire it into `Send`**

Add to `controllers/documents.go` (near the bottom, with the other helpers), and add `"context"` and `"stonesuite-backend/services"` to the import block:
```go
// notifyOwnerOfSend best-effort-notifies the record's internal owner that
// the document was sent, with the same PDF the customer received attached.
// notify is injected (defaults to services.SendNotification) so tests don't
// need a live notify service. A failure here is logged and swallowed —
// the document has already been sent and recorded by the time this runs,
// and a Notify outage must never undo that.
func notifyOwnerOfSend(
	ctx context.Context,
	notify func(context.Context, services.NotificationRequest) error,
	tenantID, ownerUserID string,
	doc docpdf.PrintableDoc, number, workflowKey, recordID string,
	sentTo []string, pdf []byte, fileName string,
) {
	if ownerUserID == "" {
		return
	}
	err := notify(ctx, services.NotificationRequest{
		TenantID:   tenantID,
		Recipients: []services.RecipientTarget{{UserID: ownerUserID}},
		EventType:  "document.sent",
		Resource:   workflowKey,
		ResourceID: recordID,
		Title:      doc.Kind + " " + number + " sent",
		Body:       "Sent to " + strings.Join(sentTo, ", "),
		Channels:   []string{"email"},
		Attachments: []services.NotifyAttachment{
			{FileName: fileName, ContentType: "application/pdf", Content: pdf},
		},
	})
	if err != nil {
		log.Printf("documents: notify owner of send for record %s: %v", recordID, err)
	}
}
```
Add `"log"` to the import block if not already present.

Update `Send`'s call to `loadForRender` (now returns an extra value) and add the notify call after the existing audit log:
```go
func (h *DocumentOps) Send(w http.ResponseWriter, r *http.Request) {
	recordID := r.PathValue("id")
	pool, doc, meta, identityID, ownerUserID, ok := h.loadForRender(w, r, recordID, authz.ActionUpdate)
	if !ok {
		return
	}
	// ... existing body unchanged down to the audit log call ...
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
(`tenant` is already resolved once inside `loadForRender`; resolving it again here via `tenancy.TenantFromContext` is cheap — it reads from request context, no DB call — and keeps `notifyOwnerOfSend`'s signature free of a `*tenancy.Tenant` dependency for easier testing.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go test ./controllers/ -run 'TestNotifyOwnerOfSend|TestDocumentOps_RequiresAuth' -v`
Expected: PASS.

- [ ] **Step 6: Full build/vet/test**

Run: `cd C:\Users\Lenovo\StoneSuite-Backend && go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 7: Run the security reviewer**

Dispatch the `tenancy-security-reviewer` agent against this task's diff — it touches the shared `loadForRender` used by every `DocumentOps` endpoint (`GetPDF`/`Send`/`Sends`), and adds a new outbound call carrying `ownerUserID`/`tenantID`; confirm no new cross-tenant or IDOR surface (the owner id and tenant id both come from the already-gated `authRecordAccess`/`tenancy.TenantFromContext`, not from request input).

- [ ] **Step 8: Commit**

```bash
cd C:\Users\Lenovo\StoneSuite-Backend
git add controllers/documents.go controllers/documents_test.go
git commit -m "feat(documents): notify the record owner with the PDF when a document is sent"
```

---

## Self-Review

**Spec coverage** (against `2026-08-25-document-send-owner-notification-design.md`):
- D1 (revert generic `sendEmail()` → Notify routing) → Task 6 Step 1. ✓
- D2 (Notify addresses the owner, not the customer; customer path unchanged) → Task 7; `services.SendDocumentEmail` (customer email) is untouched by this plan. ✓
- D3 (owner notification carries the PDF) → Tasks 2–5 (notify-side plumbing) + Task 7 (backend sends it). ✓
- D4 (attachment lives in its own table, not on `notifications`) → Task 1. ✓
- §4.1–4.4 architecture → Tasks 1–7 map directly to each subsection. ✓
- §6 security notes (no new HTTP-reachable read path for attachment bytes; `ownerUserID`/`tenantID` sourced from the trusted gate, not request input) → Task 5 (`GetAttachment` only called from `attemptEmail`, never a handler) and Task 7 Step 7 (security review confirms the trust chain). ✓
- §7 testing → each task's Steps 1–5 (or equivalent) match the spec's named test cases per component. ✓
- §8 out of scope (push attachments, feed/list endpoint changes, generalizing to multiple attachments) → no task touches `push.go`, `ListForUser`, or adds a `[]AttachmentInput`. ✓

**Placeholder scan:** no TBD/TODO; every code step has complete, real code. The one explicit judgment call left to the implementer (Task 3 Step 2's "grep the real middleware helper name") is flagged precisely because it's a genuine unknown this plan's author couldn't verify without reading a file not yet opened — not a vague "handle it."

**Type consistency:** `notifications.AttachmentInput` (Task 2) flows unchanged into Task 3 (`Store.SaveAttachment`) and Task 5 (`Store.GetAttachment` → mapped to `channels.EmailAttachment`). `channels.EmailAttachment` (Task 4) matches the type `Deps.SendEmail` and `attemptEmail` use in Task 5. `services.NotifyAttachment` (Task 6) matches what Task 7's `notifyOwnerOfSend` constructs. `loadForRender`'s new 6-value return (Task 7 Step 1) is updated at both its call sites (`GetPDF`, `Send`) in the same step.
