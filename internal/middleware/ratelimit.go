// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md (rate tiers)
// Valkey-backed fixed-window rate limiter. Key: oga9:rate:{ip}:{bucket}
package middleware

import (
	"net"
	"net/http"
	"strconv"
	"time"

	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/httpx"
)

// RateLimit returns a limiter allowing `limit` requests per `window` per
// client IP per bucket. Exceed → 429 standard envelope.
func RateLimit(vk valkey.Client, bucket string, limit int64, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIPFrom(r.Context())
			if ip == "" {
				if host, _, e := net.SplitHostPort(r.RemoteAddr); e == nil {
					ip = host
				} else {
					ip = r.RemoteAddr
				}
			}
			key := "oga9:rate:" + ip + ":" + bucket
			ctx := r.Context()
			n, err := vk.Do(ctx, vk.B().Incr().Key(key).Build()).AsInt64()
			if err != nil {
				// fail-open: a Valkey blip must not take down auth entirely
				next.ServeHTTP(w, r)
				return
			}
			if n == 1 {
				// A failed EXPIRE would leave a counter that never decays and
				// blocks the IP forever — delete and let the window restart.
				if err := vk.Do(ctx, vk.B().Expire().Key(key).Seconds(int64(window.Seconds())).Build()).Error(); err != nil {
					_ = vk.Do(ctx, vk.B().Del().Key(key).Build()).Error()
				}
			}
			if n > limit {
				w.Header().Set("Retry-After", strconv.Itoa(int(window.Seconds())))
				httpx.WriteErr(w, 429, "rate_limited", "too many requests, slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
