// Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
// Outbox dispatcher: claims pending rows under the oga9:outbox:lock lease,
// fans each out to registered consumers. Consumer contract (§05): the log
// row is checked before processing and inserted in the SAME tx as the
// consumer's own writes. Failed passes stay pending (attempts++) until the
// attempt cap, then the event is dead-lettered for `events:retry_dead`.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
)

const (
	// MaxAttempts is the per-event consumer-retry cap before dead-lettering.
	MaxAttempts = 8
	batchSize   = 50
	lockTTL     = 60 // oga9:outbox:lock lease seconds (cache-keys-catalog)
)

// Consumer processes one event inside the dispatcher's tx — its writes and
// the event_consumer_log insert commit or roll back together.
type Consumer func(ctx context.Context, q *db.Queries, e Event) error

type Dispatcher struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	vk        valkey.Client
	consumers map[string]Consumer
}

func NewDispatcher(pool *pgxpool.Pool, vk valkey.Client) *Dispatcher {
	return &Dispatcher{pool: pool, q: db.New(pool), vk: vk, consumers: map[string]Consumer{}}
}

// Register adds a named consumer — the name is the event_consumer_log key,
// so renames lose idempotency: treat names as permanent.
func (d *Dispatcher) Register(name string, c Consumer) { d.consumers[name] = c }

func (d *Dispatcher) names() []string {
	n := make([]string, 0, len(d.consumers))
	for k := range d.consumers {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// ProcessTask is the asynq handler for events:dispatch_outbox. One
// dispatcher runs at a time via the Valkey lease; a second tick while the
// lease is held is a no-op.
func (d *Dispatcher) ProcessTask(ctx context.Context, t *asynq.Task) error {
	ok, err := d.vk.Do(ctx, d.vk.B().Set().Key(LockKey).Value("1").Nx().ExSeconds(lockTTL).Build()).AsBool()
	if err != nil {
		log.Printf("outbox: lock acquire failed (Valkey): %v", err)
		return nil // next tick recovers
	}
	if !ok {
		return nil // lease held by another dispatcher — spec no-op
	}
	defer d.vk.Do(ctx, d.vk.B().Del().Key(LockKey).Build())

	rows, err := d.q.FetchPendingEvents(ctx, db.FetchPendingEventsParams{
		MaxAttempts: MaxAttempts, BatchSize: batchSize,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := d.dispatch(ctx, r); err != nil {
			log.Printf("outbox: event %s (%s) failed: %v", r.ID.String(), r.EventType, err)
		}
	}
	return nil
}

func toEvent(r db.FetchPendingEventsRow) (Event, error) {
	var e Event
	if err := json.Unmarshal(r.Payload, &e.Payload); err != nil {
		return e, err
	}
	e = Event{
		ID: uuid.UUID(r.ID.Bytes), Type: r.EventType,
		AggregateType: r.AggregateType, AggregateID: uuid.UUID(r.AggregateID.Bytes),
		TenantID:       uuid.UUID(r.TenantID.Bytes),
		OrganizationID: uuid.UUID(r.OrganizationID.Bytes),
		ActorID:        uuid.UUID(r.ActorID.Bytes),
		Payload:        e.Payload,
		OccurredAt:     r.OccurredAt.Time, Attempts: r.Attempts,
	}
	return e, nil
}

// dispatch runs every registered consumer over one event: each logs into
// event_consumer_log inside its own tx. When all consumers have logged,
// dispatched_at is set; otherwise attempts++ and the event is retried on
// the next tick (or dead-lettered at the cap).
func (d *Dispatcher) dispatch(ctx context.Context, r db.FetchPendingEventsRow) error {
	e, err := toEvent(r)
	if err != nil {
		return d.q.DeadLetterEvent(ctx, db.DeadLetterEventParams{ID: r.ID, LastError: pgtype.Text{String: "payload unmarshal: " + err.Error(), Valid: true}})
	}
	failed := ""
	for _, name := range d.names() {
		logged, err := d.q.ConsumerLogged(ctx, db.ConsumerLoggedParams{Consumer: name, EventID: r.ID})
		if err != nil {
			return err
		}
		if logged {
			continue
		}
		if err := d.runConsumer(ctx, name, e); err != nil {
			failed = fmt.Sprintf("%s: %v", name, err)
			log.Printf("outbox: consumer %s on %s failed: %v", name, e.Type, err)
		}
	}
	if failed == "" {
		return d.q.MarkEventDispatched(ctx, r.ID)
	}
	if e.Attempts+1 >= MaxAttempts {
		return d.q.DeadLetterEvent(ctx, db.DeadLetterEventParams{
			ID: r.ID, LastError: pgtype.Text{String: "dead: " + failed, Valid: true},
		})
	}
	return d.q.RecordEventAttempt(ctx, db.RecordEventAttemptParams{
		ID: r.ID, LastError: pgtype.Text{String: failed, Valid: true},
	})
}

func (d *Dispatcher) runConsumer(ctx context.Context, name string, e Event) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := d.q.WithTx(tx)
	if err := d.consumers[name](ctx, q, e); err != nil {
		return err
	}
	if err := q.LogConsumerProcessed(ctx, db.LogConsumerProcessedParams{Consumer: name, EventID: pgtype.UUID{Bytes: e.ID, Valid: true}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
