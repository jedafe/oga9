// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md (rate tiers)
// Valkey-backed fixed-window rate limiter. Key: oga9:rate:{ip}:{bucket}
package middleware

import (
	"net"
	"net/http"
	"strconv"
	"time"

	valkey "github.com/valkey-io/valkey-go"
)

// RateLimit returns a limiter allowing `limit` requests per `window` per
// client IP per bucket. Exceed → 429 standard envelope.
func RateLimit(vk valkey.Client, bucket string, limit int64, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil { ip = r.RemoteAddr }
			key := "oga9:rate:" + ip + ":" + bucket
			ctx := r.Context()
			n, err := vk.Do(ctx, vk.B().Incr().Key(key).Build()).AsInt64()
			if err == nil && n == 1 {
				vk.Do(ctx, vk.B().Expire().Key(key).Seconds(int64(window.Seconds())).Build())
			}
			if err != nil || n > limit {
				w.Header().Set("Retry-After", strconv.Itoa(int(window.Seconds())))
				writeErr(w, 429, "rate_limited", "too many requests, slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
