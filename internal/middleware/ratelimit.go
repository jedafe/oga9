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

// RateLimitReads — the global read tier (§13.03): bounds GET/HEAD traffic
// per IP without touching the stricter per-endpoint write/auth buckets.
// Probes (/health) are exempt.
func RateLimitReads(vk valkey.Client, bucket string, limit int64, window time.Duration) func(http.Handler) http.Handler {
	limited := RateLimit(vk, bucket, limit, window)
	return func(next http.Handler) http.Handler {
		h := limited(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
				next.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

// RateLimitWrites — the global write tier (§13.03): bounds non-GET/HEAD
// traffic per IP so mutations can't flood the API; stricter per-endpoint
// buckets (auth, payments, impersonation, exports) stack on top. Probes
// are exempt; reads are covered by RateLimitReads instead.
func RateLimitWrites(vk valkey.Client, bucket string, limit int64, window time.Duration) func(http.Handler) http.Handler {
	limited := RateLimit(vk, bucket, limit, window)
	return func(next http.Handler) http.Handler {
		h := limited(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}
