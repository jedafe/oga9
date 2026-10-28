#!/usr/bin/env bash
# Ref: oga9-engineering/sections/09-deployment-editions.md — backup tooling.
# pg_dump (custom format) + uploads dir tarball under $OGA9_BACKUP_DIR.
set -euo pipefail
: "${OGA9_DATABASE_URL:?set OGA9_DATABASE_URL}"
BACKUP_DIR="${OGA9_BACKUP_DIR:-./backups}"
UPLOADS_DIR="${OGA9_UPLOADS_DIR:-./data/uploads}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
pg_dump --format=custom --file "$BACKUP_DIR/oga9-$STAMP.dump" "$OGA9_DATABASE_URL"
[ -d "$UPLOADS_DIR" ] && tar -czf "$BACKUP_DIR/oga9-$STAMP-uploads.tar.gz" -C "$UPLOADS_DIR" .
echo "backup: $BACKUP_DIR/oga9-$STAMP.*"
