// Ref: oga9-engineering/adr/ADR-016-rules-workflow-engine.md — org rules
// resolve through this engine (Rule 23): an active rule_definitions row
// pins or bounds an org_settings key; org_settings is the floor when no
// rule binds, and the catalog default is last. Evaluation of a binding
// rule is logged for auditability (ADR-016 consequence).
package rules

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
)

// Definition is the JSONB contract for rule_definitions rows:
//
//	{"value": X}            — pin the key to X
//	{"min":n,"max":m,"default":d} — bound an org_settings value (clamped)
//	{"require":true}        — mark an org_settings key required
type Definition struct {
	Value   json.RawMessage `json:"value"`
	Min     *float64        `json:"min"`
	Max     *float64        `json:"max"`
	Default json.RawMessage `json:"default"`
	Require bool            `json:"require"`
}

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }

// Value resolves a settings key for an org: rule_definitions first
// (pinned value wins; bounds clamp the org_settings value), then
// org_settings, else ErrNoRows. Callers keep their own decode defaults.
func (s *Service) Value(ctx context.Context, orgID uuid.UUID, key string) ([]byte, error) {
	r, err := s.q.GetRule(ctx, db.GetRuleParams{Key: key, OrganizationID: toPg(orgID)})
	if err == nil {
		var d Definition
		if json.Unmarshal(r.Definition, &d) == nil {
			if d.Value != nil {
				slog.Info("rule_eval", "key", key, "org_id", orgID,
					"rule_id", uuid.UUID(r.ID.Bytes), "kind", r.Kind, "result", "pinned")
				return d.Value, nil
			}
			// bounded rule: org_settings value clamped into [min,max],
			// falling back to the rule default when unset.
			raw, serr := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
				OrganizationID: toPg(orgID), Key: key})
			if serr != nil && d.Default != nil {
				slog.Info("rule_eval", "key", key, "org_id", orgID,
					"rule_id", uuid.UUID(r.ID.Bytes), "kind", r.Kind, "result", "rule_default")
				return d.Default, nil
			}
			if serr != nil {
				return nil, serr
			}
			var n float64
			if json.Unmarshal(raw, &n) == nil && (d.Min != nil || d.Max != nil) {
				if d.Min != nil && n < *d.Min {
					n = *d.Min
				}
				if d.Max != nil && n > *d.Max {
					n = *d.Max
				}
				out, _ := json.Marshal(n)
				slog.Info("rule_eval", "key", key, "org_id", orgID,
					"rule_id", uuid.UUID(r.ID.Bytes), "kind", r.Kind, "result", "clamped")
				return out, nil
			}
			return raw, nil
		}
	}
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	return s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: toPg(orgID), Key: key})
}

// Bool/Int/Text are typed conveniences over Value with catalog defaults.
func (s *Service) Bool(ctx context.Context, orgID uuid.UUID, key string, def bool) bool {
	if raw, err := s.Value(ctx, orgID, key); err == nil {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return b
		}
	}
	return def
}

func (s *Service) Int(ctx context.Context, orgID uuid.UUID, key string, def int64) int64 {
	if raw, err := s.Value(ctx, orgID, key); err == nil {
		var n int64
		if json.Unmarshal(raw, &n) == nil {
			return n
		}
	}
	return def
}

func (s *Service) Text(ctx context.Context, orgID uuid.UUID, key, def string) string {
	if raw, err := s.Value(ctx, orgID, key); err == nil {
		var t string
		if json.Unmarshal(raw, &t) == nil && t != "" {
			return t
		}
	}
	return def
}

// ---------- org-facing management (rules editable via settings, ADR-016) ----------

func (s *Service) List(ctx context.Context, orgID uuid.UUID) ([]db.RuleDefinitions, error) {
	return s.q.ListRules(ctx, toPg(orgID))
}

// Set pins a key for the org — kind 'value' definition {value: v}.
func (s *Service) Set(ctx context.Context, orgID uuid.UUID, key string, value json.RawMessage) (uuid.UUID, error) {
	def, _ := json.Marshal(Definition{Value: value})
	row, err := s.q.UpsertOrgRule(ctx, db.UpsertOrgRuleParams{
		OrganizationID: toPg(orgID), Key: key, Kind: "value",
		Definition: def, Priority: 0, Active: true})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.UUID(row.ID.Bytes), nil
}

func (s *Service) Retire(ctx context.Context, orgID, ruleID uuid.UUID) error {
	n, err := s.q.RetireRule(ctx, db.RetireRuleParams{ID: toPg(ruleID), OrganizationID: toPg(orgID)})
	if err == nil && n == 0 {
		err = pgx.ErrNoRows
	}
	return err
}
