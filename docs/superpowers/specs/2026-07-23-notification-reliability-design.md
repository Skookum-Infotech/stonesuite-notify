# Notification Reliability Layer — Design

Status: Approved (design phase) — 2026-07-23
Branch: `feat-notification-reliability`

## 1. Goal

`stonesuite-notify` currently creates the in-app row synchronously and fires
email/push in a detached, fire-and-forget goroutine (`Handler.dispatchChannels`
in `controllers/notifications.go`). Failures are logged and lost; there is no
retry, no per-recipient enable/disable, and no record of what was actually
delivered.

This adds four must-have components on top of the existing architecture,
without new infrastructure:

1. **Preference Manager** — tenant-level defaults + per-user overrides for
   email/in-app/push.
2. **Queue Consumer** — claims newly-created delivery work.
3. **Retry Worker** — exponential backoff for failed deliveries, plus
   recovery of deliveries stuck mid-processing.
4. **Delivery Logs** — one row per (notification, channel) recording status
   history and provider response, queryable for support/debugging.

**Non-goals:** no external broker (Postgres only, confirmed via `go.mod` —
no message-queue dependency exists today), no dead-letter table (`failed` is
terminal and already queryable), no `LISTEN`/`NOTIFY` (ticker-based polling
only, consistent with the rest of the service's simplicity).

## 2. Architecture

```
Create (internal, per recipient)
    │
    ├─ preferences.Resolve(tenant, user) → (emailEnabled, inAppEnabled, pushEnabled)
    │
    ├─ notifications.Store.Create(...)              [always writes the row —
    │     recipient_email + visible_in_app=inAppEnabled   canonical content
    │                                                       record for every channel]
    │
    └─ deliveries.Store.Enqueue(...) — one row per channel:
          in_app  → written already-terminal (sent/skipped), no polling
          email   → pending  (only if caller requested "email" AND emailEnabled)
          push    → pending  (only if pushEnabled; skipped at send-time if no
                                subscriptions exist)

QueueConsumer (ticks ~2s)          RetryWorker (ticks ~15s)
   ClaimPending() —                   ClaimDue() —
   status='pending'                   status='retrying' AND next_attempt_at<=now()
   FOR UPDATE SKIP LOCKED              OR status='processing' stuck >5m
          │                                    │
          └──────────────┬─────────────────────┘
                          ▼
                 workers.attemptDelivery()
             (shared by both workers; resolves
              email/push at send-time, never
              trusts stale data on the row)
                          │
              ┌───────────┼───────────┐
              ▼           ▼           ▼
            sent      retrying      failed
       (provider_    (backoff,   (max attempts
        response      attempts++)  exhausted)
        recorded)
```

## 3. Components

### `preferences/` (new package, mirrors `pushsubs/` structure)
- `Store` interface: `Resolve(ctx, tenantID, userID) (email, inApp, push bool, err error)`, `GetTenantDefaults`, `SetTenantDefaults`, `GetUserOverride`, `SetUserOverride`.
- `PGStore` implementation. Resolution order: user override column (if non-NULL) → tenant default column (if a tenant row exists) → hardcoded default `true`. Missing rows at any level are not an error — they mean "inherit."
- Scope is **email + in-app only**, matching the original ask ("enable/disable email and in-app notifications"). Push is included as a third resolvable flag because the delivery/retry/logging pipeline treats it as a first-class channel alongside the other two, but push delivery still additionally depends on the recipient having at least one saved subscription — this is unchanged from today.

### `deliveries/` (new package)
- `Store` interface: `Enqueue(ctx, ...) (*Delivery, error)`, `ClaimPending(ctx, batchSize) ([]Delivery, error)`, `ClaimDue(ctx, batchSize) ([]Delivery, error)`, `MarkSent(ctx, id, providerResponse)`, `MarkRetrying(ctx, id, lastError, providerResponse)` (increments attempts, computes backoff, flips to `failed` if attempts exhausted), `ListForNotification(ctx, notificationID) ([]Delivery, error)`.
- `PGStore` implementation backed by `notification_deliveries`.
- `ClaimPending`/`ClaimDue` both use `UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED) RETURNING *` so two instances of either worker never double-claim a row.

### `workers/` (new package)
- `consumer.go` — `QueueConsumer`: ticks every 2s, calls `ClaimPending`, runs `attemptDelivery` for each claimed row.
- `retry.go` — `RetryWorker`: ticks every 15s, calls `ClaimDue` (due retries **and** the stale-processing sweep — see §5), runs `attemptDelivery` for each.
- `dispatch.go` — `attemptDelivery(ctx, deps, delivery, notification)`: the single place that calls `channels.SendNotificationEmail` / `channels.SendWebPush`, resolving the recipient's email/subscriptions fresh at send time (never trusting anything cached on the delivery row itself — the delivery row stores only `recipient_user_id` + `channel`, no address/endpoint). Stale-subscription pruning (404/410 → `pushsubs.Store.Delete`) moves here from `controllers.dispatchChannels`.
- Both workers are started in `main.go` as goroutines tied to the existing shutdown `ctx`, same lifecycle pattern as everything else in that file.

### `controllers/` changes
- `Handler.Create` (`controllers/notifications.go`): replaces the current "insert then `go dispatchChannels`" body with "resolve preferences → insert (always) → enqueue deliveries." The HTTP response shape is unchanged (still returns the created in-app rows); enqueue failures are logged, not surfaced, matching the existing "side channels never fail the create response" guarantee — see §6 for what happens if enqueueing itself fails.
- New `PreferencesHandler` (`controllers/preferences.go`): `GET /api/preferences` and `PUT /api/preferences` (self-service, `RequireAuth`), `PUT /api/tenant-defaults` (`RequireInternalSecret`, called by StoneSuite-Backend when an org's admin changes org-wide settings — this service has no admin RBAC of its own).
- New endpoint on the existing `Handler`: `GET /api/notifications/{id}/deliveries` (`RequireInternalSecret`) — returns every delivery row for one notification, for support/ops use ("did this actually get emailed").

## 4. Data flow (Create, per recipient)

1. Resolve `(emailEnabled, inAppEnabled, pushEnabled)` via `preferences.Resolve`. A DB error here fails closed for that recipient's optional channels only (see §6) — it never blocks the in-app row.
2. Call `notifications.Store.Create` — always writes the row (canonical record for every channel), with `recipient_email` set from the request and `visible_in_app = inAppEnabled`.
3. Enqueue delivery rows:
   - `in_app`: written directly as `sent` (if `inAppEnabled`) or `skipped` (if not) — synchronous, since the "delivery" is just the row that already exists; never touched by a worker.
   - `email`: enqueued `pending` only if the request's `channels` included `"email"` **and** `emailEnabled`.
   - `push`: enqueued `pending` if `pushEnabled` (subscription existence is checked later, at send time, not here).
4. Workers pick up `pending`/`retrying` rows on their own schedule and call `attemptDelivery`, which resolves the actual address/subscriptions at that moment and records the outcome.

## 5. Database schema (consolidated, final)

```sql
-- notifications: two additive columns
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS recipient_email VARCHAR(320) NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS visible_in_app BOOLEAN NOT NULL DEFAULT true;

-- tenant-wide defaults; absence of a row means "all enabled"
CREATE TABLE IF NOT EXISTS tenant_notification_defaults (
    tenant_id      UUID PRIMARY KEY,
    email_enabled  BOOLEAN NOT NULL DEFAULT true,
    in_app_enabled BOOLEAN NOT NULL DEFAULT true,
    push_enabled   BOOLEAN NOT NULL DEFAULT true,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- per-user overrides; NULL column = inherit tenant default
CREATE TABLE IF NOT EXISTS user_notification_preferences (
    tenant_id      UUID NOT NULL,
    user_id        UUID NOT NULL,
    email_enabled  BOOLEAN,
    in_app_enabled BOOLEAN,
    push_enabled   BOOLEAN,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

-- queue + delivery log, single source for both
CREATE TABLE IF NOT EXISTS notification_deliveries (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id   UUID NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    tenant_id         UUID NOT NULL,
    recipient_user_id UUID NOT NULL,
    channel           VARCHAR(16) NOT NULL,               -- 'email' | 'push' | 'in_app'
    status            VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|processing|sent|retrying|failed|skipped
    attempts          INT NOT NULL DEFAULT 0,
    max_attempts      INT NOT NULL DEFAULT 5,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error        TEXT,
    provider_response JSONB,                              -- push: per-endpoint outcomes, see below
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_deliveries_claim
    ON notification_deliveries (status, next_attempt_at);

CREATE INDEX IF NOT EXISTS idx_deliveries_notification
    ON notification_deliveries (notification_id);
```

No `target` column (email address / push endpoint) is stored on
`notification_deliveries` — confirmed instruction. Email address comes from
`notifications.recipient_email`; push subscriptions are looked up fresh from
`push_subscriptions` at send time via `(tenant_id, recipient_user_id)`.

**Push per-endpoint tracking:** since a recipient can have multiple devices
but the delivery row is per-(notification, recipient, channel) rather than
per-device, `provider_response` for a push delivery holds a JSON map keyed by
`push_subscriptions.id`:
```json
{"endpoints": {"<sub-id-1>": {"status": "sent"}, "<sub-id-2>": {"status": "failed", "error": "..."}}}
```
On retry, `attemptDelivery` only re-targets endpoints not already marked
`sent` in this map (and re-lists current subscriptions, so a device removed
since the last attempt is naturally dropped). The delivery reaches `sent`
once every currently-subscribed endpoint has succeeded at least once.

## 6. Error handling & edge cases

- **Preference resolution DB error:** log and treat as "all enabled" (fail open) for that recipient rather than blocking notification creation — an outage in the preferences table must never prevent the in-app row (source of truth) from being written.
- **Enqueue failure** (DB error writing a `notification_deliveries` row after the `notifications` row already committed): logged; the in-app notification still exists and is returned in the response. That channel simply never gets a delivery row, which is visible as "missing" in the delivery log — an acceptable, observable degradation rather than failing the whole create call over a side effect.
- **Concurrent claiming:** `ClaimPending`/`ClaimDue` claim-and-flip-to-`processing` in one `UPDATE ... RETURNING` statement using `FOR UPDATE SKIP LOCKED`, so multiple worker instances (future horizontal scaling) never process the same row twice.
- **Crash mid-processing:** a row stuck in `processing` is picked up by `RetryWorker`'s claim query once `updated_at` is more than 5 minutes old, and treated as a failed attempt (attempts++, backoff applied) — no separate sweep job needed.
- **Push subscription gone (404/410):** unchanged from today — pruned via `pushsubs.Store.Delete`, now invoked from `workers.attemptDelivery` instead of `controllers.dispatchChannels`. That endpoint's entry in `provider_response` is recorded as failed; it's simply absent from the next attempt's subscription list (deleted), so it won't be retried.
- **Retries exhausted:** `status='failed'` is terminal. No dead-letter table — the `failed` row itself, queryable via the delivery-log endpoint, is the record for support/debugging.
- **Backoff formula:** `backoff(n) = min(30m, 1m * 2^(n-1))` where `n` is the attempt count after increment; `max_attempts=5` total attempts before terminal `failed`.
- **Tenant defaults changed after user overrides exist:** no special handling needed — overrides always win when non-NULL, so this falls out naturally from the resolution order.

## 7. Testing plan

- `preferences`: table-driven tests for `Resolve` — no rows (all true), tenant-only row, user override partially set (mixed inherit/override), fully overridden.
- `deliveries`: `ClaimPending`/`ClaimDue` return only eligible rows and atomically flip status (verified via concurrent-claim test — two goroutines claiming from the same pending set must not get overlapping rows); `MarkSent`/`MarkRetrying` status transitions including the exhausted-attempts → `failed` path.
- `workers`: `attemptDelivery` tested against a fake `channels` sender (inject success / error / stale-subscription) with an injected clock — verifies status transitions and the backoff formula (`backoff(1)=1m`, `backoff(5)=16m`, capped at 30m) without real network calls or real sleeps; push per-endpoint partial-failure case (2 devices, 1 succeeds 1 fails → `retrying`, next attempt only re-targets the failed one).
- `controllers`: extend existing fakes in `notifications_test.go` to cover preference-gated enqueue (in-app visibility, email/push enqueued-or-not per resolved preference) and new `PreferencesHandler` tests.
- Integration: `go test ./...` against a real local Postgres, same as today (no test-DB abstraction exists in the repo currently, and none is being added — consistent with "no additional infrastructure").

## 8. API additions

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/api/preferences` | user | Effective + override preferences for the calling user |
| PUT | `/api/preferences` | user | Set the calling user's own overrides (partial; omitted fields stay inherited) |
| PUT | `/api/tenant-defaults` | internal secret | Set a tenant's org-wide defaults |
| GET | `/api/notifications/{id}/deliveries` | internal secret | Delivery log rows for one notification |

## 9. Configuration

No new environment variables. Poll intervals, batch size, and max attempts
are unexported constants in the `workers` package (2s consumer tick, 15s
retry tick, 5-minute stale-processing threshold, batch size 20, 5 max
attempts) — consistent with the existing "hardcode what doesn't need to vary
per deployment" pattern in this codebase.
