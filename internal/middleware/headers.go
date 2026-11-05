// Ref: §13 hardening — response-header baseline for the JSON API.
package middleware

import "net/http"

// SecurityHeaders sets the API's response-header baseline: no MIME
// sniffing, no framing, no referrer leakage, no shared/proxy caching of
// API responses, and a deny-all CSP (the API serves JSON/blobs, never an
// active document a CSP would need to permit). prod additionally sends
// HSTS — only meaningful once TLS terminates with a real cert, so dev
// http responses stay clean.
func SecurityHeaders(prod bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'")
			h.Set("Cache-Control", "no-store")
			if prod {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
