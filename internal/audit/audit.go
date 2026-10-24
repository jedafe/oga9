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
	_, err := tx.Exec(ctx,
		`INSERT INTO audit_events (tenant_id, organization_id, actor_id, action, target_type, target_id, context)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.TenantID, e.OrganizationID, e.ActorID, e.Action, e.TargetType, e.TargetID, ctxJSON)
	return err
}
