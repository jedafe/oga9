// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Client IP resolution honoring trusted proxies (X-Forwarded-For) — without
// this, every request behind a proxy shares one rate-limit bucket.
package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
)

const CtxClientIP ctxKey = "client_ip"

// ResolveIP puts the real client IP into the request context. The peer IP is
// used directly unless it's inside a trusted proxy CIDR — only then is the
// leftmost X-Forwarded-For hop honored.
func ResolveIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIPOf(r, trusted)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), CtxClientIP, ip)))
		})
	}
}

func ClientIPFrom(ctx context.Context) string {
	if v, ok := ctx.Value(CtxClientIP).(string); ok {
		return v
	}
	return ""
}

// ClientIPOf resolves the client IP for a request given trusted proxy CIDRs.
func ClientIPOf(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(trusted) == 0 {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	isTrusted := false
	for _, n := range trusted {
		if n.Contains(peer) {
			isTrusted = true
			break
		}
	}
	if !isTrusted {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// The rightmost UNTRUSTED hop is the attributable client claim —
		// the leftmost is attacker-controlled wherever a proxy appends
		// XFF, and trusting it gives callers a fresh rate-limit bucket
		// per request.
		for i := len(parts) - 1; i >= 0; i-- {
			cand := strings.TrimSpace(parts[i])
			if cand == "" {
				continue
			}
			ip := net.ParseIP(cand)
			trustedHop := false
			if ip != nil {
				for _, n := range trusted {
					if n.Contains(ip) {
						trustedHop = true
						break
					}
				}
			}
			if !trustedHop {
				return cand
			}
		}
		if first := strings.TrimSpace(parts[0]); first != "" {
			return first // every listed hop is trusted — earliest claim is all we have
		}
	}
	return host
}
