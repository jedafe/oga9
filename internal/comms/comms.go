// Ref: oga9-engineering/sections/14-comms-documents.md — announcements +
// notifications. Announcement publish is a fan-out (Rule 8): emit the
// event in-tx (Rule 12), enqueue notifications:dispatch, return — the
// worker resolves the audience snapshot and inserts in_app rows gated
// by notification_preferences.
package comms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound = errors.New("comms: not_found")
	ErrBadValue = errors.New("comms: invalid_value")
	ErrTooLarge = errors.New("comms: upload_too_large")
)

type Service struct {
	pool       *pgxpool.Pool
	q          *db.Queries
	store      *storage.Store
	limitFn    func(ctx context.Context, tenantID uuid.UUID, key string) int64
	fallbackMB int64 // used when no plan limit resolves (dev/new plans)
	// Rule-26 read seams — comms never reads activities/finance tables;
	// the owning modules answer through these (wired in main).
	activityInfo func(ctx context.Context, activityID uuid.UUID) (orgID uuid.UUID, title string, unitID uuid.UUID, startsAt time.Time, ok bool)
	paymentInfo  func(ctx context.Context, paymentID uuid.UUID) (membershipID uuid.UUID, amountMinor int64, currency string, ok bool)
}

// SetActivityInfo wires the activities snapshot seam (Rule 26).
func (s *Service) SetActivityInfo(fn func(ctx context.Context, activityID uuid.UUID) (orgID uuid.UUID, title string, unitID uuid.UUID, startsAt time.Time, ok bool)) {
	s.activityInfo = fn
}

// SetPaymentInfo wires the finance snapshot seam (Rule 26).
func (s *Service) SetPaymentInfo(fn func(ctx context.Context, paymentID uuid.UUID) (membershipID uuid.UUID, amountMinor int64, currency string, ok bool)) {
	s.paymentInfo = fn
}

// SetLimitFn wires the entitlement-limit lookup (Rule 25) — upload size
// caps come from limit.storage.mb, never a constant.
func (s *Service) SetLimitFn(fn func(ctx context.Context, tenantID uuid.UUID, key string) int64) {
	s.limitFn = fn
}

// SetUploadFallbackMB — the cap when limit.storage.mb can't resolve
// (dev DB, new plan not yet seeded). Still env-driven (Rule 3).
func (s *Service) SetUploadFallbackMB(mb int64) { s.fallbackMB = mb }

// Kinds — canonical notification kinds (§14 "seeded kinds"). Producers
// emit these names; /me/preferences gates on kind+channel. `chat_message`
// is an extra §24 kind beyond the seeded list.
var Kinds = map[string]bool{
	"announcement":       true, // §14 announcement fan-out
	"meeting_reminder":   true, // §13 MeetingReminderDue
	"dues_reminder":      true, // §12 DuesDefaulterFlagged
	"payment_receipt":    true, // §12 ReceiptIssued
	"application_update": true,
	"welfare_update":     true,
	"action_item":        true, // §13 ActionItemOverdue
	"invitation":         true,
	"birthday":           true,
	"minutes_approved":   true, // §13 MinutesApproved
	"term_notice":        true,
	"subscription":       true,
	"platform":           true,
	"chat_message":       true, // §24 channel message (muted channels skip)
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

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

// Audience mirrors announcements.audience: {"all":true} or category/unit
// filters resolved to a member snapshot at dispatch.
type Audience struct {
	All        bool        `json:"all"`
	Categories []string    `json:"categories"`
	UnitIDs    []uuid.UUID `json:"unit_ids"`
	Positions  []string    `json:"positions"` // org_positions slugs — §14 audience spec
}

// Publish inserts the announcement (draft|published) and emits
// AnnouncementPublished when live; the handler fans out dispatch.
func (s *Service) Publish(ctx context.Context, oc *tenant.OrgCtx, title, body string, aud Audience, draft bool) (uuid.UUID, error) {
	if title == "" || body == "" {
		return uuid.Nil, ErrBadValue
	}
	if !aud.All && len(aud.Categories) == 0 && len(aud.UnitIDs) == 0 && len(aud.Positions) == 0 {
		aud.All = true // empty filter = everyone
	}
	aj, _ := json.Marshal(aud)
	state := "published"
	if draft {
		state = "draft"
	}
	var row db.Announcements
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateAnnouncement(ctx, db.CreateAnnouncementParams{
			OrganizationID: toPg(oc.OrgID), AuthorID: toPg(oc.MembershipID),
			Title: title, Body: body, Audience: aj, State: state})
		if err != nil {
			return err
		}
		if draft {
			return nil
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AnnouncementPublished",
			"announcement", fromPg(row.ID), oc.UserID, true,
			map[string]any{"announcement_id": fromPg(row.ID), "org_id": oc.OrgID})
	})
	return fromPg(row.ID), err
}

func (s *Service) List(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Announcements, error) {
	return s.q.ListAnnouncements(ctx, db.ListAnnouncementsParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

// PublishDraft moves a draft announcement to published — the AnnouncementPublished
// event + dispatch fan-out fire exactly as an immediate publish would.
func (s *Service) PublishDraft(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		a, err := q.PublishAnnouncement(ctx, db.PublishAnnouncementParams{ID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound // not a draft of this org (or already live)
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AnnouncementPublished",
			"announcement", fromPg(a.ID), oc.UserID, true,
			map[string]any{"announcement_id": fromPg(a.ID), "org_id": oc.OrgID})
	})
}

func (s *Service) Retract(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		a, err := q.RetractAnnouncement(ctx, db.RetractAnnouncementParams{ID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AnnouncementRetracted",
			"announcement", fromPg(a.ID), oc.UserID, true,
			map[string]any{"announcement_id": fromPg(a.ID)})
	})
}

// ---------- notifications (/me surface) ----------

func (s *Service) MyNotifications(ctx context.Context, userID uuid.UUID, unreadOnly bool) ([]db.Notifications, error) {
	return s.q.MyNotifications(ctx, db.MyNotificationsParams{UserID: toPg(userID), Column2: unreadOnly})
}

func (s *Service) MarkRead(ctx context.Context, userID, nid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.MarkNotificationRead(ctx, db.MarkNotificationReadParams{ID: toPg(nid), UserID: toPg(userID)})
		if err != nil {
			return err
		}
		if n == 0 {
			// already-read is a replay, not a missing row — stay idempotent.
			if _, err := q.GetNotification(ctx, db.GetNotificationParams{ID: toPg(nid), UserID: toPg(userID)}); err != nil {
				return ErrNotFound
			}
			return nil
		}
		// NotificationRead is an analytics event (catalog §14) — carry the
		// row's org so per-org reporting joins correctly.
		var oid uuid.UUID
		if row, err := q.GetNotification(ctx, db.GetNotificationParams{ID: toPg(nid), UserID: toPg(userID)}); err == nil {
			oid = fromPg(row.OrganizationID)
		}
		var tid uuid.UUID
		if t, err := q.GetOrgTenantID(ctx, toPg(oid)); err == nil {
			tid = fromPg(t)
		}
		return events.Emit(ctx, q, tid, oid, "NotificationRead",
			"notification", nid, userID, false, map[string]any{"notification_id": nid})
	})
}

func (s *Service) MyPreferences(ctx context.Context, userID uuid.UUID) ([]db.NotificationPreferences, error) {
	return s.q.MyPreferences(ctx, toPg(userID))
}

func (s *Service) SetPreference(ctx context.Context, userID uuid.UUID, kind, channel string, enabled bool) error {
	if kind == "" || (channel != "in_app" && channel != "email" && channel != "push") {
		return ErrBadValue
	}
	return s.q.UpsertPreference(ctx, db.UpsertPreferenceParams{
		UserID: toPg(userID), Kind: kind, Channel: channel, Enabled: enabled})
}

// EmailTarget is an opted-in email recipient — the notification row id
// lets the sender stamp delivered_at.
type EmailTarget struct {
	NotificationID uuid.UUID
	To             string
}

// DispatchAnnouncement is the notifications:dispatch worker body: resolve
// the audience snapshot, gate per-user on preferences, insert in_app rows
// (deduped — a retried fan-out never double-notifies). allowEmail is the
// publisher's org.comms.broadcast grant recorded at publish time — the
// email channel fans out only for broadcast-capable publishers (§14 node).
// Returns the notification-id+address pairs the worker fans out as
// notifications:email tasks (Rule 8).
func (s *Service) DispatchAnnouncement(ctx context.Context, announcementID uuid.UUID, allowEmail bool) ([]EmailTarget, error) {
	a, err := s.q.GetAnnouncement(ctx, toPg(announcementID))
	if err != nil {
		return nil, ErrNotFound
	}
	if a.State != "published" {
		// retracted (or re-drafted) after enqueue — the fan-out must not
		// deliver a dead announcement.
		return nil, nil
	}
	var aud Audience
	_ = json.Unmarshal(a.Audience, &aud)
	units := make([]pgtype.UUID, len(aud.UnitIDs))
	for i, u := range aud.UnitIDs {
		units[i] = toPg(u)
	}
	users, err := s.q.AudienceRecipients(ctx, db.AudienceRecipientsParams{
		OrganizationID: a.OrganizationID, AllMembers: aud.All,
		Categories: aud.Categories, UnitIds: units, Positions: aud.Positions})
	if err != nil {
		return nil, err
	}
	userIDs := make([]pgtype.UUID, len(users))
	for i, r := range users {
		userIDs[i] = r.UserID
	}
	// batch the per-recipient lookups — 2 queries, not 5·N inside the tx.
	prefs, err := s.q.NotifPrefsForUsers(ctx, db.NotifPrefsForUsersParams{
		Kind: "announcement", Column2: userIDs})
	if err != nil {
		return nil, err
	}
	existing, err := s.q.NotifExistingByRef(ctx, db.NotifExistingByRefParams{
		Kind: "announcement", RefID: a.ID, Column3: userIDs})
	if err != nil {
		return nil, err
	}
	// absent pref row = enabled (NotificationWanted semantics).
	prefOn := map[[2]string]bool{}
	for _, p := range prefs {
		key := [2]string{uuid.UUID(p.UserID.Bytes).String(), p.Channel}
		prefOn[key] = p.Enabled
	}
	haveInApp := map[string]bool{}
	var emailRow map[string]db.NotifExistingByRefRow = map[string]db.NotifExistingByRefRow{}
	for _, n := range existing {
		uid := uuid.UUID(n.UserID.Bytes).String()
		if n.Channel == "in_app" {
			haveInApp[uid] = true
		} else if n.Channel == "email" {
			emailRow[uid] = n
		}
	}
	tenantID, err := func() (uuid.UUID, error) {
		t, err := s.q.GetOrgTenantID(ctx, a.OrganizationID)
		return fromPg(t), err
	}()
	if err != nil {
		return nil, err
	}
	emails := make([]EmailTarget, 0, len(users))
	err = s.withTx(ctx, func(q *db.Queries) error {
		for _, r := range users {
			uid := uuid.UUID(r.UserID.Bytes).String()
			if off, set := prefOn[[2]string{uid, "in_app"}]; set && !off {
				// in_app off — email may still be on; handled below
			} else if !haveInApp[uid] {
				if _, err := q.CreateNotification(ctx, db.CreateNotificationParams{
					UserID: r.UserID, OrganizationID: a.OrganizationID, Kind: "announcement",
					Title: a.Title, Body: pgtype.Text{String: a.Body, Valid: true},
					RefType: pgtype.Text{String: "announcement", Valid: true},
					RefID:   a.ID, Channel: "in_app"}); err != nil {
					return err
				}
			}
			if !allowEmail {
				continue // announce != broadcast: in_app only (§14 nodes)
			}
			// email channel is a separate preference — opted-in members get
			// a notifications:email fan-out (delivered_at set by the sender).
			if off, set := prefOn[[2]string{uid, "email"}]; set && !off || r.Email == "" {
				continue
			}
			// a channel='email' row tracks delivery — delivered_at is
			// stamped by the notifications:email worker after send.
			if row, ok := emailRow[uid]; ok {
				if !row.DeliveredAt.Valid {
					// row exists but the email task never landed/succeeded —
					// re-enqueue with the REAL id so delivery still tracks.
					emails = append(emails, EmailTarget{NotificationID: fromPg(row.ID), To: r.Email})
				}
				continue
			}
			row, err := q.CreateNotification(ctx, db.CreateNotificationParams{
				UserID: r.UserID, OrganizationID: a.OrganizationID, Kind: "announcement",
				Title: a.Title, Body: pgtype.Text{String: a.Body, Valid: true},
				RefType: pgtype.Text{String: "announcement", Valid: true},
				RefID:   a.ID, Channel: "email"})
			if err != nil {
				return err
			}
			emails = append(emails, EmailTarget{NotificationID: fromPg(row.ID), To: r.Email})
		}
		return events.Emit(ctx, q, tenantID, fromPg(a.OrganizationID), "BroadcastQueued",
			"announcement", fromPg(a.ID), uuid.Nil, false,
			map[string]any{"announcement_id": fromPg(a.ID), "recipients": len(users)})
	})
	return emails, err
}

// StaleEmailRefs — channel='email' rows that never reached delivered_at
// past the grace window (enqueue loss / worker died between commit and
// fan-out). The redrive sweep re-enqueues them; the row is durable so
// nothing is lost (Rule 27).
func (s *Service) StaleEmailRefs(ctx context.Context) ([]db.StaleEmailNotificationsRow, error) {
	return s.q.StaleEmailNotifications(ctx)
}

func textPtr(s *string) pgtype.Text { // nil → invalid (keep existing)
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// EmailSubject/EmailBody render the announcement for the email channel —
// the worker calls these once per fan-out batch.
func (s *Service) EmailSubject(ctx context.Context, announcementID uuid.UUID) string {
	if a, err := s.q.GetAnnouncement(ctx, toPg(announcementID)); err == nil {
		return a.Title
	}
	return ""
}

func (s *Service) EmailBody(ctx context.Context, announcementID uuid.UUID) string {
	if a, err := s.q.GetAnnouncement(ctx, toPg(announcementID)); err == nil {
		return a.Body
	}
	return ""
}

// ---------- domain-event consumers (Rule 27: idempotent) ----------

// MinutesArchivedConsumer — §13/§14: MinutesApproved archives the approved
// body as a documents row (kind=minutes, members visibility) and emits
// MinutesArchived. Payload-driven: the event carries body/title/unit —
// this module never reads activities tables (Rule 26).
func (s *Service) MinutesArchivedConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "MinutesApproved" {
		return nil
	}
	if s.store == nil {
		// a nil store is a wiring bug — fail loudly (retry→DLQ) rather
		// than silently dropping approved minutes.
		return errors.New("comms: store not wired")
	}
	mid := e.AggregateID
	if v, ok := e.Payload["minutes_id"].(string); ok {
		if id, err := uuid.Parse(v); err == nil {
			mid = id
		}
	}
	body, _ := e.Payload["body"].(string)
	if body == "" {
		return nil // pre-enrichment events carry no body — nothing to archive
	}
	meetingTitle, _ := e.Payload["meeting_title"].(string)
	var unit, approver pgtype.UUID
	if v, ok := e.Payload["unit_id"].(string); ok {
		if id, err := uuid.Parse(v); err == nil {
			unit = toPg(id)
		}
	}
	if v, ok := e.Payload["approved_by"].(string); ok {
		if id, err := uuid.Parse(v); err == nil {
			approver = toPg(id)
		}
	}
	day := time.Now()
	if v, ok := e.Payload["starts_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			day = t
		}
	}
	key := uuid.NewString()
	if err := s.store.Put(ctx, key, strings.NewReader(body)); err != nil {
		return err
	}
	title := fmt.Sprintf("Minutes — %s (%s)", meetingTitle, day.Format("2006-01-02"))
	row, err := q.CreateDocument(ctx, db.CreateDocumentParams{
		OrganizationID: toPg(e.OrganizationID), Kind: "minutes", Title: title,
		StorageKey: key, Visibility: "members",
		UnitID: unit, UploadedBy: approver})
	if err != nil {
		return err
	}
	if err := q.InsertVersionRow(ctx, db.InsertVersionRowParams{
		DocumentID: row.ID, Version: 1, StorageKey: key, CreatedBy: approver}); err != nil {
		return err
	}
	return events.Emit(ctx, q, e.TenantID, e.OrganizationID, "MinutesArchived",
		"minutes", mid, e.ActorID, false,
		map[string]any{"minutes_id": mid, "meeting_id": e.Payload["meeting_id"],
			"document_id": fromPg(row.ID)})
}

// notifyOnce inserts an in_app notification for one user, gated on
// preferences and deduped on (user,kind,ref_id) — at-least-once replays
// never double-notify. Emits NotificationQueued on insert.
func (s *Service) notifyOnce(ctx context.Context, q *db.Queries, e events.Event, userID uuid.UUID, kind, title, body, refType string, refID pgtype.UUID) error {
	want, err := q.NotificationWanted(ctx, db.NotificationWantedParams{
		UserID: toPg(userID), Kind: kind, Channel: "in_app"})
	if err != nil {
		return err
	}
	if b, isBool := want.(bool); isBool && !b {
		return nil
	}
	exists, err := q.NotificationExists(ctx, db.NotificationExistsParams{
		UserID: toPg(userID), Kind: kind, RefID: refID, Channel: "in_app"})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	row, err := q.CreateNotification(ctx, db.CreateNotificationParams{
		UserID: toPg(userID), OrganizationID: toPg(e.OrganizationID), Kind: kind,
		Title: title, Body: pgtype.Text{String: body, Valid: body != ""},
		RefType: pgtype.Text{String: refType, Valid: true},
		RefID:   refID, Channel: "in_app"})
	if err != nil {
		return err
	}
	return events.Emit(ctx, q, e.TenantID, e.OrganizationID, "NotificationQueued",
		"notification", fromPg(row.ID), e.ActorID, false,
		map[string]any{"notification_id": fromPg(row.ID), "kind": kind,
			"user_id": userID, "source_event": e.Type})
}

// memberUser resolves the membership in an event payload key to its user.
func memberUser(ctx context.Context, q *db.Queries, e events.Event, keys ...string) (db.GetMembershipByIDRow, bool) {
	for _, k := range keys {
		if v, ok := e.Payload[k].(string); ok {
			if mid, err := uuid.Parse(v); err == nil {
				if m, err := q.GetMembershipByID(ctx, toPg(mid)); err == nil {
					return m, true
				}
			}
		}
	}
	return db.GetMembershipByIDRow{}, false
}

// EventNotifyConsumer — member-targeted domain events become in_app
// notifications gated by notification_preferences (seeded §14 kinds);
// dedupe is per (user,kind,ref) where ref is the triggering entity — or
// the event id when the trigger has no stable row (a re-flagged defaulter
// must notify again, not dedupe against the first flag forever).
func (s *Service) EventNotifyConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	switch e.Type {
	case "ActionItemOverdue":
		m, ok := memberUser(ctx, q, e, "assignee_id")
		if !ok {
			return nil
		}
		var ref pgtype.UUID
		if v, ok := e.Payload["item_id"].(string); ok {
			if id, err := uuid.Parse(v); err == nil {
				ref = toPg(id)
			}
		}
		if !ref.Valid {
			ref = m.ID
		}
		return s.notifyOnce(ctx, q, e, fromPg(m.UserID), "action_item",
			"Action item overdue", "", "action_item", ref)
	case "DuesDefaulterFlagged":
		m, ok := memberUser(ctx, q, e, "membership_id")
		if !ok {
			return nil
		}
		body := ""
		if c, ok := e.Payload["count"].(float64); ok {
			body = fmt.Sprintf("%d obligation(s) overdue", int(c))
		}
		// ref = event id: each good->poor transition notifies once.
		return s.notifyOnce(ctx, q, e, fromPg(m.UserID), "dues_reminder",
			"Dues in arrears", body, "organization_membership", toPg(e.ID))
	case "MeetingReminderDue":
		// fan out to the meeting's audience (unit-scoped or org-wide).
		v, ok := e.Payload["activity_id"].(string)
		if !ok || s.activityInfo == nil {
			return nil
		}
		aid, err := uuid.Parse(v)
		if err != nil {
			return nil
		}
		orgID, title, unitID, startsAt, ok := s.activityInfo(ctx, aid)
		if !ok {
			return nil
		}
		users, err := q.MeetingAudienceUserIDs(ctx, db.MeetingAudienceUserIDsParams{
			OrganizationID: toPg(orgID), Column2: toPg(unitID)})
		if err != nil {
			return err
		}
		body := fmt.Sprintf("%s starts %s", title, startsAt.Format("2006-01-02 15:04"))
		for _, uid := range users {
			// ref = event id: a rescheduled meeting re-arms the reminder and
			// its new event must notify again (activity-ref would suppress it).
			if err := s.notifyOnce(ctx, q, e, fromPg(uid), "meeting_reminder",
				"Meeting reminder", body, "activity", toPg(e.ID)); err != nil {
				return err
			}
		}
		return nil
	case "ReceiptIssued":
		v, ok := e.Payload["payment_id"].(string)
		if !ok || s.paymentInfo == nil {
			return nil
		}
		pid, err := uuid.Parse(v)
		if err != nil {
			return nil
		}
		memberID, amount, currency, ok := s.paymentInfo(ctx, pid)
		if !ok {
			return nil // payment row gone
		}
		m, err := q.GetMembershipByID(ctx, toPg(memberID))
		if err != nil {
			return nil
		}
		return s.notifyOnce(ctx, q, e, fromPg(m.UserID), "payment_receipt",
			"Payment receipt issued",
			fmt.Sprintf("%d %s confirmed", amount, currency),
			"payment", toPg(pid))
	case "MinutesApproved":
		// approved minutes are org-visible governance — notify the roster.
		var ref pgtype.UUID
		if v, ok := e.Payload["minutes_id"].(string); ok {
			if id, err := uuid.Parse(v); err == nil {
				ref = toPg(id)
			}
		}
		users, err := q.MeetingAudienceUserIDs(ctx, db.MeetingAudienceUserIDsParams{
			OrganizationID: toPg(e.OrganizationID), Column2: pgtype.UUID{}})
		if err != nil {
			return err
		}
		for _, uid := range users {
			if err := s.notifyOnce(ctx, q, e, fromPg(uid), "minutes_approved",
				"Minutes approved", "", "minutes", ref); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}
