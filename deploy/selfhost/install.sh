#!/usr/bin/env bash
# Ref: §09 self-hosted install: copy bundle, set secrets, bring stack up.
# Reverse proxy/TLS: terminate upstream (e.g. Caddy `oga9.example.com {
# reverse_proxy 127.0.0.1:8080 }`); OGA9_BIND defaults to loopback.
set -euo pipefail
cd "$(dirname "$0")"
[ -f .env ] || { cp .env.example .env; echo "created .env — EDIT IT (secrets), then re-run"; exit 1; }
[ -f ./license.json ] || printf '{}' > ./license.json  # community install — mount point must exist
[ -d ./migrations ] || { echo "missing ./migrations — unpack the release bundle or set OGA9_MIGRATIONS_DIR"; exit 1; }
docker compose --env-file .env up -d
echo "health: curl -fsS http://127.0.0.1:8080/health"
