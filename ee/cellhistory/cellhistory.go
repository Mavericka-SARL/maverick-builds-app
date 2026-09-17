// Package cellhistory is the enterprise per-cell change history: every value
// an intersection has held, who entered it, when, and through what — the
// value the grid shows first, and rows that were later deleted with the
// reason they went. Licensed under ee/LICENSE; gated by
// license.FeatureCellHistory.
//
// The record is runtime.fact_input itself (append-only; readers take the
// latest row) plus runtime.fact_input_history, which a trigger fills with
// every deleted row (migration 081). Nothing here writes.
package cellhistory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one value the cell held.
type Entry struct {
	ID        string    `json:"id"`
	Value     float64   `json:"value"`
	EnteredAt time.Time `json:"entered_at"`
	// EnteredBy is the account that wrote the row; empty for rows whose
	// author no longer exists.
	EnteredBy struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"entered_by"`
	// Source says how the value arrived: "typed" (a person in the grid or
	// an import they ran), "form" (a form integration's posting, with the
	// mapping's name), or "copied" for a row that came with a revision copy
	// (its entered_at predates the revision).
	Source struct {
		Kind  string `json:"kind"`
		Ref   string `json:"ref,omitempty"`
		Label string `json:"label,omitempty"`
	} `json:"source"`
	// Current marks the row the grid shows now: the latest live row.
	Current bool `json:"current"`
	// Deleted rows carry when and why they left the record.
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	DeleteReason string     `json:"delete_reason,omitempty"`
}

// deleteReasons renders the codes the delete sites declare.
var deleteReasons = map[string]string{
	"form_reposted":      "replaced when the form integration re-posted its values",
	"import_full_reload": "removed by an import in full-reload mode",
	"member_reparented":  "re-keyed when the dimension member was moved under another parent",
}

// DescribeReason turns a declared reason code into a sentence.
func DescribeReason(code string) string {
	if d, ok := deleteReasons[code]; ok {
		return d
	}
	if code == "" {
		return "removed"
	}
	return strings.ReplaceAll(code, "_", " ")
}

// History returns every value the intersection held, newest first, live rows
// and archived ones together. dimMembers is the cell address exactly as
// fact_input stores it: {dimension id: member code}.
func History(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID, metricID string, dimMembers map[string]string, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if dimMembers == nil {
		dimMembers = map[string]string{}
	}
	dims, _ := json.Marshal(dimMembers)
	var revCreated time.Time
	_ = pool.QueryRow(ctx, `SELECT created_at FROM model.revision WHERE id = $1::uuid`, revisionID).Scan(&revCreated)

	rows, err := pool.Query(ctx, `
		SELECT x.id::text, x.value::float8, x.entered_at, COALESCE(x.entered_by::text,''), COALESCE(x.source_ref::text,''),
		       x.deleted_at, x.delete_reason,
		       COALESCE(u.display_name,''), COALESCE(u.email,''),
		       COALESCE(fm.name,'')
		FROM (
		    SELECT id, value, entered_at, entered_by, source_ref, NULL::timestamptz AS deleted_at, '' AS delete_reason
		    FROM runtime.fact_input
		    WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id = $3::uuid AND dim_members = $4::jsonb
		    UNION ALL
		    SELECT id, value, entered_at, entered_by, source_ref, deleted_at, delete_reason
		    FROM runtime.fact_input_history
		    WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id = $3::uuid AND dim_members = $4::jsonb
		) x
		LEFT JOIN identity."user" u ON u.id = x.entered_by
		LEFT JOIN model.form_metric_mapping fm ON fm.id = x.source_ref
		ORDER BY x.entered_at DESC, x.id DESC
		LIMIT $5
	`, modelID, revisionID, metricID, string(dims), limit)
	if err != nil {
		return nil, fmt.Errorf("cell history: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var by, ref, name, email, mapping string
		if err := rows.Scan(&e.ID, &e.Value, &e.EnteredAt, &by, &ref, &e.DeletedAt, &e.DeleteReason, &name, &email, &mapping); err != nil {
			return nil, err
		}
		e.EnteredBy.ID, e.EnteredBy.Name, e.EnteredBy.Email = by, name, email
		switch {
		case mapping != "":
			e.Source.Kind, e.Source.Ref, e.Source.Label = "form", ref, mapping
		case !revCreated.IsZero() && e.EnteredAt.Before(revCreated):
			e.Source.Kind, e.Source.Label = "copied", "came with the revision copy"
		default:
			e.Source.Kind = "typed"
		}
		if e.DeletedAt != nil {
			e.DeleteReason = DescribeReason(e.DeleteReason)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The row the grid shows: the newest live row, in the readers' own
	// order (entered_at DESC, id DESC), which the query already applied.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].EnteredAt.Equal(out[j].EnteredAt) {
			return out[i].EnteredAt.After(out[j].EnteredAt)
		}
		return out[i].ID > out[j].ID
	})
	for i := range out {
		if out[i].DeletedAt == nil {
			out[i].Current = true
			break
		}
	}
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}
