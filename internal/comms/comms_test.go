package comms

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tenant"
)

type env struct {
	svc  *Service
	chat *chat.Service
	org  *tenant.Service
	pool *pgxpool.Pool
}

func testEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil || pool.Ping(context.Background()) != nil {
		t.Skip("test db unavailable")
	}
	vk, _ := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}})
	s := NewService(pool)
	s.SetStore(storage.New(t.TempDir()))
	return &env{svc: s, chat: chat.NewService(pool, vk), org: tenant.NewService(pool, vk), pool: pool}
}

func mkUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO users (id,email,password_hash,display_name,status) VALUES ($1,$2,'x','T','active')", id, email); err != nil {
		t.Fatal(err)
	}
	return id
}

func setup(t *testing.T, e *env) (*tenant.OrgCtx, uuid.UUID, *tenant.OrgCtx) {
	ctx := context.Background()
	owner := mkUser(t, e.pool, "co"+uuid.NewString()[:8]+"@t.co")
	org, err := e.org.CreateOrg(ctx, owner, "C Org", "c"+uuid.NewString()[:6], "union", "union", "NG", "Jos", "c@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := e.org.ResolveOrgCtx(ctx, owner, org.ID)
	// second member for audience/prefs checks — the invite is
	// email-bound: it must target the acceptor's address.
	email2 := "cm" + uuid.NewString()[:8] + "@t.co"
	inv, err := e.org.CreateInvitation(ctx, oc, email2, "", "", "floor")
	if err != nil {
		t.Fatal(err)
	}
	u3 := mkUser(t, e.pool, email2)
	if _, err := e.org.AcceptInvitation(ctx, u3, inv.Token); err != nil {
		t.Fatal(err)
	}
	oc2, _ := e.org.ResolveOrgCtx(ctx, u3, org.ID)
	e.pool.Exec(ctx, "UPDATE organization_memberships SET lifecycle_state='active' WHERE id IN (SELECT id FROM organization_memberships WHERE organization_id=$1)", org.ID)
	return oc, org.ID, oc2
}

func TestAnnouncementFlow(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	oc, orgID, oc2 := setup(t, e)

	// draft emits nothing; publish emits + dispatches to the audience
	aid, err := e.svc.Publish(ctx, oc, "AGM Notice", "Sat 10am", Audience{All: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	var st string
	e.pool.QueryRow(ctx, "SELECT state FROM announcements WHERE id=$1", aid).Scan(&st)
	if st != "draft" {
		t.Fatalf("draft, got %s", st)
	}
	aid, err = e.svc.Publish(ctx, oc, "AGM Notice", "Sat 10am", Audience{All: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	emails, err := e.svc.DispatchAnnouncement(ctx, aid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 {
		t.Fatalf("expected 2 email recipients (default-on pref), got %d", len(emails))
	}
	// every active member got an in_app row (owner + member); each opted-in
	// member also has a channel='email' row tracking delivered_at.
	var n int
	e.pool.QueryRow(ctx, "SELECT count(*) FROM notifications WHERE organization_id=$1 AND kind='announcement' AND channel='in_app'", orgID).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 in_app notifications, got %d", n)
	}
	var ne int
	e.pool.QueryRow(ctx, "SELECT count(*) FROM notifications WHERE organization_id=$1 AND kind='announcement' AND channel='email'", orgID).Scan(&ne)
	if ne != 2 {
		t.Fatalf("expected 2 email tracking rows, got %d", ne)
	}
	// prefs gate: member2 disables BOTH channels → redispatch adds no
	// in_app row for them and collects no email for them either.
	if err := e.svc.SetPreference(ctx, oc2.UserID, "announcement", "in_app", false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetPreference(ctx, oc2.UserID, "announcement", "email", false); err != nil {
		t.Fatal(err)
	}
	aid2, _ := e.svc.Publish(ctx, oc, "Follow-up", "x", Audience{All: true}, false)
	emails2, err := e.svc.DispatchAnnouncement(ctx, aid2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails2) != 1 {
		t.Fatalf("only the owner should want the email channel, got %d", len(emails2))
	}
	list, _ := e.svc.MyNotifications(ctx, oc2.UserID, false)
	for _, nt := range list {
		if nt.Title == "Follow-up" {
			t.Fatal("preference-off member should not receive the second announcement")
		}
	}
	// read + read-event; retract CAS
	var nid uuid.UUID
	e.pool.QueryRow(ctx, "SELECT id FROM notifications WHERE user_id=$1 AND kind='announcement' LIMIT 1", oc2.UserID).Scan(&nid)
	if err := e.svc.MarkRead(ctx, oc2.UserID, nid); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Retract(ctx, oc, aid); err != nil {
		t.Fatal(err)
	}
	e.pool.QueryRow(ctx, "SELECT state FROM announcements WHERE id=$1", aid).Scan(&st)
	if st != "retracted" {
		t.Fatalf("expected retracted, got %s", st)
	}
}

func TestDocumentFlow(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	oc, _, _ := setup(t, e)

	did, err := e.svc.Upload(ctx, oc, "policy", "Bylaws", "public", uuid.Nil,
		strings.NewReader("policy body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ProcessUpload(ctx, did); err != nil {
		t.Fatal(err)
	}
	rc, d, err := e.svc.Download(ctx, oc, did, false)
	if err != nil || d.Title != "Bylaws" {
		t.Fatalf("download: %v", err)
	}
	rc.Close()
	// public listing sees it — addressed by slug, and only while the
	// org is directory-opted-in (§03 opt-in honored by the docs surface)
	var slug string
	e.pool.QueryRow(ctx, "SELECT slug FROM organizations WHERE id=$1", d.OrganizationID).Scan(&slug)
	if none, _ := e.svc.PublicDocs(ctx, slug); len(none) != 0 {
		t.Fatal("non-listed org must not expose a public docs listing")
	}
	e.pool.Exec(ctx, "UPDATE organizations SET is_public_listed=true WHERE id=$1", d.OrganizationID)
	pub, err := e.svc.PublicDocs(ctx, slug)
	if err != nil || len(pub) != 1 {
		t.Fatalf("public docs: %v %d", err, len(pub))
	}
	// executable is poisoned
	bad, _ := e.svc.Upload(ctx, oc, "archive", "x", "members", uuid.Nil,
		strings.NewReader("#!/bin/sh\nrm -rf /"))
	if err := e.svc.ProcessUpload(ctx, bad); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	e.pool.QueryRow(ctx, "SELECT deleted_at IS NOT NULL FROM documents WHERE id=$1", bad).Scan(&deleted)
	if !deleted {
		t.Fatal("executable upload should have been soft-deleted")
	}
	// version bump
	ver, err := e.svc.NewVersion(ctx, oc, did, strings.NewReader("v2"))
	if err != nil || ver != 2 {
		t.Fatalf("version: %v %d", err, ver)
	}
	if err := e.svc.Archive(ctx, oc, did); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Upload(ctx, oc, "nonsense", "x", "members", uuid.Nil, strings.NewReader("x")); err != ErrBadValue {
		t.Fatalf("bad kind should fail, got %v", err)
	}
}

func TestChatFlow(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	oc, orgID, oc2 := setup(t, e)

	// general channel auto-created + everyone sees it (derived membership)
	cid, err := e.chat.CreateChannel(ctx, oc, "general", "", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	var chCount int
	e.pool.QueryRow(ctx, "SELECT count(*) FROM chat_channels WHERE organization_id=$1", orgID).Scan(&chCount)
	chans, err := e.chat.Channels(ctx, oc2)
	if err != nil || len(chans) == 0 {
		t.Fatal("member should see the general channel")
	}
	mid, err := e.chat.PostMessage(ctx, oc2, cid, "text", "hello", uuid.Nil, uuid.Nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := e.chat.Messages(ctx, oc, cid, nil)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages: %v %d", err, len(msgs))
	}
	// edit own within window
	if err := e.chat.EditMessage(ctx, oc2, cid, mid, "edited"); err != nil {
		t.Fatal(err)
	}
	// non-owner cannot edit or delete
	if err := e.chat.EditMessage(ctx, oc, cid, mid, "hijack"); err != chat.ErrForbidden {
		t.Fatalf("non-owner edit should fail, got %v", err)
	}
	if err := e.chat.Delete(ctx, oc, cid, mid, false); err != chat.ErrForbidden {
		t.Fatalf("non-owner delete should fail, got %v", err)
	}
	if err := e.chat.MarkRead(ctx, oc, cid, mid); err != nil {
		t.Fatal(err)
	}
	// moderator path works
	if err := e.chat.Delete(ctx, oc, cid, mid, true); err != nil {
		t.Fatal(err)
	}
	// read cursor must point at a live message in the channel — a deleted
	// (or foreign-channel) id is refused now.
	if err := e.chat.MarkRead(ctx, oc, cid, mid); err != chat.ErrNotFound {
		t.Fatalf("deleted-message cursor should fail, got %v", err)
	}
	// non-member custom channel is invisible
	customID, _ := e.chat.CreateChannel(ctx, oc, "custom", "admins", uuid.Nil)
	if ok := e.chat.CanSubscribe(ctx, customID, oc2.MembershipID, oc2.UserID); ok {
		t.Fatal("non-member should not subscribe to a custom channel")
	}
	// reply must reference a message in the same channel
	if _, err := e.chat.PostMessage(ctx, oc, customID, "text", "reply",
		uuid.Nil, uuid.New(), 0); err != chat.ErrBadValue {
		t.Fatalf("bogus reply_to should fail, got %v", err)
	}
	// dm caps at two members
	dmID, _ := e.chat.CreateChannel(ctx, oc, "dm", "pair", uuid.Nil)
	e.chat.AddMember(ctx, oc, dmID, oc2.MembershipID, "member")
	var third uuid.UUID
	e.pool.QueryRow(ctx,
		`INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','t','active')
		 RETURNING id`, "dm3"+uuid.NewString()[:8]+"@t.co").Scan(&third)
	inv3, _ := e.org.CreateInvitation(ctx, oc, "dm3x"+uuid.NewString()[:8]+"@t.co", "", "", "floor")
	u3 := mkUser(t, e.pool, inv3.Email)
	e.org.AcceptInvitation(ctx, u3, inv3.Token)
	oc3, _ := e.org.ResolveOrgCtx(ctx, u3, oc.OrgID)
	if err := e.chat.AddMember(ctx, oc, dmID, oc3.MembershipID, "member"); err != chat.ErrBadValue {
		t.Fatalf("third dm member should be refused, got %v", err)
	}
}

// TestDocumentVersionsV1 — §14: prior versions stay retrievable. The v1
// row is written at upload so a v2 bump can't orphan the original blob.
func TestDocumentVersionsV1(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	oc, _, _ := setup(t, e)

	did, err := e.svc.Upload(ctx, oc, "report", "v1", "members", uuid.Nil,
		strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	ver, err := e.svc.NewVersion(ctx, oc, did, strings.NewReader("replacement"))
	if err != nil || ver != 2 {
		t.Fatalf("new version: %d %v", ver, err)
	}
	var v1key, headkey string
	if err := e.pool.QueryRow(ctx,
		"SELECT storage_key FROM document_versions WHERE document_id=$1 AND version=1",
		did).Scan(&v1key); err != nil {
		t.Fatalf("v1 row missing — original blob orphaned: %v", err)
	}
	e.pool.QueryRow(ctx, "SELECT storage_key FROM documents WHERE id=$1", did).Scan(&headkey)
	if headkey == v1key {
		t.Fatal("head still points at v1 key after bump")
	}
	// a poisoned v3 drops ONLY that version — head reverts to v2
	v3 := "#!/bin/sh\necho bad"
	if _, err := e.svc.NewVersion(ctx, oc, did, strings.NewReader(v3)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ProcessUpload(ctx, did); err != nil {
		t.Fatal(err)
	}
	var v int
	var alive bool
	e.pool.QueryRow(ctx,
		"SELECT version, deleted_at IS NULL FROM documents WHERE id=$1", did).Scan(&v, &alive)
	if v != 2 || !alive {
		t.Fatalf("poisoned v3 must drop only the version row (doc alive at v2), got v%d alive=%v", v, alive)
	}
}

// TestSecondDMChannel — the 000157 unique-index regression: a second dm
// channel must create its own row, never resolve to the existing one.
func TestSecondDMChannel(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	oc, _, _ := setup(t, e)

	dm1, err := e.chat.CreateChannel(ctx, oc, "dm", "pair-1", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	dm2, err := e.chat.CreateChannel(ctx, oc, "dm", "pair-2", uuid.Nil)
	if err != nil {
		t.Fatalf("second dm channel must create, got %v", err)
	}
	if dm1 == dm2 {
		t.Fatal("second dm resolved to the FIRST channel — unique index collapse")
	}
	custom2, err := e.chat.CreateChannel(ctx, oc, "custom", "admins-2", uuid.Nil)
	if err != nil {
		t.Fatalf("second custom channel must create, got %v", err)
	}
	if custom2 == uuid.Nil {
		t.Fatal("nil custom channel id")
	}
	// derived kinds still dedupe
	gen1, _ := e.chat.CreateChannel(ctx, oc, "general", "", uuid.Nil)
	gen2, _ := e.chat.CreateChannel(ctx, oc, "general", "", uuid.Nil)
	if gen1 != gen2 {
		t.Fatal("general channel must dedupe to one row")
	}
}
