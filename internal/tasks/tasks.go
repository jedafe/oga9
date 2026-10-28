// Ref: oga9-engineering/sections/08-job-queue-asynq.md, conventions/asynq-tasks-catalog.md
// Task registry — every name here is catalogued (Rule 7) and every handler
// idempotent (Rule 27). The mux only runs registered tasks; an enqueued
// unknown type fails instead of silently dropping.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/analytics"
	"github.com/jedafe/oga9/internal/assets"
	"github.com/jedafe/oga9/internal/elections"
	"github.com/jedafe/oga9/internal/integrations"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/ai"
	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/governance"
	"github.com/jedafe/oga9/internal/license"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/platform"
	"github.com/jedafe/oga9/internal/tenant"
)

// Task names — the code-side mirror of conventions/asynq-tasks-catalog.md.
const (
	TypeDispatchOutbox    = events.TypeDispatchOutbox
	TypeRetryDead         = "events:retry_dead"
	TypeSendVerification  = "identity:send_verification"
	TypeSendPasswordReset = "identity:send_password_reset"
	TypeRetryPayment      = "billing:retry_payment"
	TypeGenObligations    = "finance:generate_obligations"
	TypeOverdueSweep      = "finance:overdue_sweep"
	TypeIssueReceipt      = "finance:issue_receipt"
	TypeDriveClosingSweep = "finance:drive_closing_sweep"
	TypeProcessWebhook    = "billing:process_webhook"
	TypeSubscriptionSweep = "billing:subscription_state_sweep"
	TypeLicenseReminder   = "license:renewal_reminder"
	TypeTenantProvision   = "tenant:provision"
	TypeTenantExport      = "tenant:export"
	TypeSchedulerTick     = "scheduler:tick"
	TypeMemberImport      = "membership:import"
	TypeMemberExport      = "membership:export"
	// §14 comms & documents
	TypeNotifDispatch    = "notifications:dispatch"
	TypeNotifEmail       = "notifications:email"
	TypeDocProcessUpload = "documents:process_upload"
	// §16 governance
	TypeTermTransitions   = "governance:term_transitions"
	TypeHandoverReminders = "governance:handover_reminders"
	// §18 assets
	TypeAssetHandoverSweep = "assets:handover_sweep"
	TypeMaintenanceDue     = "assets:maintenance_due"
	// §22 ai
	TypeAIAnswer        = "ai:answer"
	TypeAIGenerateDraft = "ai:generate_draft"
	TypeAIUsageRollup   = "ai:usage_rollup"
	// §23 integrations
	TypeWebhookProcess    = "integrations:webhook_process"
	TypeJapaSyncPush      = "integrations:japa_sync_push"
	TypeJapaSyncReconcile = "integrations:japa_sync_reconcile"
	TypeIcsFeed           = "integrations:ics_feed"
	// §21 elections
	TypeElectionsOpenVoting  = "elections:open_voting"
	TypeElectionsCloseVoting = "elections:close_voting"
	TypeElectionsReminders   = "elections:reminders"
	// §19 analytics
	TypeRefreshSnapshots  = "analytics:refresh_snapshots"
	TypeHealthScore       = "analytics:health_score"
	TypeMonthlyReport     = "analytics:monthly_report"
	TypeProjectionRebuild = "analytics:projection_rebuild"
	// §24 chat & voice
	TypePresenceSweep  = "chat:presence_sweep"
	TypeRetentionSweep = "chat:retention_sweep"
	TypeRoomSweep      = "voice:room_sweep"
	// §12: provider webhooks are deferred (ADR-012 amendment — record-keeping
	// first). Catalog-reserved stub so an early enqueue fails visibly.
	TypePaymentsProviderWebhook = "payments:provider_webhook"
	TypeGenRecurrences          = "activities:generate_recurrences"
	// §26/§27 + Rule 11
	TypeCloseDuePolls   = "comms:close_due_polls"
	TypeCloseDueSurveys = "comms:close_due_surveys"
	TypeVolunteerRemind = "activities:volunteer_remind"
	TypeDataErasure     = "platform:data_erasure"
	TypeRemindUpcoming  = "activities:remind_upcoming"
	TypeOverdueItems    = "activities:overdue_action_items"
)

type SendEmailPayload struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// NewProcessWebhookTask builds the critical-tier webhook worker task —
// payload is the durable raw-event id; the worker re-fetches (§07/§08).
func NewProcessWebhookTask(rawEventID string) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"raw_event_id": rawEventID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeProcessWebhook, b, asynq.Queue("critical"), asynq.MaxRetry(8)), nil
}

// NewMemberImportTask builds the default-tier import worker — payload is
// the durable member_imports row id; rows become applications (§11).
func NewMemberImportTask(importID uuid.UUID) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"import_id": importID.String()})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeMemberImport, b, asynq.Queue("default"), asynq.MaxRetry(8)), nil
}

// NewGenObligationsTask builds the default-tier generation worker for one
// schedule — enqueued on DuesSchedulePublished so a fresh schedule doesn't
// wait for the next daily sweep.
func NewGenObligationsTask(scheduleID uuid.UUID) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"schedule_id": scheduleID.String()})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeGenObligations, b, asynq.Queue("default"), asynq.MaxRetry(8)), nil
}

// DuesSchedulePublishedConsumer is the catalogued obligations-gen consumer:
// publishing a schedule enqueues its own generation pass (Rule 8 — enqueue
// and return, never run generation inside the dispatcher).
func DuesSchedulePublishedConsumer(queue *asynq.Client) func(context.Context, *db.Queries, events.Event) error {
	return func(ctx context.Context, q *db.Queries, e events.Event) error {
		if e.Type != "DuesSchedulePublished" {
			return nil
		}
		sid, ok := e.Payload["schedule_id"].(string)
		if !ok {
			return nil
		}
		id, err := uuid.Parse(sid)
		if err != nil {
			return nil
		}
		task, err := NewGenObligationsTask(id)
		if err != nil {
			return err
		}
		_, err = queue.EnqueueContext(ctx, task)
		return err
	}
}

// NewEmailTask builds a queueable mail task. Handlers enqueue with
// context.WithoutCancel so a client disconnect can't drop the enqueue
// (Rule 8: fire-and-forget still means "actually enqueued").
func NewEmailTask(taskType string, p SendEmailPayload) (*asynq.Task, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(taskType, b, asynq.Queue("default"), asynq.MaxRetry(5)), nil
}

// Deps are the concrete dependencies the registry's handlers need.
type Deps struct {
	Mail                 *mailer.Mailer
	Pool                 *pgxpool.Pool
	RedisAddr            string        // asynq Inspector reports queue depth over the same conn
	Valkey               valkey.Client // sweep cadence gates (oga9:sweep:*)
	Billing              *billing.Service
	Queue                *asynq.Client // sweep handlers re-drive durable rows
	Tenant               *tenant.Service
	Finance              *finance.Service
	Activities           *activities.Service
	Comms                *comms.Service
	Ent                  *entitlements.Service
	Chat                 *chat.Service
	Platform             *platform.Service
	Governance           *governance.Service
	Analytics            *analytics.Service
	Assets               *assets.Service
	Elections            *elections.Service
	Integrations         *integrations.Service
	AI                   *ai.Service
	VoiceRoomMaxAgeHours int
	License              *license.Validator
}

// tickSweeps run inside each scheduler:tick — later phases append their
// periodic jobs (dues generation, term transitions, reminders) here.
var tickSweeps []func(context.Context) error

// OnTick registers a periodic sweep driven by scheduler:tick.
func OnTick(fn func(context.Context) error) { tickSweeps = append(tickSweeps, fn) }

// dailyGate throttles a tick sweep to once per 24h window — the first tick
// after the marker expires claims it via SET NX EX (oga9:sweep:daily:{name}
// in the cache-keys catalog); a nil Valkey runs every tick (tests).
func dailyGate(vk valkey.Client, name string, fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if vk == nil {
			return fn(ctx)
		}
		claimed, err := vk.Do(ctx, vk.B().Set().Key("oga9:sweep:daily:"+name).
			Value("1").Nx().ExSeconds(86400).Build()).AsBool()
		if err != nil {
			slog.Warn("sweep_gate", "name", name, "err", err)
			return fn(ctx) // gate unreadable — run anyway, never stall the sweep
		}
		if !claimed {
			return nil // another tick already ran this window
		}
		if err := fn(ctx); err != nil {
			// release the gate so the next tick retries — a failed sweep
			// must not wait out the whole 24h window.
			_ = vk.Do(ctx, vk.B().Del().Key("oga9:sweep:daily:"+name).Build()).Error()
			return err
		}
		return nil
	}
}

// RegisterSweeps wires the current phase's periodic jobs into the tick —
// call ONCE at startup (main.go) alongside Handlers; a second call would
// duplicate every sweep. Separate sweeps so one failure can't skip the
// others for the tick. All idempotent.
func RegisterSweeps(d Deps) {
	// §12 finance periodic jobs. Generation + overdue sweep are DAILY per
	// the §12 tasks table — the tick gate (oga9:sweep:daily:* SET NX EX)
	// runs the first tick of each window and skips the rest; a published
	// schedule gets its own DuesSchedulePublished → enqueue path instead
	// of waiting a day. Drive auto-close stays per-tick (cheap one-row scan).
	OnTick(dailyGate(d.Valkey, "generate_obligations", func(ctx context.Context) error {
		if d.Finance == nil {
			return nil
		}
		_, err := d.Finance.GenerateObligations(ctx, uuid.Nil)
		return err
	}))
	OnTick(dailyGate(d.Valkey, "overdue_sweep", func(ctx context.Context) error {
		if d.Finance == nil {
			return nil
		}
		_, err := d.Finance.OverdueSweep(ctx, 7) // catalog default grace days
		return err
	}))
	OnTick(func(ctx context.Context) error {
		if d.Finance == nil {
			return nil
		}
		return d.Finance.CloseExpiredDrives(ctx)
	})
	OnTick(func(ctx context.Context) error {
		if d.Activities == nil {
			return nil
		}
		_, err := d.Activities.RemindUpcoming(ctx)
		return err
	})
	OnTick(dailyGate(d.Valkey, "overdue_action_items", func(ctx context.Context) error {
		if d.Activities == nil {
			return nil
		}
		_, err := d.Activities.OverdueActionItems(ctx)
		return err
	}))
	OnTick(dailyGate(d.Valkey, "generate_recurrences", func(ctx context.Context) error {
		if d.Activities == nil {
			return nil
		}
		_, err := d.Activities.GenerateRecurrences(ctx, 35)
		return err
	}))
	// §14.03 daily license posture check — enqueue the catalogued task so
	// it lands in queue metrics and retries like every other job.
	OnTick(dailyGate(d.Valkey, "license_reminder", func(ctx context.Context) error {
		if d.Queue == nil {
			return nil
		}
		_, err := d.Queue.EnqueueContext(ctx, asynq.NewTask(TypeLicenseReminder, nil))
		return err
	}))
	// License expiry — emits LicenseExpired once per past-due license
	// (sourced §15 degrade signal; renewal resets the marker).
	OnTick(dailyGate(d.Valkey, "license_expiry", func(ctx context.Context) error {
		if d.Billing == nil {
			return nil
		}
		_, err := d.Billing.LicenseExpirySweep(ctx)
		return err
	}))
	// §24 chat & voice periodic jobs. Retention is daily per the catalog
	// (low-tier work); stale voice rooms and the presence scan run per
	// tick — both are cheap single queries.
	// §14: enqueue-loss redrive for email-channel notification rows —
	// the durable row is the record, a dropped task is re-sent.
	OnTick(dailyGate(d.Valkey, "email_redrive", func(ctx context.Context) error {
		return emailRedrive(d.Comms, d.Queue)(ctx)
	}))
	OnTick(dailyGate(d.Valkey, "chat_retention", func(ctx context.Context) error {
		if d.Chat == nil {
			return nil
		}
		return d.Chat.RetentionSweep(ctx)
	}))
	OnTick(func(ctx context.Context) error {
		if d.Chat == nil {
			return nil
		}
		return d.Chat.RoomSweep(ctx, d.VoiceRoomMaxAgeHours)
	})
	OnTick(func(ctx context.Context) error {
		return presenceScan(d.Valkey)(ctx)
	})
	// §26 poll/survey auto-close (per-tick, cheap scans) + §27 volunteer
	// reminders + Rule-11 erasure purge (daily).
	OnTick(func(ctx context.Context) error {
		if d.Comms == nil {
			return nil
		}
		if _, err := d.Comms.CloseDuePolls(ctx); err != nil {
			return err
		}
		_, err := d.Comms.CloseDueSurveys(ctx)
		return err
	})
	OnTick(func(ctx context.Context) error {
		if d.Activities == nil || d.Comms == nil {
			return nil
		}
		_, err := d.Activities.VolunteerRemind(ctx, d.Comms.NotifyOnce)
		return err
	})
	OnTick(dailyGate(d.Valkey, "data_erasure", func(ctx context.Context) error {
		if d.Platform == nil {
			return nil
		}
		_, err := d.Platform.PurgeExpired(ctx, d.Platform.ErasureGraceDays(ctx))
		return err
	}))
	// member_imports rows are the durable work record — an enqueue that
	// died leaves the row stranded at 'processing', so the sweep
	// re-drives it (webhook stale-redrive parity).
	OnTick(func(ctx context.Context) error {
		ids, err := d.Tenant.StaleImportIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			task, err := NewMemberImportTask(id)
			if err != nil {
				return err
			}
			if _, err := d.Queue.EnqueueContext(ctx, task); err != nil {
				return err // leave the rest for the next pass
			}
		}
		return nil
	})
	// §15 provision redrive — a worker that died mid-pipeline leaves the
	// row 'processing' past its lease. The stored payload IS the original
	// request; re-enqueue so the claim path resumes from its checkpoints
	// (tenant → org → done). Runs per tick — cheap scan, almost always 0.
	OnTick(func(ctx context.Context) error {
		if d.Platform == nil || d.Queue == nil {
			return nil
		}
		rows, err := d.Platform.StaleProvisions(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var in platform.ProvisionRequest
			if json.Unmarshal(row.Payload, &in) != nil {
				continue // unparseable intent — the row fails on next claim
			}
			task, err := NewProvisionTask(in)
			if err != nil {
				return err
			}
			if _, err := d.Queue.EnqueueContext(ctx, task); err != nil {
				return err // leave the rest for the next pass
			}
		}
		return nil
	})
}

// Handlers returns the full registry: implemented handlers + explicit stubs
// for seeds whose schema lands with their owning phase. A stub FAILS (not
// acks): an early enqueue retries then archives — the missing implementation
// stays visible in the archived-depth metric instead of being silently
// swallowed.
func Handlers(d Deps) map[string]asynq.HandlerFunc {
	return map[string]asynq.HandlerFunc{
		TypeSendVerification:  sendMail(d.Mail),
		TypeSendPasswordReset: sendMail(d.Mail),
		TypeRetryDead:         retryDead(d.Pool),
		TypeSchedulerTick:     schedulerTick(d.RedisAddr, d.Pool),
		TypeSubscriptionSweep: subscriptionSweep(d.Billing, d.Queue),
		TypeProcessWebhook:    processWebhook(d.Billing),
		TypeMemberImport:      memberImport(d.Tenant),
		TypeMemberExport:      memberExport(d.Tenant),
		// §12 finance-lite
		TypeGenObligations:    genObligations(d.Finance),
		TypeOverdueSweep:      overdueSweep(d.Finance),
		TypeIssueReceipt:      issueReceipt(d.Finance),
		TypeDriveClosingSweep: driveClosingSweep(d.Finance),
		TypeRetryPayment:      retryPayment(d.Finance),
		TypeGenRecurrences: actSweep(func(c context.Context) error {
			if d.Activities == nil {
				return nil
			}
			_, e := d.Activities.GenerateRecurrences(c, 35)
			return e
		}),
		TypeRemindUpcoming: actSweep(func(c context.Context) error {
			if d.Activities == nil {
				return nil
			}
			_, e := d.Activities.RemindUpcoming(c)
			return e
		}),
		TypeOverdueItems: actSweep(func(c context.Context) error {
			if d.Activities == nil {
				return nil
			}
			_, e := d.Activities.OverdueActionItems(c)
			return e
		}),
		// §14 comms & documents fan-out (Rule 8) + upload validation.
		TypeNotifDispatch:    notifDispatch(d.Comms, d.Queue),
		TypeNotifEmail:       notifEmail(d.Mail, d.Pool, d.Ent),
		TypeDocProcessUpload: docProcess(d.Comms),
		// §24 chat & voice sweeps.
		TypePresenceSweep:  presenceSweep(d.Valkey),
		TypeRetentionSweep: chatRetention(d.Chat),
		TypeRoomSweep:      roomSweep(d.Chat, d.VoiceRoomMaxAgeHours),
		// Stubs — registered now (catalog-reserved), implemented when their
		// sections land: license 14; provision 12; provider webhooks when
		// gateways land (ADR-012 deferred surface).
		// §16 governance — daily cadence (catalogued).
		TypeTermTransitions:   termTransitions(d.Tenant),
		TypeHandoverReminders: handoverReminders(d.Governance),
		// §19 analytics — 15min snapshots, monthly score/report, on-demand rebuild.
		TypeRefreshSnapshots:  refreshSnapshots(d.Analytics),
		TypeHealthScore:       healthScore(d.Analytics),
		TypeMonthlyReport:     monthlyReport(d.Analytics),
		TypeProjectionRebuild: refreshSnapshots(d.Analytics),
		// §18 assets — handover sweep (manual redrive; TermEnded consumer
		// is the primary path), weekly maintenance-due notices.
		TypeAssetHandoverSweep: assetHandoverSweep(d.Assets),
		TypeMaintenanceDue:     maintenanceDue(d.Assets),
		// §21 elections — date-driven voting windows, sweeps are idempotent.
		TypeElectionsOpenVoting:  electionsOpen(d.Elections),
		TypeElectionsCloseVoting: electionsClose(d.Elections),
		TypeElectionsReminders:   electionsReminders(d.Elections),
		// §23 integrations — webhook processing + japa sync cadence.
		TypeWebhookProcess:    webhookProcess(d.Integrations),
		TypeJapaSyncPush:      japaSyncPush(d.Integrations),
		TypeJapaSyncReconcile: japaSyncReconcile(d.Integrations),
		TypeIcsFeed:           icsFeed(d.Integrations),
		// §22 ai — chat/draft are request-driven (handlers answer inline);
		// the rollup computes the monthly meter on the low queue.
		TypeAIAnswer:                aiAnswer(d.AI),
		TypeAIGenerateDraft:         aiDraft(d.AI),
		TypeAIUsageRollup:           aiUsageRollup(d.AI),
		TypeLicenseReminder:         licenseReminder(d.License),
		TypeTenantProvision:         tenantProvision(d.Platform),
		TypeTenantExport:            tenantExport(d.Platform),
		TypePaymentsProviderWebhook: stub(TypePaymentsProviderWebhook, "payment gateways (ADR-012)"),
		// §26/§27 sweeps + Rule-11 erasure.
		TypeCloseDuePolls: func(ctx context.Context, t *asynq.Task) error {
			if d.Comms == nil {
				return nil
			}
			_, e := d.Comms.CloseDuePolls(ctx)
			return e
		},
		TypeCloseDueSurveys: func(ctx context.Context, t *asynq.Task) error {
			if d.Comms == nil {
				return nil
			}
			_, e := d.Comms.CloseDueSurveys(ctx)
			return e
		},
		TypeVolunteerRemind: func(ctx context.Context, t *asynq.Task) error {
			if d.Activities == nil || d.Comms == nil {
				return nil
			}
			_, e := d.Activities.VolunteerRemind(ctx, d.Comms.NotifyOnce)
			return e
		},
		TypeDataErasure: func(ctx context.Context, t *asynq.Task) error {
			if d.Platform == nil {
				return nil
			}
			var p struct {
				UserID string `json:"user_id"`
			}
			_ = json.Unmarshal(t.Payload(), &p)
			if p.UserID != "" {
				uid, err := uuid.Parse(p.UserID)
				if err != nil {
					return nil
				}
				return d.Platform.EraseUser(ctx, uid)
			}
			_, err := d.Platform.PurgeExpired(ctx, d.Platform.ErasureGraceDays(ctx))
			return err
		},
	}
}

// actSweep adapts a sweep func (closures nil-guard d.Activities themselves).
func actSweep(fn func(context.Context) error) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error { return fn(ctx) }
}

// genObligations — finance:generate_obligations {schedule_id?}: nil
// schedule fans every active schedule (daily sweep); a specific id runs
// just that one (schedule publish path).
func genObligations(svc *finance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			ScheduleID string `json:"schedule_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		sid, _ := uuid.Parse(p.ScheduleID)
		_, err := svc.GenerateObligations(ctx, sid)
		return err
	}
}

// overdueSweep — finance:overdue_sweep {}: grace-expired obligations flip
// to overdue and dues_standing re-derives for affected members.
func overdueSweep(svc *finance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		_, err := svc.OverdueSweep(ctx, 7) // org.finance.grace_days default
		return err
	}
}

// issueReceipt — finance:issue_receipt {payment_id}: receipt issuance is
// synchronous on confirm; this task is the retry path for a confirmed
// payment whose receipt insert failed (idempotent by payment unique).
func issueReceipt(svc *finance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		pid, err := uuid.Parse(p.PaymentID)
		if err != nil {
			return nil
		}
		return svc.EnsureReceipt(ctx, pid)
	}
}

// driveClosingSweep — finance:drive_closing_sweep {}: drives past
// closes_at auto-close (catalogued low tier; rolled into the tick sweep
// since it is org-global).
func driveClosingSweep(svc *finance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		return svc.CloseExpiredDrives(ctx)
	}
}

// retryPayment — billing:retry_payment {payment_id}: a failed payment
// re-enters 'initiated' for re-approval (§12, no gateway in MVP).
func retryPayment(svc *finance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		pid, err := uuid.Parse(p.PaymentID)
		if err != nil {
			return nil
		}
		return svc.RetryPayment(ctx, pid)
	}
}

func sendMail(m *mailer.Mailer) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p SendEmailPayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		if err := m.Send(p.To, p.Subject, p.Body); err != nil {
			slog.Warn("mail_task_failed", "type", t.Type(), "err", err)
			return err // asynq retries
		}
		return nil
	}
}

// stub is a placeholder for a catalogued task whose domain schema hasn't
// landed yet — it returns an error so an early enqueue retries then parks
// in the DLQ where the queue-depth metric can see it.
func stub(taskType, phase string) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		return fmt.Errorf("task %s not implemented until %s", taskType, phase)
	}
}

// retryDead re-drives dead-lettered domain_events: the dispatcher parks
// events at the attempt cap (dispatched_at set + attempts>=MaxAttempts);
// resetting them makes them claimable on the next dispatch tick (§05/§08).
func retryDead(pool *pgxpool.Pool) asynq.HandlerFunc {
	q := db.New(pool)
	return func(ctx context.Context, t *asynq.Task) error {
		n, err := q.RetryDeadEvents(ctx, db.RetryDeadEventsParams{
			MaxAttempts: events.MaxAttempts, BatchSize: 100, MaxRedrives: 3,
		})
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("events_retry_dead", "redrove", n)
		}
		return nil
	}
}

// subscriptionSweep drives the §07 lifecycle machine's time-based
// transitions (trial→grace→restricted, lapsed active→past_due) and
// re-drives durable webhook rows whose enqueue failed (Rule 9 — the
// durable record exists precisely so nothing is lost).
func subscriptionSweep(b *billing.Service, queue *asynq.Client) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if err := b.SweepStates(ctx); err != nil {
			return err
		}
		if err := b.ExpireAddonSweep(ctx); err != nil {
			return err
		}
		ids, err := b.StaleWebhookEvents(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			task, err := NewProcessWebhookTask(id.String())
			if err != nil {
				return err
			}
			if _, err := queue.EnqueueContext(ctx, task); err != nil {
				return err // leave the rest for the next pass
			}
		}
		return nil
	}
}

// memberImport turns the durable import row into 'applied' applications —
// per-row failures land in errors, never the DLQ (idempotent re-run safe:
// a crashed worker replays into duplicate applications, so the row-level
// `data` stays the single retry unit).
func memberImport(svc *tenant.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			ImportID string `json:"import_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		id, err := uuid.Parse(p.ImportID)
		if err != nil {
			return nil // malformed payload — retrying won't fix it; archive
		}
		if err := svc.ProcessImport(ctx, id); err != nil {
			if errors.Is(err, tenant.ErrImportNotFound) {
				return nil // the durable row is gone — retrying can't help
			}
			return err
		}
		return nil
	}
}

// processWebhook applies a recorded billing webhook event — idempotent via
// processed_at (§07, Rule 27).
func processWebhook(b *billing.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			RawEventID string `json:"raw_event_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		id, err := uuid.Parse(p.RawEventID)
		if err != nil {
			return nil // malformed payload — retrying won't fix it; archive
		}
		return b.ProcessWebhook(ctx, id)
	}
}

// schedulerTick is the cron driver (§08): runs registered sweeps and reports
// the queue-depth metric — asynq per-queue pending/retry/archived counts
// plus the domain_events DLQ depth (ADR-020 reliability signal).
func schedulerTick(redisAddr string, pool *pgxpool.Pool) asynq.HandlerFunc {
	q := db.New(pool)
	return func(ctx context.Context, t *asynq.Task) error {
		ins := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisAddr})
		defer ins.Close()
		queues, err := ins.Queues()
		if err != nil {
			return err
		}
		for _, name := range queues {
			qi, err := ins.GetQueueInfo(name)
			if err != nil {
				continue
			}
			if qi.Pending+qi.Retry+qi.Archived > 0 {
				slog.Info("queue_depth", "queue", name, "pending", qi.Pending, "active", qi.Active, "retry", qi.Retry, "archived", qi.Archived)
			}
		}
		if dead, err := q.CountDeadEvents(ctx, events.MaxAttempts); err == nil && dead > 0 {
			slog.Warn("events_dlq", "dead", dead)
		}
		// Sweeps run concurrently — a slow one can't stall the tick or the
		// others; a panic is contained to its own sweep.
		var wg sync.WaitGroup
		for _, sweep := range tickSweeps {
			wg.Add(1)
			go func(sw func(context.Context) error) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						slog.Error("tick_sweep_panic", "err", r)
					}
				}()
				if err := sw(ctx); err != nil {
					slog.Warn("tick_sweep_failed", "err", err)
				}
			}(sweep)
		}
		wg.Wait()
		return nil
	}
}

// NewMemberExportTask builds the export worker — payload is the durable
// member_exports row id; the CSV lands on the row (§11).
func NewMemberExportTask(exportID uuid.UUID) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"export_id": exportID.String()})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeMemberExport, b, asynq.Queue("default"), asynq.MaxRetry(5)), nil
}

// memberExport fills the durable export row with the org's member CSV —
// idempotent: a retried worker rewrites the same row.
func memberExport(svc *tenant.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			ExportID string `json:"export_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		id, err := uuid.Parse(p.ExportID)
		if err != nil {
			return nil
		}
		if err := svc.ProcessExport(ctx, id); err != nil {
			if errors.Is(err, tenant.ErrImportNotFound) {
				return nil
			}
			return err
		}
		return nil
	}
}

// NewDispatchTask — notifications:dispatch {announcement_id, email}:
// resolves the audience snapshot into in_app notification rows (Rule 8
// fan-out). allowEmail records the publisher's org.comms.broadcast grant
// at publish time — the email channel only fans out for broadcast-capable
// publishers.
func NewDispatchTask(announcementID uuid.UUID, allowEmail bool) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]any{"announcement_id": announcementID.String(), "email": allowEmail})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeNotifDispatch, b, asynq.Queue("default"), asynq.MaxRetry(8)), nil
}

func notifDispatch(svc *comms.Service, queue *asynq.Client) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			AnnouncementID string `json:"announcement_id"`
			Email          *bool  `json:"email"` // absent on pre-flag payloads → allow
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		id, err := uuid.Parse(p.AnnouncementID)
		if err != nil {
			return nil
		}
		allowEmail := p.Email == nil || *p.Email
		targets, err := svc.DispatchAnnouncement(ctx, id, allowEmail)
		if err != nil {
			if errors.Is(err, comms.ErrNotFound) {
				return nil // announcement retracted/deleted before dispatch
			}
			return err
		}
		// Rule 8: email fan-out is fire-and-forget from the worker — one
		// notifications:email per opted-in recipient (notification_id lets
		// the sender stamp delivered_at). Failed enqueues are logged AND
		// recovered by the stale-email redrive sweep — never swallowed.
		subject, body := svc.EmailSubject(ctx, id), svc.EmailBody(ctx, id)
		for _, tg := range targets {
			b, err := json.Marshal(map[string]string{
				"notification_id": tg.NotificationID.String(),
				"to":              tg.To,
				"subject":         subject,
				"body":            body,
			})
			if err != nil {
				continue
			}
			if _, err := queue.EnqueueContext(ctx, asynq.NewTask(TypeNotifEmail, b,
				asynq.Queue("default"), asynq.MaxRetry(5))); err != nil {
				slog.Warn("email_fanout_enqueue", "notification_id", tg.NotificationID, "err", err)
			}
		}
		return nil
	}
}

// emailRedrive — channel='email' rows that never reached delivered_at
// past the grace window (the enqueue died between commit and fan-out):
// the notification row is durable, so the sweep re-enqueues the send
// (idempotent — delivered_at already set means the row isn't stale).
func emailRedrive(svc *comms.Service, queue *asynq.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		if svc == nil || queue == nil {
			return nil
		}
		rows, err := svc.StaleEmailRefs(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			aid := uuid.UUID(r.RefID.Bytes)
			b, err := json.Marshal(map[string]string{
				"notification_id": uuid.UUID(r.ID.Bytes).String(),
				"to":              r.Email,
				"subject":         svc.EmailSubject(ctx, aid),
				"body":            svc.EmailBody(ctx, aid),
			})
			if err != nil {
				continue
			}
			if _, err := queue.EnqueueContext(ctx, asynq.NewTask(TypeNotifEmail, b,
				asynq.Queue("default"), asynq.MaxRetry(5))); err != nil {
				return err
			}
		}
		return nil
	}
}

// notifEmail — notifications:email: send, then stamp delivered_at on the
// notification row (delivery tracking for the email channel). Each send
// increments limit.messaging.monthly (§14 soft limit — warn at 80%).
func notifEmail(m *mailer.Mailer, pool *pgxpool.Pool, ent *entitlements.Service) asynq.HandlerFunc {
	q := db.New(pool)
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			NotificationID string `json:"notification_id"`
			To             string `json:"to"`
			Subject        string `json:"subject"`
			Body           string `json:"body"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		if err := m.Send(p.To, p.Subject, p.Body); err != nil {
			slog.Warn("mail_task_failed", "type", t.Type(), "err", err)
			return err // asynq retries — delivered_at stays unset until success
		}
		if id, err := uuid.Parse(p.NotificationID); err == nil {
			// stamp + NotificationDelivered + usage metering in one tx
			// (Rule 12). A failure logs but does NOT retry — the mail
			// already went out and a retry would double-send.
			if tx, err := pool.Begin(ctx); err == nil {
				tq := q.WithTx(tx)
				if err := tq.MarkNotificationDelivered(ctx, pgtype.UUID{Bytes: id, Valid: true}); err == nil {
					if n, err := tq.GetNotificationForDelivery(ctx, pgtype.UUID{Bytes: id, Valid: true}); err == nil {
						tid := uuid.UUID(n.TenantID.Bytes)
						_ = events.Emit(ctx, tq, tid, uuid.UUID(n.OrganizationID.Bytes),
							"NotificationDelivered", "notification", id, uuid.Nil, false,
							map[string]any{"notification_id": id})
						// §14: email broadcasts count against the monthly
						// messaging soft limit (period = first of month).
						period := time.Date(time.Now().Year(), time.Now().Month(), 1,
							0, 0, 0, 0, time.UTC)
						used, uerr := tq.AddUsage(ctx, db.AddUsageParams{
							TenantID: n.TenantID, LimitKey: "limit.messaging.monthly",
							Delta: 1, PeriodStart: pgtype.Date{Time: period, Valid: true}})
						if uerr == nil && ent != nil {
							if lim := ent.Limit(ctx, tid, "limit.messaging.monthly"); lim > 0 && used*100 >= lim*80 {
								slog.Warn("messaging_soft_limit", "tenant_id", tid,
									"used", used, "limit", lim)
							}
						}
					}
				}
				if err := tx.Commit(ctx); err != nil {
					slog.Warn("notif_delivery_stamp", "id", id, "err", err)
				}
			}
		}
		return nil
	}
}

// NewDocProcessTask — documents:process_upload {document_id}: validates
// the blob landed + is non-empty; a bad upload soft-deletes the row.
func NewDocProcessTask(documentID uuid.UUID) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"document_id": documentID.String()})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeDocProcessUpload, b, asynq.Queue("default"), asynq.MaxRetry(5)), nil
}

func docProcess(svc *comms.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			DocumentID string `json:"document_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		id, err := uuid.Parse(p.DocumentID)
		if err != nil {
			return nil
		}
		return svc.ProcessUpload(ctx, id)
	}
}

// presenceSweep — chat:presence_sweep (§24): presence keys self-expire at
// 90s TTL, so the sweep is the depth signal + safety net, not the expiry
// mechanism (deleting live keys would break heartbeats).
func presenceSweep(vk valkey.Client) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		return presenceScan(vk)(ctx)
	}
}

func presenceScan(vk valkey.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		if vk == nil {
			return nil
		}
		n, cur := 0, uint64(0)
		for {
			res, err := vk.Do(ctx, vk.B().Scan().Cursor(cur).Match("oga9:presence:*").Count(200).Build()).AsScanEntry()
			if err != nil {
				return err
			}
			n += len(res.Elements)
			if res.Cursor == 0 {
				break
			}
			cur = res.Cursor
		}
		if n > 0 {
			slog.Info("presence_sweep", "live_keys", n)
		}
		return nil
	}
}

// chatRetention — chat:retention_sweep {} (low tier): org.chat.retention_days
// per org; past-window messages soft-delete.
func chatRetention(svc *chat.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		return svc.RetentionSweep(ctx)
	}
}

// roomSweep — voice:room_sweep {}: orphaned 'live' rooms (crash, never
// ended) close past the age cap; emits VoiceRoomEnded per room.
// maxAgeHours comes from OGA9_VOICE_ROOM_MAX_AGE_HOURS (Rule 3).
func roomSweep(svc *chat.Service, maxAgeHours int) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		return svc.RoomSweep(ctx, maxAgeHours)
	}
}

// NewProvisionTask — tenant:provision {request fields}: the request_id is
// the idempotency key (durable tenant_provisions row claims the work).
func NewProvisionTask(in platform.ProvisionRequest) (*asynq.Task, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeTenantProvision, b, asynq.Queue("critical"), asynq.MaxRetry(3)), nil
}

// licenseReminder — license:renewal_reminder (daily via tick + task):
// re-checks the signed file offline and warns inside the grace window
// (§09 degradation matrix — refresh when reachable, warn when not).
func licenseReminder(v *license.Validator) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if v == nil {
			return nil
		}
		st, f, err := v.Check()
		switch {
		case err != nil:
			slog.Warn("license_check", "status", st, "err", err)
		case st == license.StatusGrace:
			slog.Warn("license_grace", "expires_at", f.ExpiresAt, "deployment_id", f.DeploymentID)
		case st == license.StatusExpired || st == license.StatusInvalid:
			slog.Error("license_degraded", "status", st, "deployment_id", f.DeploymentID)
		}
		return nil
	}
}

// tenantProvision runs the §15 pipeline; a conflict means another worker
// holds the row — retry later (MaxRetry 3 covers transient races).
func tenantProvision(svc *platform.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var in platform.ProvisionRequest
		if err := json.Unmarshal(t.Payload(), &in); err != nil {
			return fmt.Errorf("tenant:provision payload: %w", err)
		}
		if svc == nil {
			return fmt.Errorf("platform service not wired")
		}
		_, _, _, err := svc.Provision(ctx, in, in.RequestedBy)
		return err
	}
}

// NewTenantExportTask — tenant:export {export_id, passphrase}:
// durable-row-driven (the row is the idempotency record; retries re-run
// build+store). passphrase rides the task payload only — ADR-026 keeps
// it out of tenant_exports.options.
func NewTenantExportTask(exportID uuid.UUID, passphrase string) (*asynq.Task, error) {
	b, err := json.Marshal(map[string]string{"export_id": exportID.String(), "passphrase": passphrase})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeTenantExport, b, asynq.Queue("default"), asynq.MaxRetry(3)), nil
}

// tenantExport — §13.06/ADR-026: builds the signed tenant_export_v1
// bundle, stores the blob, marks the tenant_exports row done.
func tenantExport(svc *platform.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p struct {
			ExportID   string `json:"export_id"`
			Passphrase string `json:"passphrase"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return fmt.Errorf("tenant:export payload: %w", err)
		}
		if svc == nil {
			return fmt.Errorf("platform service not wired")
		}
		id, err := uuid.Parse(p.ExportID)
		if err != nil {
			return nil // malformed id — retrying won't help
		}
		return svc.RunTenantExport(ctx, id, p.Passphrase)
	}
}

// termTransitions — governance:term_transitions (§16): ends terms whose
// ends_at passed — the tenant service's close flow revokes appointments,
// emits AppointmentEnded/TermEnded, and invalidates permission caches.
func termTransitions(svc *tenant.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.CloseExpiredTerms(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("term_transitions", "closed", n)
		}
		return nil
	}
}

// handoverReminders — governance:handover_reminders (§16): terms inside
// the 14-day window get a handover snapshot generated + archived. The
// service dedupes per term, so a re-run or a term-close generation can
// never double-emit HandoverGenerated.
func handoverReminders(svc *governance.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		terms, err := svc.TermsEndingSoon(ctx, 14)
		if err != nil {
			return err
		}
		for _, t := range terms {
			orgID := uuid.UUID(t.OrganizationID.Bytes)
			tid, err := svc.OrgTenant(ctx, orgID)
			if err != nil {
				return err
			}
			oc := &tenant.OrgCtx{OrgID: orgID, TenantID: tid}
			if err := svc.ArchiveHandover(ctx, oc, uuid.UUID(t.ID.Bytes)); err != nil {
				return err // deduped — safe to retry the whole task
			}
		}
		return nil
	}
}

// refreshSnapshots — analytics:refresh_snapshots / projection_rebuild
// (§19): rebuild every live org's role snapshots from the projection +
// section producers. Idempotent upserts — safe to re-drive.
func refreshSnapshots(svc *analytics.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.RefreshSnapshots(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("refresh_snapshots", "orgs", n)
		}
		return nil
	}
}

// healthScore — analytics:health_score (§19): monthly score + KPI
// threshold evaluation per org.
func healthScore(svc *analytics.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.ComputeHealthScores(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("health_score", "orgs", n)
		}
		return nil
	}
}

// monthlyReport — analytics:monthly_report (§19): one run per org per
// period (dedupe inside the service).
func monthlyReport(svc *analytics.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.MonthlyReports(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("monthly_report", "runs", n)
		}
		return nil
	}
}

// assetHandoverSweep — assets:handover_sweep (§18): manual redrive of the
// term-close flag pass. Payload may carry {"term_id": "..."}; without one
// the task no-ops (the TermEnded consumer is the primary path).
func assetHandoverSweep(svc *assets.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		var p struct {
			TermID string `json:"term_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil || p.TermID == "" {
			return nil
		}
		termID, err := uuid.Parse(p.TermID)
		if err != nil {
			return nil
		}
		n, err := svc.FlagUnreturned(ctx, termID)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("asset_handover_sweep", "flagged", n)
		}
		return nil
	}
}

// maintenanceDue — assets:maintenance_due (§18, weekly): custodians get a
// MaintenanceDue notice inside a 7-day lookahead.
func maintenanceDue(svc *assets.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.MaintenanceDueSweep(ctx, 7)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("maintenance_due", "records", n)
		}
		return nil
	}
}

// electionsOpen/close/reminders — §21 window sweeps; all idempotent CAS
// transitions (safe to re-drive).
func electionsOpen(svc *elections.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.OpenSweep(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("elections_open", "transitions", n)
		}
		return nil
	}
}

func electionsClose(svc *elections.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.CloseSweep(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("elections_close", "concluded", n)
		}
		return nil
	}
}

func electionsReminders(svc *elections.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.RemindersSweep(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("elections_reminders", "emitted", n)
		}
		return nil
	}
}

// webhookProcess — integrations:webhook_process (§23, critical): drains
// unprocessed receipts (payload may carry a specific receipt_id).
func webhookProcess(svc *integrations.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		var p struct {
			ReceiptID string `json:"receipt_id"`
		}
		_ = json.Unmarshal(t.Payload(), &p)
		if p.ReceiptID != "" {
			id, err := uuid.Parse(p.ReceiptID)
			if err != nil {
				return nil
			}
			return svc.ProcessReceipt(ctx, id)
		}
		return svc.ProcessQueuedReceipts(ctx)
	}
}

// japaSyncPush — integrations:japa_sync_push (§23, default): drains
// queued push jobs (transport lands when JEOS ingestion is spec'd; the
// job records outcome either way).
func japaSyncPush(svc *integrations.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.PushSyncJobs(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("japa_sync_push", "jobs", n)
		}
		return nil
	}
}

// japaSyncReconcile — daily pull-fallback reconcile (§23).
func japaSyncReconcile(svc *integrations.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.Reconcile(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("japa_sync_reconcile", "orgs", n)
		}
		return nil
	}
}

// icsFeed — integrations:ics_feed (§23, low): registered cadence; feeds
// render on demand.
func icsFeed(svc *integrations.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		return svc.IcsFeedSweep(ctx)
	}
}

// §22 ai — async answer/draft carry the asker's identity in payload;
// the task rebuilds the OrgCtx server-side (Rule 24 — the ids resolve
// from the tables, never from the payload's claims alone).
func aiAnswer(svc *ai.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		var p struct {
			OrgID    string `json:"org_id"`
			UserID   string `json:"user_id"`
			Persona  string `json:"persona"`
			Question string `json:"question"`
			ConvID   string `json:"conversation_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		oc, err := aiOrgCtx(ctx, svc, p.OrgID, p.UserID)
		if err != nil {
			return err
		}
		conv, _ := uuid.Parse(p.ConvID)
		_, _, _, err = svc.Chat(ctx, oc, conv, p.Persona, p.Question)
		return err
	}
}

func aiDraft(svc *ai.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		if svc == nil {
			return nil
		}
		var p struct {
			OrgID    string `json:"org_id"`
			UserID   string `json:"user_id"`
			Kind     string `json:"kind"`
			Prompt   string `json:"prompt"`
			TargetID string `json:"target_id"`
		}
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		oc, err := aiOrgCtx(ctx, svc, p.OrgID, p.UserID)
		if err != nil {
			return err
		}
		tid, _ := uuid.Parse(p.TargetID)
		_, err = svc.Draft(ctx, oc, p.Kind, p.Prompt, tid)
		return err
	}
}

// aiOrgCtx rebuilds the request-time OrgCtx for a queued AI job — the
// membership+tenant resolve from the tables (never the payload's word).
func aiOrgCtx(ctx context.Context, svc *ai.Service, orgID, userID string) (*tenant.OrgCtx, error) {
	o, err := uuid.Parse(orgID)
	if err != nil {
		return nil, fmt.Errorf("ai job: bad org_id")
	}
	u, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("ai job: bad user_id")
	}
	return svc.ResolveMember(ctx, o, u)
}

func aiUsageRollup(svc *ai.Service) asynq.HandlerFunc {
	return func(ctx context.Context, _ *asynq.Task) error {
		if svc == nil {
			return nil
		}
		n, err := svc.UsageRollup(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			slog.Info("ai_usage_rollup", "tenants", n)
		}
		return nil
	}
}
