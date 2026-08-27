# Notify-Routed Customer Document Email — Cross-Repo Design Spec

**Date:** 2026-08-28 · **Status:** Approved; ready for implementation planning
**Author:** Backend/notify architecture pass (Claude)
**Repos affected:** `StoneSuite-Backend` (this repo) and `stonesuite-notify` (sibling service,
`C:\Users\Lenovo\stonesuite-notify`). This spec is committed identically to both repos'
`docs/superpowers/specs/` — each repo's history should show the decision that affected it,
since they cannot share a single commit.

**Supersedes in part:** `2026-08-25-document-send-owner-notification-design.md`'s decision D1
("generic email → Notify routing: revert"). That decision stands for the *standalone*
transactional emails in `services/email.go` (password reset, invites, portal invite, note
confirmation) — those are out of scope here and remain a future Phase 2. This spec specifically
re-opens routing for the **document-send customer email** (`controllers/documents.go` `Send`),
because the blocker that killed the generic attempt — Notify requiring a real `recipientUserId`
— is fixed here at the root, not worked around per call site.

## 1. Problem

Today, `documents.go`'s `Send` handler (Sales Order, Estimate, Quote, Invoice, etc. — every
workflow type with a printable document) emails the customer's PDF two ways in parallel:

1. `services.SendDocumentEmail()` → Resend → SMTP fallback → **direct** to the customer. No
   retry, no delivery log, no audit trail beyond the one `document.sent` entry recorded by the
   backend itself.
2. `notifyOwnerOfSend()` → Notify `/api/notifications/internal` → the record's internal owner
   (staff), with the same PDF attached — this path already gets Notify's queue, retries, and
   delivery log, because the owner has a real `users.id`.

The customer doesn't get any of that reliability layer, because Notify's data model requires a
real `tenantId` + `recipientUserId` per notification (`notifications.CreateInput.Validate()`),
and a document-send recipient is often just an email address on the record with no StoneSuite
account behind it.

## 2. Decisions

| # | Decision | Choice |
|---|---|---|
| D1 | How does Notify represent a recipient with no `users.id` | **`recipientUserId` becomes optional.** A recipient must supply `recipientUserId` OR `recipientEmail` (or both) — not always the former. Notify already never looks up an email from a userId (it owns no user directory; the caller always supplies the address for delivery to work), so this is a validation relaxation, not new lookup logic. |
| D2 | In-app / preferences for an email-only recipient | **Skip preference resolution entirely.** No tenant/user override can exist without a `userId`. Use a fixed `{EmailEnabled: true, InAppEnabled: false, PushEnabled: false}` — no bell row, no push (impossible without a device subscription tied to a user), email always attempted. |
| D3 | Email content/branding for the customer copy | **Preserve the current branded template**, not Notify's generic `<h2>title</h2><p>body</p>`. New optional `emailBodyHTML` field on the create request; when present, `channels/email.go` sends it verbatim instead of rendering the generic template. `title`/`body` stay required (short, for audit) but aren't what the customer sees. |
| D4 | Multiple `to` / `cc` addresses (Notify has no CC concept, one address per notification row) | **One notification per address.** Every `to` and `cc` address becomes its own recipient entry in the same create request — its own notification row, its own delivery/retry, its own audit entry. Each recipient's email shows only their own address in `To:` (no shared header), a behavior change from today's single multi-recipient message. |
| D5 | Backend behavior when Notify is unreachable | **Fail the request.** `documents.go` `Send` returns the same `502 "Failed to send email."` it does today, just keyed off Notify's response instead of Resend's. No silent-success fallback to direct Resend. |
| D6 | Fate of `services.SendDocumentEmail` and friends | **Delete once `documents.go` stops calling them.** Grep confirms `SendDocumentEmail`/`sendDocViaResend`/`buildMIME`/`writeQuotedPrintable`/`EmailAttachment` (in `services/email.go`) have no other caller — dead code after this change, removed rather than left unused. |

## 3. Current state (verified directly against both repos' code)

- `notifications.CreateInput.Validate()` (`stonesuite-notify/notifications/types.go:80-96`)
  requires `TenantID`, `RecipientUserID`, `EventType`, `Resource`, `ResourceID`, `Title`.
- `notifications.recipient_user_id` and `notification_deliveries.recipient_user_id` are both
  `UUID NOT NULL` (`database/migrations/schema.sql:11,98`).
- `controllers.Handler.Create` (`stonesuite-notify/controllers/notifications.go:238-359`) calls
  `PreferenceStore.Resolve(ctx, tenantId, recipient.UserID)` unconditionally per recipient, then
  `enqueueDeliveries` (`:445-465`), which enqueues in_app (skipped/sent by `InAppEnabled`),
  email (if `wantsEmail && EmailEnabled`), and push (if `PushEnabled && PushConfigured()`).
- `channels.SendNotificationEmail` (`stonesuite-notify/channels/email.go:42-58`) takes a single
  `to string` — no CC. `renderEmailHTML` (`:60-66`) is the generic
  `<div><h2>{title}</h2><p>{body}</p>{optional link}</div>` template.
- `services.NotificationRequest`/`RecipientTarget` (`StoneSuite-Backend/services/notify.go`)
  already carries an optional `Email` field per recipient and an `Attachments` list — the wire
  shape already anticipated an email-only recipient; only Notify's server-side validation blocks
  it today.
- `controllers/documents.go` `Send` (`:139-214`) currently: renders the PDF, calls
  `services.SendDocumentEmail(to, cc, subject, html, [pdf])` (step 2, `502` on failure), records
  the send + audit, then best-effort calls `notifyOwnerOfSend` (failure logged, swallowed).

## 4. Design

### 4.1 `stonesuite-notify` — schema

```sql
ALTER TABLE notifications ALTER COLUMN recipient_user_id DROP NOT NULL;
ALTER TABLE notifications ADD CONSTRAINT notifications_recipient_identified
    CHECK (recipient_user_id IS NOT NULL OR recipient_email <> '');

ALTER TABLE notification_deliveries ALTER COLUMN recipient_user_id DROP NOT NULL;
```

Appended to `database/migrations/schema.sql` (idempotent per the file's existing convention —
`DROP NOT NULL` and `ADD CONSTRAINT ... IF NOT EXISTS`-guarded via a `DO $$ ... $$` block, since
Postgres has no `ADD CONSTRAINT IF NOT EXISTS`).

### 4.2 `stonesuite-notify` — request shape, validation, Create handler

- `CreateInput.Validate()`: `recipientUserId` required *unless* `recipientEmail` is non-empty.
  Existing internal recipients (owner pings, etc.) are unaffected — they still pass a real
  `userId` and validate exactly as before.
- New field `EmailBodyHTML string` on both the wire `createNotificationRequest`/`recipient`
  structs (actually top-level, not per-recipient — see §4.4 on why one request = one content) and
  `CreateInput`. Persisted nowhere new — passed straight through to the delivery worker via the
  existing attachment-style plumbing (in-memory at `Create` time; **must** still be persisted for
  retry, same reasoning as `notification_attachments` in the owner-ping design — see D3a below).
- `controllers.Handler.Create`: for each recipient, if `recipient.UserID == ""`, skip
  `PreferenceStore.Resolve` and use `preferences.Preferences{EmailEnabled: true, InAppEnabled:
  false, PushEnabled: false}` directly.

**D3a (resolved during planning, flagged here so it isn't missed):** `emailBodyHTML` can be
large (a full branded HTML email) and, like the PDF attachment, must survive a retry minutes
later — the worker can't rely on the original in-memory request. Extend the existing
`notification_attachments` table (already one row per notification, already read only by the
email delivery worker) with a nullable `email_body_html TEXT` column, rather than a new table —
both columns are "extra content only the email worker reads," so they belong together.

### 4.3 `stonesuite-notify` — delivery

`channels.SendNotificationEmail` gains an `emailBodyHTML string` parameter: when non-empty, use
it verbatim as the HTML body instead of calling `renderEmailHTML(title, body, link)`. Subject
stays `title` either way (Resend/SMTP both need a subject line separately from the HTML body).

### 4.4 `StoneSuite-Backend` — `documents.go` `Send`

Replace the `services.SendDocumentEmail(...)` call (step 2) with:

```go
recipients := make([]services.RecipientTarget, 0, len(to)+len(cc))
for _, addr := range append(append([]string{}, to...), normalizeRecipients(req.CC)...) {
    recipients = append(recipients, services.RecipientTarget{Email: addr})
}
err := services.SendNotification(r.Context(), services.NotificationRequest{
    TenantID:      tenant.ID,
    Recipients:    recipients,
    EventType:     "document.sent",
    Resource:      meta.WorkflowKey,
    ResourceID:    recordID,
    Title:         subject,
    Body:          "Document sent.", // short, audit-only — real content is EmailBodyHTML
    EmailBodyHTML: documentEmailHTML(doc, req.Message),
    Channels:      []string{"email"},
    Attachments:   []services.NotifyAttachment{{FileName: fileName, ContentType: "application/pdf", Content: pdf}},
})
if err != nil {
    fail(w, http.StatusBadGateway, "Failed to send email.")
    return
}
```

One request, one `recipients` array entry per `to`/`cc` address (D4) — Notify fans that out into
one notification+delivery row per address server-side. `notifyOwnerOfSend` (separate call, real
`userId`) is unchanged.

### 4.5 `StoneSuite-Backend` — cleanup

Delete `SendDocumentEmail`, `sendDocViaResend`, `buildMIME`, `writeQuotedPrintable`,
`EmailAttachment` from `services/email.go` (D6). `services.NotifyAttachment`/`RecipientTarget`
gain nothing new in shape (already had `Email`); `NotificationRequest` gains `EmailBodyHTML
string \`json:"emailBodyHtml,omitempty"\`` matching Notify's field name.

## 5. Known trade-off (accepted, documented for the frontend team)

A Resend/SMTP failure today fails `Send to Customer` synchronously. After this change,
`documents/send` only fails synchronously if Notify itself is unreachable or rejects the
request; once Notify accepts it (`201`), the actual send is async with retries over up to ~30
min between attempts (5 max, per Notify's existing `RetryWorker`). If every attempt fails (e.g.
a bad address), the document-send response already reported success — the failure only appears
in Notify's delivery log (`GET /api/admin/notifications/{id}/deliveries`), which nothing in the
frontend currently surfaces. Accepted as the intended reliability trade; a delivery-log UI is
out of scope for this spec.

## 6. Testing

- `stonesuite-notify`: table tests for `CreateInput.Validate()` (email-only recipient passes;
  no-userId-no-email fails); a store test for the nullable-column insert path (both directions);
  a `channels/email` test for `emailBodyHTML` override vs. generic template; a `controllers`
  test for `Create` skipping `PreferenceStore.Resolve` when `UserID == ""`.
- `StoneSuite-Backend`: update `controllers/documents_test.go` to mock `services.SendNotification`
  instead of `SendDocumentEmail`, covering the to+cc → one-recipient-per-address fan-out and the
  502-on-Notify-failure path; update `services/notify_test.go` for `EmailBodyHTML`.

## 7. Out of scope (Phase 2, separate spec)

Migrating the standalone transactional emails in `services/email.go` (password reset, onboarding
invite, user invite, portal invite, customer note confirmation) to Notify. Each of those has its
own question of whether a real `tenantId`/`recipientUserId` is available at that call site —
deliberately not re-litigated here.
