# Changelog

All notable changes to the OGA9 platform. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
entries reference engineering phases in `.oos/roadmaps/ROADMAP-BACKEND.md`.

## [Unreleased]

### Added — docs surface
-  — SvelteKit SSR public docs app (port 3012, docs.oga9.com),
  markdown-driven content pipeline + nav tree, japa  pattern

### Added — Phase 03 lead-in
- `audit_events` append-only audit trail + `internal/audit` tx-scoped writer (Rule 10 groundwork)
- CI workflow: build, vet, gofmt, migration apply, sqlc-sync check, tests on Postgres 18 + Valkey

### Added — Phase 02 (Identity & Sessions)
- Register/login/logout with JWT + server-side JTI sessions; `/me`, session list, per-session revoke
- Email verification + password reset via single-use HMAC action tokens (Valkey `oga9:auth:used_token` burn)
- TOTP MFA lifecycle: staged server-side secret → confirm issues 8 one-time recovery codes →
  login accepts TOTP or recovery code → disable requires password + live code
- OAuth2/OIDC plumbing (google, japaconnect): state nonce, code exchange, verified-email-only linking,
  find-or-provision; activates per-provider via `OGA9_OAUTH_*` env config
- Valkey fixed-window rate limiting on public auth routes (`oga9:rate:{ip}:{bucket}`, fail-open)
- Trusted-proxy client IP resolution (`OGA9_TRUSTED_PROXIES`, XFF honored only from trusted CIDRs)
- Password policy (≥8 chars, letter+digit), email format validation, `email_not_verified` login gate,
  `POST /auth/verify/resend`
- Domain-event outbox (`domain_events`) with same-transaction emission (Rule 12): UserRegistered,
  UserLoggedIn, SessionRevoked, IdentityVerified, PasswordChanged
- Async mail via Asynq (`identity:send_verification`, `identity:send_password_reset`, retry ×5)
- sqlc-generated `internal/db` — 22 typed identity queries; all service SQL goes through generated code
- `internal/auth` test suite (9 tests: unit + live-stack integration gated on `OGA9_TEST_*` env)

### Added — Phase 01 (Foundational migrations)
- Groups 1–3 applied + verified on Postgres 18: extensions, tenants, system settings,
  plans/capabilities/limits, users/sessions/oauth, groups/roles/permissions/assignments
- Seeds: 101 permission nodes, 5 plans, 3 system groups

### Added — Phase 00 (Bootstrap)
- Go monolith per §01: chi router, pgxpool, Valkey, Asynq server+scheduler (critical/default/low),
  Sentry hooks, graceful shutdown, env-only config (Rule 3)
- docker-compose dev stack (Postgres 18 on :5434, Valkey, Mailpit), Makefile, sqlc.yaml, AGPLv3

### Security
- Constant-time action-token signature compare; HS256-only JWT parsing; 1 MiB JSON body cap;
  HTTP server timeouts; anti-enumeration responses on forgot/resend; recovery codes hashed
  (sha256(code+user_id)), shown once, consumed on use

### Fixed (found in review sweeps)
- `/me` returned Go field names → proper snake_case JSON
- Verify-email tokens reusable → single-use burn
- DB errors masked as `email_taken`/`invalid_credentials`/`user_not_found` → honest 500 mapping
- `mfa_secret` bytea→base32 decode (`convert_from`); MFA error codes surfaced (not 500)
- Events emitted outside mutation transaction → `WithTx` outbox emission
- Event names `user.*` → catalog PascalCase (`UserRegistered`, ...)
- Rate limiter fail-closed → fail-open on Valkey error
- Outbox append failures silently swallowed → `emit` returns error; event failure aborts the tx (Rule 12)
- `mfa/disable` 500ed on non-MFA accounts → `mfa_secret` read as nullable bytea
- Suspended accounts kept live sessions and could re-login via OAuth → session
  resolution now checks `users.status`; OAuth callback applies the same
  status/verified gates as password login
- `japaconnect_user_id` never written → linked on japaconnect OAuth login (ADR-021)
- `Name <a@b>` registration emails stored verbatim → normalized to `addr.Address`
- `mfa_secret` stored as plaintext base32 → AES-256-GCM envelope
  (`OGA9_DATA_ENCRYPTION_KEY`, dev fallback derived from JWT secret)
- Recovery codes were 48-bit truncated UUIDs → 16-char base32 (~80-bit), dashed
- `SessionRevoked` events emitted for no-op revokes → only on actual revocation;
  `DisableMFA` now emits `MFADisabled` (cataloged) instead and clears recovery codes
- `RevokeSession` is now idempotent and reports `revoked: bool`
- Registration honored no kill-switch → `platform.registration_open` system
  setting enforced (seeded `true`)
- MFA not enforced for privileged accounts → `mfa_setup_required` gate:
  `platform.*`/`org_admin` logins get an `mfa_token` and enroll via
  `mfa/setup`+`mfa/confirm` before any session is issued; OAuth MFA users
  complete via new `POST /auth/mfa/complete`
- Unknown-email logins skipped bcrypt (timing oracle) → constant dummy compare;
  suspension/verification errors now surface only after credentials verify
- JWT issuer claim set but unchecked → `iss=oga9` enforced at parse
- Mail enqueue used cancellable request ctx → `context.WithoutCancel`
- Rate-limit key could permanently block an IP on EXPIRE failure → DEL fallback
- `internal/audit` inlined SQL → routes through sqlc `InsertAuditEvent`
- Envelope helpers duplicated across packages → shared `internal/httpx`
- `config` panicked on missing env and swallowed bad durations/ints → aggregated
  `Load()` errors; `config`→`auth` import removed (provider map built in main)
- Migration numbering squatted on group-4 slots → `domain_events`→000040,
  `audit_events`→000043 (canonical group-6), index/recovery folded into 000011/
  000010; group-2 slot 000007 now seeds `system_settings` defaults
- Permission seed drift → added `org.members.view`, `org.finance.dues.manage`,
  `org.documents.view`, `org.governance.positions.assign`,
  `platform.ai_assistant.enabled` (106 nodes); `api.keys.self_manage` +
  `org.listing.manage` cataloged in spec
- `plan_capabilities` seeded uncataloged keys → keys added to
  plans-entitlements catalog (spec)
- `.env.example` missing half the vars config reads → complete
- compose services lacked profiles → `mvp`/`full` on postgres/valkey/mailpit
- `/health` ignored Valkey → pings both dependencies
