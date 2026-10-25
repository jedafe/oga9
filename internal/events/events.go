// Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
// Transactional outbox: Emit appends inside the caller's mutation tx
// (Rule 12 — an append failure aborts the mutation); the dispatcher
// delivers to registered consumers at-least-once with event_consumer_log
// dedupe (Rule 27).
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
)

// TypeDispatchOutbox is the Asynq task that drives outbox delivery
// (critical tier — asynq-tasks-catalog).
const TypeDispatchOutbox = "events:dispatch_outbox"

// LockKey is the Valkey dispatch lease (oga9:outbox:lock, 60s — catalogued).
const LockKey = "oga9:outbox:lock"

// Event is a domain_event row handed to consumers.
type Event struct {
	ID             uuid.UUID
	Type           string
	AggregateType  string
	AggregateID    uuid.UUID
	TenantID       uuid.UUID // Nil = platform-scoped
	OrganizationID uuid.UUID
	ActorID        uuid.UUID
	Payload        map[string]any
	OccurredAt     time.Time
	Attempts       int32
}

// UUID reads a uuid from the event payload (missing/bad → Nil).
func (e *Event) UUID(key string) uuid.UUID {
	s, ok := e.Payload[key].(string)
	if !ok {
		return uuid.Nil
	}
	u, _ := uuid.Parse(s)
	return u
}

func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }

// Emit appends an outbox event inside q's transaction (Rule 12). The event
// lands atomically with the state mutation and an append failure aborts the
// mutation — callers must propagate the error, never swallow. tenantID/orgID
// uuid.Nil → NULL scope columns (platform events); `sourced` marks ADR-004
// replayable events per the domain-events catalog.
func Emit(ctx context.Context, q *db.Queries, tenantID, orgID uuid.UUID, eventType, aggType string, aggID, actorID uuid.UUID, sourced bool, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["actor_id"] = actorID
	payload["aggregate_id"] = aggID
	payload["aggregate_type"] = aggType
	payload["occurred_at"] = time.Now().UTC()
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return q.EmitDomainEvent(ctx, db.EmitDomainEventParams{
		EventType: eventType, AggregateType: aggType, AggregateID: pgtype.UUID{Bytes: aggID, Valid: true},
		TenantID: pgUUID(tenantID), OrganizationID: pgUUID(orgID), ActorID: pgUUID(actorID),
		Payload: b, Sourced: sourced,
	})
}
