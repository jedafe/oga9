#!/usr/bin/env bash
# Ref: oga9-engineering/sections/09-deployment-editions.md — restore tooling.
# Usage: restore.sh <backup.dump> [uploads.tar.gz]  — restores into a CLEAN
# database; run on a throwaway DB first (CI restore-test job does exactly that).
set -euo pipefail
: "${OGA9_DATABASE_URL:?set OGA9_DATABASE_URL}"
DUMP="${1:?usage: restore.sh <dump> [uploads.tgz]}"
UPLOADS_TGZ="${2:-}"
UPLOADS_DIR="${OGA9_UPLOADS_DIR:-./data/uploads}"
pg_restore --clean --if-exists --no-owner --no-privileges --dbname "$OGA9_DATABASE_URL" "$DUMP"
[ -n "$UPLOADS_TGZ" ] && mkdir -p "$UPLOADS_DIR" && tar -xzf "$UPLOADS_TGZ" -C "$UPLOADS_DIR"
echo "restore complete"
