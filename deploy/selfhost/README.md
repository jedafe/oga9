# OGA9 Self-Hosted Bundle

<!-- Ref: oga9-engineering/sections/09-deployment-editions.md -->

One host, four containers: `api` (API + workers + scheduler, one binary),
`postgres` (pgvector/pg18), `valkey`, and a one-shot `migrate` job.
The control app is excluded (ADR-018) — operator functions are not part
of this edition.

## Contents

- `docker-compose.yml` — the full stack
- `install.sh` / `upgrade.sh` — lifecycle tooling
- `manifest.yaml` — pinned service versions for this bundle
- `.env.example` — configuration template
- `migrations/` — DB migrations applied by the `migrate` service
- `license.json` — signed license file (community installs ship `{}`)

## Requirements

- Docker + Docker Compose v2
- 2 vCPU / 2 GB RAM minimum
- A reverse proxy for TLS (Caddy, nginx, Traefik) — the API binds to
  loopback by default

## Install

```sh
cp .env.example .env   # edit: OGA9_JWT_SECRET, OGA9_DB_PASSWORD (required)
./install.sh           # brings the stack up; migrate runs first
curl -fsS http://127.0.0.1:8080/health
```

`install.sh` refuses to run with an unedited `.env` — secrets are never
defaulted.

## Upgrade

```sh
./upgrade.sh 1.0.1     # re-tags OGA9_VERSION, runs migrate, restarts api
```

Migrations run before the new API starts (compose dependency ordering).
Rollback = `./upgrade.sh <previous-tag>` — schema changes are additive;
check release notes for exceptions.

## Backup & restore

Run on the host with a postgres 18 client, or inside the postgres
container:

```sh
# host (needs pg_dump/pg_restore 18)
OGA9_DATABASE_URL="postgres://oga9:$OGA9_DB_PASSWORD@127.0.0.1:5432/oga9?sslmode=disable" \
  scripts/backup.sh          # custom-format dump + uploads tarball
scripts/restore.sh <dump> [uploads.tgz]   # restore into a CLEAN database

# or via compose (no host tooling needed)
docker compose exec postgres pg_dump -U oga9 -Fc oga9 > backup.dump
```

`$OGA9_BACKUP_DIR` controls backup output (default `./backups`).

## License

Community installs run without a license. Premium: drop the signed file
at `OGA9_LICENSE_FILE_HOST` and set `OGA9_LICENSE_PUBLIC_KEY`. The file
verifies OFFLINE — a control-plane outage never blocks the deployment;
past `license.offline_grace_days` the deployment logs a degradation
warning per the §09 matrix. Issue/download licenses from the cloud
control plane (`GET /platform/licenses/{deployment_id}/file`).

## Reverse proxy (TLS)

Point your proxy at `127.0.0.1:8080`. Caddy example:

```caddy
oga9.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Set `OGA9_BASE_DOMAIN`, `OGA9_BASE_URL`, `OGA9_APP_URL` to the public
hostname and add the proxy to `OGA9_TRUSTED_PROXIES` so rate limits
count the real client IP.

## Troubleshooting

- `docker compose logs -f api` — structured JSON logs (one line per
  request/job)
- `curl -fsS :8080/health` — DB + cache liveness
- `docker compose ps` — the `migrate` service must show
  `exited (0)`; non-zero means a migration failed (see
  `docker compose logs migrate`)
