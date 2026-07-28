#!/usr/bin/env bash
# Brings up a self-contained local dev environment for stonesuite-notify —
# Postgres + Mailpit via docker-compose.dev.yml, a generated .env.local
# (JWT/internal secrets + a VAPID keypair, created once on first run and
# reused after that so tokens/subscriptions keep working across restarts) —
# then runs the service in the foreground.
#
# Usage:   scripts/dev-up.sh
# Stop:    Ctrl+C, then scripts/dev-down.sh to stop Postgres/Mailpit too
#          (add -v to also delete the Postgres data volume).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

ENV_FILE=".env.local"

if [ ! -f "$ENV_FILE" ]; then
  echo "No $ENV_FILE yet — generating one (dev-only secrets, gitignored) ..."
  VAPID_KEYS=$(go run ./scripts/genvapid)
  {
    echo "DATABASE_URL=postgres://notify:notify@localhost:5544/stonesuite_notify?sslmode=disable"
    echo "JWT_SECRET=dev_jwt_secret_$(date +%s)"
    echo "INTERNAL_SERVICE_SECRET=dev_internal_secret_$(date +%s)"
    echo "SMTP_HOST=localhost"
    echo "SMTP_PORT=1025"
    echo "EMAIL_FROM=notify@stonesuite.local"
    echo "$VAPID_KEYS"
    echo "VAPID_SUBJECT=mailto:dev@stonesuite.local"
  } > "$ENV_FILE"
  echo "Wrote $ENV_FILE. If you need tokens minted here to validate against a real"
  echo "StoneSuite-Backend instance too, edit JWT_SECRET to match its JWT_SECRET."
fi

echo "Starting Postgres + Mailpit ..."
docker compose -f docker-compose.dev.yml up -d

echo -n "Waiting for Postgres ..."
until docker compose -f docker-compose.dev.yml exec -T postgres pg_isready -U notify >/dev/null 2>&1; do
  echo -n "."
  sleep 1
done
echo " ready"

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

echo
echo "Mailpit UI:  http://localhost:8025"
echo "Service:     http://localhost:${PORT:-8090}"
echo "Smoke test:  scripts/smoke-test.sh (once the service below is up)"
echo
exec go run .
