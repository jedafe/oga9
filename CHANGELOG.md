# Changelog

All notable changes to the OGA9 platform. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
entries reference engineering phases in `.oos/roadmaps/ROADMAP-BACKEND.md`.

## [Unreleased]

### Added — Phase 05 (Domain Events Infrastructure)
- Group-6 migrations: `event_consumer_log` (consumer,event PK — Rule 27
  idempotency ledger) + `entity_links` (ADR-015 graph edges)
- `internal/events`: consolidated `Emit` (single outbox writer — replaces
  the duplicated emit helpers in auth+tenant; uuid.Nil scope → NULL for
  platform events), `Event`/`Consumer` types, `Dispatcher`
- `events:dispatch_outbox` critical-tier task on the Asynq scheduler
  (`OGA9_OUTBOX_DISPATCH_INTERVAL`, default 10s): `oga9:outbox:lock` SET NX
  EX 60s lease → batch-claims pending events (50, `attempts < 8`) → fans out
  to registered consumers; consumer writes + `event_consumer_log` insert in
  one tx → `dispatched_at`; per-event failures record `attempts`/`last_error`
  and dead-letter at the cap (`events:retry_dead` resets, Phase 06)
- `entity_links` consumer: derives organization→tenant `belongs_to`,
  unit→org `child_of`, appointment→position `holds` + →term `during` edges
- `RelatedEntities` recursive-CTE helper — bidirectional depth-capped walk
- End-to-end smoke test: in-tx emit → leased claim → edge written →
  dispatched → replay dedupes (consumer_log)

### Added — Phase 04 (RBAC & Positions)
- Group-5 migrations: org_positions, position_permissions, terms
  (+`one_current_term` partial index), officer_appointments,
  committee_memberships (§03)
- Org-scoped permission resolver: role grants (tenant) ∪ active
  current-term position grants; `_branch`/`_committee` suffix resolution
  via appointment scope columns; cached `oga9:perm:{user}:{org}` (5 min)
- `InvalidatePermCache` + Rule 6 triggers wired: role assign on
  org-create/invite-accept, appointment, committee add/remove, term close
- Term manager: `CloseTerm` ends open appointments → `AppointmentEnded`
  events; `AppointOfficer` emits `LeaderAppointed`; `TermStarted` on
  term create (all same-tx, sourced per catalog)


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
- `CreateOrg` masked every transaction error behind `GetOrg(uuid.Nil)` →
  template_not_found, slug_taken, and DB errors now surface with their real
  codes/statuses instead of a misleading 404 org_not_found
- Emailed action links pointed at POST-only API routes (un-clickable in a
  browser) → `OGA9_APP_URL` (default: `OGA9_BASE_URL`) now fronts user-facing
  links (`/verify/{token}`, `/reset/{token}`); OAuth callback redirects to
  `{app}/auth/callback` when an app origin is configured, JSON otherwise
- Org create never instantiated template `positions`/`committees` → both now
  land in the same tx (positions as executive org_positions, committees as
  committee units); `TemplateInstantiated` deferred list shrinks to
  `contribution_types` (Phase 09)
- `ResolveOrgCtx` ignored membership lifecycle → ADR-014 terminal states
  (suspended/expelled/resigned/deceased) now resolve to `not_a_member`
- `oga9:outbox:lock` released unconditionally → fenced release (token +
  compare-and-delete) so an expired-lease takeover isn't killed by the
  prior holder
- MFA gate consumed via GET→DEL race → atomic `GETDEL`
- Outbox retries burned every dispatch tick → `next_attempt_at` exponential
  backoff (10s·2^attempts, 1h cap) per §05 contract; migration 000046
- Committee add/remove mutated state silently → `CommitteeMemberAdded` /
  `CommitteeMemberRemoved` (catalogued, sourced) emitted in the same tx
- Committee-type units emitted no governance event → `CommitteeCreated`
  now fires alongside `OrgUnitCreated` (create + template paths)
- Free-text enums: org/unit/position slugs validated (`a-z0-9-`, ≤63,
  no edge hyphens); org_type must be a template-taxonomy type (defaults to
  the template slug); invitation category + committee role whitelisted
- `UpdateUnit` reparent cycle check ran outside the write tx (TOCTOU) →
  parent-chain walk moved inside the transaction
- `events.Emit` mutated the caller's payload map → copies before injecting
  envelope keys
- `uuid.MustParse` on action-token subjects could panic on malformed
  (though signed) claims → graceful parse error
- Login/forgot/resend didn't normalize `Name <a@b>` input → same
  `mail.ParseAddress` normalization as Register
- Dead `HasPermission` query (superseded by ResolveOrgPermissions) removed
- GlitchTip compose service crash-looped on fresh clones (missing
  `glitchtip` database) → `scripts/init-db.sql` creates it on volume init
- Stale comments: `RequireOrgPermission` referenced pre-Phase-04 resolver
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
- `createOrgReq`/`inviteReq` missing json tags → snake_case keys never
  unmarshalled; `POST /orgs` was unreachable (tagged now)
- `plan_capabilities` under-seeded → paid plans lacked the cumulative
  starter+ capabilities; reseeding migration 000009 (all plans get
  org_core/members/events_meetings/comms_documents)
- Invitation revoke unscoped → cross-org mutation by id; `SetInvitationStatus`
  now requires `organization_id`, returns row only on pending→status flip,
  and `InvitationRevoked` emits only on real revokes (404 otherwise)
- `domain_events` drifted from §05 → migration 000045 adds `organization_id`
  (+ payload backfill) and `sourced`, renames `name`→`event_type`,
  `published_at`→`dispatched_at`; emitters now pass sourced flags and
  documented payload keys per the domain-events catalog
- Accepting an invite for an existing member → 500 on unique violation →
  idempotent accept (consumes invite, keeps membership); concurrent accepts
  guarded by pending-status flip
- Org settings whitelist drifted from catalog → synced all keys + pattern
  keys (`org.welfare.cap.{kind}_minor`, `org.welfare.once_per.{kind}_days`)
- OAuth accounts with provider-unverified email stranded → still provisioned
  + linked, verification link sent via `identity:send_verification`;
  providers returning no email rejected (`oauth_email_missing`)
- JWT sub vs session user unchecked → `resolveSessionToken` asserts
  `sub == session.user_id`
- `SessionRevoked` emitted on already-revoked logouts → emits only on
  actual revoke
- Verify/reset token burn outlived tx rollback → burn released on tx failure
- Recovery-code consume raced (read-modify-write) → atomic single UPDATE
  (`ConsumeRecoveryCode` query)
- `joined_at` set on `applied` memberships → NULL until `active`
- `member_no_hint`/request fields silently dropped → tagged
- Mail enqueue failures silent → logged; mail bodies now carry real
  `OGA9_BASE_URL` links; SMTP auth via `OGA9_SMTP_USERNAME/PASSWORD`
- mfa gate nonce reusable after `mfa/confirm` → consumed on enrollment
- `billing_email` stored `Name <a@b>` verbatim → normalized to `addr.Address`
- custom_field_groups/fields/values missing → migrations 000062–64;
  template `seed.field_groups` instantiate at org create (source='template')
- Template `units`/`positions`/`committees`/`contribution_types` never
  instantiated → units loop removed (labels ≠ units); deferred parts listed
  in `TemplateInstantiated` payload for module-phase consumers
- compose missing `api` service (roadmap 00.05) → added to `mvp`/`full`
- `make lint` swallowed gofmt failures → fails build like CI
- §02 session denylist unimplemented → `oga9:auth:deny:{jti}` (catalogued)
  checked in `ResolveSession` before the row lookup; set on logout,
  per-session revoke, and password-reset mass revoke
  (`RevokeSessionByID`/`RevokeAllUserSessions` now RETURN jti)
- `POST /orgs` provisioned a throwaway tenant per org → now attaches to the
  caller's administered tenant (server-side via `org_admin` role, Rule 24);
  first org provisions the tenant (billing_email required only then);
  optional `tenant_id` verified against admin rights (`no_administered_tenant`
  403), ambiguous multi-tenant callers get `tenant_required` 400;
  `limit.organizations` intentionally unenforced until Phase 07 entitlements
- `org.governance.terms.manage` missing from permission seed → terms routes
  permanently 403; node added to 000018 (org_admin roles pick it up via the
  `org.%` grant), catch-up applied to migrated DBs (spec §16/catalog)
- `CreateTerm`/`Appoint` parsed `starts_at`/`ends_at` BEFORE body decode →
  terms always 400ed, appointment `starts_at` silently ignored; decode now
  precedes parsing; `ends_at > starts_at` validated
- `CloseTerm` (explicit close) emitted no `TermClosed` event — only the
  auto-close path in `CreateTerm` did; both now emit
- `TenantProvisioned` never emitted when `POST /orgs` provisions a tenant —
  catalogued (sourced) event now fires in the provision tx (plan_slug null
  until Phase 07)
- Permission resolver missed the §03 group-roles union term —
  `organization_memberships.user_group_id → user_roles → role_permissions`
  now resolves into `oga9:perm` sets
- `ArchiveUnit` only blocked on child units — §10 requires blocking on
  active members too; `committee_memberships` + open scoped appointments
  now count as dependents (`unit_has_dependents`)
- `officer_appointments` scope columns accepted any same-org unit —
  `committee_id` must now be a `committee` unit, `branch_id` a structural
  unit (`invalid_scope_unit` 400)
- `committee_memberships` had no uniqueness → duplicate seats possible;
  `UNIQUE(organization_id, org_unit_id, member_id)` added (spec §03 + live DB)
- `CreateOrg` bad `billing_email`/slug mapped to "value must be valid json"
  → dedicated `invalid_billing_email` 400; `ErrBadValue` message generalized
- `uuid.Parse` failures on term/appointment/committee route params silently
  became 404s → now honest 400s
- Session denylist TTL hardcoded 24h → tracks `OGA9_SESSION_TTL`
- Action tokens embedded `|` in URL path (mail-link fragile) → `~` separator
- Event payloads lacked `aggregate_type`/`occurred_at` required by the
  domain-events catalog → injected by both emit helpers; nil-able
  tenant/org scope now maps uuid.Nil → NULL (platform events)
- Sentry/GlitchTip initialized but never captured → `sentryhttp` middleware
  wired (Repanic defers to chi Recoverer for the 500 envelope)
