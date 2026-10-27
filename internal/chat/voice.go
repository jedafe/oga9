// Ref: oga9-engineering/sections/24-realtime-chat-voice.md — voice rooms.
// Room state lives in the DB (§24: "room state persisted server-side;
// LiveKit carries only media"); join issues a short-lived LiveKit access
// token — a plain JWT with the video grant, signed with the API secret.
package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var ErrVoiceUnavailable = errors.New("chat: voice_provider_unconfigured")

// voiceCfg holds the LiveKit provider credentials — env-configured
// (OGA9_LIVEKIT_*); an unconfigured provider disables voice cleanly
// (Rule 29: predictable degradation).
type voiceCfg struct {
	url, apiKey, apiSecret string
	joinWindowSec          int
}

// SetVoiceProvider wires the LiveKit provider; empty keys = voice off.
func (s *Service) SetVoiceProvider(url, apiKey, apiSecret string, joinWindowSec int) {
	s.voice = &voiceCfg{url: url, apiKey: apiKey, apiSecret: apiSecret, joinWindowSec: joinWindowSec}
}

// VoiceProviderURL exposes the LiveKit endpoint for client hand-off.
func (s *Service) VoiceProviderURL() string {
	if s.voice == nil {
		return ""
	}
	return s.voice.url
}

// StartRoom opens a room — optionally spawned from a channel (membership
// checked) or linked to a meeting/event in the same org.
func (s *Service) StartRoom(ctx context.Context, oc *tenant.OrgCtx, title string, channelID, activityID uuid.UUID) (uuid.UUID, error) {
	if s.voice == nil || s.voice.apiKey == "" {
		return uuid.Nil, ErrVoiceUnavailable
	}
	if channelID != uuid.Nil {
		if ok, err := s.memberOK(ctx, channelID, oc.MembershipID); err != nil || !ok {
			return uuid.Nil, ErrNotMember
		}
	}
	if activityID != uuid.Nil {
		// the linked meeting/event must belong to THIS org — the FK only
		// proves existence (validated through the activities seam, Rule 26).
		if s.activityGate == nil || !s.activityGate(ctx, oc.OrgID, activityID) {
			return uuid.Nil, ErrNotFound
		}
	}
	var row db.VoiceRooms
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateVoiceRoom(ctx, db.CreateVoiceRoomParams{
			OrganizationID: toPg(oc.OrgID), ChannelID: toPg(channelID),
			ActivityID: toPg(activityID), Title: pgText(title),
			Provider: "livekit", RoomName: "oga9-" + uuid.NewString()[:12],
			StartedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoiceRoomStarted",
			"voice_room", fromPg(row.ID), oc.UserID, true,
			map[string]any{"room_id": fromPg(row.ID), "channel_id": channelID, "started_by": oc.MembershipID})
	})
	return fromPg(row.ID), err
}

// JoinRoom records the participant and returns the room name + a
// short-lived LiveKit access token (≤ join window, §24).
func (s *Service) JoinRoom(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) (roomName, token, providerURL string, err error) {
	if s.voice == nil || s.voice.apiKey == "" {
		return "", "", "", ErrVoiceUnavailable
	}
	room, err := s.q.GetVoiceRoom(ctx, db.GetVoiceRoomParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && room.State != "live") {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	// channel-spawned rooms restrict joiners to the channel membership;
	// activity/standalone rooms admit any org member.
	if room.ChannelID.Valid {
		if ok, err := s.q.IsChannelMember(ctx, db.IsChannelMemberParams{
			ID: room.ChannelID, ID_2: toPg(oc.MembershipID)}); err != nil || !ok {
			return "", "", "", ErrNotMember
		}
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		if err := q.JoinVoiceRoom(ctx, db.JoinVoiceRoomParams{
			RoomID: room.ID, MembershipID: toPg(oc.MembershipID)}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoiceParticipantJoined",
			"voice_room", rid, oc.UserID, false,
			map[string]any{"room_id": rid, "membership_id": oc.MembershipID})
	})
	if err != nil {
		return "", "", "", err
	}
	// LiveKit access token: iss=key, sub=identity, exp=join window, and the
	// video grant scoped to THIS room.
	win := time.Duration(s.voice.joinWindowSec) * time.Second
	if win <= 0 {
		win = 5 * time.Minute
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": s.voice.apiKey,
		"sub": oc.MembershipID.String(),
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(win).Unix(),
		"video": map[string]any{
			"roomJoin":     true,
			"room":         room.RoomName,
			"canPublish":   true,
			"canSubscribe": true,
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.voice.apiSecret))
	if err != nil {
		return "", "", "", err
	}
	return room.RoomName, tok, s.voice.url, nil
}

// EndRoom — starter or org.chat.moderate (handler decides). Emits
// VoiceRoomEnded with duration + participant count.
func (s *Service) EndRoom(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID, moderate bool) error {
	room, err := s.q.GetVoiceRoom(ctx, db.GetVoiceRoomParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if room.State != "live" {
		return ErrNotFound
	}
	if !moderate && fromPg(room.StartedBy) != oc.MembershipID {
		return ErrForbidden
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.EndVoiceRoom(ctx, db.EndVoiceRoomParams{ID: room.ID, OrganizationID: room.OrganizationID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		if err := q.EndVoiceRoomParticipants(ctx, room.ID); err != nil {
			return err
		}
		var parts int
		if pp, err := q.VoiceRoomParticipants(ctx, room.ID); err == nil {
			parts = len(pp)
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoiceRoomEnded",
			"voice_room", rid, oc.UserID, true,
			map[string]any{"room_id": rid,
				"duration_sec": int(time.Since(room.StartedAt.Time).Seconds()),
				"participants": parts})
	})
}

// LeaveRoom marks the participant's left_at and emits Left.
func (s *Service) LeaveRoom(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) error {
	if _, err := s.q.GetVoiceRoom(ctx, db.GetVoiceRoomParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return ErrNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.LeaveVoiceRoom(ctx, db.LeaveVoiceRoomParams{
			RoomID: toPg(rid), MembershipID: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return nil // not an open participant — nothing left to emit
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoiceParticipantLeft",
			"voice_room", rid, oc.UserID, false,
			map[string]any{"room_id": rid, "membership_id": oc.MembershipID})
	})
}

func (s *Service) LiveRooms(ctx context.Context, oc *tenant.OrgCtx) ([]db.VoiceRooms, error) {
	return s.q.LiveVoiceRooms(ctx, toPg(oc.OrgID))
}

// RoomSweep — voice:room_sweep: rooms abandoned 'live' past maxAgeHours
// (server crash, never ended) still accept joiners; close them and stamp
// every open participant row, emitting VoiceRoomEnded per room.
func (s *Service) RoomSweep(ctx context.Context, maxAgeHours int) error {
	rooms, err := s.q.StaleLiveVoiceRooms(ctx, int32(maxAgeHours))
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rooms {
		orgID := fromPg(r.OrganizationID)
		var tid uuid.UUID
		if t, err := s.q.GetOrgTenantID(ctx, r.OrganizationID); err == nil {
			tid = fromPg(t)
		}
		err := s.withTx(ctx, func(q *db.Queries) error {
			n, err := q.EndVoiceRoom(ctx, db.EndVoiceRoomParams{ID: r.ID, OrganizationID: r.OrganizationID})
			if err != nil || n == 0 {
				return err
			}
			if err := q.EndVoiceRoomParticipants(ctx, r.ID); err != nil {
				return err
			}
			return events.Emit(ctx, q, tid, orgID, "VoiceRoomEnded",
				"voice_room", fromPg(r.ID), uuid.Nil, true,
				map[string]any{"room_id": fromPg(r.ID), "reason": "sweep",
					"duration_sec": int(time.Since(r.StartedAt.Time).Seconds())})
		})
		if err != nil {
			// one bad room can't stall the sweep — collect and surface.
			errs = append(errs, fmt.Errorf("room %s: %w", uuid.UUID(r.ID.Bytes), err))
		}
	}
	return errors.Join(errs...)
}
