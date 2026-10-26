// Ref: oga9-engineering/sections/09-deployment-editions.md + ADR-020 —
// phase 13.05 observability: structured JSON request logs + per-route
// latency metrics surfaced on /platform/metrics.
package obs

import (
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// init sets the process-wide JSON logger — one structured line per
// event, greppable and parseable by any log shipper.
func init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
}

func routePattern(r *http.Request) string {
	p := chi.RouteContext(r.Context()).RoutePattern()
	if p == "" {
		return "unmatched"
	}
	return p
}

// probe paths — k8s/monitor hits flood logs and skew latency stats.
func isProbe(r *http.Request) bool { return r.URL.Path == "/health" }

// RequestLog emits one structured line per request. The route is the chi
// template (/orgs/{org_id}/...) — never the raw path, so logs carry no
// per-entity id spam.
func RequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http_request",
			"request_id", chimiddleware.GetReqID(r.Context()),
			"method", r.Method,
			"route", routePattern(r),
			"status", ww.Status(),
			"ms", time.Since(start).Milliseconds())
	})
}

const reservoir = 256

type routeStats struct {
	count   uint64
	err4xx  uint64
	err5xx  uint64
	samples []float64 // ms — Vitter reservoir, keeps memory flat
}

var (
	mu     sync.Mutex
	routes = map[string]*routeStats{}
	rng    = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func record(route string, ms float64, status int) {
	mu.Lock()
	defer mu.Unlock()
	st := routes[route]
	if st == nil {
		st = &routeStats{}
		routes[route] = st
	}
	st.count++
	if status >= 500 {
		st.err5xx++
	} else if status >= 400 {
		st.err4xx++
	}
	if len(st.samples) < reservoir {
		st.samples = append(st.samples, ms)
	} else if j := rng.Int63n(int64(st.count)); j < reservoir {
		st.samples[j] = ms
	}
}

// Metrics records request latency per route template (not raw path).
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		record(routePattern(r), float64(time.Since(start).Microseconds())/1000, ww.Status())
	})
}

// RouteSnapshot is the per-route latency view for /platform/metrics.
type RouteSnapshot struct {
	Count  uint64  `json:"count"`
	Err4xx uint64  `json:"err_4xx"`
	Err5xx uint64  `json:"err_5xx"`
	P50    float64 `json:"p50_ms"`
	P95    float64 `json:"p95_ms"`
	P99    float64 `json:"p99_ms"`
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(q*float64(len(sorted)-1)+0.5)]
}

// Snapshot copies the current reservoir into percentile summaries.
func Snapshot() map[string]RouteSnapshot {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]RouteSnapshot, len(routes))
	for k, st := range routes {
		s := append([]float64(nil), st.samples...)
		sort.Float64s(s)
		out[k] = RouteSnapshot{
			Count: st.count, Err4xx: st.err4xx, Err5xx: st.err5xx,
			P50: pct(s, .5), P95: pct(s, .95), P99: pct(s, .99)}
	}
	return out
}
