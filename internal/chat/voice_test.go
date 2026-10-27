// Ref: oga9-engineering/sections/24-realtime-chat-voice.md — voice room
// lifecycle: start → join (token) → end → participant log; orphan sweep.
package chat

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/tenant"
)

func TestVoiceRoomLifecycle(t *testing.T) {
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc := NewService(pool, nil)
	svc.SetVoiceProvider("https://rtc.test", "key", "secret", 60)
	ctx := context.Background()

	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid pgtype.UUID
	pool.QueryRow(ctx, `INSERT INTO tenants (name,slug,billing_email,status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"t-"+suffix, "t-"+suffix, "t-"+suffix+"@x.test").Scan(&tid)
	pool.QueryRow(ctx, `INSERT INTO organizations (tenant_id,name,slug,org_type) VALUES ($1,$2,$3,'association') RETURNING id`,
		tid, "o-"+suffix, "o-"+suffix).Scan(&oid)
	pool.QueryRow(ctx, `INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','u','active') RETURNING id`,
		"u-"+suffix+"@x.test").Scan(&uid)
	pool.QueryRow(ctx, `INSERT INTO organization_memberships (organization_id,user_id,category,lifecycle_state) VALUES ($1,$2,'regular','active') RETURNING id`,
		oid, uid).Scan(&mid)
	oc := &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes)}

	rid, err := svc.StartRoom(ctx, oc, "quick huddle", uuid.Nil, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	// join → token issued (spec test 5)
	roomName, token, url, err := svc.JoinRoom(ctx, oc, rid)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || roomName == "" || url != "https://rtc.test" {
		t.Fatalf("join: %q %q %q", roomName, token, url)
	}
	// end → room closes; participants logged
	if err := svc.EndRoom(ctx, oc, rid, false); err != nil {
		t.Fatal(err)
	}
	var state string
	pool.QueryRow(ctx, `SELECT state FROM voice_rooms WHERE id=$1`, rid).Scan(&state)
	if state != "ended" {
		t.Fatalf("state: %s", state)
	}
	var left bool
	pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM voice_room_participants WHERE room_id=$1 AND left_at IS NOT NULL)`, rid).Scan(&left)
	if !left {
		t.Fatal("participants not closed out on end")
	}
	// re-joining an ended room fails
	if _, _, _, err := svc.JoinRoom(ctx, oc, rid); err == nil {
		t.Fatal("join on ended room should fail")
	}
	// non-starter can't end (moderate=false → forbidden)
	oc2 := &tenant.OrgCtx{OrgID: oc.OrgID, TenantID: oc.TenantID, MembershipID: uuid.New()}
	rid2, _ := svc.StartRoom(ctx, oc, "r2", uuid.Nil, uuid.Nil)
	if err := svc.EndRoom(ctx, oc2, rid2, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-starter end: %v", err)
	}
	if err := svc.EndRoom(ctx, oc2, rid2, true); err != nil { // moderator ends
		t.Fatal(err)
	}
}

func TestRoomSweep(t *testing.T) {
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc := NewService(pool, nil)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid pgtype.UUID
	pool.QueryRow(ctx, `INSERT INTO tenants (name,slug,billing_email,status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"t-"+suffix, "t-"+suffix, "t-"+suffix+"@x.test").Scan(&tid)
	pool.QueryRow(ctx, `INSERT INTO organizations (tenant_id,name,slug,org_type) VALUES ($1,$2,$3,'association') RETURNING id`,
		tid, "o-"+suffix, "o-"+suffix).Scan(&oid)
	pool.QueryRow(ctx, `INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','u','active') RETURNING id`,
		"u-"+suffix+"@x.test").Scan(&uid)
	pool.QueryRow(ctx, `INSERT INTO organization_memberships (organization_id,user_id,category,lifecycle_state) VALUES ($1,$2,'regular','active') RETURNING id`,
		oid, uid).Scan(&mid)
	// orphaned 'live' room older than the max age → sweep ends it
	var rid pgtype.UUID
	pool.QueryRow(ctx, `INSERT INTO voice_rooms (organization_id, title, room_name, started_by, state, started_at)
		VALUES ($1,'stale',$2,$3,'live',now()-interval '25 hours') RETURNING id`,
		oid, "stale-"+suffix, mid).Scan(&rid)
	if err := svc.RoomSweep(ctx, 24); err != nil {
		t.Fatal(err)
	}
	var state string
	pool.QueryRow(ctx, `SELECT state FROM voice_rooms WHERE id=$1`, rid).Scan(&state)
	if state != "ended" {
		t.Fatalf("orphan room not swept: %s", state)
	}
}
