// Ref: oga9-engineering/sections/22-ai.md — permission-scoped chat,
// HITL generations (AI never writes domain tables), usage metering
// (limit.ai.requests → hard stop + upgrade hint, Rule 25).
package ai

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrBadValue    = errors.New("bad value")
	ErrBadState    = errors.New("bad_generation_state")
	ErrQuota       = errors.New("ai_quota_exceeded")
	ErrNotProvider = errors.New("ai_provider_unconfigured")
)

var personas = map[string]bool{"general": true, "secretary": true, "treasurer": true, "historian": true}
var draftKinds = map[string]bool{"agenda": true, "minutes": true, "announcement": true, "report": true, "insight": true}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }

// Citation links an answer back to a source entity (§22 rule — every
// answer carries citations).
type Citation struct {
	Type  string    `json:"type"`
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
}

// Answer is the provider seam's output: body + the permission-scoped
// citations the response claims.
type Answer struct {
	Body      string     `json:"body"`
	Citations []Citation `json:"citations"`
	TokensIn  int        `json:"tokens_in"`
	TokensOut int        `json:"tokens_out"`
}

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// providerFn is the LLM seam — wired in main to the
	// OpenAI-compatible adapter when OGA9_AI_PROVIDER_URL is set. It receives the
	// asker's OrgCtx so answers stay permission-scoped.
	providerFn func(ctx context.Context, oc *tenant.OrgCtx, persona, question string) (*Answer, error)
	draftFn    func(ctx context.Context, oc *tenant.OrgCtx, kind, prompt string) (json.RawMessage, int, int, error)
	applyFn    func(ctx context.Context, oc *tenant.OrgCtx, g db.AiGenerations) error
	limitFn    func(ctx context.Context, tenantID uuid.UUID, key string) int64
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetProvider(fn func(context.Context, *tenant.OrgCtx, string, string) (*Answer, error)) {
	s.providerFn = fn
}
func (s *Service) SetDraftProvider(fn func(context.Context, *tenant.OrgCtx, string, string) (json.RawMessage, int, int, error)) {
	s.draftFn = fn
}
func (s *Service) SetApply(fn func(context.Context, *tenant.OrgCtx, db.AiGenerations) error) {
	s.applyFn = fn
}
func (s *Service) SetLimit(fn func(context.Context, uuid.UUID, string) int64) { s.limitFn = fn }

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// checkQuota — limit.ai.requests per plan (Rule 25): over → hard stop
// + AIQuotaExceeded (upgrade-path notification rides the event).
func (s *Service) checkQuota(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx) error {
	if s.limitFn == nil {
		return nil
	}
	lim := s.limitFn(ctx, oc.TenantID, "limit.ai.requests")
	if lim < 0 {
		return nil // -1 = unlimited (entitlements convention)
	}
	if lim == 0 {
		return ErrQuota
	}
	used, err := q.TenantUsageThisMonth(ctx, toPg(oc.TenantID))
	if err != nil {
		return err
	}
	if used.Requests >= lim {
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AIQuotaExceeded",
			"tenant", oc.TenantID, oc.UserID, false,
			map[string]any{"tenant_id": oc.TenantID, "limit_key": "limit.ai.requests", "used": used.Requests}); err != nil {
			return err
		}
		return ErrQuota // hard stop (Rule 25) — upgrade hint rides the event
	}
	return nil
}

// ---------- chat ----------

// Chat — user message → provider answer (permission-scoped citations) →
// both messages + usage log + AIQueryAnswered in one tx.
func (s *Service) Chat(ctx context.Context, oc *tenant.OrgCtx, convID uuid.UUID,
	persona, question string) (uuid.UUID, uuid.UUID, *Answer, error) {
	if question == "" {
		return uuid.Nil, uuid.Nil, nil, ErrBadValue
	}
	if persona == "" {
		persona = "general"
	}
	if !personas[persona] {
		return uuid.Nil, uuid.Nil, nil, ErrBadValue
	}
	if s.providerFn == nil {
		return uuid.Nil, uuid.Nil, nil, ErrNotProvider
	}
	// quota is checked OUTSIDE the state tx — a rejected request has no
	// mutation to roll back with, and AIQuotaExceeded must persist as the
	// standalone audit event (Rule 25).
	if err := s.checkQuota(ctx, s.q, oc); err != nil {
		return uuid.Nil, uuid.Nil, nil, err
	}
	// provider call BEFORE the tx — an LLM round-trip inside a
	// transaction pins a pooled connection for its full latency.
	a, err := s.providerFn(ctx, oc, persona, question)
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, err
	}
	var cid, mid uuid.UUID
	ans := a
	err = s.withTx(ctx, func(q *db.Queries) error {
		if convID == uuid.Nil {
			id, err := q.CreateConversation(ctx, db.CreateConversationParams{
				UserID: toPg(oc.UserID), OrgID: toPg(oc.OrgID), Persona: persona})
			if err != nil {
				return err
			}
			cid = fromPg(id)
		} else {
			c, err := q.GetConversation(ctx, db.GetConversationParams{
				ID: toPg(convID), UserID: toPg(oc.UserID)})
			if err != nil {
				return ErrNotFound
			}
			// a conversation is org-scoped — continuing it under a
			// different org ctx would mix context (and citations).
			if c.OrganizationID != toPg(oc.OrgID) {
				return ErrNotFound
			}
			cid = fromPg(c.ID)
		}
		if _, err := q.AddMessage(ctx, db.AddMessageParams{
			ConversationID: toPg(cid), Role: "user", Body: question}); err != nil {
			return err
		}
		cites, _ := json.Marshal(a.Citations)
		mid2, err := q.AddMessage(ctx, db.AddMessageParams{
			ConversationID: toPg(cid), Role: "assistant", Body: a.Body,
			Citations: cites,
			TokensIn:  pgtype.Int4{Int32: int32(a.TokensIn), Valid: true},
			TokensOut: pgtype.Int4{Int32: int32(a.TokensOut), Valid: true}})
		if err != nil {
			return err
		}
		mid = fromPg(mid2)
		if err := q.LogUsage(ctx, db.LogUsageParams{
			TenantID: toPg(oc.TenantID), UserID: toPg(oc.UserID), Kind: "chat",
			TokensIn:  pgtype.Int4{Int32: int32(a.TokensIn), Valid: true},
			TokensOut: pgtype.Int4{Int32: int32(a.TokensOut), Valid: true}}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AIQueryAnswered",
			"ai_conversation", cid, oc.UserID, false,
			map[string]any{"conversation_id": cid, "citations": a.Citations})
	})
	return cid, mid, ans, err
}

func (s *Service) Conversations(ctx context.Context, oc *tenant.OrgCtx) ([]db.AiConversations, error) {
	return s.q.ListConversations(ctx, db.ListConversationsParams{UserID: toPg(oc.UserID), OrganizationID: toPg(oc.OrgID)})
}

func (s *Service) Messages(ctx context.Context, oc *tenant.OrgCtx, convID uuid.UUID) ([]db.AiMessages, error) {
	c, err := s.q.GetConversation(ctx, db.GetConversationParams{
		ID: toPg(convID), UserID: toPg(oc.UserID)})
	if err != nil || c.OrganizationID != toPg(oc.OrgID) {
		return nil, ErrNotFound
	}
	return s.q.ListMessages(ctx, toPg(convID))
}

// ---------- generations (HITL) ----------

// Draft — §22: sensitive domains produce drafts ONLY; humans review and
// apply. AI never writes domain tables.
func (s *Service) Draft(ctx context.Context, oc *tenant.OrgCtx, kind, prompt string) (uuid.UUID, error) {
	if !draftKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	if s.draftFn == nil {
		return uuid.Nil, ErrNotProvider
	}
	if oc.MembershipID == uuid.Nil {
		return uuid.Nil, ErrBadValue // drafts need an author membership
	}
	// same as Chat: the quota rejection must not roll back its own audit.
	if err := s.checkQuota(ctx, s.q, oc); err != nil {
		return uuid.Nil, err
	}
	// same: draft provider outside the tx.
	draft, tin, tout, err := s.draftFn(ctx, oc, kind, prompt)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		gid, err := q.CreateGeneration(ctx, db.CreateGenerationParams{
			OrgID: toPg(oc.OrgID), AuthorID: toPg(oc.MembershipID), Kind: kind, Draft: draft})
		if err != nil {
			return err
		}
		id = fromPg(gid)
		if err := q.LogUsage(ctx, db.LogUsageParams{
			TenantID: toPg(oc.TenantID), UserID: toPg(oc.UserID), Kind: "draft",
			TokensIn:  pgtype.Int4{Int32: int32(tin), Valid: true},
			TokensOut: pgtype.Int4{Int32: int32(tout), Valid: true}}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AIDraftGenerated",
			"ai_generation", id, oc.UserID, false,
			map[string]any{"generation_id": id, "kind": kind})
	})
	return id, err
}

func (s *Service) ListGenerations(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.AiGenerations, error) {
	return s.q.ListGenerations(ctx, db.ListGenerationsParams{
		OrgID: toPg(oc.OrgID), State: pgtype.Text{String: state, Valid: state != ""}})
}

// Review — approve → the apply seam lands the draft in its domain;
// reject closes it. pending_review only.
func (s *Service) Review(ctx context.Context, oc *tenant.OrgCtx, gid uuid.UUID, approve bool) error {
	g, err := s.q.GetGeneration(ctx, db.GetGenerationParams{ID: toPg(gid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	state := "rejected"
	if approve {
		state = "approved"
	}
	if err := s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetGenerationState(ctx, db.SetGenerationStateParams{
			ID: g.ID, OrganizationID: g.OrganizationID, State: state, ReviewedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // already decided
		}
		ev := "AIDraftRejected"
		if approve {
			ev = "AIDraftApproved"
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"ai_generation", gid, oc.UserID, approve,
			map[string]any{"generation_id": gid, "reviewer_id": oc.MembershipID})
	}); err != nil {
		return err
	}
	if !approve || s.applyFn == nil {
		return nil
	}
	// approve committed — the domain apply runs on ITS own tx (Rule 26
	// boundary), then a second tx records applied + AIDraftApplied.
	if err := s.applyFn(ctx, oc, g); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.MarkGenerationApplied(ctx, db.MarkGenerationAppliedParams{
			ID: g.ID, OrganizationID: g.OrganizationID}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AIDraftApplied",
			"ai_generation", gid, oc.UserID, true,
			map[string]any{"generation_id": gid, "kind": g.Kind})
	})
}

// ResolveMember rebuilds the OrgCtx for a queued ai:answer/draft job —
// the membership and tenant resolve from the tables, never from the
// task payload (Rule 24 pattern).
func (s *Service) ResolveMember(ctx context.Context, orgID, userID uuid.UUID) (*tenant.OrgCtx, error) {
	m, err := s.q.GetMembership(ctx, db.GetMembershipParams{
		OrganizationID: toPg(orgID), UserID: toPg(userID)})
	if err != nil || m.LifecycleState != "active" {
		return nil, ErrNotFound
	}
	tid, err := s.q.OrgTenantID(ctx, m.OrganizationID)
	if err != nil {
		return nil, ErrNotFound
	}
	return &tenant.OrgCtx{
		OrgID: fromPg(m.OrganizationID), TenantID: fromPg(tid),
		UserID: fromPg(m.UserID), MembershipID: fromPg(m.ID),
		State: m.LifecycleState,
	}, nil
}

// ---------- usage ----------

// MyUsage — /me/ai/usage: the member's request/token counts this month.
// Takes userID directly — /me routes carry no OrgCtx.
func (s *Service) MyUsage(ctx context.Context, userID uuid.UUID) (db.UserUsageThisMonthRow, error) {
	return s.q.UserUsageThisMonth(ctx, toPg(userID))
}

// TenantUsage — /platform/ai/usage: per-tenant rollups for the control
// plane.
func (s *Service) TenantUsage(ctx context.Context) ([]db.TenantUsageRollupRow, error) {
	return s.q.TenantUsageRollup(ctx)
}

// UsageRollup — ai:usage_rollup (low): the metered rollup is computed on
// demand; the sweep is where a future billable-usage sync lands.
func (s *Service) UsageRollup(ctx context.Context) (int, error) {
	rows, err := s.q.TenantUsageRollup(ctx)
	return len(rows), err
}
