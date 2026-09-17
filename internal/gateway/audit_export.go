package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/ee/auditexport"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Audit export and retention (enterprise). The scope is the listing's own
// (auditScope); the streaming, formats, cursor and sweep live in
// ee/auditexport. An export is recorded in the log it exports.

// auditExport handles GET /api/admin/audit/export. format=csv downloads a
// file; format=jsonl streams one event per line for a collector, which
// follows the X-Next-Cursor header (or `next_cursor` is absent when done).
func (h *handler) auditExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	cond, args, visible, err := h.auditScope(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	qs := r.URL.Query()
	format := auditexport.Format(strings.ToLower(qs.Get("format")))
	if format == "" {
		format = auditexport.FormatCSV
	}
	if format != auditexport.FormatCSV && format != auditexport.FormatJSONL {
		jsonErr(w, fmt.Errorf("format must be csv or jsonl"), http.StatusBadRequest)
		return
	}
	q := auditexport.Query{ScopeCond: cond, ScopeArgs: args, Category: qs.Get("category"), EventType: qs.Get("event_type"), After: qs.Get("after")}
	if !visible {
		q.ScopeCond, q.ScopeArgs = "FALSE", nil
	}
	parseTime := func(name string) (*time.Time, error) {
		v := qs.Get(name)
		if v == "" {
			return nil, nil
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return &t, nil
			}
		}
		return nil, fmt.Errorf("%s must be a date (2006-01-02) or an RFC 3339 time", name)
	}
	if q.Since, err = parseTime("since"); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if q.Until, err = parseTime("until"); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if q.Until != nil && len(qs.Get("until")) == len("2006-01-02") {
		// A bare date as "until" means the whole of that day.
		end := q.Until.Add(24 * time.Hour)
		q.Until = &end
	}
	q.Limit, _ = strconv.Atoi(qs.Get("limit"))
	if q.After != "" {
		// Checked before the response is committed: once streaming starts
		// there is no status left to send.
		if _, _, err := auditexport.DecodeCursor(q.After); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
	}

	tenant := "platform"
	if !visible || cond != "" {
		if ids, ok := args[0].([]string); ok && len(ids) == 1 {
			tenant = ids[0][:8]
		} else {
			tenant = "tenant"
		}
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	switch format {
	case auditexport.FormatCSV:
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="audit-%s-%s.csv"`, tenant, stamp))
	default:
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	}
	// Headers must be decided before the body starts streaming, so the cursor
	// is sent as a trailer-free header only when the page is known to be
	// full — which we learn while streaming. A collector therefore reads the
	// cursor from the last line instead: see below.
	cw := &countingWriter{w: w}
	res, err := auditexport.Stream(ctx, h.db.For(ctx), q, format, cw)
	if err != nil {
		if cw.n == 0 {
			// Nothing sent yet, so a real status can still go out.
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		h.log.Warn().Err(err).Msg("audit export failed mid-stream")
		return
	}
	if format == auditexport.FormatJSONL && res.NextCursor != "" {
		// The final line names the next page; a collector loops until the
		// stream ends without one.
		_ = json.NewEncoder(w).Encode(map[string]string{"next_cursor": res.NextCursor})
	}
	// Recorded AFTER streaming, so an export never contains itself, and only
	// for the first page of a paged pull — otherwise every collector poll
	// would mint the event the next poll then fetches, forever.
	if q.After == "" {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventAuditExported,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "audit_log", ResourceID: "export",
			Metadata: map[string]string{"format": string(format), "since": qs.Get("since"), "until": qs.Get("until"),
				"category": q.Category, "event_type": q.EventType, "events": strconv.Itoa(res.Count)},
		})
	}
}

// auditSettings handles GET and PUT on /api/admin/audit/settings.
func (h *handler) auditSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s, err := auditexport.GetSettings(ctx, h.db.For(ctx))
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"retention_days": s.RetentionDays, "min_retention_days": auditexport.MinRetentionDays, "updated_at": s.UpdatedAt})
	case http.MethodPut:
		var body struct {
			RetentionDays int `json:"retention_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		s, err := auditexport.UpdateSettings(ctx, h.db.For(ctx), body.RetentionDays)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventAuditRetentionUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "audit_log", ResourceID: "settings",
			Metadata: map[string]string{"retention_days": strconv.Itoa(s.RetentionDays)},
		})
		jsonOK(w, map[string]any{"retention_days": s.RetentionDays, "min_retention_days": auditexport.MinRetentionDays, "updated_at": s.UpdatedAt})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// countingWriter records whether any body bytes have gone out, which
// decides whether an error can still become a status code.
type countingWriter struct {
	w http.ResponseWriter
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}
