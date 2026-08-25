# Document-Send Owner Notification — Cross-Repo Design Spec

**Date:** 2026-08-25 · **Status:** Approved architecture; ready for implementation planning
**Author:** Backend/notify architecture pass (Claude)
**Repos affected:** `StoneSuite-Backend` (this repo) and `stonesuite-notify` (sibling service,
`C:\Users\Lenovo\stonesuite-notify`, branch `feat/notify-platform`). This spec is committed
identically to both repos' `docs/superpowers/specs/` — each repo's history should show the
decision that affected it, since they cannot share a single commit.

---

## 1. Problem

Two separate things were found tangled together in a pre-existing, uncommitted WIP on the
backend (`services/notify.go`, and edits to `services/email.go`/`config/config.go`/
`.env.example` present before this work started):

1. A generic attempt to route **all** outbound backend email through `stonesuite-notify`
   (`sendEmail()` in `services/email.go`), using hardcoded placeholder `"system"`
   tenant/user ids.
2. No connection at all between the just-built Documents feature (`controllers/documents.go`
   `Send` — see `2026-08-24-phase-1b-documents-design.md`) and the notify service, even
   though sending a document to a customer is exactly the kind of business event the notify
   service exists to surface to staff.

Investigating (1) surfaced that it doesn't fit: `stonesuite-notify`'s data model requires a
real `tenantId` + `recipientUserId` per notification (`notifications.CreateInput.Validate()`),
and most of backend's current transactional-email call sites (password reset, onboarding
invite, portal invite) fire before a real `users.id` exists or don't thread one down to
`sendEmail()`. Forcing them through Notify with placeholder ids would either 400 at the
notify service or (worse) succeed against a bogus tenant/user pairing.

## 2. Decisions

| # | Decision | Choice |
|---|---|---|
| D1 | Generic `sendEmail()` → Notify routing | **Revert.** Not every email has a real tenant+user id available at that call site; forcing it breaks more than it fixes. `sendEmail()` goes back to Resend → SMTP → no-op only. |
| D2 | Who does Notify actually address for documents | **The record's internal owner**, not the customer. The customer already gets the PDF directly (Resend/SMTP, already built and reviewed in Phase 1b). Notify pings the owner's real `users.id` that the send happened. |
| D3 | Does the owner notification carry the PDF | **Yes** — the owner's notification email should include the same PDF attachment the customer received, not just a text status line. |
| D4 | Where the attachment lives in `stonesuite-notify` | **A new, narrowly-scoped table** (`notification_attachments`), not a column on `notifications`. That table is read by list/summary/feed endpoints backing the in-app bell UI (`body VARCHAR(500)`, no binary column) — bolting PDF bytes onto it risks bloating or leaking through those unrelated read paths. The attachment is only ever read by the email delivery worker. |

## 3. Current state (verified directly against both repos' code, not assumed)

- The backend's `NotificationRequest`/`RecipientTarget` JSON shape already matches the
  notify service's `createNotificationRequest`/`recipientTarget` field-for-field — this was
  initially misdiagnosed as a shape mismatch; it isn't. The **one real bug** is the URL:
  backend posts to `/api/internal/notifications`; the service's actual route is
  `POST /api/notifications/internal` (`stonesuite-notify/main.go:160`).
- `X-Api-Key` is the correct header name (`middleware.RequireServiceKey`,
  `stonesuite-notify/middleware/auth.go:211,216`) — a structured token
  (`nk_<env>_<id>_<secret>`, `apikeys.FormatToken`/`ParseToken`), not a bare secret.
  `Environment` is derived from the key itself server-side
  (`serviceKey.Environment`, `stonesuite-notify/controllers/notifications.go:269`) — the
  caller never sends it explicitly. `stonesuite-notify`'s `CLAUDE.md` describes an older
  shared-secret (`X-Internal-Secret`) model; the code has since moved to scoped API keys
  (commit `0fd7af8`) and is the authority here.
- Delivery is asynchronous: `Create` writes the `notifications` row and `pending`
  `notification_deliveries` rows synchronously, then returns; `workers.QueueConsumer`
  (2s poll) and `workers.RetryWorker` (15s poll, exponential backoff, `MaxAttempts = 5`)
  actually send, via `attemptDelivery` → `attemptEmail`/`attemptPush`
  (`stonesuite-notify/workers/dispatch.go`). This is why the attachment must be **persisted**
  (D4), not just passed in-memory through the `Create` call — a retry minutes later still
  needs the bytes.
- Backend already resolves the record owner's real `users.id` on every document-endpoint
  call: `workflow.ResolveRecordAccess` → `RecordAccessInfo.OwnerUserID`
  (`workflow/attachments.go:120`), consumed today by `authRecordAccess`
  (`controllers/attachments.go`) but not currently propagated out of
  `DocumentOps.loadForRender` (`controllers/documents.go`).
- Per Phase 1b's own revised decision (§10 of the Documents spec), **the PDF is never
  persisted to R2** — it exists only as in-memory bytes during the `Send` request. This
  is why the notify-service attachment must travel synchronously in the `Create` call
  body (base64, like the customer email already does via `services.SendDocumentEmail`),
  not as a reference/URL to a stored object.

## 4. Architecture

### 4.1 `stonesuite-notify` — schema

Append to `database/migrations/schema.sql` (idempotent, matches this repo's own
"editing that file *is* the migration" convention — every statement `IF NOT EXISTS`):

```sql
CREATE TABLE IF NOT EXISTS notification_attachments (
    notification_id UUID PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
    file_name        TEXT NOT NULL,
    content_type     TEXT NOT NULL,
    content          BYTEA NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

One optional row per notification (1:1, hence `notification_id` as the PK, not a
separate surrogate key — a notification has at most one attachment in this design).
`ON DELETE CASCADE` so the attachment can never outlive its notification row. Never
selected by `notifications.Store`'s list/summary/history queries — only by a new,
narrow accessor used exclusively by the email worker.

### 4.2 `stonesuite-notify` — request shape, store, Create handler

- `notifications.AttachmentInput{FileName, ContentType string; Content []byte}` — new type.
- `notifications.CreateInput` gains `Attachment *AttachmentInput` (nil for the overwhelming
  majority of notifications, which have none).
- `controllers.createNotificationRequest` gains a matching `Attachment *struct{FileName,
  ContentType string; ContentBase64 string} \`json:"attachment,omitempty"\`` on the wire
  (base64 over JSON, same convention `services.SendDocumentEmail`'s Resend path already
  uses on the backend side) — decoded to bytes before building `CreateInput`.
- `notifications.Store` gains `SaveAttachment(ctx, notificationID string, a AttachmentInput)
  error` and `GetAttachment(ctx, notificationID string) (*AttachmentInput, error)`
  (`nil, nil` when none exists — not an error case).
- `controllers.Handler.Create`: after `h.Store.Create(...)` succeeds for a recipient, if
  `req.Attachment != nil` **and** that recipient's resolved preferences/requested channels
  include email, call `h.Store.SaveAttachment(...)`. Skip entirely otherwise — no point
  persisting a PDF for a push-only or in-app-only notification that will never read it.

### 4.3 `stonesuite-notify` — delivery

- `workers.attemptEmail` (`workers/dispatch.go`): before calling `deps.SendEmail`, calls
  `deps.Notifications.GetAttachment(ctx, n.ID)` (new field on `Deps`, defaulting to the
  real store method, injectable for tests like the rest of `Deps`). Passes the result
  (possibly nil) through to `SendEmail`.
- `channels.SendNotificationEmail` signature gains a trailing `attachment
  *AttachmentInput` (or an equivalent small struct in the `channels` package to keep it
  dependency-free of `notifications` — implementer's call, consistent with existing
  package boundaries) parameter. `sendViaResend` adds the `attachments` array (base64)
  to its payload when present; `sendViaSMTP` builds a `multipart/mixed` message (HTML +
  base64 attachment part) instead of the current bare HTML body when present — mirroring
  `services/email.go`'s `buildMIME` on the backend side, so both services carry the same
  approach rather than inventing a second MIME-assembly style.

### 4.4 StoneSuite-Backend — revert D1, fix the URL, wire D2/D3

- `services/email.go`: remove the `NotifyURL`/`NotifyAPIKey` branch added to `sendEmail()`.
  `sendEmail()` returns to exactly its pre-WIP form (Resend → SMTP → no-op). No behavior
  change to any of `SendOnboardingInviteEmail`, `SendPasswordSetupEmail`,
  `SendUserInviteEmail`, `SendPortalInviteEmail`, `SendPasswordResetEmail`,
  `SendCustomerPortalInviteEmail`, `SendCustomerNoteConfirmationEmail`.
- `services/notify.go`: keep `SendNotification`/`NotificationRequest`/`RecipientTarget` as
  a general-purpose helper (still useful for future real tenant+user call sites). Fix the
  URL: `%s/api/notifications/internal` (was `%s/api/internal/notifications`). Add an
  `Attachments []NotifyAttachment{FileName, ContentType string; Content []byte}` field to
  `NotificationRequest`, marshaled as base64 on the wire (mirrors §4.2's
  `createNotificationRequest.Attachment` — singular on the notify side since one
  notification has at most one attachment, but the backend's helper can stay generically
  named/shaped as a slice if that's more natural for `services.SendDocumentEmail`'s
  existing `[]EmailAttachment` idiom; implementer's call, just ensure the JSON body
  ultimately matches what `createNotificationRequest` decodes).
- Keep the `NotifyURL`/`NotifyAPIKey` additions to `config/config.go` and `.env.example` —
  still needed by `services.SendNotification`.
- `controllers/documents.go`:
  - `DocumentOps.loadForRender` — extend its return to also surface the record's
    `OwnerUserID` (already resolved inside `authRecordAccess`/`ResolveRecordAccess`,
    currently discarded before reaching the caller).
  - `Send`: after the existing `workflow.InsertDocumentSend` + `workflow.LogAudit` calls,
    if `ownerUserID != ""`, best-effort call `services.SendNotification` with:
    `TenantID: tenant.ID`, `Recipients: []services.RecipientTarget{{UserID: ownerUserID}}`,
    `EventType: "document.sent"`, `Resource: meta.WorkflowKey`, `ResourceID: recordID`,
    `Title:` e.g. `d.Kind + " " + meta.Number + " sent"`, `Body:` e.g.
    `"Sent to " + joinRecipients(to)`, `Channels: []string{"email"}`, and the same in-memory
    `pdf` bytes + `fileName` already produced for the customer email, as the attachment.
    A `SendNotification` error is logged (`log.Printf`), never surfaced to the HTTP
    response — the document-send has already succeeded and been recorded by this point;
    a Notify outage must not undo that.

## 5. Data flow (document send, both emails)

```
DocumentOps.Send
  ├─ render PDF (docpdf.Render)                          [unchanged from Phase 1b]
  ├─ email customer (services.SendDocumentEmail)          [unchanged from Phase 1b]
  ├─ workflow.InsertDocumentSend                          [unchanged from Phase 1b]
  ├─ workflow.LogAudit                                    [unchanged from Phase 1b]
  └─ if ownerUserID != "": services.SendNotification      [NEW]
        └─ POST stonesuite-notify /api/notifications/internal
              ├─ notifications.Store.Create               (row created, synchronous)
              ├─ notifications.Store.SaveAttachment        (PDF persisted, synchronous)
              └─ enqueue pending email delivery            (synchronous)
                    └─ workers.QueueConsumer (~2s later)
                          └─ attemptEmail
                                ├─ GetAttachment            (PDF reloaded)
                                └─ channels.SendNotificationEmail (+ attachment)
```

## 6. Security & tenancy notes

- `notification_attachments.content` is `BYTEA`, only ever queried by
  `notification_id` (never listed/scanned in bulk) and only by the internal email worker
  — no new HTTP-reachable read path is introduced for it.
- The backend's `Send` handler already runs the full RBAC/IDOR gate
  (`authRecordAccess`) before this notification is even considered — `ownerUserID` is
  sourced from that same trusted resolution, not from client input.
- Cross-service auth is unchanged: the scoped `X-Api-Key` token already carries the
  caller's environment; this feature adds no new credential or trust boundary.
- `stonesuite-notify`'s own design already treats channel delivery as fire-and-forget
  from the caller's perspective (`channels/` package doc comment) — this feature's
  backend-side best-effort call follows that same principle on the calling side too.

## 7. Testing

- **`stonesuite-notify`:** table test for `AttachmentInput` JSON round-trip; store test
  (`SaveAttachment`/`GetAttachment`, including the nil/no-attachment case) — dbtest,
  matching this repo's existing store-test pattern; `attemptEmail` test with a fake
  `Notifications.GetAttachment` returning a non-nil attachment, asserting it's passed to
  the fake `SendEmail`; `channels.SendNotificationEmail`/`sendViaSMTP`/`sendViaResend`
  attachment-path tests, mirroring `services/email.go`'s existing MIME/Resend tests on
  the backend side.
- **StoneSuite-Backend:** `services/notify.go` — request-marshal test asserting the
  corrected URL and the attachment field's JSON shape. `controllers/documents.go` — extend
  the existing `Send` test coverage with an injected fake `SendNotification` (mirroring
  the existing `renderPDF`-injection seam) asserting: called with the right owner id when
  one exists, not called when the record has no owner, and a failure from it doesn't fail
  the HTTP response.
- **Cross-repo:** no automated cross-repo integration test (out of scope, no shared CI
  today) — verified manually against a locally-running `stonesuite-notify` instance
  during implementation, per that repo's own `README`/`CLAUDE.md` run instructions.

## 8. Explicitly out of scope (YAGNI)

- Attachment support for push notifications — email only, per D3's actual ask (a readable
  status ping with the PDF), and push payloads aren't suited to binary attachments anyway.
- Any change to `notifications`/list/summary/history endpoints or the in-app feed UI.
- Reworking any other backend call site to pass a real tenant+user id through
  `sendEmail()`/Notify (D1 reverts that entirely; a future, separate effort could
  selectively convert individual call sites that do have real ids, but that's not this).
- Generalizing `stonesuite-notify`'s attachment support beyond "at most one file per
  notification" (multiple attachments, non-PDF types) — not needed by this call site.

## 9. Net new surface

**stonesuite-notify:** 1 table (`notification_attachments`), 1 new type
(`AttachmentInput`), 2 new store methods, 1 extended request/handler path, 1 extended
`Deps` field, 1 extended channel-sender signature with Resend/SMTP attachment support.
**StoneSuite-Backend:** revert of the `sendEmail()` Notify branch (net code removal), 1
URL fix + 1 new field in `services/notify.go`, 1 extended `DocumentOps.loadForRender`
return value, ~15 new lines in `Send` for the best-effort owner notification.
