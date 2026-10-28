# OGA9 — Organizations and Group Administrator/Applications

Go modular monolith + SvelteKit clients + Flutter mobile (post-MVP).

**Spec repo:** [jedafe/oga9-engineering](https://github.com/jedafe/oga9-engineering) — included here as the `.oos/` submodule (OGA9-OS).

## Layout

```
.oos/       ← OGA9-OS spec submodule (read-only — edit spec in its own repo)
cmd/api/    backend entry point
internal/   bounded-context packages
apps/main/  SvelteKit webapp + PWA (all user interfaces)
apps/control/ → private jedafe/oga9-cloud repo (ecosystem ops, cloud-only — ADR-025)
apps/mobile/  Flutter (post-MVP)
db/         migrations + sqlc queries
```

## Spec-first rule

Read `.oos/OGA9-OS.md` (master index) → `AGENTS.md` → your roadmap → the
referenced `sections/` file before writing code. The spec is updated before
the code — never let them drift.

## Submodule workflow

```bash
git submodule update --init        # first clone
git submodule update --remote      # pull latest spec
# spec changes: cd .oos, edit/commit/push there, then bump pointer here
```

## License

AGPLv3 (community edition). Commercial terms apply to enterprise and
licensed self-hosted tiers (ADR-010); cloud-only operator surfaces live in
the private `jedafe/oga9-cloud` repo (ADR-025).
