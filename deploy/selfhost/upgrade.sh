#!/usr/bin/env bash
# Ref: §09 upgrade: pull new image tag → migrations run first (one-shot
# migrate service) → api restarts on the new schema. Rollback = redeploy
# the previous tag (schema is additive; see release notes for exceptions).
set -euo pipefail
cd "$(dirname "$0")"
NEW="${1:?usage: upgrade.sh <oga9-version>}"
sed -i "s/^OGA9_VERSION=.*/OGA9_VERSION=$NEW/" .env
docker compose --env-file .env up -d --no-deps migrate
docker compose --env-file .env up -d api
echo "upgraded to $NEW — verify: curl -fsS http://127.0.0.1:8080/health"
