-- Ref: oga9-engineering/sections/12-finance-lite.md — finance-lite queries.
-- Rule 19: ledger_entries is append-only — no UPDATE/DELETE queries exist.
-- Rule 9: payment creation is idempotent on payments.idempotency_key.

-- ---------- contribution types ----------
-- name: CreateContributionType :one
INSERT INTO contribution_types (organization_id, key, label, kind)
VALUES (sqlc.arg(organization_id), sqlc.arg(key), sqlc.arg(label), sqlc.arg(kind))
RETURNING *;

-- name: GetContributionType :one
SELECT * FROM contribution_types WHERE id = $1 AND organization_id = $2;

-- name: ListContributionTypes :many
SELECT * FROM contribution_types WHERE organization_id = $1 ORDER BY key;

-- name: EnsureContributionType :one
-- Idempotent type bootstrap for system-created obligations (defaulter
-- fines): the org-extensible taxonomy keeps 'fine' as a catalogued key.
INSERT INTO contribution_types (organization_id, key, label, kind)
VALUES ($1, $2, $3, $4)
ON CONFLICT (organization_id, key) DO UPDATE SET key = EXCLUDED.key
RETURNING id;

-- ---------- dues schedules ----------
-- name: CreateDuesSchedule :one
INSERT INTO dues_schedules (organization_id, contribution_type_id, name,
  amount_minor, currency, recurrence, applies_to, starts_on, ends_on, active)
VALUES (sqlc.arg(organization_id), sqlc.arg(contribution_type_id), sqlc.arg(name),
  sqlc.arg(amount_minor), sqlc.arg(currency), sqlc.arg(recurrence),
  sqlc.arg(applies_to), sqlc.arg(starts_on), sqlc.arg(ends_on), sqlc.arg(active))
RETURNING *;

-- name: GetDuesSchedule :one
SELECT * FROM dues_schedules WHERE id = $1 AND organization_id = $2;

-- name: ListDuesSchedules :many
SELECT * FROM dues_schedules WHERE organization_id = $1 ORDER BY starts_on DESC, id;

-- name: UpdateDuesSchedule :execrows
-- Full edit surface (catalog PATCH): NULL args keep the column; explicit
-- clear_ends_on writes NULL (the "ends_on": null PATCH semantic).
UPDATE dues_schedules SET
  name = COALESCE(sqlc.narg(name)::text, name),
  amount_minor = COALESCE(sqlc.narg(amount_minor)::bigint, amount_minor),
  recurrence = COALESCE(sqlc.narg(recurrence)::text, recurrence),
  applies_to = COALESCE(sqlc.narg(applies_to)::jsonb, applies_to),
  starts_on = COALESCE(sqlc.narg(starts_on)::date, starts_on),
  ends_on = CASE WHEN sqlc.arg(clear_ends_on)::boolean THEN NULL
                 ELSE COALESCE(sqlc.narg(ends_on)::date, ends_on) END,
  active = COALESCE(sqlc.narg(active)::boolean, active)
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(organization_id)::uuid;

-- name: RegistrationSchedules :many
-- 'registration' contribution types back the ADR-014 approved→registered
-- guard: an org that charges registration publishes a schedule for it.
SELECT s.* FROM dues_schedules s
JOIN contribution_types ct ON ct.id = s.contribution_type_id
WHERE s.organization_id = $1 AND s.active AND ct.key = 'registration';

-- name: UnpaidRegistrationObligations :one
-- approved→registered guard: count the member's unsettled registration
-- obligations (open/partially_paid/overdue). 0 = clear to register.
SELECT count(*) FROM obligations o
JOIN contribution_types ct ON ct.id = o.contribution_type_id
WHERE o.organization_id = $1 AND o.membership_id = $2
  AND ct.key = 'registration'
  AND o.status IN ('open','partially_paid','overdue');

-- name: CountTenantActiveMembers :one
-- limit.members.active enforcement: active+life_member count across the
-- tenant's orgs (limits resolve at tenant scope, §07).
SELECT count(*) FROM organization_memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE o.tenant_id = $1::uuid AND m.lifecycle_state IN ('active','life_member');

-- name: ActiveDuesSchedules :many
-- 'registration'-type schedules are excluded: their obligations mint on
-- MemberApproved via the consumer, one per new member — periodic fan-out
-- would charge the whole existing roster and double-charge new members.
SELECT * FROM dues_schedules s WHERE s.active
  AND s.starts_on <= now()::date AND (s.ends_on IS NULL OR s.ends_on >= now()::date)
  AND NOT EXISTS (SELECT 1 FROM contribution_types ct
    WHERE ct.id = s.contribution_type_id AND ct.key = 'registration');

-- ---------- obligations ----------
-- Idempotent generation: the (membership,type,period) unique makes replay
-- a no-op; RETURNING surfaces only freshly-created rows for events.
-- name: CreateObligation :one
INSERT INTO obligations (organization_id, membership_id, contribution_type_id,
  schedule_id, period_label, amount_minor, currency, due_date)
VALUES (sqlc.arg(organization_id), sqlc.arg(membership_id), sqlc.arg(contribution_type_id),
  sqlc.arg(schedule_id), sqlc.arg(period_label), sqlc.arg(amount_minor),
  sqlc.arg(currency), sqlc.arg(due_date))
ON CONFLICT (membership_id, contribution_type_id, period_label) DO NOTHING
RETURNING *;

-- name: ObligationPayees :many
-- Active members matching a schedule's applies_to filter — {} or missing
-- keys mean "everyone". 'floor' semantics: applies_to.category when present
-- is the allow-list; life members opt out per category rule.
SELECT id FROM organization_memberships m
WHERE m.organization_id = sqlc.arg(organization_id)
  AND (m.lifecycle_state = 'active'
       -- "life members exempt per org rule": they join the payee set only
       -- when the schedule opts in (applies_to.include_life_members).
       OR (m.lifecycle_state = 'life_member' AND sqlc.arg(include_life)::bool))
  AND (COALESCE(sqlc.arg(categories)::text[], '{}'::text[]) = '{}'::text[]
       OR m.category = ANY(sqlc.arg(categories)::text[]))
  AND (COALESCE(sqlc.arg(unit_ids)::uuid[], '{}'::uuid[]) = '{}'::uuid[]
       OR m.org_unit_id = ANY(sqlc.arg(unit_ids)::uuid[]));

-- name: GetObligation :one
SELECT * FROM obligations WHERE id = $1 AND organization_id = $2;

-- name: ListObligations :many
SELECT * FROM obligations WHERE organization_id = $1
  AND ($2::uuid IS NULL OR membership_id = $2)
  AND ($3::text = '' OR status = $3)
  AND ($4::text = '' OR period_label = $4)
ORDER BY due_date DESC LIMIT 500;

-- name: SetObligationStatus :execrows
-- waived/cancelled are terminal; 'paid' MAY reopen to open/partially_paid —
-- a reversed payment legitimately un-pays the obligation (recompute-driven).
UPDATE obligations SET status = $3 WHERE id = $1 AND organization_id = $2
  AND status <> ALL (ARRAY['waived','cancelled']::text[]);

-- name: WaiveObligation :execrows
UPDATE obligations SET status = 'waived', waived_by = $3
WHERE id = $1 AND organization_id = $2 AND status IN ('open','partially_paid','overdue');

-- name: ConfirmedPaymentsForObligation :one
-- Status derives from confirmed ledger money, not payment row state alone.
SELECT COALESCE(sum(p.amount_minor), 0)::bigint AS paid_minor
FROM payments p
WHERE p.obligation_id = $1 AND p.status IN ('confirmed','reconciled');

-- name: MarkObligationsOverdue :many
-- Per-org grace (Rule 23): org.finance.grace_days org_setting wins; the arg
-- is the catalog default when the org hasn't set one.
UPDATE obligations o SET status = 'overdue'
WHERE o.status IN ('open','partially_paid')
  AND o.due_date < now()::date - COALESCE((
      SELECT (s.value #>> '{}')::int FROM org_settings s
      WHERE s.organization_id = o.organization_id
        AND s.key = 'org.finance.grace_days'
        AND jsonb_typeof(s.value) = 'number'), sqlc.arg(grace_days)::int)
RETURNING id, organization_id, membership_id;

-- name: OpenObligationsForMember :many
SELECT * FROM obligations WHERE membership_id = $1
  AND status IN ('open','partially_paid','overdue') ORDER BY due_date;

-- ---------- payments (Rule 9 idempotency) ----------
-- name: CreatePayment :one
INSERT INTO payments (organization_id, membership_id, obligation_id, amount_minor,
  currency, channel, external_ref, benefit_member_id, drive_id, activity_id,
  idempotency_key, claim_id, recorded_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(membership_id), sqlc.arg(obligation_id),
  sqlc.arg(amount_minor), sqlc.arg(currency), sqlc.arg(channel), sqlc.arg(external_ref),
  sqlc.arg(benefit_member_id), sqlc.arg(drive_id), sqlc.narg(activity_id)::uuid,
  sqlc.arg(idempotency_key), sqlc.arg(claim_id), sqlc.arg(recorded_by))
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- name: PaymentByIdempotencyKey :one
SELECT * FROM payments WHERE organization_id = $1 AND idempotency_key = $2;

-- name: PaymentOrgForIdempotencyKey :one
-- The key is globally unique; a replay scoped to another org must surface a
-- conflict, not a 500 (Rule 9's marker is per-payer, not per-org).
SELECT organization_id FROM payments WHERE idempotency_key = $1;

-- name: GetPayment :one
SELECT * FROM payments WHERE id = $1 AND organization_id = $2;

-- name: GetPaymentAny :one
-- Task context — the org is read from the row itself (issue_receipt retry).
SELECT * FROM payments WHERE id = $1;


-- name: ConfirmPayment :one
UPDATE payments SET status = 'confirmed', approved_by = $3, confirmed_at = now()
WHERE id = $1 AND organization_id = $2 AND status = 'initiated'
  AND (recorded_by IS NULL OR recorded_by <> $3)
RETURNING *;

-- name: FailPayment :execrows
UPDATE payments SET status = 'failed' WHERE id = $1 AND organization_id = $2
  AND status = 'initiated';


-- name: ReversePayment :one
UPDATE payments SET status = 'reversed'
WHERE id = $1 AND organization_id = $2 AND status IN ('confirmed','reconciled')
RETURNING *;

-- name: ReconcilePayment :one
UPDATE payments SET status = 'reconciled'
WHERE id = $1 AND organization_id = $2 AND status = 'confirmed'
RETURNING *;

-- name: OrgPaymentTotals :one
SELECT
  COALESCE(sum(CASE WHEN p.status IN ('confirmed','reconciled') THEN p.amount_minor END),0)::bigint AS collected_minor,
  COALESCE(sum(CASE WHEN p.status = 'initiated' THEN p.amount_minor END),0)::bigint AS pending_minor,
  count(*) FILTER (WHERE p.status = 'confirmed') AS confirmed_count
FROM payments p WHERE p.organization_id = $1;

-- ---------- ledger (append-only) ----------
-- name: PostLedgerEntry :one
INSERT INTO ledger_entries (organization_id, payment_id, reversal_of, direction,
  account, amount_minor, currency, memo, posted_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(payment_id), sqlc.arg(reversal_of),
  sqlc.arg(direction), sqlc.arg(account), sqlc.arg(amount_minor), sqlc.arg(currency),
  sqlc.arg(memo), sqlc.arg(posted_by))
RETURNING *;

-- name: LedgerMemoExists :one
-- idempotent welfare disbursement: BenefitDisbursed is at-least-once —
-- the consumer dedupes on the benefit-keyed memo.
SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE organization_id = $1 AND memo = $2);

-- name: ListLedgerEntries :many
SELECT * FROM ledger_entries WHERE organization_id = $1
  AND ($2::text = '' OR account = $2)
ORDER BY posted_at DESC LIMIT 500;

-- name: OrgLedgerTotals :many
SELECT account, direction, COALESCE(sum(amount_minor),0)::bigint AS total_minor
FROM ledger_entries WHERE organization_id = $1 GROUP BY account, direction;

-- name: OrgLedgerTimeTotals :one
-- §12 org statement: purse balance plus month/year inflow-outflow.
SELECT
  COALESCE(sum(CASE WHEN direction = 'credit' THEN amount_minor END),0)::bigint AS credit_minor,
  COALESCE(sum(CASE WHEN direction = 'debit' THEN amount_minor END),0)::bigint AS debit_minor,
  COALESCE(sum(CASE WHEN direction = 'credit' AND posted_at >= date_trunc('month', now()) THEN amount_minor END),0)::bigint AS month_credit_minor,
  COALESCE(sum(CASE WHEN direction = 'debit' AND posted_at >= date_trunc('month', now()) THEN amount_minor END),0)::bigint AS month_debit_minor,
  COALESCE(sum(CASE WHEN direction = 'credit' AND posted_at >= date_trunc('year', now()) THEN amount_minor END),0)::bigint AS year_credit_minor,
  COALESCE(sum(CASE WHEN direction = 'debit' AND posted_at >= date_trunc('year', now()) THEN amount_minor END),0)::bigint AS year_debit_minor
FROM ledger_entries WHERE organization_id = $1;

-- name: PaymentCreditEntry :one
SELECT * FROM ledger_entries WHERE payment_id = $1 AND direction = 'credit'
  AND reversal_of IS NULL ORDER BY posted_at LIMIT 1;

-- ---------- receipts ----------
-- name: NextReceiptSeq :one
UPDATE organizations SET receipt_no_seq = receipt_no_seq + 1
WHERE id = $1 RETURNING receipt_no_seq;

-- name: CreateReceipt :one
INSERT INTO receipts (organization_id, payment_id, receipt_no, issued_to)
VALUES (sqlc.arg(organization_id), sqlc.arg(payment_id), sqlc.arg(receipt_no), sqlc.arg(issued_to))
RETURNING *;


-- name: ReceiptForPayment :one
SELECT * FROM receipts WHERE payment_id = $1;

-- ---------- reconciliation ----------
-- name: CreateReconciliation :one
INSERT INTO reconciliation_records (organization_id, payment_id, reconciled_by, note)
VALUES (sqlc.arg(organization_id), sqlc.arg(payment_id), sqlc.arg(reconciled_by), sqlc.arg(note))
RETURNING *;

-- ---------- drives ----------
-- name: CreateDrive :one
INSERT INTO contribution_drives (organization_id, title, purpose, kind,
  fixed_amount_minor, target_amount_minor, benefit_member_id,
  contribution_type_id, closes_at, created_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(title), sqlc.arg(purpose), sqlc.arg(kind),
  sqlc.arg(fixed_amount_minor), sqlc.arg(target_amount_minor), sqlc.arg(benefit_member_id),
  sqlc.arg(contribution_type_id), sqlc.arg(closes_at), sqlc.arg(created_by))
RETURNING *;

-- name: GetDrive :one
SELECT * FROM contribution_drives WHERE id = $1 AND organization_id = $2;

-- name: ListDrives :many
SELECT * FROM contribution_drives WHERE organization_id = $1
  AND ($2::text = '' OR state = $2) ORDER BY created_at DESC;

-- name: SetDriveState :execrows
UPDATE contribution_drives SET state = $3 WHERE id = $1 AND organization_id = $2
  AND state = $4::text;

-- name: DrivePayments :many
SELECT * FROM payments WHERE drive_id = $1
  AND status IN ('confirmed','reconciled') ORDER BY created_at;

-- name: DriveContributionRows :many
-- Contributor list with per-claim hide flags (§12 drive transparency):
-- the service masks member identity for viewers without review rights.
SELECT p.id, p.membership_id, p.amount_minor, p.currency, p.channel,
       p.confirmed_at, c.hide_contributor
FROM payments p
LEFT JOIN payment_claims c ON c.id = p.claim_id
WHERE p.drive_id = $1 AND p.status IN ('confirmed','reconciled')
ORDER BY p.created_at;

-- name: DriveCollected :one
SELECT COALESCE(sum(amount_minor),0)::bigint AS collected_minor,
       count(*)::int AS contributors
FROM payments WHERE drive_id = $1 AND status IN ('confirmed','reconciled');

-- name: ExpiredOpenDrives :many
SELECT id, organization_id FROM contribution_drives
WHERE state = 'open' AND closes_at < now();

-- ---------- claims ----------
-- name: CreateClaim :one
INSERT INTO payment_claims (organization_id, membership_id, drive_id, obligation_id,
  amount_minor, currency, channel, external_ref, receipt_media_id, note, hide_contributor)
VALUES (sqlc.arg(organization_id), sqlc.arg(membership_id), sqlc.arg(drive_id),
  sqlc.arg(obligation_id), sqlc.arg(amount_minor), sqlc.arg(currency), sqlc.arg(channel),
  sqlc.arg(external_ref), sqlc.arg(receipt_media_id), sqlc.arg(note), sqlc.arg(hide_contributor))
ON CONFLICT (membership_id, external_ref) WHERE external_ref IS NOT NULL AND state <> 'rejected' DO NOTHING
RETURNING *;

-- name: GetClaim :one
SELECT * FROM payment_claims WHERE id = $1 AND organization_id = $2;

-- name: ListClaims :many
SELECT * FROM payment_claims WHERE organization_id = $1
  AND ($2::text = '' OR state = $2) ORDER BY created_at DESC LIMIT 500;


-- name: ConfirmClaim :one
UPDATE payment_claims SET state = 'confirmed', reviewed_by = $3, reviewed_at = now(),
  review_note = $4, payment_id = $5
WHERE id = $1 AND organization_id = $2 AND state = 'submitted' RETURNING *;

-- name: RejectClaim :one
UPDATE payment_claims SET state = 'rejected', reviewed_by = $3, reviewed_at = now(),
  review_note = $4
WHERE id = $1 AND organization_id = $2 AND state = 'submitted' RETURNING *;

-- ---------- dues standing + renewals ----------
-- name: SetDuesStanding :execrows
UPDATE organization_memberships SET dues_standing = $3
WHERE id = $1 AND organization_id = $2;

-- name: MembersWithOverdueObligations :many
SELECT DISTINCT m.id, m.organization_id FROM organization_memberships m
JOIN obligations o ON o.membership_id = m.id
WHERE m.organization_id = $1 AND o.status = 'overdue'
  AND m.lifecycle_state IN ('active','suspended','life_member');

-- name: SetRenewalPayment :execrows
-- The linked payment must be a confirmed/reconciled payment of THIS org AND
-- this membership — a renewal settles with the member's own verified money,
-- and one payment settles at most one renewal (the partial unique index
-- membership_renewals_payment_uq is the concurrent-writer backstop).
UPDATE membership_renewals r SET payment_id = $3
WHERE r.id = $1 AND r.membership_id IN (
    SELECT m.id FROM organization_memberships m WHERE m.organization_id = $2)
  AND EXISTS (SELECT 1 FROM payments p WHERE p.id = $3
    AND p.organization_id = $2 AND p.membership_id = r.membership_id
    AND p.status IN ('confirmed','reconciled'))
  AND NOT EXISTS (SELECT 1 FROM membership_renewals r2 WHERE r2.payment_id = $3);


-- name: GetDuesScheduleByID :one
-- Task context — org is read from the row itself.
SELECT * FROM dues_schedules WHERE id = $1;

-- name: RetryPaymentByID :execrows
UPDATE payments SET status = 'initiated' WHERE id = $1 AND status = 'failed';

-- name: GetReceiptByIDOrg :one
-- Receipts are org-scoped via their payment.
SELECT r.* FROM receipts r JOIN payments p ON p.id = r.payment_id
WHERE r.id = $1 AND p.organization_id = $2;

-- name: MyClaimsAll :many
-- /me/claims — every claim the user filed across their memberships.
SELECT c.* FROM payment_claims c
JOIN organization_memberships m ON m.id = c.membership_id
WHERE m.user_id = $1 ORDER BY c.created_at DESC;

-- name: MyFinanceFeed :many
-- /me/finance — open obligations + payments across all of the user's
-- memberships (cross-org member view; per-org reads stay org-scoped).
-- Chronological interleave: payments sort by created_at, obligations by
-- due_date — a single COALESCE key mixes both streams.
SELECT * FROM (
  SELECT 'obligation' AS kind, o.id, o.period_label AS label, o.amount_minor,
    o.currency, o.status, o.due_date::text AS ref_date, NULL::timestamptz AS at,
    o.organization_id
  FROM obligations o
  JOIN organization_memberships m ON m.id = o.membership_id
  WHERE m.user_id = $1 AND o.status IN ('open','partially_paid','overdue')
  UNION ALL
  SELECT 'payment', p.id, p.channel, p.amount_minor, p.currency, p.status,
    NULL, p.created_at, p.organization_id
  FROM payments p
  JOIN organization_memberships m ON m.id = p.membership_id
  WHERE m.user_id = $1
) feed
ORDER BY COALESCE(at, ref_date::timestamptz) DESC LIMIT 100;

-- name: GetOrgTenantID :one
-- Events emitted from org-scoped finance sweeps/tasks need the org's tenant.
SELECT tenant_id FROM organizations WHERE id = $1;

-- name: UpdateDrive :execrows
-- Open drives only: title/purpose/close-window edits (drives.manage PATCH).
-- NULL args keep the column — a partial PATCH changes only what it sends.
UPDATE contribution_drives SET
  title = COALESCE(sqlc.narg(title)::text, title),
  purpose = COALESCE(sqlc.narg(purpose)::text, purpose),
  closes_at = COALESCE(sqlc.narg(closes_at)::timestamptz, closes_at)
WHERE id = $1 AND organization_id = $2 AND state = 'open';

-- name: GetMembershipOrg :one
-- FK-existence is not org-scope: linked memberships must be same-org.
SELECT organization_id FROM organization_memberships WHERE id = $1;

-- name: GetMemberStanding :one
SELECT dues_standing FROM organization_memberships WHERE id = $1;

-- ---------- statement read-models (§12 "who's paid") ----------
-- name: ObligationRollCall :many
-- /finance/obligations/status + /schedules/{sid}/status: per-member roll —
-- obligation, amount, paid-so-far, status — filterable by schedule/period/
-- contribution type.
SELECT o.id, o.membership_id, m.member_no, o.contribution_type_id,
       o.schedule_id, o.period_label, o.amount_minor, o.currency, o.status,
       o.due_date,
       COALESCE((SELECT sum(p.amount_minor) FROM payments p
                 WHERE p.obligation_id = o.id
                   AND p.status IN ('confirmed','reconciled')),0)::bigint AS paid_minor
FROM obligations o
JOIN organization_memberships m ON m.id = o.membership_id
WHERE o.organization_id = $1
  AND ($2::uuid IS NULL OR o.schedule_id = $2)
  AND ($3::text = '' OR o.period_label = $3)
  AND ($4::uuid IS NULL OR o.contribution_type_id = $4)
ORDER BY o.due_date DESC, m.member_no LIMIT 500;

-- name: ContributionTotals :many
-- /finance/contributions: per-type contribution history — confirmed money
-- grouped by contribution type (ad-hoc payments bucket as 'general').
SELECT COALESCE(ct.key, dct.key,
                CASE WHEN p.benefit_member_id IS NOT NULL THEN 'donation' END,
                'general') AS contribution_type,
       COALESCE(ct.label, dct.label,
                CASE WHEN p.benefit_member_id IS NOT NULL THEN 'Member-directed donation' END,
                'General / ad-hoc') AS label,
       count(*)::int AS payments_count,
       COALESCE(sum(p.amount_minor),0)::bigint AS total_minor
FROM payments p
LEFT JOIN obligations o ON o.id = p.obligation_id
LEFT JOIN contribution_types ct ON ct.id = o.contribution_type_id
LEFT JOIN contribution_drives d ON d.id = p.drive_id
LEFT JOIN contribution_types dct ON dct.id = d.contribution_type_id
WHERE p.organization_id = $1 AND p.status IN ('confirmed','reconciled')
GROUP BY COALESCE(ct.key, dct.key,
                  CASE WHEN p.benefit_member_id IS NOT NULL THEN 'donation' END,
                  'general'),
         COALESCE(ct.label, dct.label,
                  CASE WHEN p.benefit_member_id IS NOT NULL THEN 'Member-directed donation' END,
                  'General / ad-hoc')
ORDER BY total_minor DESC;

-- name: ActivityDonations :many
-- /activities/{aid}/donations: event-linked contributions (payments.activity_id
-- is untyped until g10 lands the activities table — query the column anyway).
SELECT * FROM payments WHERE organization_id = $1 AND activity_id = $2
  AND status IN ('confirmed','reconciled') ORDER BY created_at;

-- name: AdjustFineForPeriod :execrows
-- Re-mark correction between fined statuses (late<->absent): the open fine
-- takes the new amount rather than keeping the stale one or duplicating
-- against the (member,type,period) unique. A previously-waived fine is
-- REVIVED (re-mark made it due again — the waive was for the old mark).
-- partially_paid/paid rows are left alone — money already moved; that's a
-- credit/reversal decision.
UPDATE obligations o SET amount_minor = $4,
  status = CASE WHEN o.status='waived' THEN 'open' ELSE o.status END
FROM contribution_types ct
WHERE ct.id = o.contribution_type_id AND ct.key = 'fine'
  AND o.membership_id = $1 AND o.organization_id = $2
  AND o.period_label = $3 AND o.status IN ('open','overdue','waived');

-- name: WaiveFineForPeriod :many
-- Re-mark correction: revoke an open assessed fine for one member+period
-- (e.g. 'attendance:{activity}') — a corrected mark must not keep the fine.
UPDATE obligations o SET status = 'waived'
FROM contribution_types ct
WHERE ct.id = o.contribution_type_id AND ct.key = 'fine'
  AND o.membership_id = $1 AND o.organization_id = $2
  AND o.period_label = $3 AND o.status IN ('open','partially_paid','overdue')
RETURNING o.id;
