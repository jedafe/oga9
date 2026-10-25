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
// aggregate, target = the entity behind a payload key.
type linkRule struct {
	SourceType, TargetType string
	TargetKey              string
	Rel                    string
}

var linkRules = map[string][]linkRule{
	"OrganizationCreated": {
		{SourceType: "organization", TargetType: "tenant", TargetKey: "tenant_id", Rel: "belongs_to"},
	},
	"OrgUnitCreated": {
		{SourceType: "org_unit", TargetType: "organization", TargetKey: "organization_id", Rel: "child_of"},
	},
	"LeaderAppointed": {
		{SourceType: "officer_appointment", TargetType: "org_position", TargetKey: "position_id", Rel: "holds"},
		{SourceType: "officer_appointment", TargetType: "term", TargetKey: "term_id", Rel: "during"},
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
		// emitters use both "organization_id" (catalog) and "org_id" spellings
		target := e.UUID(r.TargetKey)
		if target == uuid.Nil {
			target = e.UUID("organization_id")
		}
		if target == uuid.Nil {
			target = e.UUID("org_id")
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
