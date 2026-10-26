// Ref: oga9-engineering/sections/24-realtime-chat-voice.md — channels,
// derived membership (general = all active; unit = home unit; explicit
// rows for exco/custom/dm), messages CRUD (soft delete, Rule 11),
// read states + mutes. Post fans out to Valkey pubsub for /ws.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound          = errors.New("chat: not_found")
	ErrBadValue          = errors.New("chat: invalid_value")
	ErrForbidden         = errors.New("chat: forbidden")
	ErrNotMember         = errors.New("chat: not_a_channel_member")
	ErrMuted             = errors.New("chat: muted")
	ErrEditWindow        = errors.New("chat: edit_window_expired")
	ErrInvalidTransition = errors.New("chat: invalid_state_transition")
)

var channelKinds = map[string]bool{"general": true, "exco": true, "unit": true,
	"committee": true, "custom": true, "dm": true}
var messageKinds = map[string]bool{"text": true, "voice": true, "file": true, "system": true}

// Bus is the realtime pub/sub key — catalogued oga9: prefix.
const BusKey = "oga9:realtime:chat"

type Service struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	vk      valkey.Client
	voice   *voiceCfg
	docGate func(ctx context.Context, orgID, docID uuid.UUID) bool
	// activityGate validates a voice-room's activity link belongs to the
	// org — wired from activities.Service (Rule 26: chat never reads the
	// activities table itself).
	activityGate func(ctx context.Context, orgID, activityID uuid.UUID) bool
}

// SetDocGate wires the documents-module check for media_id attachments
// (Rule 26 — chat never reads the documents table).
func (s *Service) SetDocGate(fn func(ctx context.Context, orgID, docID uuid.UUID) bool) {
	s.docGate = fn
}

// SetActivityGate wires the activities-module check for activity-linked
// voice rooms (voice_rooms.activity_id).
func (s *Service) SetActivityGate(fn func(ctx context.Context, orgID, activityID uuid.UUID) bool) {
	s.activityGate = fn
}

func NewService(pool *pgxpool.Pool, vk valkey.Client) *Service {
	return &Service{pool: pool, q: db.New(pool), vk: vk}
}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Channels lists what the caller can see (derived membership, §24 rules).
func (s *Service) Channels(ctx context.Context, oc *tenant.OrgCtx) ([]db.ChatChannels, error) {
	return s.q.ChannelsForMember(ctx, db.ChannelsForMemberParams{
		OrganizationID: toPg(oc.OrgID), ID: toPg(oc.MembershipID)})
}

// CreateChannel — general|unit need nothing extra; exco/custom/dm start
// with the creator as owner. name required unless general.
func (s *Service) CreateChannel(ctx context.Context, oc *tenant.OrgCtx, kind, name string, unitID uuid.UUID) (uuid.UUID, error) {
	if !channelKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	if kind != "general" && name == "" {
		return uuid.Nil, ErrBadValue
	}
	// unit_id is meaningful only on unit|committee channels (§24 schema);
	// it must name a unit IN this org — the FK only proves existence.
	if kind == "unit" || kind == "committee" {
		if unitID == uuid.Nil {
			return uuid.Nil, ErrBadValue
		}
		if _, err := s.q.GetOrgUnit(ctx, db.GetOrgUnitParams{
			ID: toPg(unitID), OrganizationID: toPg(oc.OrgID)}); err != nil {
			return uuid.Nil, ErrNotFound
		}
	} else if unitID != uuid.Nil {
		return uuid.Nil, ErrBadValue
	}
	var row db.ChatChannels
	created := true
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateChannel(ctx, db.CreateChannelParams{
			OrganizationID: toPg(oc.OrgID), Kind: kind, UnitID: toPg(unitID),
			Name: pgtype.Text{String: name, Valid: name != ""}, CreatedBy: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			// channel already exists (unique index) — return it; the
			// create event must not re-fire for an existing channel.
			created = false
			row, err = q.GetChannelByKind(ctx, db.GetChannelByKindParams{
				OrganizationID: toPg(oc.OrgID), Kind: kind, UnitID: toPg(unitID)})
		}
		if err != nil {
			return err
		}
		// creator joins as owner on non-general channels
		if kind != "general" {
			if err := q.JoinChannel(ctx, db.JoinChannelParams{
				ChannelID: row.ID, MembershipID: toPg(oc.MembershipID), Column3: "owner"}); err != nil {
				return err
			}
		}
		if !created {
			return nil
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ChannelCreated",
			"chat_channel", fromPg(row.ID), oc.UserID, false,
			map[string]any{"channel_id": fromPg(row.ID), "kind": kind,
				"unit_id": unitID, "name": name})
	})
	return fromPg(row.ID), err
}

// ArchiveChannel — channels.manage retires a non-general channel; derived
// kinds (unit/exco/committee) die with their source, not here.
func (s *Service) ArchiveChannel(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) error {
	ch, err := s.q.GetChannel(ctx, db.GetChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if ch.Kind == "general" || ch.Kind == "unit" || ch.Kind == "exco" || ch.Kind == "committee" {
		return ErrBadValue // derived channels archive with their source
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.ArchiveChannel(ctx, db.ArchiveChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ChannelArchived",
			"chat_channel", cid, oc.UserID, false, map[string]any{"channel_id": cid})
	})
}

// RemoveMember unseats a member on a custom/dm channel — officer removal
// (channels.manage) or self-leave (memberID == caller). Derived kinds
// refuse: their seats come from membership/appointments.
func (s *Service) RemoveMember(ctx context.Context, oc *tenant.OrgCtx, cid, memberID uuid.UUID) error {
	ch, err := s.q.GetChannel(ctx, db.GetChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if ch.Kind != "custom" && ch.Kind != "dm" {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.RemoveChannelMember(ctx, db.RemoveChannelMemberParams{
			ChannelID: toPg(cid), MembershipID: toPg(memberID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ChannelMemberRemoved",
			"chat_channel", cid, oc.UserID, false,
			map[string]any{"channel_id": cid, "membership_id": memberID})
	})
}

func (s *Service) orgSettingText(ctx context.Context, orgID uuid.UUID, key, fallback string) string {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{OrganizationID: toPg(orgID), Key: key})
	if err != nil {
		return fallback
	}
	var v string
	if json.Unmarshal(raw, &v) != nil || v == "" {
		return fallback
	}
	return v
}

func (s *Service) orgSettingInt(ctx context.Context, orgID uuid.UUID, key string, fallback int64) int64 {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{OrganizationID: toPg(orgID), Key: key})
	if err != nil {
		return fallback
	}
	var v int64
	if json.Unmarshal(raw, &v) != nil {
		return fallback
	}
	return v
}

func (s *Service) memberOK(ctx context.Context, channelID, memberID uuid.UUID) (bool, error) {
	return s.q.IsChannelMember(ctx, db.IsChannelMemberParams{ID: toPg(channelID), ID_2: toPg(memberID)})
}

// PostMessage — caller must already hold org.chat.post (route gates it);
// this checks channel membership + mute + general post scope.
func (s *Service) PostMessage(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, kind, body string, mediaID, replyTo uuid.UUID, voiceMs int) (uuid.UUID, error) {
	ch, err := s.q.GetChannel(ctx, db.GetChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if !messageKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	if kind == "voice" {
		if max := s.orgSettingInt(ctx, oc.OrgID, "org.chat.voice_max_seconds", 300); max > 0 &&
			int64(voiceMs) > max*1000 {
			return uuid.Nil, ErrBadValue
		}
	}
	if ch.Kind == "general" &&
		s.orgSettingText(ctx, oc.OrgID, "org.chat.general_post_scope", "all") == "exco" {
		// general channel posts are officer-only under this org rule.
		exco, err := s.q.IsActiveOfficer(ctx, toPg(oc.MembershipID))
		if err != nil || !exco {
			return uuid.Nil, ErrForbidden
		}
	}
	if replyTo != uuid.Nil {
		// replies must point at a live message in THIS channel.
		if _, err := s.q.GetMessage(ctx, db.GetMessageParams{
			ID: toPg(replyTo), ChannelID: toPg(cid)}); err != nil {
			return uuid.Nil, ErrBadValue
		}
	}
	if mediaID != uuid.Nil {
		// attachments must be documents of THIS org (validated through
		// the comms seam, Rule 26) — cross-org or missing ids refuse.
		if s.docGate == nil || !s.docGate(ctx, oc.OrgID, mediaID) {
			return uuid.Nil, ErrNotFound
		}
	}
	ok, err := s.memberOK(ctx, cid, oc.MembershipID)
	if err != nil || !ok {
		return uuid.Nil, ErrNotMember
	}
	// muted_until is the member's own notification preference (§24 mute is
	// self-only) — it silences delivery, never the right to post. A future
	// moderator-mute would be a separate flag.
	var row db.ChatMessages
	err = s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.PostMessage(ctx, db.PostMessageParams{
			ChannelID: toPg(cid), MembershipID: toPg(oc.MembershipID), Kind: kind,
			Body:    pgtype.Text{String: body, Valid: body != ""},
			MediaID: toPg(mediaID), VoiceDurationMs: pgtype.Int4{Int32: int32(voiceMs), Valid: voiceMs > 0},
			ReplyToID: toPg(replyTo)})
		if err != nil {
			return err
		}
		if err := q.TouchChannel(ctx, toPg(cid)); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MessagePosted",
			"chat_message", fromPg(row.ID), oc.UserID, false,
			map[string]any{"message_id": fromPg(row.ID), "channel_id": cid, "kind": kind}); err != nil {
			return err
		}
		if kind == "voice" {
			// §24 catalogs a voice-specific event alongside MessagePosted.
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoiceNotePosted",
				"chat_message", fromPg(row.ID), oc.UserID, false,
				map[string]any{"message_id": fromPg(row.ID), "channel_id": cid,
					"duration_ms": voiceMs})
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	// Rule 8: fan-out to /ws subscribers — fire-and-forget.
	if s.vk != nil {
		b, _ := json.Marshal(map[string]any{"channel_id": cid, "message_id": fromPg(row.ID),
			"kind": kind, "membership_id": oc.MembershipID, "body": body,
			"created_at": row.CreatedAt.Time})
		s.vk.Do(ctx, s.vk.B().Publish().Channel(BusKey).Message(string(b)).Build())
	}
	return fromPg(row.ID), nil
}

// Messages — paginated by created_at cursor (§24: before cursor).
func (s *Service) Messages(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, before *time.Time) ([]db.ChatMessages, error) {
	ok, err := s.memberOK(ctx, cid, oc.MembershipID)
	if err != nil || !ok {
		return nil, ErrNotMember
	}
	var cur pgtype.Timestamptz
	if before != nil {
		cur = pgtype.Timestamptz{Time: *before, Valid: true}
	}
	return s.q.ChannelMessages(ctx, db.ChannelMessagesParams{ChannelID: toPg(cid), Column2: cur})
}

// Delete — own message, or moderator (handler passes moderate=true).
func (s *Service) Delete(ctx context.Context, oc *tenant.OrgCtx, cid, mid uuid.UUID, moderate bool) error {
	if _, err := s.q.GetChannel(ctx, db.GetChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return ErrNotFound
	}
	m, err := s.q.GetMessage(ctx, db.GetMessageParams{ID: toPg(mid), ChannelID: toPg(cid)})
	if err != nil {
		return ErrNotFound
	}
	if !moderate && fromPg(m.MembershipID) != oc.MembershipID {
		return ErrForbidden
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SoftDeleteMessage(ctx, db.SoftDeleteMessageParams{ID: toPg(mid), ChannelID: toPg(cid)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MessageDeleted",
			"chat_message", mid, oc.UserID, false, map[string]any{"message_id": mid, "by": oc.UserID})
	})
}

// EditMessage — own message only, inside org.chat.edit_window_minutes.
func (s *Service) EditMessage(ctx context.Context, oc *tenant.OrgCtx, cid, mid uuid.UUID, body string) error {
	if body == "" {
		return ErrBadValue
	}
	m, err := s.q.GetMessage(ctx, db.GetMessageParams{ID: toPg(mid), ChannelID: toPg(cid)})
	if err != nil {
		return ErrNotFound
	}
	if fromPg(m.MembershipID) != oc.MembershipID {
		return ErrForbidden
	}
	if win := s.orgSettingInt(ctx, oc.OrgID, "org.chat.edit_window_minutes", 15); win > 0 &&
		m.CreatedAt.Time.Add(time.Duration(win)*time.Minute).Before(time.Now()) {
		return ErrEditWindow
	}
	n, err := s.q.EditMessage(ctx, db.EditMessageParams{
		ID: toPg(mid), ChannelID: toPg(cid), Body: pgtype.Text{String: body, Valid: true}, MembershipID: toPg(oc.MembershipID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkRead(ctx context.Context, oc *tenant.OrgCtx, cid, mid uuid.UUID) error {
	if mid != uuid.Nil {
		// the read cursor must point at a message IN this channel —
		// the FK accepts any message id, which would corrupt read state.
		if _, err := s.q.GetMessage(ctx, db.GetMessageParams{
			ID: toPg(mid), ChannelID: toPg(cid)}); err != nil {
			return ErrNotFound
		}
	}
	if ok, err := s.memberOK(ctx, cid, oc.MembershipID); err != nil || !ok {
		return ErrNotMember
	}
	return s.q.MarkChannelRead(ctx, db.MarkChannelReadParams{
		ChannelID: toPg(cid), MembershipID: toPg(oc.MembershipID), LastReadMessageID: toPg(mid)})
}

func (s *Service) Mute(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, until *time.Time) error {
	if ok, err := s.memberOK(ctx, cid, oc.MembershipID); err != nil || !ok {
		return ErrNotMember
	}
	var mu pgtype.Timestamptz
	if until != nil {
		mu = pgtype.Timestamptz{Time: *until, Valid: true}
	}
	return s.q.MuteChannel(ctx, db.MuteChannelParams{
		ChannelID: toPg(cid), MembershipID: toPg(oc.MembershipID), MutedUntil: mu})
}

// CanSubscribe answers the realtime gateway's check (Rule 26): channel
// membership AND that the membership belongs to the session user — a
// client-supplied membership_id can't borrow another member's seat.
func (s *Service) CanSubscribe(ctx context.Context, channelID, memberID, userID uuid.UUID) bool {
	ok, _ := s.q.IsChannelMemberForUser(ctx, db.IsChannelMemberForUserParams{
		ID: toPg(channelID), ID_2: toPg(memberID), UserID: toPg(userID)})
	return ok
}

// AddMember seats a member on a custom/dm/committee channel — general,
// unit and exco membership is derived, never row-managed (§24).
func (s *Service) AddMember(ctx context.Context, oc *tenant.OrgCtx, cid, memberID uuid.UUID, role string) error {
	ch, err := s.q.GetChannel(ctx, db.GetChannelParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if ch.Kind == "general" || ch.Kind == "unit" || ch.Kind == "exco" {
		return ErrBadValue // derived channels reject explicit rows
	}
	dm := ch.Kind == "dm"
	if role != "owner" && role != "moderator" && role != "member" {
		role = "member"
	}
	// the seat must be a member of THIS org — the FK only proves existence.
	if org, err := s.q.GetMembershipOrg(ctx, toPg(memberID)); err != nil || fromPg(org) != oc.OrgID {
		return ErrNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if dm {
			// lock the channel row so two concurrent adds can't both pass
			// the 2-seat headcount — FOR UPDATE must live inside the tx.
			if _, err := q.LockChannel(ctx, toPg(cid)); err != nil {
				return err
			}
			if ids, err := q.ChannelMemberIDs(ctx, toPg(cid)); err == nil && len(ids) >= 2 {
				return ErrBadValue // a dm is two members, no more
			}
		}
		if err := q.JoinChannel(ctx, db.JoinChannelParams{
			ChannelID: toPg(cid), MembershipID: toPg(memberID), Column3: role}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ChannelMemberAdded",
			"chat_channel", cid, oc.UserID, false,
			map[string]any{"channel_id": cid, "membership_id": memberID, "role": role})
	})
}

// RetentionSweep — chat:retention_sweep (§24): org.chat.retention_days
// per org; past-window messages soft-delete, each emitting MessageDeleted
// in the same tx (Rule 12) so consumers stay consistent.
func (s *Service) RetentionSweep(ctx context.Context) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		rows, err := q.RetentionSweepChat(ctx)
		if err != nil {
			return err
		}
		tenantCache := map[uuid.UUID]uuid.UUID{}
		for _, r := range rows {
			orgID := fromPg(r.OrganizationID)
			tid, ok := tenantCache[orgID]
			if !ok {
				if t, err := q.GetOrgTenantID(ctx, r.OrganizationID); err == nil {
					tid = fromPg(t)
				}
				tenantCache[orgID] = tid
			}
			if err := events.Emit(ctx, q, tid, orgID, "MessageDeleted",
				"chat_message", fromPg(r.ID), uuid.Nil, false,
				map[string]any{"message_id": fromPg(r.ID), "reason": "retention"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// voice is the LiveKit provider config (SetVoiceProvider); pgText lives
// here because voice.go uses it for the optional title.
func (s *Service) voiceEnabled() bool { return s.voice != nil && s.voice.apiKey != "" }

func pgText(v string) pgtype.Text { return pgtype.Text{String: v, Valid: v != ""} }
