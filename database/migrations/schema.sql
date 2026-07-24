-- Notifications schema. Editing this file IS the migration: every statement
-- is idempotent (CREATE ... IF NOT EXISTS) so ApplySchema can run on every
-- boot against any existing DB state, mirroring StoneSuite-Backend's
-- tenant-schema convention.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS notifications (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    recipient_user_id UUID NOT NULL,
    actor_user_id     UUID,
    event_type        VARCHAR(64)  NOT NULL,
    resource          VARCHAR(64)  NOT NULL,
    resource_id       UUID         NOT NULL,
    title             VARCHAR(200) NOT NULL,
    body              VARCHAR(500) NOT NULL DEFAULT '',
    link              VARCHAR(300) NOT NULL DEFAULT '',
    read_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Serves both the unread-count poll and the dropdown list, which always
-- filter by (tenant_id, recipient_user_id) and sort newest first.
CREATE INDEX IF NOT EXISTS idx_notifications_recipient
    ON notifications (tenant_id, recipient_user_id, created_at DESC);

-- Partial index for the unread-count poll specifically — smaller and cheaper
-- to maintain than indexing every row, since read notifications never
-- appear in that query.
CREATE INDEX IF NOT EXISTS idx_notifications_unread
    ON notifications (tenant_id, recipient_user_id)
    WHERE read_at IS NULL;

-- One row per browser/device the user has granted push permission on.
-- endpoint is the full Push API subscription URL and is unique per
-- browser+device, so re-subscribing (e.g. after clearing site data)
-- naturally replaces the stale row via ON CONFLICT upsert.
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    user_id     UUID NOT NULL,
    endpoint    TEXT NOT NULL UNIQUE,
    p256dh_key  TEXT NOT NULL,
    auth_key    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user
    ON push_subscriptions (tenant_id, user_id);
