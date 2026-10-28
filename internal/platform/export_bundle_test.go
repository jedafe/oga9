// Ref: oga9-engineering/adr/ADR-026-tenant-portability-migration.md —
// export → signed bundle → confirm → post-migration state (§15 scenario 6).
package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tenantexport"
)

func TestTenantExportBundleAndConfirm(t *testing.T) {
	svc, _, pool := testSvc(t)
	svc.SetStore(storage.New(t.TempDir()))
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	svc.SetExportSigningKey(seed)
	ctx := context.Background()

	tenantID := mkTenant(t, pool, "exp-"+uuid.NewString()[:8])
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	orgID := mkOrgMember(t, pool, tenantID, uid, "active")
	if _, err := pool.Exec(ctx,
		"UPDATE tenants SET owner_org_id=$1 WHERE id=$2", orgID, tenantID); err != nil {
		t.Fatal(err)
	}

	eid, err := svc.RequestTenantExport(ctx, tenantID, uid, ExportOptions{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := svc.RunTenantExport(ctx, eid, ""); err != nil {
		t.Fatalf("worker: %v", err)
	}

	blob, status, err := svc.DownloadTenantExport(ctx, tenantID, eid)
	if err != nil || status != "done" {
		t.Fatalf("download: %v %s", err, status)
	}
	data, _ := io.ReadAll(blob)
	blob.Close()

	a, err := tenantexport.Open(bytes.NewReader(data), "")
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer a.Close()
	m := a.Manifest()
	if m.Format != tenantexport.FormatV1 || m.TenantID != tenantID.String() {
		t.Fatalf("bad manifest: %+v", m)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if err := a.Verify(pub); err != nil {
		t.Fatalf("verify: %v", err)
	}
	tables := map[string]bool{}
	_ = a.Walk(func(path string, r io.Reader, _ int64) error {
		tables[path] = true
		io.Copy(io.Discard, r)
		return nil
	})
	for _, want := range []string{"data/tenants.jsonl", "data/organizations.jsonl", "data/users.jsonl", "data/organization_memberships.jsonl"} {
		if !tables[want] {
			t.Fatalf("bundle missing %s (have %v)", want, tables)
		}
	}
	// users projection is credential-free by default (ADR-020).
	var users []map[string]any
	err = a.Walk(func(path string, r io.Reader, _ int64) error {
		if path == "data/users.jsonl" {
			b, _ := io.ReadAll(r)
			for _, line := range bytes.Split(b, []byte("\n")) {
				if len(line) > 0 {
					var row map[string]any
					_ = json.Unmarshal(line, &row)
					users = append(users, row)
				}
			}
		}
		io.Copy(io.Discard, r)
		return nil
	})
	for _, u := range users {
		if _, has := u["password_hash"]; has {
			t.Fatal("default export leaked password_hash")
		}
	}

	// confirm → readonly_grace: tenant goes restricted, export confirmed.
	state, err := svc.ConfirmTenantMigration(ctx, tenantID, eid, uid, false)
	if err != nil || state != "readonly_grace" {
		t.Fatalf("confirm: %v %s", err, state)
	}
	var status2 string
	_ = pool.QueryRow(ctx, "SELECT status FROM tenants WHERE id=$1", tenantID).Scan(&status2)
	if status2 != "restricted" {
		t.Fatalf("tenant status = %s, want restricted", status2)
	}
	if _, err := svc.ConfirmTenantMigration(ctx, tenantID, eid, uid, false); !errors.Is(err, ErrExportConfirmed) {
		t.Fatalf("re-confirm: %v, want ErrExportConfirmed", err)
	}
}

func TestTenantExportPurgeRequiresDoubleConfirm(t *testing.T) {
	svc, _, pool := testSvc(t)
	svc.SetStore(storage.New(t.TempDir()))
	ctx := context.Background()

	tenantID := mkTenant(t, pool, "exp-"+uuid.NewString()[:8])
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	mkOrgMember(t, pool, tenantID, uid, "active")

	eid, err := svc.RequestTenantExport(ctx, tenantID, uid, ExportOptions{PostState: "purge"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunTenantExport(ctx, eid, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmTenantMigration(ctx, tenantID, eid, uid, false); !errors.Is(err, ErrPurgeNotConfirmed) {
		t.Fatalf("purge without flag: %v, want ErrPurgeNotConfirmed", err)
	}
	state, err := svc.ConfirmTenantMigration(ctx, tenantID, eid, uid, true)
	if err != nil || state != "purge" {
		t.Fatalf("confirm purge: %v %s", err, state)
	}
	var status2 string
	_ = pool.QueryRow(ctx, "SELECT status FROM tenants WHERE id=$1", tenantID).Scan(&status2)
	if status2 != "cancelled" {
		t.Fatalf("tenant status = %s, want cancelled", status2)
	}
	var remaining int
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM organizations WHERE tenant_id=$1 AND deleted_at IS NULL", tenantID).Scan(&remaining)
	if remaining != 0 {
		t.Fatal("orgs not soft-deleted after purge confirm")
	}
}

func TestTenantExportCredentialsNeedPassphrase(t *testing.T) {
	svc, _, pool := testSvc(t)
	svc.SetStore(storage.New(t.TempDir()))
	ctx := context.Background()

	tenantID := mkTenant(t, pool, "exp-"+uuid.NewString()[:8])
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	mkOrgMember(t, pool, tenantID, uid, "active")

	eid, err := svc.RequestTenantExport(ctx, tenantID, uid, ExportOptions{IncludeCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	// no passphrase on the payload → permanent failure, row marked failed.
	if err := svc.RunTenantExport(ctx, eid, ""); err != nil {
		t.Fatalf("worker should mark failed, not retry: %v", err)
	}
	var status string
	_ = pool.QueryRow(ctx, "SELECT status FROM tenant_exports WHERE id=$1", eid).Scan(&status)
	if status != "failed" {
		t.Fatalf("export status = %s, want failed", status)
	}
}
