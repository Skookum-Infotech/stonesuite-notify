# stonesuite-notify

Standalone notifications microservice for StoneSuite. It persists in-app notifications, serves them to the frontend bell, and fans events out over email and Web Push. It does not own user identity — it trusts sessions minted by StoneSuite-Backend (same JWT secret) rather than handling login itself.

## Requirements

- Go 1.25+
- PostgreSQL

## Configuration

The service reads all configuration from environment variables (see [config/config.go](config/config.go)).

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | Postgres connection string |
| `JWT_SECRET` | yes | Must match StoneSuite-Backend's `JWT_SECRET` |
| `INTERNAL_SERVICE_SECRET` | yes | Shared secret for service-to-service notification creation |
| `PORT` | no | Default `8090` |
| `CORS_ORIGIN` | no | Comma-separated allowlist of browser origins |
| `RESEND_API_KEY` | no | Email via Resend (tried first) |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `EMAIL_FROM` | no | Email via SMTP (fallback if no Resend key) |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT` | no | Web Push (VAPID) |

Email and push are optional channels — if unconfigured, the service still runs and simply skips that channel (logged); in-app notifications keep working.

## Running

```bash
go run .
```

The database schema (`database/migrations/schema.sql`) is embedded and applied automatically on every boot — every statement is idempotent, so this is safe to run repeatedly.

## Testing

```bash
go test ./...
```

## API

All responses are wrapped in `{"success": bool, "message"?: string, "data"?: object}`.

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/healthz` | none | Liveness check |
| GET | `/readyz` | none | Readiness check (pings the database) |
| GET | `/api/notifications/summary` | user | Unread count for the bell badge |
| GET | `/api/notifications` | user | List notifications (`unreadOnly`, `limit` query params) |
| POST | `/api/notifications/{id}/read` | user | Mark one notification read |
| POST | `/api/notifications/read-all` | user | Mark all notifications read |
| POST | `/api/notifications/internal` | internal secret | Create notification(s) for one or more recipients |
| GET | `/api/push/vapid-public-key` | none | Public VAPID key for `PushManager.subscribe()` |
| POST | `/api/push/subscribe` | user | Save a browser's push subscription |
| DELETE | `/api/push/subscribe` | user | Remove a push subscription |

User-authenticated routes accept a JWT via `Authorization: Bearer <token>` or the `auth_token` cookie. The internal create endpoint is gated by an `X-Internal-Secret` header instead.
