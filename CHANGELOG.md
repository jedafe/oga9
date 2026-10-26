# Changelog

All notable changes to the OGA9 platform. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
entries reference engineering phases in `.oos/roadmaps/ROADMAP-BACKEND.md`.

### Fixed — Phase 08–11 review
- Registered `notifications:dispatch`, `notifications:email`, and
  `documents:process_upload` in the task registry — the handlers existed
  but were never wired, so announcement fan-out, email delivery, and
  upload validation silently retried into the DLQ.
- `handler/chat.go`: `HasPermission` called with `(orgID, membershipID)`
  instead of `(userID, orgID)` — `org.chat.moderate` never resolved, so
  moderators could not delete others' messages or end others' rooms.
- Announcement drafts were dead-ends: new
  `POST /announcements/{aid}/publish` (org.comms.announce) fires the same
  event + dispatch fan-out as an immediate publish.
- Document visibility now enforced on reads: `exco` needs an active
  appointment, `committee` needs a seat on the doc's unit, `private` is
  uploader-only; `org.documents.publish` reads all. Hidden docs 404.
- `StartRoom` validates `activity_id` belongs to the org through the
  activities seam (Rule 26) — cross-org links refused.
- Missing channel events emitted (Rule 12): `ChannelCreated`,
  `ChannelMemberAdded`, `ChannelArchived`, `ChannelMemberRemoved`; new
  routes `POST /channels/{cid}/archive` and
  `DELETE /channels/{cid}/members/{mid}` (custom/dm only).
- 11.08 sweeps landed: `chat:retention_sweep` (per-org
  `org.chat.retention_days` soft-deletes past-window messages),
  `voice:room_sweep` (orphaned live rooms end + participants stamped),
  `chat:presence_sweep` (presence-depth scan; TTL stays the mechanism).
- `MinutesApproved` consumer archives the body as a `minutes` document +
  emits `MinutesArchived`; entity_links gains `minutes_of`/`archived_as`
  edges. Meeting→minutes→document chain is now traversable.
- Member-targeted events (`ActionItemOverdue`, `DuesDefaulterFlagged`)
  now create in_app notifications (preference-gated, deduped) and emit
  `NotificationQueued`.
- Notification mark-read is idempotent (already-read → 200, not 404).
- `AgendaPublished` emits when a meeting is created with an agenda.
- Recurrence generation scans forward from `after` — past occurrences
  are never minted retroactively.
- Attendance re-mark (late/absent → present/excused) revokes the
  assessed fine via the finance seam (`RevokeFine` → waived obligation
  + `ObligationWaived`).
- Room end stamps all open participant rows (`left_at`).
- Spec drift reconciled: routes catalog (async export, publish, archive,
  member-remove), `membership:export` payload `{export_id}`,
  `payments.drive_id` in §12 DDL, roadmap 11.08.

## Phase 12 — Subscription Admin / Control Plane (§15)

- `tenant:provision` is now real: `POST /platform/tenants` creates a
  durable `tenant_provisions` row (idempotency on `request_id`), then the
  critical-queue task provisions tenant + owner user + trial subscription
  (billing seam) + owner org from template (tenant bootstrap seam) and
  emits `TenantProvisioned`. Replays return the recorded ids.
- Operator routes (platform.* nodes): tenants list/get/suspend/reinstate,
  subscriptions list + audited `/{sid}/override` (SubscriptionChanged →
  entitlement flush), plans upsert, deployments list/create,
  `support/impersonate` + `/{sid}/end`, audit query, usage meters,
  feature-flags list/set.
- Suspended tenants block login for non-platform users (auth gate);
  platform operators still authenticate to reinstate.
- Impersonation is audit-first (Rule 10 — failed audit aborts), 30min TTL
  enforced on read; `GET /me/support-sessions` is the tenant-visible
  banner surface (`{impersonated, sessions}`).
- `deployments`, `support_sessions`, `feature_flags`, `tenant_provisions`
  tables (000160); flag seeds incl. `platform.ai_assistant.enabled`.
- Suspension is scoped access, not blanket lockout: login is blocked only
  when the user has no live membership in ANY non-suspended tenant, and
  `ResolveOrgCtx` refuses suspended-tenant orgs on both cached and fresh
  paths (write-through `oga9:tenant_status:{tid}` makes it immediate).
- `user_sessions.support_session_id` (000161) links impersonation
  sessions to their audit record; session listings expose it.
- Voice-room sweep age cap is `OGA9_VOICE_ROOM_MAX_AGE_HOURS`
  (default 12) — no hardcoded sweep constants.
- Usage meters recompute on `SubscriptionChanged`/`TenantProvisioned` via
  the `usage_recompute` consumer (orgs, active members, branch-counted
  units; absolute `SetUsage` write — replay-safe). `limit.storage.mb`
  joins when documents carry a byte-size column.
- Welcome mail lands: auth mints a 48h `password_reset` action token for
  the provisioned owner; `platform.SetWelcomeMail` wires mailer+token and
  stays fire-and-forget so mail failure never fails the pipeline.

## [Unreleased]

### Added — Phase 11: Comms & Documents (§14), part 1
- Migrations 000150–000153: `member_exports` (§11 durable export row),
  `announcements`, `notifications`, `notification_preferences`,
  `documents` + `document_versions` (soft-delete, Rule 11), and the
  deferred `payment_claims.receipt_media_id` FK (`NOT VALID` — existing
  claim rows predate documents). g11 renumbered past the applied max —
  golang-migrate silently skips versions below it; MIGRATION-ORDER's
  reserved 000090–000099 is now documented-dead.
- `membership:export` is a real async path: `POST /members/export` →
  durable row + task → `GET /members/exports/{eid}` streams the CSV.
  Synchronous `GET` stays for small exports.
- `internal/comms` + `comms_documents` app group: announcements
  (draft|published|retracted, audience = all|categories|unit_ids),
  `AnnouncementPublished`/`AnnouncementRetracted`/`BroadcastQueued` in-tx,
  `notifications:dispatch` resolves the audience snapshot → per-user
  `notification_preferences` gate → `in_app` rows; `/me/notifications`,
  `/me/notifications/{id}/read`, `/me/preferences` GET|PATCH.
- `notifications:email` reuses the sendMail body.

### Added — Phase 11: Comms & Documents (§14/§24), part 2
- `internal/storage`: disk-backed blob store under `OGA9_UPLOADS_DIR`
  (Rule 29 — no external service needed; S3-compatible backends plug in
  behind the same interface later).
- Documents: multipart upload → `documents` row + `DocumentUploaded` +
  `documents:process_upload` (validates the blob landed, soft-deletes
  poisoned uploads), PATCH title/visibility/`clear_unit` (visibility
  changes emit `DocumentAccessChanged`), soft delete (Rule 11), versioned
  re-upload (`DocumentVersioned`, version rows), org download, public
  `/directory/orgs/{slug}/docs` (visibility=public only).
- `comms_documents` manifest fix (000154): all §14 + §24 nodes declared.
- §24 chat (000155): `chat_channels`/`chat_channel_members`/
  `chat_messages`/`chat_read_states`; channels with derived membership
  (general = all active, unit = home unit, explicit rows for
  exco/custom/dm/committee); messages CRUD with soft delete + mutes +
  read states; `MessagePosted`/`MessageDeleted` in-tx.
- `internal/realtime`: `/ws` gateway (coder/websocket) — session-authed
  upgrade, per-channel subscribe gated through `chat.CanSubscribe`
  (Rule 26), Valkey pub/sub `oga9:realtime:chat` fans messages out to
  connected members across instances.
- Catalogs synced: routes (§14 download/versions + `/ws`), cache keys
  (`oga9:realtime:chat`), settings (`org.chat.voice_max_seconds`,
  `org.chat.edit_window_minutes`).
- Seeded notification kinds are emitted inline by producers
  (announcement today; others land with their consumer domains).

### Added — Phase 11 closeout (§14/§24 hardening)
- `notifications:email` fan-out: `AudienceRecipients` joins emails, the
  email channel has its own preference gate, and the dispatch worker
  enqueues one `notifications:email` per opted-in address (in_app and
  email are independently gated).
- Voice rooms per §24 (000158): `voice_rooms`/`voice_room_participants`,
  start → join (short-lived LiveKit JWT signed with
  `OGA9_LIVEKIT_API_KEY/SECRET`, video grant scoped to the room) → leave →
  end (starter or `org.chat.moderate`), with `VoiceRoomStarted`/
  `VoiceRoomEnded`/`VoiceParticipantJoined`/`VoiceParticipantLeft` in-tx.
  Unconfigured provider returns 503 (Rule 29).
- `oga9:presence:{member}` heartbeat every 30s while subscribed
  (90s TTL); `{"unsubscribe": "<cid>"}` releases the seat.
- Chat hardening: functional unique on `(org,kind,COALESCE(unit_id,…))`
  stops duplicate general channels (NULLs aren't equal in plain UNIQUE);
  `CreateChannel` falls back to `GetChannelByKind` on conflict;
  exco/committee channel derivation; `IsChannelMemberForUser` binds WS
  subscriptions to the session user; voice duration cap; dispatch dedupe;
  `Content-Disposition` sanitization.
- Review fixes: `/directory/orgs/{slug}/docs` routed, WS OriginPatterns
  enforced, anonymous guest rows refused, guest-contact dedupe (000156),
  `NotificationRead` + `delivered_at`, executable magic-byte sniff.
- Tests: `internal/comms` (announce→dispatch→prefs→retract, document
  lifecycle, chat flow) and `internal/realtime` (subscribe→presence→
  fan-out→spoof rejection, end-to-end over live valkey+postgres).

### Added — Phase 10: Events & Meetings (§13)
- Schema g10 (000140–000145): `activities` spine (kind=meeting|event,
  unit-scoped, quorum/capacity columns), `meeting_agendas`, `minutes`
  (versioned, draft|approved|archived), `action_items`, `attendances`
  (composite mark sheet), `event_registrations` (member-partial-unique,
  guests carry name/contact), `activity_recurrences` (RRULE templates),
  and the `payments.activity_id` FK backfill (stub → ALTER pattern).
  Numbering per the renumbered MIGRATION-ORDER (000140–000149).
- `internal/activities` service + `handler/activities.go` + routes behind
  `RequireAppActive("events_meetings")`: meetings/events CRUD with
  notice-window warnings and quorum/capacity resolved from org_settings
  (Rule 23); agenda upsert → `AgendaPublished`; versioned minutes with
  draft→approved CAS + older-version archival → `MinutesApproved`;
  action items (assignee self-complete vs `org.action_items.manage`),
  daily overdue sweep → `ActionItemOverdue`; bulk attendance sheet →
  `AttendanceRecorded` per mark + late/absent fines posted through the
  finance `AssessFine` seam (Rule 26 — deduped per `attendance:{id}`
  period); `EvaluateQuorum` → `QuorumEvaluated`; event register
  (member dedupe + `event_full` capacity) → `EventRegistrationConfirmed`,
  check-in → `CheckedIn`; org + `/me` calendars and `/me/action-items`;
  `activities:generate_recurrences` (minimal FREQ/INTERVAL/COUNT/UNTIL
  RRULE, 35-day horizon, `rec:`-tag dedupe), `remind_upcoming` →
  `MeetingReminderDue`, `overdue_action_items` — all three task types
  registered; the daily pair ride the `oga9:sweep:daily:*` gate.
- `finance.AssessFine` exported as the cross-module penalty seam;
  `assessFine` generalised with a caller-scoped period label.

### Fixed — Phase 10 review, round 1
- `CheckedIn` emitted the activity id as `registration_id` — now `r.ID`.
- `remind_upcoming` re-emitted `MeetingReminderDue` every tick for every
  activity in its 48h window (~576 dupes per meeting): new
  `activities.reminder_sent` flag (000146) + `ClaimReminders` emit-once CAS.
- `PATCH /meetings/{aid}` now applies `agenda` (meeting-only →
  `AgendaPublished`) and validates `status` ∈ scheduled|completed|cancelled.
- Register path serializes per event via `FOR UPDATE` — two concurrent
  registrations can no longer both pass the capacity headcount.
- Unit-scoped meetings evaluate quorum against the unit roster
  (`committee_memberships`), not the whole org.
- Action-item complete route corrected to spec-literal
  `PATCH /meetings/{aid}/action-items/{iid}` (`{"state":"done"}` self or
  `org.action_items.manage`; field edits manage-only).
- `actErr` default was a 400 — internal errors now 500 with server-side
  logging; malformed `?unit=` is a 400 instead of a silent no-filter.
- Recurrence instances now carry `activities.recurrence_id` (000147) with
  an atomic `UNIQUE(recurrence_id, starts_at)` dedupe — replaces the
  `rec:`-in-description tag and its LIKE existence check.
- RRULE grammar expanded: FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL,
  COUNT, UNTIL (inclusive), BYDAY (WEEKLY), BYMONTHDAY (MONTHLY) — a
  day-scan matcher replaces the fixed stepper; unknown tokens fail
  closed rather than generating wrong instances.
- Rescheduling an activity (`starts_at` PATCH) re-arms its reminder —
  `reminder_sent` clears when the start moves, not on field-only edits.
- `ClaimReminders` scoped to meetings only (§13 notice rules are
  meeting-scoped; events have no catalogued reminder event).
- `/me/calendar` excludes pending states positively
  (`active|suspended|life_member`) — an applicant's pending membership no
  longer leaks the org calendar.
- Attendance fines resolve `org.dues.currency` from org_settings instead
  of hardcoded NGN.
- Action-item PATCH: `{"state":"cancelled"}` (manage-only) via
  `CancelActionItem`; unknown state values and empty PATCH bodies now
  400 instead of silent no-ops.
- `actSweep` handler closures nil-guard `d.Activities` (the comment
  claimed the adapter guarded; a nil service would have panic-retried
  forever in the worker).
- **Cross-org scoping (Rule 24).** `ApproveMinutes`, `CheckIn`,
  `Complete/Patch/CancelActionItem`, `ListMinutes`, `ListActionItems`
  addressed rows by id without verifying `organization_id` — a caller's
  OrgCtx did not stop org A's IDs being mutated/read by org B. All are
  now EXISTS/`IN`-scoped through the parent activity; regression-tested.
- Claim-then-emit sweeps (`overdue_action_items`, `remind_upcoming`)
  claimed rows outside a tx then emitted separately — a failed emit
  permanently dropped the event. Both now claim + emit in one tx.
- `DraftMinutes` maps the `UNIQUE(activity_id,version)` collision from
  concurrent drafts to `invalid_state_transition` instead of a 500.
- `MarkAttendance` refuses marks on cancelled meetings.

### Fixed — Phase 08–10 review, final
- **§13 route surface wired.** Meetings/events CRUD, attendance (POST +
  GET sheet), minutes (draft/list/approve), agenda, quorum, action-items,
  register/check-in/registrations, org calendar, and recurrences now sit
  behind `RequireAppActive("events_meetings")` with their §13 permission
  nodes — the whole org surface was previously unreachable (only
  `/me/calendar` + `/me/action-items` existed).
- **Recurrence dedupe actually dedupes.** `GenerateRecurrences` used
  `CreateActivity` without `recurrence_id` — the
  `UNIQUE(recurrence_id, starts_at)` index could never fire, so every
  daily sweep re-minted the full horizon. Now uses the purpose-built
  `CreateRecurrenceInstance` (ON CONFLICT DO NOTHING → replay mints 0),
  drops the `rec:` description tag, and COUNT caps the series total via
  `CountRecurrenceInstances` instead of per-window.
- **Recurrence time-of-day.** Templates carry required `time_of_day`
  ("HH:MM"); instances no longer inherit the cron run's timestamp.
  `POST/GET /orgs/{id}/recurrences` registers and lists series.
- **`ActionItemCompleted` emits** on both self and manage-complete paths
  (was catalogued but never produced).
- **`EventCompleted` only fires for events** — meetings closing emit
  nothing (was kind-agnostic).
- **`events_meetings` manifest drift** (000148): declares all 7 §13 nodes
  — `org.meetings.manage`, `org.meetings.attendance.record`,
  `org.action_items.manage` were missing.
- **Guest check-in** via `registration_id` (CheckInByID); `registered_by`
  on event_registrations (000149) backs `org.events.guest_cap` — a member
  can no longer stuff a roster with unbounded guests.
- **Quorum denominator** counts committee roster AND members whose home
  unit is the activity's unit — branch-scoped meetings (no committee
  rows) can now pass quorum. Emit failure propagates instead of dropping
  `QuorumEvaluated`.
- **PATCH semantics**: `clear_unit` strips unit scope; reschedule
  re-evaluates `notice_warning`; unit-filtered calendar includes
  org-wide activities.
- **ADR-014 `invited → withdrawn`** added (any → withdrawn while
  pre-active); `Eligible` surfaced on `/me/memberships` rows; malformed
  `?unit=` on members list is a 400.
- **`PatchDrive` nil-deref** — `*closesAt` was evaluated unconditionally;
  a title-only PATCH panicked → 500.
- **`finance:issue_receipt` can succeed** — `EnsureReceipt` queried the
  payment without org scope and always 404'd; new `GetPaymentAny`
  (task-context lookup) fixes it.
- **`DuesDefaulterFlagged` can't be lost** — standing read, recompute and
  the flag emit now share one tx; a failed emit rolls the standing back
  so the next tick retries (same class fixed in the P10 sweeps).
- **`once`-schedule collision** — period_label for one-off schedules is
  now `once:{schedule}` so a second one-off of the same contribution type
  still generates obligations.
- **`payments.activity_id` write path** — `PaymentIn.activity_id` +
  `finSvc.SetActivityGate(actSvc.ActivityInOrg)` (Rule 26 seam); event
  donations view is no longer read-only-empty.
- **`OpenDrive` org-validates** `benefit_member_id` +
  `contribution_type_id` (cross-org FK linkage was insertable).
- **Life-member rule** — `applies_to.include_life_members` opts life
  members into a schedule's payee set ("dues-exempt per org rule" is now
  actually an org rule).
- **Overdue stays overdue** — `refreshObligation` no longer flips
  `overdue` back to `open`/`partially_paid` on a reversal; only full
  settlement clears it.
- **Ad-hoc claim allocation** — `confirm` accepts `drive_id` /
  `obligation_id` overrides (§12 "officer assigns on confirm").
- **`/finance/statement` is a real statement** — purse balance +
  month/year inflow-outflow + per-account breakdown (was bare
  account×direction totals).
- **`/finance/contributions` attribution** — drive payments use the
  drive's contribution type; member-directed donations bucket as
  `donation` (was all `general`).
- Spec synced: api-routes (§13 read surface + recurrences),
  system-settings (`org.events.guest_cap`), §13 schema/API/rules.

### Fixed — Phase 05–09 review, round 5
- **Claim dedupe 500 → 409.** `payment_claims` insert conflict now maps to
  `ErrAlreadyClaimed` (was dead code: `ON CONFLICT DO NOTHING` returns
  `pgx.ErrNoRows`, which escaped as an internal error). Rejected claims no
  longer burn the member's `external_ref` — the dedupe index is now
  `WHERE state <> 'rejected'` so corrected proof can resubmit (000108).
- **Disbursed drives refuse late confirms.** A payment initiated while a
  drive was open can't confirm after `disbursed`/`cancelled` — the credit
  would land outside the disbursed total. `closed` still confirms
  (money sent before the deadline).
- **Idempotency-key payload binding.** Replays compare amount/currency/
  channel/payee/obligation/drive/benefit — key reuse with a different
  payment returns `idempotency_key_in_use` instead of silently replaying.
- **Obligation ownership.** `record_payment` with `obligation_id` requires
  the payer to own the obligation (on-behalf payments go through ad-hoc
  claims — keeps the who's-paid roll call honest).
- **Renewal↔payment is 1:1.** `membership_renewals_payment_uq` partial
  unique + a `NOT EXISTS` guard — one confirmed payment settles at most
  one renewal (000108).
- **ADR-014 `any → withdrawn` while pre-active.** `approved`/`registered`
  members can now withdraw; emits the newly-catalogued `MemberWithdrawn`
  (sourced — joins the membership-history stream).
- **MFA-complete materializes applications.** The §11 verified-bind hook
  now also runs after `mfa/complete` (was login/verify/OAuth only).
- **Schedule PATCH can clear `ends_on`** (`{"ends_on":null}` →
  `clear_ends_on`), `applies_to` defaults `{}` on create, drive PATCH is
  a real partial update (was all-or-nothing requiring `title`).
- **`DuesSchedulePublished` consumer** enqueues
  `finance:generate_obligations` for the new schedule — no more waiting
  for the daily sweep (catalog's obligations-gen consumer, now real).
- **Sweep cadence.** Obligation generation + overdue sweep are daily per
  the §12 table, gated by `oga9:sweep:daily:{name}` SET NX EX — was
  every 5-min tick. Drive auto-close stays per-tick.
- **Error hygiene.** `finErr` no longer leaks internal error text;
  malformed UUID path params are 400s, not 404s/409s;
  `ConfirmClaim`/`RejectClaim` only mask `ErrNoRows` as
  `invalid_state_transition` (real DB errors surface).
- **finance_lite manifest** declares the full §12 node surface (000108 —
  same drift class 000107 fixed for the other manifests).
- Sweep/task events resolve the org's tenant (`GetOrgTenantID`) — no more
  NULL-tenant `ReceiptIssued`/`ContributionDriveClosed` on the retry path.
- `/me/finance` feed interleaves obligations (due_date) and payments
  (created_at) chronologically instead of obligations-last.
- Spec synced: MIGRATION-ORDER (g10 renumber to 000140–000149 — lower
  versions are silently skipped), api-routes (`/orgs/{id}/resign`,
  self-withdraw, decision `withdraw`), domain-events (`MemberWithdrawn`,
  `PaymentReceived` payload), cache-keys (`oga9:sweep:daily:*`), §11/§12.

### Added — Phase 09: Finance-lite (§12)
- Schema g12 (000100–000106): `contribution_types`, `dues_schedules`,
  `obligations`, `contribution_drives`, `payments` (+`claim_id` FK in
  000104), `payment_claims`, `ledger_entries` (append-only, Rule 19),
  `receipts` (org-scoped `RCP-YYYY-NNNNN` via `organizations.receipt_no_seq`),
  `reconciliation_records`, and `membership_renewals.payment_id` FK.
  `payments.activity_id`/`receipt_media_id` are untyped FKs until groups
  10/11 land (MIGRATION-ORDER).
- Obligation generation (`finance:generate_obligations`): active schedules
  fan out to matching active members per `applies_to` category/unit filter;
  period labels derive from recurrence (Rule 23); the
  `(membership,type,period)` unique makes every pass idempotent.
- Verified-provenance payments: `POST /finance/payments` requires
  `Idempotency-Key` (Rule 9 — replays return the original); officer path
  is record→approve with recorder≠approver enforced in the confirm CAS
  (`separation_of_duties`); member path is claim→confirm, the claim row
  carrying provenance (`receipt_media_id` gated by
  `org.finance.claims.require_receipt` for non-cash).
- Confirm posts ledger credit + receipt + obligation refresh + sourced
  events in one tx (Rule 12). Reversals append `reversal_of` debit
  entries — ledger rows are never updated. Reconcile marks
  confirmed→reconciled with a reconciliation_record.
- Contribution drives: open (voluntary|fixed_amount) → close (manual or
  `finance:drive_closing_sweep` at `closes_at`) → disburse (debit ledger
  entry + `ContributionDisbursed`).
- `finance:overdue_sweep` marks grace-expired obligations overdue and
  re-derives `dues_standing` (good|poor); a `dues_standing` outbox consumer
  recomputes on payment/obligation events.
- `billing:retry_payment` activated — failed payments re-enter `initiated`
  for re-approval.
- Renewal → payment link: `POST .../renew` accepts `payment_id`;
  `membership_renewals.payment_id` FK wired (000106).
- Routes: the §12 surface under `RequireAppActive("finance_lite")` +
  per-node gates; `/me/finance` + `/me/claims` are cross-org member views.
  Catalog updated with the previously unlisted drives/claims/ledger/
  statement/confirm rows.
- Tests: `TestFinanceFlow` covers generation + idempotent replay, SoD
  confirm, partial→paid, reversal, claim provenance, drive disburse, and
  cross-org isolation.

### Fixed — Phase 05–08 deep review
- **Intake dead-ends closed.** Invited members land `applied` with no
  application row — `TransitionMember` now runs the intake verbs
  (screen/approve/reject/withdraw) on memberships, and membership-level
  approve mints `member_no` when absent (`SetMemberNo` is live). Re-invite
  resets resigned/withdrawn/rejected members to `applied`; expelled stays
  barred (`forbidden`).
- **Pre-signup applications materialize.** `MaterializeApprovedApplications`
  links `user_id` (CAS on NULL) and creates the `approved` membership +
  member_no + migrated sponsors; wired into verify-email, login, and OAuth
  callback via a nil-safe `MemberMaterializer` seam — best-effort, never
  fails the auth response.
- `app_installations` backfill for pre-Phase-07 tenants (000077) —
  `RequireAppActive` no longer 403s legacy orgs; `member_no` unique index
  (000078) + approved-application email index (000079).
- `Apply` rejects existing members (`already_a_member`) instead of a 500
  on the org+user unique at approve; public apply dedupes member emails;
  open-application checks only count undecided statuses — decided rows no
  longer block re-application.
- `MemberHistory` now matches the `membership_history` view contract —
  sourced, membership-family aggregates (identity events no longer leak).
- Sponsor fixes: screener extras deduped against the primary + each other
  (one person can't satisfy `sponsor_required=2`), and restricted to
  screen/approve actions.
- `rejected`/`withdrawn` added to `lifecycleBlocked` and the permission
  resolver's lifecycle filter — intake exits confer no org context/grants.
- Self-service exits: `POST /orgs/{id}/resign` (suspended members carry no
  OrgCtx but may still resign); self `withdraw` on pre-active memberships;
  self-PATCH limited to `org_unit_id` (category stays admin-only);
  `/renew` restricted to `manage` until Phase 09 wires payment (self-renew
  would mint free good-standing); renewals restore `dues_standing='good'`.
- Outbox dispatcher: `FOR NO KEY UPDATE SKIP LOCKED` batch claim inside
  the lease (FOR UPDATE would self-deadlock on `event_consumer_log`'s
  FK check), and `LogConsumerProcessed` inserts FIRST inside the consumer
  tx — the atomic dedupe, so an overlapping dispatcher skips rather than
  replays.
- Billing: `ChangePlan` CAS on `plan_slug` (raced writes surface a
  conflict, never silently overwrite); `canonicalJSON` uses `UseNumber`
  so >2^53 license limits survive signing; license limit merge treats
  `-1` (unlimited) as absorbing.
- Imports: stale 'processing' rows re-driven by a `scheduler:tick` sweep
  (webhook redrive parity); enqueue failure fails the row visibly;
  `IssueCard` returns the live card on a unique-race instead of 500;
  `memberImport` archives on a missing durable row.
- `entity_links` targets resolve only per-rule keys — a missing payload
  key skips the edge instead of linking a wrong-typed id; webhook rate
  limit raised to 300/min for provider bursts.
- Catalog: `CardIssued`, `LifeMemberAwarded` flagged sourced per the §11
  history-view contract.
- Tests: `TestIntakeDeadEnds` covers invite→screen→approve→register→
  activate, pre-signup materialize + idempotent replay, and the
  sponsor double-count guard.

### Fixed — Phase 08 review, round 3
- `membership_cards` partial index is now UNIQUE (000076) — one live card
  per member; `IssueCard` is idempotent and returns the live card instead
  of double-emitting `CardIssued` (Rule 27).
- Staged-sponsor migration on approve uses INSERT-SELECT + DELETE (was
  in-place UPDATE that could violate the membership unique index when an
  extra sponsor duplicated the primary).
- Extras equal to the primary sponsor are deduped at apply.
- Public pre-signup applications dedupe on email-or-phone — repeat
  submissions no longer stack open applications.
- `RenewMember` rejects backwards windows (`ends_at <= starts_at`).

### Fixed — Phase 08 review, round 2 (spec gaps)
- Member home-unit scope: `organization_memberships.org_unit_id`
  (000075) — the §11 `unit` directory filter is now real
  (`GET /members?unit=`, `PATCH {unit_id}` validated same-org).
- `sponsor_required: 2` implemented: applications carry
  `sponsor_member_ids[]` — the primary lands in
  `sponsor_member_id`, extras stage in `member_sponsors.application_id`
  (new nullable column + partial uniques, 000075) and migrate onto the
  membership on approve; the approve gate counts total sponsors.
- Applicant self-withdrawal: `POST /orgs/{id}/applications/{aid}/withdraw`
  — deliberately outside the orgCtx group (a pre-decision applicant has
  no membership); ownership + open-status checked in the service.
- `membership_history` DB view over `domain_events` — sourced
  membership-family aggregates (§11 "sourced event stream").
- Import dedupe widened to email-or-phone — phone-only rows replayed
  into duplicates.
- Empty `sponsorIDs` slice no longer panics the extras loop.
- Spec updated first: §11 documents `org_unit_id`, application-stage
  sponsors, `membership_history`; routes catalog gains the withdraw row.

### Fixed — Phase 08 review, round 1
- `DecideApplication` no longer emits an empty `event_type` on withdraw
  (no `MemberWithdrawn` event exists in the catalog — Rule 12) and no
  longer double-emits `MemberApproved` on the application + membership
  aggregates; pre-signup approvals still emit the app-level event.
- `sponsor_required` enforced at approve, not apply (§11 test scenario) —
  intake stays open so a screener can attach sponsors; public apply to
  sponsor-required orgs is no longer dead-on-arrival.
- Terminal transitions emit `CommitteeMemberRemoved` per vacated seat
  (catalogued, sourced, feeds rbac invalidation consumers) — seats were
  being deleted silently.
- `membership:import` is idempotent (Rule 27): retried workers skip rows
  that already produced an open application (org+email dedupe) instead of
  minting duplicates.
- `MemberImported` now emits once at finish with the catalogued
  `{import_id,ok,err}` payload and `sourced=no` (was per-row, sourced).
- Import accepts real CSV (`{filename, csv}` header-row text) as well as
  pre-parsed `rows`; `answers`/card `design` default to `{}` instead of
  violating NOT NULL.
- `MemberHistory` includes the member's application-level events — the
  intake trail belongs to the replayable journey.
- `GET /directory/orgs/{slug}/apply` (public) serves the org's
  `app_fields` + `sponsor_required` so the form renders before submit.

### Added — Phase 08: Members (§11)
- g9 migrations 000070–000074: `membership_applications`, `member_sponsors`,
  `membership_cards`, `member_imports` (+`data` payload column for the
  async worker), `membership_renewals`; `organization_memberships` gains
  `dues_standing` + `last_renewed_at`; `organizations` gains
  `member_no_seq` (atomic per-org member number sequence).
- ADR-014 state machine in `tenant/members.go`: invited→applied→screened
  →approved→registered→active + suspended/expelled/resigned/deceased/
  life_member. API verbs map onto legal transitions; illegal moves return
  `invalid_state_transition` (409). Transitions are CAS (`WHERE state =
  from`) so concurrent moves can't double-apply.
- Application pipeline: `POST /orgs/{id}/applications` (manage) + public
  `POST /directory/orgs/{slug}/apply` (listed orgs, pre-signup); decisions
  screen/approve/reject/withdraw (`org.members.approve`). Approve
  materializes the membership, mints `member_no` from
  `org.membership.number_format` ({SLUG}-{YYYY}-{seq5}), records sponsors
  + `SponsorshipRecorded` — pre-signup apps defer the membership until the
  user registers (Phase-02 hook).
- `approved→registered` is a no-gate transition for now — the registration
  payment guard lands with §12 finance (payment event consumer).
- `POST …/transition` side effects in-tx: activation grants the `member`
  role + stamps `joined_at`; terminal transitions vacate appointments
  (AppointmentEnded each), clear committee seats, revoke cards. Every
  transition emits its catalogued event + invalidates perm/org-ctx caches
  (Rules 6/12).
- Resolver now filters `lifecycle_state NOT IN ('suspended','expelled',
  'resigned','deceased')` across role, group, position and committee
  paths — suspended members lose grants immediately (ADR-014).
- Renewals: `POST …/renew` (manage or self) → `membership_renewals`
  (UNIQUE period → `renewal_exists` 409) + `last_renewed_at` +
  `MembershipRenewed`.
- Cards: `POST …/card` (`org.members.card.issue`) → unique serial +
  `CardIssued`; revoke on terminal transitions.
- Directory: `GET /orgs/{id}/members` (`view_any`, category/state
  filters), `GET …/{mid}` (view_any or self), `PATCH` (manage, category),
  `GET …/history` (sourced event stream), `GET /me/memberships`.
- Import: `POST …/import` (`org.members.import`) → durable
  `member_imports` row + `membership:import` worker → rows become
  `applied` applications (never direct members), per-row errors in
  `errors`; `membership:export` registered as Phase-14 stub (sync CSV
  export serves the route meanwhile).
- Members routes sit behind `RequireAppActive("members")` — first real
  consumer of the §06 activation seam.
- `Eligible(state, dues)` computed helper — eligibility is never stored.
- Tests: full lifecycle + sponsor guard + invalid transitions +
  suspend-grant-drop + card/renew/duplicate + terminal teardown
  (`member_test.go`), import row semantics.

### Fixed — Phase 04–07 review, round 4
- Migration numbering: `000060_no_tenant_platform_admin` squatted on a
  group-8 slot held by `000060_org_invitations` — golang-migrate refused
  the source outright (CI's psql replay masked it). Renumbered to 000065.
- §03 scope filter enforced: `ResolveOrgPermissions` no longer unions
  scoped appointment grants into the base node set — a branch/committee-
  scoped seat confers only the `_branch`/`_committee` variants via
  `ScopedPositionNodes` (was: branch officer held the unsuffixed node).
- `DeactivateApp` re-checks the dependents graph inside its tx on the
  `FOR UPDATE` installation rows — a concurrent activate(dependent)/
  deactivate(base) race can't split the dependency graph anymore.
- Cancelled subscriptions are terminal: `ActivateSubscription` guards
  `state <> 'cancelled'` so a late `invoice.paid` can't resurrect a
  cancelled sub (was unconditional `SetSubscriptionState`).
- `payment.failed`/`invoice.payment_failed` only moves an `active` sub to
  past_due — trials keep their window, lapsed/cancelled states ack as
  no-ops instead of transitioning backwards.
- `ChangePlan` requires payment for a paid target whenever the current
  plan can't prove the move is a downgrade (unpriced/unlisted current
  plan, non-enterprise term) — closes a bare `plan_slug` write to paid.
- Entitlement enforcement points wired (§07): `limit.organizations`
  counted inside the create tx at `POST /orgs`; `limit.branches`
  (hq|branch|chapter meter, committees/wings excluded) enforced on
  branch|chapter `CreateUnit` via `CheckLimit` → `limit_exceeded` 402.
- App manifest seeds referenced uncatalogued nodes — 000066 maps
  `org.directory.manage`→`org.listing.manage` (ADR-021) and
  `org.documents.manage`→`org.documents.upload`.
- `/me/entitlements` resolves tenants via the `org.admin.billing` node
  like the rest of the billing surface (was org_admin role only — a
  delegated billing admin couldn't read it).
- Office/committee seats require `lifecycle_state='active'` — applied/
  suspended members get `member_not_active` (409); appointment
  `starts_at` must fall inside the term window.
- `EndTermAppointments` sets `ends_at = GREATEST(starts_at, CURRENT_DATE)`
  — future-dated appointments no longer end before they start.
- Invite acceptance survives a departed inviter — `granted_by` falls back
  to NULL instead of aborting the accept tx.
- Outbox `last_error` now joins all failing consumers (was last-one-wins).
- Production requires an explicit `OGA9_LICENSE_SIGNING_KEY` — no more
  silent derivation from the JWT secret (dev fallback unchanged).
- `billing:retry_payment` stub label corrected to Phase 09 (§12 payments).
- New tests: scoped-grant resolution + lifecycle/term guards
  (`rbac_test.go`), webhook terminal-state guards + unpriced-plan gate
  (`billing_test.go`), starter `limit.organizations` rejection
  (`tenant_test.go`).

### Fixed — Phase 07 review, round 3
- `CloneTenantRoles` no longer clones platform.*-carrying roles into
  tenants — `platform_admin` is control-plane only (000065 removes clones
  that already landed)
- Subscription transitions are compare-and-set (`WHERE state = from`) — a
  stale read can't regress state or emit a phantom `SubscriptionChanged`
  (e.g. late `payment.failed` overwriting a processed `invoice.paid`)
- `ProcessWebhook` locks the event row `FOR UPDATE` — concurrent workers
  serialize; `processed_at` is rechecked under lock
- License signed payloads are JSON-encoded — a `deployment_id` containing
  the old `|` separator could shift bytes between fields
- `DeactivateApp` treats a lost race (`ErrNoRows`) as an idempotent no-op

### Fixed — Phase 07 review, round 2
- `ProcessWebhook` no longer drops events on transient `GetSubscription`
  failures — only `subscription_not_found` acks; other errors retry
- Signed-but-malformed webhook bodies return `400 invalid_webhook_payload`
  (was 500); stale-webhook redrive is bounded to a 2-day window so a
  poisoned row can't re-enqueue hourly forever
- App events now carry the `app_installations.id` as `aggregate_id` (was
  the tenant uuid); re-activating an already-active app is a no-op with no
  phantom `AppActivated`; `DeactivateApp` only transitions `active` rows
- Resubscribe path: a cancelled-only tenant can create a fresh (free-plan)
  subscription again — the 409 check only blocks on open subscriptions
- `/me/entitlements` logs per-tenant resolve failures instead of silently
  omitting them; unused `PlanExists` query removed; tests are
  repeat-safe on a shared database

### Fixed — Phase 07 deep review
- Self-serve billing restricted to free/downgrade plan moves:
  `POST /billing/subscription` + `change-plan` return `402 payment_required`
  for paid plans/upgrades — upgrades land via provider checkout webhook or
  §15 platform override, never a bare `plan_slug` write
- Entitlement resolution honors subscription state (Rule 28):
  `restricted`/`cancelled` tenants resolve the starter floor — records stay
  reachable, paid capabilities stop resolving
- `billing:subscription_state_sweep` now lapses `active` subscriptions whose
  `current_period_end` passed without a provider event (→ `past_due`) — a
  silent provider no longer means free-forever
- `ProcessWebhook` marks `processed_at` inside the transition tx — a crash
  can't re-apply `activate()` and double-extend the period
- Webhook durability floor: the sweep re-drives `billing_webhook_events`
  rows recorded >5min ago but never processed (enqueue-failure case)
- Licenses are ed25519-signed (was symmetric HMAC) — self-hosted
  deployments verify offline with only the public key; signatures cover the
  canonical JSON so jsonb key-ordering can't break verification. Re-issuing
  a `deployment_id` renews in place (upsert). Issue validates tenant +
  future expiry → `404 tenant_not_found` / `400 invalid_expiry`
- `subscriptions_one_open` partial unique index (000059) — one open
  (non-cancelled) subscription per tenant; concurrent creates 409
- Billing tenant resolution is node-based (`org.admin.billing`, Rule 20) —
  delegated billing admins resolve without holding the org_admin role;
  multi-tenant callers must pass `?tenant_id` (`400 tenant_required`)
- `DeactivateApp` is idempotent — never-installed/inactive apps return
  success without emitting phantom `AppDeactivated` events; `GetApp` errors
  no longer masquerade as `app_not_found`
- `middleware.RequireAppActive(svc, appID)` — the §06 "deactivation
  disables" enforcement seam for module routes (lands with Phase 08+)
- Migration fixes: 000058 grant restricted to the NULL-tenant role; 000051
  manifest publishes `AnnouncementPublished` (catalogued name)

### Added — Phase 07 (App Framework & Entitlements, §06/§07)
- Group-7 migrations (000050–000057): `app_registry` + 6 MVP app seeds with
  manifests (requires/permissions/events/entitlements), `app_installations`,
  `subscriptions`, `usage_meters`, `licenses`, `billing_webhook_events`
  (Rule 9 durable record), `billing.trial_days` setting seed; 000058 seeds a
  `platform_admin` role granting `platform.*` nodes
- App activation graph (`internal/tenant/apps.go`): activate requires the
  dependency closure active + the manifest's entitlement feature (Rule 25,
  402 not_entitled); deactivate rejects mvp_required and active-dependents
  (`app_has_dependents`, 409) — deactivation disables, never deletes
- `internal/entitlements` service: `IsEnabled`/`Limit`/`CheckLimit`/`Set`,
  merged plan ∪ license resolution (starter floor for pre-existing tenants),
  `oga9:entitlements:{tenant_id}` 10-min cache + invalidation via a
  registered outbox consumer on SubscriptionChanged/AppActivated/
  AppDeactivated/LicenseIssued
- Subscription lifecycle (trial→active⇄past_due→grace→restricted→cancelled):
  provision now creates a starter trial + installs mvp_required apps in the
  same tx; `billing:subscription_state_sweep` (critical, hourly) drives
  time-based transitions; every transition emits `SubscriptionChanged`
  same-tx
- Licenses (Rule 29): `POST /platform/licenses` issues signed license files
  (ed25519, `OGA9_LICENSE_SIGNING_KEY`); `POST /license/validate` is the
  unauthenticated deployment callback verifying the signature
- Billing webhook (Rule 9): `POST /webhooks/billing/{provider}` verifies
  HMAC-SHA256 (`X-Webhook-Signature`), durably records
  `UNIQUE(provider,event_id)` = replay guard, enqueues
  `billing:process_webhook` (critical) which applies the transition and
  marks `processed_at` — idempotent
- Routes: `GET /platform/apps`, `GET /platform/plans`,
  `GET/POST/DELETE /orgs/{id}/apps[/{app_id}]`, `GET/POST
  /billing/subscription`, `POST /billing/change-plan`, `POST
  /billing/cancel`, `GET /billing/invoices` (empty until g13),
  `GET /me/entitlements`; billing tenant resolved server-side from the
  caller's org_admin tenants (Rule 24)
- `RequirePlatformNode` middleware — platform.* nodes via NULL-tenant roles
- New config: `OGA9_BILLING_SWEEP_INTERVAL` (1h),
  `OGA9_LICENSE_SIGNING_KEY`, `OGA9_BILLING_WEBHOOK_SECRET`

### Added — review-fix sweep + governance reads
- GET list routes for governance (catalogued): `/orgs/{id}/positions`,
  `/terms`, `/appointments`, `/units/{id}/members` — member-visible reads
  gated on `org.members.view`, joining position/term/officer display names
- `events:retry_dead` re-drive cap: `redrive_count` column (migration
  000047), max 3 cycles so permanently-poison events can't loop forever
- Template position seeds accept `{"slug","executive"}` objects alongside
  bare slugs — non-EXCO seed roles expressible without schema change

### Fixed (deep-review sweep)
- `UpdateUnit` retyping a committee with seats/scoped appointments now
  rejects with `unit_dependents` — same guard as ArchiveUnit
- `CreateUnit` sibling-slug collisions → `slug_taken` 409 (was raw 500)
- Mailer sent no `From:` header — strict receivers showed a blank sender
- OAuth app callback moved token to `#fragment` (no server-log/Referer
  exposure)
- `scheduler:tick` sweeps run concurrently with panic containment — a slow
  or crashing sweep can't stall the tick
- Stub task handlers fail (retry→archive) instead of silently acking, so
  an early enqueue surfaces in the archived-depth metric

### Added — Phase 06 (Job Queue Conventions, §08)
- `internal/tasks` becomes the code-side task registry: all nine §08
  foundation seeds registered (Rule 7) — `identity:send_verification`,
  `identity:send_password_reset`, `events:retry_dead`, `scheduler:tick`
  implemented; `billing:retry_payment`, `billing:subscription_state_sweep`,
  `license:renewal_reminder`, `tenant:provision` wired as explicit no-op
  stubs pinned to their owning phases
- `events:retry_dead` (default tier per §08) — `RetryDeadEvents` resets
  dead-lettered `domain_events` (`dispatched_at`/`attempts`/`last_error`/
  `next_attempt_at` cleared) in bounded batches; scheduled via
  `OGA9_EVENTS_RETRY_DEAD_INTERVAL` (default 1m)
- `scheduler:tick` cron driver via `OGA9_SCHEDULER_TICK_INTERVAL`
  (default 5m): runs `tasks.OnTick`-registered sweeps and reports the
  queue-depth metric — per-queue pending/retry/archived via asynq
  Inspector + `CountDeadEvents` DLQ depth (ADR-020 reliability signal)

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
