// Ref: oga9-engineering/conventions/api-routes-catalog.md (CORS preamble)
// Origin-allowlisted CORS for browser-direct API calls. Bearer-token auth
// means no credential sharing — the matched origin echoes back, never `*`.
// Preflights short-circuit here, before any auth middleware runs.
package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// CORS — allowedOrigins are host patterns: exact host or `*.base` (any
// subdomain, any scheme). Options are answered only for listed origins;
// unlisted origins get no CORS headers (the browser blocks client-side).
func CORS(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			// cache correctness even on rejection: response shape varies by Origin
			w.Header().Add("Vary", "Origin")
			host := origin
			if u, err := url.Parse(origin); err == nil && u.Host != "" {
				host = u.Host
			}
			if !originAllowed(host, allowed) {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-API-Key")
			h.Set("Access-Control-Expose-Headers", "Retry-After")
			h.Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originAllowed matches exact hosts and `*.base` subdomains (host may carry
// a port — e.g. localhost:3010 in dev).
func originAllowed(host string, patterns []string) bool {
	base := host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		base = host[:i]
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, "*.") {
			if base == p[2:] || strings.HasSuffix(base, "."+p[2:]) {
				return true
			}
			continue
		}
		if host == p || base == p {
			return true
		}
	}
	return false
}

// CORSOrigins derives the allowlist from deployment config: base domain +
// subdomains + the app origin; localhost joins in non-production so dev
// SvelteKit (bun dev on :3010) can call the API directly.
func CORSOrigins(baseDomain, appURL string, dev bool) []string {
	origins := []string{baseDomain, "*." + baseDomain}
	if u, err := url.Parse(appURL); err == nil && u.Host != "" {
		origins = append(origins, u.Host)
	}
	if dev {
		origins = append(origins, "localhost", "127.0.0.1")
	}
	return origins
}
