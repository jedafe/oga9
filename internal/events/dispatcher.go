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
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

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

// errAlreadyLogged marks a consumer whose log insert found the row already
// present — a second dispatcher overlapping on lease lapse hit the atomic
// dedupe and must skip the body, not replay it.
var errAlreadyLogged = errors.New("consumer_already_logged")

// ProcessTask is the asynq handler for events:dispatch_outbox. One
// dispatcher runs at a time via the Valkey lease; a second tick while the
// lease is held is a no-op.
func (d *Dispatcher) ProcessTask(ctx context.Context, t *asynq.Task) error {
	token := uuid.NewString()
	ok, err := d.vk.Do(ctx, d.vk.B().Set().Key(LockKey).Value(token).Nx().ExSeconds(lockTTL).Build()).AsBool()
	if err != nil {
		log.Printf("outbox: lock acquire failed (Valkey): %v", err)
		return nil // next tick recovers
	}
	if !ok {
		return nil // lease held by another dispatcher — spec no-op
	}
	// Fenced release: if our 60s lease expired mid-run and another dispatcher
	// acquired it, the lock is theirs — only delete when we still hold it.
	defer d.vk.Do(ctx, d.vk.B().Eval().
		Script(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`).
		Numkeys(1).Key(LockKey).Arg(token).Build())

	// The claim tx holds FOR UPDATE SKIP LOCKED on the batch for the whole
	// pass: a second dispatcher whose lease lapped ours skips these rows
	// entirely. Event-row bookkeeping (dispatched/attempt/dead marks) runs
	// inside this same tx — the locks are already ours, so no self-block.
	// Consumer work stays in per-consumer txs: their writes + log insert
	// commit independently of this batch bookkeeping.
	claimTx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer claimTx.Rollback(ctx)
	q := d.q.WithTx(claimTx)
	rows, err := q.FetchPendingEvents(ctx, db.FetchPendingEventsParams{
		MaxAttempts: MaxAttempts, BatchSize: batchSize,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := d.dispatch(ctx, q, r); err != nil {
			log.Printf("outbox: event %s (%s) failed: %v", r.ID.String(), r.EventType, err)
		}
	}
	return claimTx.Commit(ctx)
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
// the next tick (or dead-lettered at the cap). Event-row writes go
// through the claim tx q — the row lock is already held there.
func (d *Dispatcher) dispatch(ctx context.Context, q *db.Queries, r db.FetchPendingEventsRow) error {
	e, err := toEvent(r)
	if err != nil {
		return q.DeadLetterEvent(ctx, db.DeadLetterEventParams{ID: r.ID, LastError: pgtype.Text{String: "payload unmarshal: " + err.Error(), Valid: true}})
	}
	var failures []string
	for _, name := range d.names() {
		logged, err := q.ConsumerLogged(ctx, db.ConsumerLoggedParams{Consumer: name, EventID: r.ID})
		if err != nil {
			return err
		}
		if logged {
			continue
		}
		if err := d.runConsumer(ctx, name, e); err != nil {
			if errors.Is(err, errAlreadyLogged) {
				continue // overlapping dispatcher logged it first — done, not failed
			}
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			log.Printf("outbox: consumer %s on %s failed: %v", name, e.Type, err)
		}
	}
	if len(failures) == 0 {
		return q.MarkEventDispatched(ctx, r.ID)
	}
	failed := strings.Join(failures, "; ")
	if e.Attempts+1 >= MaxAttempts {
		return q.DeadLetterEvent(ctx, db.DeadLetterEventParams{
			ID: r.ID, LastError: pgtype.Text{String: "dead: " + failed, Valid: true},
		})
	}
	return q.RecordEventAttempt(ctx, db.RecordEventAttemptParams{
		ID: r.ID, LastError: pgtype.Text{String: failed, Valid: true},
	})
}

// runConsumer claims the consumer_log row BEFORE running the body — the
// insert is the atomic dedupe (0 rows = someone else logged it). A body
// failure rolls the log row back with the consumer's writes, so the pair
// always retries together (§05 contract).
func (d *Dispatcher) runConsumer(ctx context.Context, name string, e Event) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := d.q.WithTx(tx)
	n, err := q.LogConsumerProcessed(ctx, db.LogConsumerProcessedParams{Consumer: name, EventID: pgtype.UUID{Bytes: e.ID, Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return errAlreadyLogged
	}
	if err := d.consumers[name](ctx, q, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
