// Ref: oga9-engineering/sections/10-organization-core.md (ADR-022)
// Custom fields framework — groups/fields CRUD, merged schema for form
// rendering, entity-scoped values. Definitions are org schema; the schema
// cache (oga9:fields:schema:{org}:{entity}) invalidates on every mutation.
package tenant

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

var fieldKinds = map[string]bool{
	"text": true, "textarea": true, "number": true, "money": true, "date": true,
	"datetime": true, "boolean": true, "select": true, "multiselect": true,
	"file": true, "member_ref": true, "url": true, "phone": true, "email": true,
}
var fieldVisibilities = map[string]bool{"member": true, "exco": true, "admin": true, "public": true}
var fieldEntities = map[string]bool{
	"member": true, "application": true, "organization": true, "event": true,
	"welfare_case": true, "asset": true, "project": true,
}

func schemaKey(orgID uuid.UUID, entityType string) string {
	return "oga9:fields:schema:" + orgID.String() + ":" + entityType
}

func (s *Service) invalidateFieldSchema(ctx context.Context, orgID uuid.UUID, entityType string) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key(schemaKey(orgID, entityType)).Build()).Error()
}

func (s *Service) ListFieldGroups(ctx context.Context, oc *OrgCtx, entityType string) ([]db.CustomFieldGroups, error) {
	return s.q.ListFieldGroups(ctx, db.ListFieldGroupsParams{OrganizationID: toPg(oc.OrgID), Column2: entityType})
}

func (s *Service) CreateFieldGroup(ctx context.Context, oc *OrgCtx, entityType, name, slug string, sortOrder int32, visibility string) (uuid.UUID, error) {
	if !fieldEntities[entityType] || !fieldVisibilities[visibility] || !validSlug(slug) || name == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		gid, err := q.CreateCustomFieldGroup(ctx, db.CreateCustomFieldGroupParams{
			OrganizationID: toPg(oc.OrgID), EntityType: entityType, Name: name,
			Slug: slug, SortOrder: sortOrder, Visibility: visibility, Source: "custom",
		})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrSlugTaken
			}
			return err
		}
		id = uuid.UUID(gid.Bytes)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CustomFieldGroupCreated",
			"custom_field_group", id, oc.UserID, true,
			map[string]any{"group_id": id, "entity_type": entityType, "source": "custom"})
	})
	if err == nil {
		s.invalidateFieldSchema(ctx, oc.OrgID, entityType)
	}
	return id, err
}

func (s *Service) UpdateFieldGroup(ctx context.Context, oc *OrgCtx, gid uuid.UUID, name string, sortOrder int32, visibility string) error {
	// empty visibility = keep existing (COALESCE in the query)
	if (visibility != "" && !fieldVisibilities[visibility]) || name == "" {
		return ErrBadValue
	}
	var entityType string
	err := s.withTx(ctx, func(q *db.Queries) error {
		rows, err := q.UpdateFieldGroup(ctx, db.UpdateFieldGroupParams{
			ID: toPg(gid), OrganizationID: toPg(oc.OrgID), Name: name,
			SortOrder: sortOrder, Visibility: pgText(visibility),
		})
		if err != nil {
			return ErrNotFound
		}
		entityType = rows.EntityType
		return nil
	})
	if err == nil {
		s.invalidateFieldSchema(ctx, oc.OrgID, entityType)
	}
	return err
}

func (s *Service) DeleteFieldGroup(ctx context.Context, oc *OrgCtx, gid uuid.UUID) error {
	n, err := s.q.DeleteFieldGroup(ctx, db.DeleteFieldGroupParams{ID: toPg(gid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	// group removal keeps the fields (FK SET NULL) but changes the schema
	s.invalidateFieldSchema(ctx, oc.OrgID, "")
	for _, et := range []string{"member", "application", "organization", "event", "welfare_case", "asset", "project"} {
		s.invalidateFieldSchema(ctx, oc.OrgID, et)
	}
	return nil
}

func (s *Service) ListFields(ctx context.Context, oc *OrgCtx, entityType string) ([]db.CustomFields, error) {
	return s.q.ListFields(ctx, db.ListFieldsParams{OrganizationID: toPg(oc.OrgID), Column2: entityType})
}

// FieldSchema is the merged group+field layout a form renders against —
// cached per (org, entity_type); mutations invalidate (catalog TTL 15min).
// Definitions are filtered to the caller's visibility tier (admin-only field
// definitions don't leak to regular members); the cache holds the full
// layout and the tier filter applies per request after the cached read.
var fieldVisRank = map[string]int{"member": 0, "public": 0, "exco": 1, "admin": 2}

func (s *Service) fieldTier(ctx context.Context, oc *OrgCtx) int {
	if s.HasPermission(ctx, oc.UserID, oc.OrgID, "org.fields.manage") ||
		s.HasPermission(ctx, oc.UserID, oc.OrgID, "org.admin.settings") {
		return 2
	}
	// exco = active appointment to an is_executive position (Rule 21) —
	// precise: non-executive committees and delegated officers don't qualify.
	if ex, err := s.q.IsExecutivePositionHolder(ctx, db.IsExecutivePositionHolderParams{
		OrganizationID: toPg(oc.OrgID), MemberID: toPg(oc.MembershipID)}); err == nil && ex {
		return 1
	}
	return 0
}

func visRank(v string) int {
	if r, ok := fieldVisRank[v]; ok {
		return r
	}
	return 2 // unknown visibility → admin-tier (default-closed)
}

func filterByVisibility(rows []db.CustomFields, tier int) []db.CustomFields {
	out := make([]db.CustomFields, 0, len(rows))
	for _, f := range rows {
		if visRank(f.Visibility) <= tier {
			out = append(out, f)
		}
	}
	return out
}

func filterGroupsByVisibility(rows []db.CustomFieldGroups, tier int) []db.CustomFieldGroups {
	out := make([]db.CustomFieldGroups, 0, len(rows))
	for _, g := range rows {
		if visRank(g.Visibility) <= tier {
			out = append(out, g)
		}
	}
	return out
}

func (s *Service) FieldSchema(ctx context.Context, oc *OrgCtx, entityType string) (map[string]any, error) {
	tier := s.fieldTier(ctx, oc)
	key := schemaKey(oc.OrgID, entityType)
	var groups []db.CustomFieldGroups
	var fields []db.CustomFields
	if raw, err := s.vk.Do(ctx, s.vk.B().Get().Key(key).Build()).AsBytes(); err == nil {
		var cached struct {
			Groups []db.CustomFieldGroups `json:"groups"`
			Fields []db.CustomFields      `json:"fields"`
		}
		if json.Unmarshal(raw, &cached) == nil {
			groups, fields = cached.Groups, cached.Fields
		}
	}
	if fields == nil {
		var err error
		if groups, err = s.ListFieldGroups(ctx, oc, entityType); err != nil {
			return nil, err
		}
		if fields, err = s.ListFields(ctx, oc, entityType); err != nil {
			return nil, err
		}
		if b, err := json.Marshal(map[string]any{"groups": groups, "fields": fields}); err == nil {
			_ = s.vk.Do(ctx, s.vk.B().Set().Key(key).Value(string(b)).ExSeconds(900).Build()).Error()
		}
	}
	return map[string]any{
		"groups": filterGroupsByVisibility(groups, tier),
		"fields": filterByVisibility(fields, tier),
	}, nil
}

func (s *Service) CreateField(ctx context.Context, oc *OrgCtx, entityType string,
	groupID uuid.UUID, key, label, helpText, kind string, required bool,
	options, validation json.RawMessage, sortOrder int32, visibility string) (uuid.UUID, error) {
	if !fieldEntities[entityType] || !fieldKinds[kind] || !fieldVisibilities[visibility] || key == "" || label == "" {
		return uuid.Nil, ErrBadValue
	}
	if (kind == "select" || kind == "multiselect") && len(options) == 0 {
		return uuid.Nil, ErrBadValue // choice kinds need an options list
	}
	if len(validation) == 0 {
		validation = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		fid, err := q.CreateCustomField(ctx, db.CreateCustomFieldParams{
			GroupID: pgUUIDPtr(groupID), OrganizationID: toPg(oc.OrgID), EntityType: entityType,
			Key: key, Label: label, HelpText: pgText(helpText), Kind: kind, Required: required,
			Options: options, Validation: validation, SortOrder: sortOrder,
			Visibility: visibility, Source: "custom",
		})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrBadValue // key already defined for this entity_type
			}
			return err
		}
		id = uuid.UUID(fid.Bytes)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CustomFieldDefined",
			"custom_field", id, oc.UserID, true,
			map[string]any{"field_id": id, "entity_type": entityType, "kind": kind, "source": "custom"})
	})
	if err == nil {
		s.invalidateFieldSchema(ctx, oc.OrgID, entityType)
	}
	return id, err
}

func (s *Service) UpdateField(ctx context.Context, oc *OrgCtx, fid uuid.UUID,
	groupID uuid.UUID, label, helpText string, required bool,
	options, validation json.RawMessage, sortOrder int32, visibility string) error {
	// empty visibility = keep existing (COALESCE in the query)
	if (visibility != "" && !fieldVisibilities[visibility]) || label == "" {
		return ErrBadValue
	}
	if len(validation) == 0 {
		validation = json.RawMessage(`{}`)
	}
	row, err := s.q.UpdateField(ctx, db.UpdateFieldParams{
		ID: toPg(fid), OrganizationID: toPg(oc.OrgID), GroupID: pgUUIDPtr(groupID),
		Label: label, HelpText: pgText(helpText), Required: required,
		Options: options, Validation: validation, SortOrder: sortOrder, Visibility: pgText(visibility),
	})
	if err != nil {
		return ErrNotFound
	}
	s.invalidateFieldSchema(ctx, oc.OrgID, row.EntityType)
	return nil
}

func (s *Service) DeleteField(ctx context.Context, oc *OrgCtx, fid uuid.UUID) error {
	var entityType string
	err := s.withTx(ctx, func(q *db.Queries) error {
		f, err := q.GetField(ctx, db.GetFieldParams{ID: toPg(fid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		entityType = f.EntityType
		n, err := q.RetireField(ctx, db.RetireFieldParams{ID: toPg(fid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		// delete + event in one tx (Rule 12) — a lost event can't strand the delete
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CustomFieldRetired",
			"custom_field", fid, oc.UserID, true, map[string]any{"field_id": fid})
	})
	if err == nil {
		s.invalidateFieldSchema(ctx, oc.OrgID, entityType)
	}
	return err
}

// SetFieldValues writes values for one entity. Entity-scoped: a member may
// write their own member-visibility fields; anything else (or another
// entity) needs org.fields.manage (Rule 20 — node, not role).
func (s *Service) SetFieldValues(ctx context.Context, oc *OrgCtx, entityType string, entityID uuid.UUID, values map[string]json.RawMessage) error {
	if !fieldEntities[entityType] {
		return ErrBadValue
	}
	self := entityType == "member" && entityID == oc.MembershipID
	canManage := s.HasPermission(ctx, oc.UserID, oc.OrgID, "org.fields.manage")
	if !self && !canManage {
		return ErrForbidden
	}
	if entityType == "member" && !self {
		// entity-scoped writes land on real memberships of THIS org — a typo'd
		// entity_id shouldn't silently orphan rows (Rule 24 scoping).
		m, err := s.q.GetMembershipByID(ctx, toPg(entityID))
		if err != nil || uuid.UUID(m.OrganizationID.Bytes) != oc.OrgID {
			return ErrNotFound
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	err := s.withTx(ctx, func(q *db.Queries) error {
		for _, k := range keys {
			f, err := q.GetFieldByKey(ctx, db.GetFieldByKeyParams{
				OrganizationID: toPg(oc.OrgID), EntityType: entityType, Key: k,
			})
			if err != nil {
				return ErrBadValue // unknown field key
			}
			if f.Visibility != "member" && !canManage {
				return ErrForbidden // members only write member-visible fields
			}
			vid, err := q.UpsertFieldValue(ctx, db.UpsertFieldValueParams{
				FieldID: f.ID, OrganizationID: toPg(oc.OrgID), EntityID: toPg(entityID),
				Value: values[k],
			})
			if err != nil {
				return err
			}
			if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CustomFieldValueSet",
				"custom_field", uuid.UUID(vid.Bytes), oc.UserID, false,
				map[string]any{"field_id": uuid.UUID(f.ID.Bytes), "entity_id": entityID}); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (s *Service) FieldValues(ctx context.Context, oc *OrgCtx, entityType string, entityID uuid.UUID) ([]db.ListFieldValuesRow, error) {
	return s.q.ListFieldValues(ctx, db.ListFieldValuesParams{
		OrganizationID: toPg(oc.OrgID), EntityID: toPg(entityID), EntityType: entityType})
}

// ---------- /orgs/{id}/me — UI introspection (Rule 20 client-side) ----------

// MyOrgAccess is the caller's org-scoped access profile: who they are here,
// which permission nodes they hold (position scopes flattened to their
// _branch/_committee forms, mirroring HasPermission), and which apps are
// active — everything the frontend needs to gate nav and actions.
func (s *Service) MyOrgAccess(ctx context.Context, oc *OrgCtx) (map[string]any, error) {
	ps, err := s.ResolvePermissions(ctx, oc.UserID, oc.OrgID)
	if err != nil {
		return nil, err
	}
	nodes := make([]string, 0, len(ps.Nodes)+len(ps.Branch)+len(ps.Committee))
	for n := range ps.Nodes {
		nodes = append(nodes, n)
	}
	for n := range ps.Branch {
		nodes = append(nodes, n+"_branch")
	}
	for n := range ps.Committee {
		nodes = append(nodes, n+"_committee")
	}
	sort.Strings(nodes)
	apps, err := s.OrgApps(ctx, oc)
	if err != nil {
		return nil, err
	}
	active := make([]OrgApp, 0, len(apps))
	for _, a := range apps {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	return map[string]any{
		"org_id": oc.OrgID, "membership_id": oc.MembershipID, "category": oc.Category,
		"lifecycle_state": oc.State, "permissions": nodes, "in_committee": ps.InCommittee,
		"apps": active,
	}, nil
}

// SetOrgDescription / SetCustomDomain — org profile writes used by PATCH
// /orgs/{id} (org.admin.settings). The custom domain feeds the portal
// host-map (GET /directory/resolve); uniqueness is DB-enforced.
func (s *Service) SetOrgDescription(ctx context.Context, oc *OrgCtx, desc string) error {
	n, err := s.q.SetOrgDescription(ctx, db.SetOrgDescriptionParams{ID: toPg(oc.OrgID), Description: pgText(desc)})
	if err == nil && n == 0 {
		return ErrOrgNotFound
	}
	return err
}

func (s *Service) SetCustomDomain(ctx context.Context, oc *OrgCtx, domain string) error {
	if domain != "" {
		domain = strings.ToLower(domain)
		if !validDomain(domain) {
			return ErrBadValue
		}
		// base-domain hijack guard: a custom domain that equals or nests
		// under the deployment's own domain would shadow platform hosts.
		if s.baseDomain != "" && (domain == s.baseDomain || strings.HasSuffix(domain, "."+s.baseDomain)) {
			return ErrBadValue
		}
	}
	n, err := s.q.SetCustomDomain(ctx, db.SetCustomDomainParams{ID: toPg(oc.OrgID), CustomDomain: pgText(domain)})
	if err == nil && n == 0 {
		return ErrOrgNotFound
	}
	if isUniqueViolation(err) {
		return ErrSlugTaken // another org already claims this domain
	}
	return err
}

// validDomain — permissive hostname shape check (labels + optional port-free host).
func validDomain(d string) bool {
	if len(d) > 253 {
		return false
	}
	for _, lbl := range strings.Split(d, ".") {
		if lbl == "" || len(lbl) > 63 || lbl[0] == '-' || lbl[len(lbl)-1] == '-' {
			return false
		}
		for _, c := range lbl {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return strings.Contains(d, ".")
}
