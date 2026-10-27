// Ref: oga9-engineering/sections/23-integrations.md — webhook signature,
// api-key revoke, japa dedupe.
package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

func testSvc(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewService(pool), pool
}

func mkOrg(t *testing.T, pool *pgxpool.Pool) *tenant.OrgCtx {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name, slug, billing_email, status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"ten-"+suffix, "ten-"+suffix, "t-"+suffix+"@x.test").Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (tenant_id, name, slug, org_type, is_public_listed) VALUES ($1,$2,$3,'association',true) RETURNING id`,
		tid, "org-"+suffix, "org-"+suffix).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	var uid pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','u','active') RETURNING id`,
		"u-"+suffix+"@x.test").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes)}
}

// TestWebhookSignature — spec test 2: a valid HMAC accepts + a wrong
// signature is rejected but the receipt is still recorded.
func TestWebhookSignature(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	// connector wired with a secret ref (dev seam returns the secret).
	svc.SetSecret(func(context.Context, string) (string, error) { return "whsec", nil })
	if _, err := svc.Configure(ctx, oc, "paystack", json.RawMessage(`{}`), "WH_SECRET"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"event":"charge.success"}`)
	mac := hmac.New(sha512.New, []byte("whsec"))
	mac.Write(raw)
	good := hex.EncodeToString(mac.Sum(nil))

	rid, ok, err := svc.Intake(ctx, "paystack", raw, good)
	if err != nil || !ok || rid == uuid.Nil {
		t.Fatalf("valid webhook: %v %v", rid, err)
	}
	rid2, ok, err := svc.Intake(ctx, "paystack", raw, "bogus")
	if !errors.Is(err, ErrBadSignature) || ok {
		t.Fatalf("bad webhook should reject: %v %v", ok, err)
	}
	var receipt bool
	svc.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_receipts WHERE id=$1 AND NOT signature_ok)`, rid2).Scan(&receipt)
	if !receipt {
		t.Fatal("rejected webhook receipt not persisted")
	}
}

// TestAPIKeyRevoke — spec test 4: revoked keys fail immediately.
func TestAPIKeyRevoke(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	kid, raw, err := svc.IssueKey(ctx, oc.UserID, []string{"read"})
	if err != nil || raw == "" {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, raw); err != nil {
		t.Fatalf("fresh key should authenticate: %v", err)
	}
	if err := svc.Revoke(ctx, oc.UserID, kid); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, raw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked key should fail: %v", err)
	}
	// unknown keys also fail closed
	if _, err := svc.Authenticate(ctx, "oga9_unknown"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("unknown key: %v", err)
	}
}

// TestJapaDedupe — spec test 1: publish→push creates the mapping;
// a re-push of the same org is a dedupe hit (no new job).
func TestJapaDedupe(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	// connector exists so the consumer has somewhere to push
	if _, err := svc.Configure(ctx, oc, "japaconnect", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	e := events.Event{Type: "OrganizationPublished", OrganizationID: oc.OrgID,
		Payload: map[string]any{"org_id": oc.OrgID.String()}}
	if err := svc.JapaPushConsumer(ctx, svc.q, e); err != nil {
		t.Fatal(err)
	}
	var jobs int
	svc.pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs sj JOIN connectors c ON c.id=sj.connector_id WHERE c.organization_id=$1`, oc.OrgID).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("expected 1 push job, got %d", jobs)
	}
	// re-push same org → dedupe (test 1)
	if err := svc.JapaPushConsumer(ctx, svc.q, e); err != nil {
		t.Fatal(err)
	}
	svc.pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs sj JOIN connectors c ON c.id=sj.connector_id WHERE c.organization_id=$1`, oc.OrgID).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("dedupe failed: %d jobs", jobs)
	}
}
