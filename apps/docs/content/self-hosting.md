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
