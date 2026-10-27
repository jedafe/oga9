// Ref: oga9-engineering/sections/05-domain-events-infrastructure.md (ADR-015)
// entity_links writer consumer: derives graph edges from domain events so
// "related entity" queries (RelatedEntities recursive CTE) stay cheap.
// Edge vocabulary is provisional per ADR-015 — no relationship-type catalog
// exists yet; keep edges small and structural.
package events

import (
	"context"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/db"
)

// linkRule maps an event to a derived edge: source = the event's
// aggregate, target = the entity behind a payload key. AltKeys are
// accepted spellings ONLY for that target type — a missing key must skip
// the edge, never guess a different-typed id (an org id is not a tenant).
type linkRule struct {
	SourceType, TargetType string
	TargetKey              string
	AltKeys                []string
	Rel                    string
}

var linkRules = map[string][]linkRule{
	"OrganizationCreated": {
		{SourceType: "organization", TargetType: "tenant", TargetKey: "tenant_id", Rel: "belongs_to"},
	},
	"OrgUnitCreated": {
		{SourceType: "org_unit", TargetType: "organization", TargetKey: "organization_id",
			AltKeys: []string{"org_id"}, Rel: "child_of"},
	},
	"LeaderAppointed": {
		{SourceType: "officer_appointment", TargetType: "org_position", TargetKey: "position_id", Rel: "holds"},
		{SourceType: "officer_appointment", TargetType: "term", TargetKey: "term_id", Rel: "during"},
	},
	// §13: approved minutes link back to their meeting; §14: the archived
	// document links to the minutes it preserves.
	"MinutesApproved": {
		{SourceType: "minutes", TargetType: "activity", TargetKey: "meeting_id",
			AltKeys: []string{"activity_id"}, Rel: "minutes_of"},
	},
	"MinutesArchived": {
		{SourceType: "minutes", TargetType: "document", TargetKey: "document_id", Rel: "archived_as"},
	},
	// §20 projects: the approval edge → the passed resolution; milestones
	// roll up to their project; the close archives the completion report.
	"ProjectApproved": {
		{SourceType: "project", TargetType: "resolution", TargetKey: "resolution_id", Rel: "approved_by"},
	},
	"MilestoneCompleted": {
		{SourceType: "milestone", TargetType: "project", TargetKey: "project_id", Rel: "part_of"},
	},
	"ProjectClosed": {
		{SourceType: "project", TargetType: "document", TargetKey: "document_id", Rel: "archived_as"},
	},
}

// EntityLinksConsumer writes edges for events matching linkRules. Writes
// run inside the dispatcher's tx; the consumer log row lands in the same
// tx — replay-safe via the edge UNIQUE constraint.
func EntityLinksConsumer(ctx context.Context, q *db.Queries, e Event) error {
	rules, ok := linkRules[e.Type]
	if !ok {
		return nil
	}
	for _, r := range rules {
		target := e.UUID(r.TargetKey)
		for _, alt := range r.AltKeys {
			if target != uuid.Nil {
				break
			}
			target = e.UUID(alt)
		}
		if target == uuid.Nil {
			continue // payload key absent — nothing to link
		}
		if err := q.LinkEntities(ctx, db.LinkEntitiesParams{
			OrganizationID: pgUUID(e.OrganizationID),
			SourceType:     e.AggregateType, SourceID: pgUUID(e.AggregateID),
			TargetType: r.TargetType, TargetID: pgUUID(target),
			RelationshipType: r.Rel, CreatedBy: pgUUID(e.ActorID),
		}); err != nil {
			return err
		}
	}
	return nil
}
