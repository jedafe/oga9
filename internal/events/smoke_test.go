package events

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
)

func TestDispatcherEndToEnd(t *testing.T) {
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{os.Getenv("OGA9_TEST_VALKEY_ADDR")}})
	if err != nil {
		t.Fatal(err)
	}
	q := db.New(pool)

	// emit an OrganizationCreated-shaped event inside a tx — ids must be
	// real rows (tenant_id/organization_id are FK columns).
	var orgID, tenantID uuid.UUID
	if err := pool.QueryRow(ctx,
		"SELECT id, tenant_id FROM organizations ORDER BY created_at DESC LIMIT 1").Scan(&orgID, &tenantID); err != nil {
		t.Skip("no organizations seeded yet")
	}
	actor := uuid.Nil
	tx, _ := pool.Begin(ctx)
	err = Emit(ctx, q.WithTx(tx), tenantID, orgID, "OrganizationCreated", "organization", orgID, actor, true,
		map[string]any{"organization_id": orgID, "tenant_id": tenantID})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(pool, vk)
	d.Register("entity_links", EntityLinksConsumer)
	// a prior crashed dispatcher may still hold the 60s lease — clear it so
	// this run isn't silently no-opped.
	_ = vk.Do(ctx, vk.B().Del().Key(LockKey).Build()).Error()
	// the shared test DB accumulates pending events older than ours —
	// batches are 50 rows, so tick until ours is delivered (deadline-bounded
	// instead of tick-count-bounded: parallel packages slow each tick).
	deadline := time.Now().Add(30 * time.Second)
	var done bool
	for !done && time.Now().Before(deadline) {
		if err := d.ProcessTask(ctx, asynq.NewTask(TypeDispatchOutbox, nil)); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx,
			"SELECT COALESCE((SELECT dispatched_at IS NOT NULL FROM domain_events WHERE aggregate_id=$1 ORDER BY occurred_at DESC LIMIT 1), false)", orgID).Scan(&done); err != nil {
			t.Fatal(err)
		}
		if !done {
			time.Sleep(100 * time.Millisecond)
		}
	}

	var evtID uuid.UUID
	if err := pool.QueryRow(ctx,
		"SELECT id FROM domain_events WHERE aggregate_id=$1 ORDER BY occurred_at DESC LIMIT 1", orgID).Scan(&evtID); err != nil {
		t.Fatal(err)
	}
	var links int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM entity_links WHERE source_id=$1 AND source_type='organization' AND relationship_type='belongs_to'", orgID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("expected 1 edge, got %d", links)
	}
	// idempotent (Rule 27): more ticks must not re-process or duplicate the edge
	if err := d.ProcessTask(ctx, asynq.NewTask(TypeDispatchOutbox, nil)); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_consumer_log WHERE event_id=$1", evtID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("consumer log rows=%d err=%v", links, err)
	}
}
