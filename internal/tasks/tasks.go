// Ref: oga9-engineering/sections/08-job-queue-asynq.md, conventions/asynq-tasks-catalog.md
// Task registry — every name here is catalogued (Rule 7) and every handler
// idempotent (Rule 27). The mux only runs registered tasks; an enqueued
// unknown type fails instead of silently dropping.
package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/mailer"
)

// Task names — the code-side mirror of conventions/asynq-tasks-catalog.md.
const (
	TypeDispatchOutbox    = events.TypeDispatchOutbox
	TypeRetryDead         = "events:retry_dead"
	TypeSendVerification  = "identity:send_verification"
	TypeSendPasswordReset = "identity:send_password_reset"
	TypeRetryPayment      = "billing:retry_payment"
	TypeProcessWebhook    = "billing:process_webhook"
	TypeSubscriptionSweep = "billing:subscription_state_sweep"
	TypeLicenseReminder   = "license:renewal_reminder"
	TypeTenantProvision   = "tenant:provision"
	TypeSchedulerTick     = "scheduler:tick"
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
	Mail      *mailer.Mailer
	Pool      *pgxpool.Pool
	RedisAddr string // asynq Inspector reports queue depth over the same conn
	Billing   *billing.Service
}

// tickSweeps run inside each scheduler:tick — later phases append their
// periodic jobs (dues generation, term transitions, reminders) here.
var tickSweeps []func(context.Context) error

// OnTick registers a periodic sweep driven by scheduler:tick.
func OnTick(fn func(context.Context) error) { tickSweeps = append(tickSweeps, fn) }

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
		TypeSubscriptionSweep: subscriptionSweep(d.Billing),
		TypeProcessWebhook:    processWebhook(d.Billing),
		// Stubs — registered now (catalog-reserved), implemented when their
		// sections land: retry_payment needs §12 payments; license 14;
		// provision 12.
		TypeRetryPayment:    stub(TypeRetryPayment, "Phase 12"),
		TypeLicenseReminder: stub(TypeLicenseReminder, "Phase 14"),
		TypeTenantProvision: stub(TypeTenantProvision, "Phase 12"),
	}
}

func sendMail(m *mailer.Mailer) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p SendEmailPayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil {
			return err
		}
		if err := m.Send(p.To, p.Subject, p.Body); err != nil {
			log.Printf("mail task %s failed: %v", t.Type(), err)
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
			log.Printf("events:retry_dead re-drove %d dead-lettered event(s)", n)
		}
		return nil
	}
}

// subscriptionSweep drives the §07 lifecycle machine's time-based
// transitions (trial→grace→restricted).
func subscriptionSweep(b *billing.Service) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		return b.SweepStates(ctx)
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
				log.Printf("queue %s: pending=%d active=%d retry=%d archived=%d",
					name, qi.Pending, qi.Active, qi.Retry, qi.Archived)
			}
		}
		if dead, err := q.CountDeadEvents(ctx, events.MaxAttempts); err == nil && dead > 0 {
			log.Printf("events DLQ depth: %d", dead)
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
						log.Printf("scheduler tick sweep panic: %v", r)
					}
				}()
				if err := sw(ctx); err != nil {
					log.Printf("scheduler tick sweep failed: %v", err)
				}
			}(sweep)
		}
		wg.Wait()
		return nil
	}
}
