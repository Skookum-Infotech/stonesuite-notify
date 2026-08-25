#!/usr/bin/env bash
# Stops the local dev Postgres/Mailpit containers started by dev-up.sh.
# Pass -v to also delete the Postgres data volume (wipes local test data).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
docker compose -f docker-compose.dev.yml down "$@"
