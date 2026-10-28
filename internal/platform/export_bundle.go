// Ref: oga9-engineering/adr/ADR-026-tenant-portability-migration.md +
// sections/15-subscription-admin.md — the Rule-28 export doubles as the
// migration vehicle: tenant:export produces a signed tenant_export_v1
// bundle the self-hosted oga9-admin import consumes.
package platform

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenantexport"
)

// ExportOptions rides tenant_exports.options — post_state applies on
// confirm (ADR-026). The encryption passphrase is NEVER persisted: it
// travels in the tenant:export task payload only.
type ExportOptions struct {
	IncludeCredentials bool   `json:"include_credentials"`
	PostState          string `json:"post_state"` // readonly_grace|purge|keep_active
}

var (
	ErrExportNotReady     = errors.New("export not finished")
	ErrExportConfirmed    = errors.New("export already confirmed")
	ErrPurgeNotConfirmed  = errors.New("purge requires confirm_purge")
	ErrBadExportOption    = errors.New("invalid export option")
	ErrPassphraseRequired = errors.New("export_passphrase required when include_credentials is set")
)

func (o ExportOptions) postState() string {
	if o.PostState == "" {
		return "readonly_grace"
	}
	return o.PostState
}

func (o ExportOptions) validate() error {
	switch o.postState() {
	case "readonly_grace", "purge", "keep_active":
		return nil
	}
	return ErrBadExportOption
}

// SetExportSigningKey wires the platform's ed25519 seed — the same key
// that signs self-hosted license files (one platform signing identity).
func (s *Service) SetExportSigningKey(seed []byte) { s.signSeed = seed }

func (s *Service) signKey() ed25519.PrivateKey {
	if len(s.signSeed) != ed25519.SeedSize {
		return nil
	}
	return ed25519.NewKeyFromSeed(s.signSeed)
}

// TenantOwnerOrg resolves tenants.owner_org_id — the org whose
// org.admin.export node gates tenant-facing export routes (§15).
func (s *Service) TenantOwnerOrg(ctx context.Context, tenantID uuid.UUID) (uuid.UUID, error) {
	t, err := s.q.GetTenantRow(ctx, toPg(tenantID))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if !t.OwnerOrgID.Valid {
		return uuid.Nil, ErrNotFound
	}
	return fromPg(t.OwnerOrgID), nil
}

// RequestTenantExport audits + creates the durable row; the handler
// enqueues tenant:export {export_id, passphrase} (Rule 8).
func (s *Service) RequestTenantExport(ctx context.Context, tenantID, actor uuid.UUID, opts ExportOptions) (uuid.UUID, error) {
	if err := opts.validate(); err != nil {
		return uuid.Nil, err
	}
	if _, err := s.q.GetTenantRow(ctx, toPg(tenantID)); err != nil {
		return uuid.Nil, ErrNotFound
	}
	optJSON, err := json.Marshal(opts)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		// audit-first (Rule 10) — the row + audit commit together.
		if err := s.audit(ctx, q, tenantID, actor, "tenant.export", "tenant", tenantID,
			map[string]any{"mode": "async", "include_credentials": opts.IncludeCredentials}); err != nil {
			return err
		}
		row, err := q.CreateTenantExport(ctx, db.CreateTenantExportParams{
			TenantID: toPg(tenantID), RequestedBy: toPg(actor), Options: optJSON})
		if err != nil {
			return err
		}
		id = fromPg(row.ID)
		return nil
	})
	return id, err
}

// buildBundle produces the signed tenant_export_v1 archive: per-table
// JSONL + blob files + manifest + signature. Cross-tenant
// organization_relationships edges are excluded and logged as
// dropped_external — their target org isn't in the bundle. When
// include_credentials is set the tar is passphrase-encrypted into the
// tenant_export_v1_enc envelope (passphrase travels via the task
// payload, never the DB).
func (s *Service) buildBundle(ctx context.Context, tenantID uuid.UUID, opts ExportOptions, passphrase string) (*os.File, func(), error) {
	if opts.IncludeCredentials && passphrase == "" {
		return nil, nil, ErrPassphraseRequired
	}
	d, err := s.collectExport(ctx, tenantID, opts.IncludeCredentials)
	if err != nil {
		return nil, nil, err
	}
	orgSet := map[string]bool{}
	for _, id := range d.orgIDs {
		orgSet[id.String()] = true
	}
	// build to a temp file, never a memory buffer — blob archives can be
	// large; the caller streams the result to blob storage.
	tmp, err := os.CreateTemp("", "oga9exp-build-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	fail := func(err error) (*os.File, func(), error) {
		cleanup()
		return nil, nil, err
	}
	w := tenantexport.NewWriter(tmp, tenantID.String(), opts.IncludeCredentials)
	if err := w.AddJSONL("tenants", json.RawMessage("["+string(d.tenant)+"]")); err != nil {
		return fail(err)
	}
	// Tables scoped by BOTH columns (audit_events…) land in both dumps —
	// merge so each table writes one data file. NULL-org notifications
	// belong to tenant users but carry no scope; fold them into
	// notifications the same way.
	tables := d.tnTables
	for tbl, rows := range d.orgTables {
		if existing, ok := tables[tbl]; ok {
			tables[tbl] = mergeRows(existing, rows)
		} else {
			tables[tbl] = rows
		}
	}
	if org, ok := tables["notifications"]; ok {
		tables["notifications"] = mergeRows(org, d.userNotifs)
	} else {
		tables["notifications"] = d.userNotifs
	}
	// extras carry no scope column today (users, notification_preferences)
	// — merge anyway so a future scope column can't produce a duplicate
	// data/<table>.jsonl member.
	for _, extra := range []struct {
		table string
		rows  json.RawMessage
	}{
		{"users", d.users},
		{"notification_preferences", d.prefs},
	} {
		if existing, ok := tables[extra.table]; ok {
			tables[extra.table] = mergeRows(existing, extra.rows)
		} else {
			tables[extra.table] = extra.rows
		}
	}
	for tbl, rows := range tables {
		if tbl == "organization_relationships" {
			var dropped []json.RawMessage
			rows, dropped = splitExternalEdges(rows, orgSet)
			if len(dropped) > 0 {
				dj, _ := json.Marshal(dropped)
				w.AddDroppedExternal(tbl, dj)
			}
		}
		if err := w.AddJSONL(tbl, rows); err != nil {
			return fail(err)
		}
	}
	// inbound edges: rows where one of our orgs is the TARGET but the
	// source org lives outside this tenant — they never entered the dump
	// (the org-scoped pass keys on the source column), so they're recorded
	// as dropped_external too.
	inbound, err := s.q.ExportInboundExternalEdges(ctx, d.orgIDs)
	if err != nil {
		return fail(err)
	}
	var inb []json.RawMessage
	if json.Unmarshal(json.RawMessage(inbound), &inb) == nil && len(inb) > 0 {
		w.AddDroppedExternal("organization_relationships(inbound)", json.RawMessage(inbound))
	}
	// blob files referenced by storage_key/file_key columns — Stat gives
	// the tar header its size so content streams through.
	if s.store != nil {
		for _, key := range collectBlobKeys(d.tnTables, d.orgTables) {
			size, err := s.store.Stat(ctx, key)
			if err != nil {
				continue // blob missing — rows still reference the key
			}
			rc, err := s.store.Open(ctx, key)
			if err != nil {
				continue
			}
			_ = w.AddBlob(key, size, rc)
			rc.Close()
		}
	}
	if err := w.Close(s.signKey()); err != nil {
		return fail(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if !opts.IncludeCredentials {
		return tmp, cleanup, nil
	}
	// credentialed: stream-encrypt into a second temp file, drop the
	// plaintext as soon as the ciphertext exists.
	enc, err := os.CreateTemp("", "oga9exp-enc-*")
	if err != nil {
		return fail(err)
	}
	cleanupEnc := func() { enc.Close(); os.Remove(enc.Name()) }
	if err := tenantexport.Encrypt(enc, tmp, passphrase); err != nil {
		cleanup()
		cleanupEnc()
		return nil, nil, err
	}
	cleanup() // plaintext temp is done
	if _, err := enc.Seek(0, io.SeekStart); err != nil {
		cleanupEnc()
		return nil, nil, err
	}
	return enc, cleanupEnc, nil
}

// splitExternalEdges partitions organization_relationships rows: edges
// whose related org lives outside this tenant can't satisfy the FK on
// import — they drop to the manifest's dropped_external list instead.
func splitExternalEdges(rows json.RawMessage, orgSet map[string]bool) (keep json.RawMessage, dropped []json.RawMessage) {
	var edges []json.RawMessage
	if json.Unmarshal(rows, &edges) != nil {
		return rows, nil
	}
	var kept []json.RawMessage
	for _, e := range edges {
		var edge struct {
			RelatedOrgID string `json:"related_org_id"`
		}
		if json.Unmarshal(e, &edge) != nil || orgSet[edge.RelatedOrgID] {
			kept = append(kept, e)
		} else {
			dropped = append(dropped, e)
		}
	}
	out, _ := json.Marshal(kept)
	if kept == nil {
		out = json.RawMessage("[]")
	}
	return out, dropped
}

// mergeRows concatenates two JSON row arrays.
func mergeRows(a, b json.RawMessage) json.RawMessage {
	var ra, rb []json.RawMessage
	_ = json.Unmarshal(a, &ra)
	_ = json.Unmarshal(b, &rb)
	out, _ := json.Marshal(append(ra, rb...))
	return out
}

// collectBlobKeys scans dumped rows for blob references — any column
// named storage_key or file_key points at the blob store.
func collectBlobKeys(tableSets ...map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var keys []string
	for _, tables := range tableSets {
		for _, raw := range tables {
			var rows []map[string]any
			if json.Unmarshal(raw, &rows) != nil {
				continue
			}
			for _, row := range rows {
				for _, col := range []string{"storage_key", "file_key"} {
					if k, ok := row[col].(string); ok && k != "" && !seen[k] {
						seen[k] = true
						keys = append(keys, k)
					}
				}
			}
		}
	}
	return keys
}

// ConfirmTenantMigration applies the tenant's chosen post-export state
// after a verified import on the target install (ADR-026):
// readonly_grace → restricted floor (Rule 28); purge → soft-delete orgs
// for the platform:data_erasure sweep (Rule 11); keep_active → fork.
func (s *Service) ConfirmTenantMigration(ctx context.Context, tenantID, exportID, actor uuid.UUID, confirmPurge bool) (string, error) {
	e, err := s.q.GetTenantExport(ctx, db.GetTenantExportParams{ID: toPg(exportID), TenantID: toPg(tenantID)})
	if err != nil {
		return "", ErrNotFound
	}
	if e.Status != "done" {
		return "", ErrExportNotReady
	}
	if e.ConfirmedAt.Valid {
		return "", ErrExportConfirmed
	}
	var opts ExportOptions
	_ = json.Unmarshal(e.Options, &opts)
	state := opts.postState()
	if state == "purge" && !confirmPurge {
		return "", ErrPurgeNotConfirmed
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.ConfirmTenantExport(ctx, db.ConfirmTenantExportParams{
			ID: toPg(exportID), TenantID: toPg(tenantID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrExportConfirmed
		}
		if err := s.audit(ctx, q, tenantID, actor, "tenant.migrate_confirm", "tenant", tenantID,
			map[string]any{"export_id": exportID, "post_state": state}); err != nil {
			return err
		}
		switch state {
		case "readonly_grace":
			if _, err := q.SetTenantStatus(ctx, db.SetTenantStatusParams{
				ID: toPg(tenantID), Status: "restricted"}); err != nil {
				return err
			}
		case "purge":
			if err := q.SoftDeleteTenantOrgs(ctx, toPg(tenantID)); err != nil {
				return err
			}
			if _, err := q.SetTenantStatus(ctx, db.SetTenantStatusParams{
				ID: toPg(tenantID), Status: "cancelled"}); err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "TenantMigrationConfirmed", "tenant",
			tenantID, actor, true, map[string]any{
				"export_id": exportID, "post_state": state,
			})
	})
	if err != nil {
		return "", err
	}
	return state, nil
}
