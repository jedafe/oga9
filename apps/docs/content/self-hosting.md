---
title: Self-Hosting
order: 10
---

# Self-Hosting OGA9

The community edition is AGPLv3, free forever on your own infrastructure:
unlimited members, one organization, one member-capable branch, core apps.

## Requirements

- Docker + Docker Compose
- ~1 GB RAM (a $5 VPS is enough)
- A domain name (optional but recommended)

## Quick start

```bash
git clone --recurse-submodules https://github.com/jedafe/oga9
cd oga9
docker compose up -d
```

The compose stack brings up Postgres 18, Valkey, the API, and Mailpit
(dev SMTP). Production deployments use the full compose profile.

## Licensing

Multi-branch, SSO, and support tiers activate via a signed license file —
no always-online checks (the platform works even if OGA9's control plane
is unreachable).

## Migrating from OGA9 Cloud

A cloud tenant can move wholesale to self-hosted. From the owner org's
admin console, request a tenant export (`POST /api/v1/tenants/{id}/exports`),
download the `.oga9exp` bundle, then import on the self-hosted install:

```bash
docker compose exec api oga9-admin import \
  --bundle /path/to/tenant-export.oga9exp \
  --dsn "$OGA9_DATABASE_URL" \
  --pubkey "$OGA9_LICENSE_PUBLIC_KEY"
```

The bundle is platform-signed (ed25519) — import verifies the signature
and per-table checksums before loading. Users are copied without
password hashes by default (they reset on first login); choosing
`include_credentials` at export time embeds them in a passphrase-
encrypted bundle (`--passphrase` at import).

Back on cloud, confirm the migration to apply the post-state picked at
export: `readonly_grace` (default, read-only), `purge` (hard delete after
the erasure grace window, double-confirmed), or `keep_active`.
