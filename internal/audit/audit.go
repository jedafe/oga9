// Ref: oga9-engineering/sections/05-domain-events-infrastructure.md (audit_events)
// Rule 10: impersonation/support-access MUST write audit_events BEFORE the
// action — a failed write aborts the request (Log returns the error; callers
// propagate, never swallow).
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jedafe/oga9/internal/db"
)

type Entry struct {
	Action         string         `json:"action"` // e.g. impersonation.start
	ActorID        *uuid.UUID     `json:"actor_id"`
	TenantID       *uuid.UUID     `json:"tenant_id"`
	OrganizationID *uuid.UUID     `json:"organization_id"`
	TargetType     string         `json:"target_type"`
	TargetID       *uuid.UUID     `json:"target_id"`
	Context        map[string]any `json:"context"` // who/when/what/approval
}

// Log appends an audit_events row inside the given transaction — the caller
// decides the tx boundary so the audit lands atomically with (or before) the
// audited mutation.
func Log(ctx context.Context, tx pgx.Tx, e Entry) error {
	ctxJSON, _ := json.Marshal(e.Context)
	return db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{
		TenantID:       optUUID(e.TenantID),
		OrganizationID: optUUID(e.OrganizationID),
		ActorID:        optUUID(e.ActorID),
		Action:         e.Action,
		TargetType:     pgtype.Text{String: e.TargetType, Valid: e.TargetType != ""},
		TargetID:       optUUID(e.TargetID),
		Context:        ctxJSON,
	})
}

func optUUID(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}
