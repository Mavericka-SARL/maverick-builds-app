package tenantdb

import (
	"context"
	"time"
)

// usedEvery is how often one process writes a tenant's use at most:
// retiring unused workspaces (internal/dormancy) counts days.
const usedEvery = time.Hour

// Used records that a tenant's own people or API clients used it — a request
// routed to it, not a platform administrator looking at it and not its own
// scheduled work — and withdraws a pending deletion notice. The write runs in
// the background and does not end with ctx.
func (r *Router) Used(ctx context.Context, customerID string) {
	if customerID == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if last, ok := r.used[customerID]; ok && now.Sub(last) < usedEvery {
		r.mu.Unlock()
		return
	}
	if r.used == nil {
		r.used = map[string]time.Time{}
	}
	r.used[customerID] = now
	r.mu.Unlock()
	go func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := r.control.Exec(ctx, `
			UPDATE platform.tenant_database SET last_used_at = now(), dormancy_warned_at = NULL
			WHERE customer_id = $1::uuid`, customerID); err != nil {
			r.cfg.Log.Warn().Err(err).Str("tenant", customerID).Msg("record tenant use")
		}
	}(context.WithoutCancel(ctx))
}
