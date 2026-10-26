// Ref: oga9-engineering/sections/24-realtime-chat-voice.md — /ws gateway.
// Session-authed upgrade; client subscribes per channel, membership is
// checked through the chat service (Rule 26); Valkey pub/sub carries the
// fan-out across instances. Presence/typing stay ephemeral (§24).
package realtime

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/middleware"
)

type Gateway struct {
	chat    *chat.Service
	vk      valkey.Client
	origins []string
}

// NewGateway — origins are the allowed WS Origin patterns (base domain +
// subdomains); without them any site could ride a member's session.
// devOrigins adds localhost/127.0.0.1 — only for non-prod deployments.
func NewGateway(c *chat.Service, vk valkey.Client, baseDomain string, devOrigins bool) *Gateway {
	origins := []string{baseDomain, "*." + baseDomain}
	if devOrigins {
		origins = append(origins, "localhost", "localhost:*", "127.0.0.1", "127.0.0.1:*")
	}
	return &Gateway{chat: c, vk: vk, origins: origins}
}

// ServeWS — GET /ws: upgrade, then accept {"subscribe":"<channel_uuid>",
// "membership":"<membership_uuid>"} control frames; forwards chat bus
// messages for subscribed channels.
func (g *Gateway) ServeWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: g.origins})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	userID := middleware.UserID(ctx) // session middleware ran before upgrade

	subs := map[uuid.UUID]uuid.UUID{} // channel → membership the client proved
	var subsMu sync.Mutex
	// websocket.Conn is NOT safe for concurrent writers — the subscribe
	// goroutine (error frames) and the pub/sub callback (fan-out) both
	// write, so serialize every write through one mutex.
	var writeMu sync.Mutex
	writeMsg := func(msg []byte) error {
		wctx, wc := context.WithTimeout(ctx, 5*time.Second)
		defer wc()
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.Write(wctx, websocket.MessageText, msg)
	}

	// presence heartbeat: refresh every subscribed membership's TTL key
	// while the socket lives; keys die ~90s after disconnect (§24).
	go func() {
		if g.vk == nil {
			return
		}
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				subsMu.Lock()
				for _, mid := range subs {
					g.vk.Do(ctx, g.vk.B().Set().Key("oga9:presence:"+mid.String()).
						Value("1").Ex(90*time.Second).Build())
				}
				subsMu.Unlock()
			}
		}
	}()

	// inbound: subscribe frames
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			var m struct {
				Subscribe   string `json:"subscribe"`
				Unsubscribe string `json:"unsubscribe"`
				Membership  string `json:"membership"`
				Typing      string `json:"typing"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Unsubscribe != "" {
				if cid, err := uuid.Parse(m.Unsubscribe); err == nil {
					subsMu.Lock()
					delete(subs, cid)
					subsMu.Unlock()
				}
				continue
			}
			// typing frames relay to channel subscribers via the bus —
			// ephemeral, never stored (§24).
			if m.Typing != "" {
				if cid, err := uuid.Parse(m.Typing); err == nil {
					if mid, ok := subs[cid]; ok {
						if b, err := json.Marshal(map[string]any{"type": "typing",
							"channel_id": cid, "membership_id": mid}); err == nil {
							g.vk.Do(ctx, g.vk.B().Publish().Channel(chat.BusKey).Message(string(b)).Build())
						}
					}
				}
				continue
			}
			if m.Subscribe == "" || m.Membership == "" {
				continue
			}
			cid, err1 := uuid.Parse(m.Subscribe)
			mid, err2 := uuid.Parse(m.Membership)
			if err1 != nil || err2 != nil {
				continue
			}
			if g.chat.CanSubscribe(ctx, cid, mid, userID) {
				subsMu.Lock()
				subs[cid] = mid
				subsMu.Unlock()
				// presence is ephemeral Valkey state (§24): a short-TTL
				// key the UI can poll; gone ~90s after disconnect.
				if g.vk != nil {
					g.vk.Do(ctx, g.vk.B().Set().Key("oga9:presence:"+mid.String()).
						Value("1").Ex(90*time.Second).Build())
				}
			} else {
				_ = writeMsg([]byte(`{"error":"not_a_channel_member","channel":"` + m.Subscribe + `"}`))
			}
		}
	}()

	// outbound: valkey pub/sub → this socket's subscribed channels
	if g.vk == nil {
		return
	}
	ded, cancelDed := context.WithCancel(ctx)
	defer cancelDed()
	err = g.vk.Receive(ded, g.vk.B().Subscribe().Channel(chat.BusKey).Build(), func(m valkey.PubSubMessage) {
		var msg struct {
			ChannelID uuid.UUID `json:"channel_id"`
		}
		if json.Unmarshal([]byte(m.Message), &msg) != nil {
			return
		}
		subsMu.Lock()
		_, ok := subs[msg.ChannelID]
		subsMu.Unlock()
		if !ok {
			return
		}
		if err := writeMsg([]byte(m.Message)); err != nil {
			cancelDed()
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("realtime: pubsub: %v", err)
	}
}
