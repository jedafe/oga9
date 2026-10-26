// Ref: oga9-engineering/sections/09-deployment-editions.md — §13.05 ops
// surface: per-queue asynq depth + domain_events DLQ + HTTP latency
// percentiles in one JSON payload (platform.analytics.view gated).
package handler

import (
	"net/http"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/obs"
)

// Metrics — GET /platform/metrics: queue depth + DLQ + per-route latency.
func Metrics(valkeyAddr string, pool *pgxpool.Pool) http.HandlerFunc {
	q := db.New(pool)
	return func(w http.ResponseWriter, r *http.Request) {
		queues := map[string]any{}
		ins := asynq.NewInspector(asynq.RedisClientOpt{Addr: valkeyAddr})
		defer ins.Close()
		if names, err := ins.Queues(); err == nil {
			for _, name := range names {
				if qi, err := ins.GetQueueInfo(name); err == nil {
					queues[name] = map[string]int{
						"pending": qi.Pending, "active": qi.Active,
						"retry": qi.Retry, "archived": qi.Archived}
				}
			}
		}
		dead, _ := q.CountDeadEvents(r.Context(), events.MaxAttempts)
		httpx.WriteData(w, 200, map[string]any{
			"http": obs.Snapshot(), "queues": queues, "dead_events": dead})
	}
}
