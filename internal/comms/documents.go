// Ref: oga9-engineering/sections/14-comms-documents.md — documents:
// upload → storage_key + row → documents:process_upload validates;
// versions bump documents.version + append document_versions; visibility
// changes emit DocumentAccessChanged; deletes are soft (Rule 11).
package comms

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tenant"
)

var docKinds = map[string]bool{"minutes": true, "constitution": true, "bylaw": true,
	"policy": true, "report": true, "resolution": true, "photo": true,
	"archive": true, "certificate": true}
var docVisibility = map[string]bool{"public": true, "members": true, "exco": true,
	"committee": true, "private": true}

// SetStore wires the blob backend (disk under OGA9_UPLOADS_DIR for MVP).
func (s *Service) SetStore(st storage.BlobStore) { s.store = st }

// PreviewURL returns an image preview URL through the store backend
// (imgproxy+S3); empty when the backend can't preview or the document
// isn't an image — callers omit the field then.
func (s *Service) PreviewURL(d db.Documents) string {
	if s.store == nil {
		return ""
	}
	ext := strings.ToLower(d.StorageKey)
	for _, e := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif"} {
		if strings.HasSuffix(ext, e) {
			return s.store.PreviewURL(d.StorageKey, 640, 640)
		}
	}
	return ""
}

// putCapped writes the blob enforcing limit.storage.mb (Rule 25) — the
// env fallback (OGA9_UPLOAD_MAX_MB) covers dev/new plans; neither is
// hardcoded. Streams at most cap+1 so an over-cap file can't grow the disk.
func (s *Service) putCapped(ctx context.Context, tenantID uuid.UUID, key string, r io.Reader) error {
	mb := int64(0)
	if s.limitFn != nil {
		mb = s.limitFn(ctx, tenantID, "limit.storage.mb")
	}
	if mb <= 0 {
		mb = s.fallbackMB
	}
	if mb <= 0 {
		return s.store.Put(ctx, key, r)
	}
	max := mb << 20
	if err := s.store.Put(ctx, key, io.LimitReader(r, max+1)); err != nil {
		return err
	}
	if sz, _ := s.store.Stat(ctx, key); sz > max {
		_ = s.store.Remove(ctx, key)
		return ErrTooLarge
	}
	return nil
}

// unitInOrg validates a caller-supplied unit scope — the FK only proves
// the unit exists; a foreign unit_id would silently corrupt committee
// visibility (same org-check as payments/claims/drives).
func (s *Service) unitInOrg(ctx context.Context, orgID, unitID uuid.UUID) bool {
	_, err := s.q.GetOrgUnit(ctx, db.GetOrgUnitParams{ID: toPg(unitID), OrganizationID: toPg(orgID)})
	return err == nil
}

// Upload writes the blob, inserts the row, emits DocumentUploaded — all
// in-tx except the blob write (a failed tx orphans the key; harmless).
func (s *Service) Upload(ctx context.Context, oc *tenant.OrgCtx, kind, title, visibility string, unitID uuid.UUID, r io.Reader) (uuid.UUID, error) {
	if !docKinds[kind] || title == "" {
		return uuid.Nil, ErrBadValue
	}
	if !docVisibility[visibility] {
		visibility = "members"
	}
	if unitID != uuid.Nil && !s.unitInOrg(ctx, oc.OrgID, unitID) {
		return uuid.Nil, ErrNotFound
	}
	key := uuid.NewString()
	if err := s.putCapped(ctx, oc.TenantID, key, r); err != nil {
		return uuid.Nil, err
	}
	return s.insertDoc(ctx, oc, kind, title, visibility, unitID, key)
}

func (s *Service) insertDoc(ctx context.Context, oc *tenant.OrgCtx, kind, title, visibility string, unitID uuid.UUID, key string) (uuid.UUID, error) {
	var row db.Documents
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateDocument(ctx, db.CreateDocumentParams{
			OrganizationID: toPg(oc.OrgID), Kind: kind, Title: title,
			StorageKey: key, Visibility: visibility, UploadedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		// v1 is a real version too — without its row the original blob is
		// orphaned the moment v2 bumps documents.storage_key (§14: prior
		// versions stay retrievable).
		if err := q.InsertVersionRow(ctx, db.InsertVersionRowParams{
			DocumentID: row.ID, Version: 1, StorageKey: key,
			CreatedBy: toPg(oc.MembershipID)}); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DocumentUploaded",
			"document", fromPg(row.ID), oc.UserID, false,
			map[string]any{"document_id": fromPg(row.ID), "kind": kind, "title": title}); err != nil {
			return err
		}
		if kind == "policy" {
			// §14 catalog: publishing a policy document is its own event.
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PolicyPublished",
				"document", fromPg(row.ID), oc.UserID, true,
				map[string]any{"document_id": fromPg(row.ID)})
		}
		return nil
	})
	return fromPg(row.ID), err
}

// docScope resolves the caller's read scopes ONCE per request — the exco
// and committee checks were previously per-document queries (N+1).
type docScope struct {
	manage     bool
	officer    bool
	committees map[uuid.UUID]bool
}

func (s *Service) scope(ctx context.Context, oc *tenant.OrgCtx, manage bool) docScope {
	sc := docScope{manage: manage}
	if manage {
		return sc
	}
	if ok, err := s.q.IsActiveOfficer(ctx, toPg(oc.MembershipID)); err == nil {
		sc.officer = ok
	}
	if units, err := s.q.UserCommitteeUnits(ctx, db.UserCommitteeUnitsParams{
		OrgID: toPg(oc.OrgID), UserID: toPg(oc.UserID)}); err == nil {
		sc.committees = make(map[uuid.UUID]bool, len(units))
		for _, u := range units {
			sc.committees[fromPg(u)] = true
		}
	}
	return sc
}

// CanReadDoc enforces the §14 visibility contract on reads: public|members
// → any holder of org.documents.view; exco → an active officer; committee →
// a member of the doc's unit committee; private → the uploader. Managers
// (org.documents.publish) read everything.
func (s *Service) CanReadDoc(ctx context.Context, oc *tenant.OrgCtx, d db.Documents, manage bool) bool {
	return s.canRead(oc, d, s.scope(ctx, oc, manage))
}

func (s *Service) canRead(oc *tenant.OrgCtx, d db.Documents, sc docScope) bool {
	if sc.manage {
		return true
	}
	switch d.Visibility {
	case "public", "members":
		return true
	case "private":
		return fromPg(d.UploadedBy) == oc.MembershipID
	case "exco":
		return sc.officer
	case "committee":
		return d.UnitID.Valid && sc.committees[fromPg(d.UnitID)]
	}
	return false
}

func (s *Service) Documents(ctx context.Context, oc *tenant.OrgCtx, kind string, canManage bool) ([]db.Documents, error) {
	rows, err := s.q.ListDocuments(ctx, db.ListDocumentsParams{OrganizationID: toPg(oc.OrgID), Column2: kind})
	if err != nil {
		return nil, err
	}
	sc := s.scope(ctx, oc, canManage)
	out := make([]db.Documents, 0, len(rows))
	for _, d := range rows {
		if s.canRead(oc, d, sc) {
			out = append(out, d)
		}
	}
	return out, nil
}

// Download returns the blob for an org-scoped read — hidden docs read as
// 404 so existence never leaks past the visibility scope.
func (s *Service) Download(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, canManage bool) (io.ReadCloser, *db.Documents, error) {
	d, err := s.q.GetDocument(ctx, db.GetDocumentParams{ID: toPg(did), OrganizationID: toPg(oc.OrgID)})
	if err != nil || !s.CanReadDoc(ctx, oc, d, canManage) {
		return nil, nil, ErrNotFound
	}
	rc, err := s.store.Open(ctx, d.StorageKey)
	return rc, &d, err
}

// PatchDocument edits title/visibility/unit scope; a visibility change
// emits DocumentAccessChanged (§14 catalog).
func (s *Service) PatchDocument(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, title, visibility *string, clearUnit bool, unitID uuid.UUID) error {
	if visibility != nil && !docVisibility[*visibility] {
		return ErrBadValue
	}
	if unitID != uuid.Nil && !s.unitInOrg(ctx, oc.OrgID, unitID) {
		return ErrNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.PatchDocument(ctx, db.PatchDocumentParams{
			ID: toPg(did), OrganizationID: toPg(oc.OrgID), Title: textPtr(title), Visibility: textPtr(visibility), ClearUnit: clearUnit, UnitID: toPg(unitID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if visibility != nil {
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DocumentAccessChanged",
				"document", did, oc.UserID, false,
				map[string]any{"document_id": did, "visibility": *visibility})
		}
		return nil
	})
}

// Archive soft-deletes (Rule 11 — blob stays for the erasure task) and
// emits DocumentRetired in the same tx (Rule 12; §10 catalog).
func (s *Service) Archive(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SoftDeleteDocument(ctx, db.SoftDeleteDocumentParams{ID: toPg(did), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DocumentRetired",
			"document", did, oc.UserID, false,
			map[string]any{"document_id": did})
	})
}

// NewVersion writes a new blob, bumps documents.version, and records the
// version row — all-in-tx after the blob lands. Same size cap as Upload;
// the handler enqueues documents:process_upload for the new key.
func (s *Service) NewVersion(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, r io.Reader) (int, error) {
	key := uuid.NewString()
	if err := s.putCapped(ctx, oc.TenantID, key, r); err != nil {
		return 0, err
	}
	var ver int
	err := s.withTx(ctx, func(q *db.Queries) error {
		v, err := q.AddDocumentVersion(ctx, db.AddDocumentVersionParams{
			ID: toPg(did), OrganizationID: toPg(oc.OrgID), StorageKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		ver = int(v)
		if err := q.InsertVersionRow(ctx, db.InsertVersionRowParams{
			DocumentID: toPg(did), Version: v, StorageKey: key, CreatedBy: toPg(oc.MembershipID)}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DocumentVersioned",
			"document", did, oc.UserID, false,
			map[string]any{"document_id": did, "version": ver})
	})
	return ver, err
}

func (s *Service) Versions(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, canManage bool) ([]db.DocumentVersions, error) {
	d, err := s.q.GetDocument(ctx, db.GetDocumentParams{ID: toPg(did), OrganizationID: toPg(oc.OrgID)})
	if err != nil || !s.CanReadDoc(ctx, oc, d, canManage) {
		return nil, ErrNotFound
	}
	return s.q.DocumentVersions(ctx, toPg(did))
}

func (s *Service) PublicDocs(ctx context.Context, slug string) ([]db.PublicDocumentsRow, error) {
	return s.q.PublicDocuments(ctx, slug)
}

// ProcessUpload is the documents:process_upload body: the blob must exist
// and be non-empty; a bad upload is REMOVED, never served — at v1 that's
// the whole document, at v2+ only the bad version (the head reverts to
// the previous version row, so a poisoned v2 can't kill a good v1).
func (s *Service) ProcessUpload(ctx context.Context, docID uuid.UUID) error {
	d, err := s.q.GetDocumentAny(ctx, toPg(docID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // row retracted before the worker ran
	}
	if err != nil {
		return err
	}
	sz, err := s.store.Stat(ctx, d.StorageKey)
	if err != nil || sz == 0 {
		_, _ = s.q.SoftDeleteDocument(ctx, db.SoftDeleteDocumentParams{
			ID: d.ID, OrganizationID: d.OrganizationID})
		return nil // poisoned upload removed — don't retry
	}
	// MIME sniff the first block — executables never belong in an org's
	// document store regardless of the claimed kind.
	rc, err := s.store.Open(ctx, d.StorageKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	head := make([]byte, 512)
	n, _ := rc.Read(head)
	if !executable(head[:n], http.DetectContentType(head[:n])) {
		return nil
	}
	badKey := d.StorageKey
	if d.Version > 1 {
		// drop the offending version row, revert the head to the
		// surviving latest version — the prior version stays retrievable.
		if _, err := s.q.DropLatestVersion(ctx, db.DropLatestVersionParams{
			DocumentID: d.ID, Version: d.Version}); err != nil {
			return err
		}
		prev, err := s.q.LatestVersionRow(ctx, d.ID)
		if err != nil {
			return err
		}
		if err := s.q.RevertDocumentHead(ctx, db.RevertDocumentHeadParams{
			ID: d.ID, OrganizationID: d.OrganizationID, StorageKey: prev.StorageKey}); err != nil {
			return err
		}
	} else {
		_, _ = s.q.SoftDeleteDocument(ctx, db.SoftDeleteDocumentParams{
			ID: d.ID, OrganizationID: d.OrganizationID})
	}
	_ = s.store.Remove(ctx, badKey)
	return nil
}

// executable — magic bytes + script shebangs: DetectContentType labels
// scripts "text/plain", so the signature check has to look at the head.
func executable(head []byte, mime string) bool {
	switch mime {
	case "application/x-msdownload", "application/x-executable",
		"application/x-mach-binary", "application/x-elf":
		return true
	}
	if len(head) >= 4 &&
		(head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F' || // ELF
			head[0] == 'M' && head[1] == 'Z' || // PE/DOS
			head[0] == '#' && head[1] == '!' || // #! script
			head[0] == 0xfe && head[1] == 0xed || head[0] == 0xce && head[1] == 0xfa) { // Mach-O
		return true
	}
	return false
}

// DocInOrg is the Rule-26 seam chat uses to validate media attachments —
// the documents module answers, chat never reads the table.
func (s *Service) DocInOrg(ctx context.Context, orgID, docID uuid.UUID) bool {
	_, err := s.q.GetDocument(ctx, db.GetDocumentParams{ID: toPg(docID), OrganizationID: toPg(orgID)})
	return err == nil
}
