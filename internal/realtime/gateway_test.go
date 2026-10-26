package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

// injectUser fakes the session middleware — production runs ServeWS
// behind auth so UserID is always in ctx.
func injectUser(uid uuid.UUID, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), middleware.CtxUserID, uid)))
	}
}

func TestGatewaySubscribeFlow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://oga9:oga9@localhost:5434/oga9_test?sslmode=disable")
	if err != nil || pool.Ping(ctx) != nil {
		t.Skip("test db unavailable")
	}
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}})
	if err != nil {
		t.Skip("valkey unavailable")
	}
	orgSvc := tenant.NewService(pool, vk)
	chatSvc := chat.NewService(pool, vk)

	// org + owner + general channel
	var uid uuid.UUID
	pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name, status) VALUES ($1,'x','u','active')
		 ON CONFLICT (email) DO UPDATE SET email=$1 RETURNING id`,
		"ws-"+uuid.NewString()[:6]+"@t.co").Scan(&uid)
	org, err := orgSvc.CreateOrg(ctx, uid, "WS Org", "ws"+uuid.NewString()[:6], "union", "union", "NG", "Jos", "w@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := orgSvc.ResolveOrgCtx(ctx, uid, org.ID)
	cid, err := chatSvc.CreateChannel(ctx, oc, "general", "", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}

	gw := NewGateway(chatSvc, vk, "oga9.com")
	srv := httptest.NewServer(http.HandlerFunc(injectUser(uid, gw.ServeWS)))
	defer srv.Close()

	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	sub, _ := json.Marshal(map[string]string{
		"subscribe": cid.String(), "membership": oc.MembershipID.String()})
	if err := c.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	// subscribe → presence key lands
	time.Sleep(200 * time.Millisecond)
	cmd := vk.Do(ctx, vk.B().Get().Key("oga9:presence:"+oc.MembershipID.String()).Build())
	if v, err := cmd.ToString(); err != nil || v != "1" {
		t.Fatalf("presence key not set: %v %q", err, v)
	}

	// posting to the channel reaches the subscriber
	if _, err := chatSvc.PostMessage(ctx, oc, cid, "text", "hi", uuid.Nil, uuid.Nil, 0); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, data, err := c.Read(wctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		ChannelID uuid.UUID `json:"channel_id"`
		Body      string    `json:"body"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.ChannelID != cid || msg.Body != "hi" {
		t.Fatalf("unexpected frame: %s", data)
	}

	// subscribing as a different member is refused
	other, _ := json.Marshal(map[string]string{
		"subscribe": cid.String(), "membership": uuid.NewString()})
	c.Write(ctx, websocket.MessageText, other)
	_, frame, err := c.Read(wctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(frame) == "" || json.Unmarshal(frame, &struct {
		Error string `json:"error"`
	}{}) != nil {
		t.Fatal("expected not_a_channel_member error frame")
	}
	var ef struct {
		Error string `json:"error"`
	}
	json.Unmarshal(frame, &ef)
	if ef.Error != "not_a_channel_member" {
		t.Fatalf("expected not_a_channel_member, got %s", frame)
	}
}
