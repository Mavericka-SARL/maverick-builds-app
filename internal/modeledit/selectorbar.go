package modeledit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MaxSelectorBarEntries caps a dashboard's selector bar: it is one row of
// selectors at the top of the page, not a filter panel.
const MaxSelectorBarEntries = 8

// SelectorBarEntry is one selector of a dashboard's selector bar
// (model.dashboard_def.selector_bar): the dimension it selects on and the
// member it opens on. Every grid and chart on the dashboard, and every KPI
// following the dashboard's selectors or the bar, follows the bar for that
// dimension and draws no selector of its own for it.
type SelectorBarEntry struct {
	DimensionID string `json:"dimension_id"`
	// Default is a member code of the dimension; empty opens on the
	// dimension's first member.
	Default string `json:"default,omitempty"`
	// Members are the member codes the selector offers, in this order;
	// none offers every member. A Data view selector offering seven views
	// that give misleading tiles could not be narrowed (2026-11-06).
	Members []string `json:"members,omitempty"`
	// DefaultMetricID makes the default follow the data (2026-11-07): the
	// selector opens on the last member it offers (its leaves, when it
	// offers every member) where this metric holds a value other than 0 —
	// the month just closed, read from a metric set in closed months — moved
	// DefaultOffset members on (1: the first open month). Default is then
	// the fallback while no member qualifies. A page opening on a fixed
	// month had to be edited at every close.
	DefaultMetricID string `json:"default_metric_id,omitempty"`
	DefaultOffset   int    `json:"default_offset,omitempty"`
}

// MaxSelectorDefaultOffset bounds DefaultOffset either way.
const MaxSelectorDefaultOffset = 12

// NormalizeSelectorBar checks a selector bar for a dashboard of modelID filed
// under revisionID ("" = filed under no revision) and returns it as stored.
// A dimension may be named by its id or by its name (the AI Developer names
// dimensions); either way it must belong to the dashboard's model and
// revision, may appear once, and Default must be one of its member codes.
// An empty bar is valid: the dashboard has none.
func NormalizeSelectorBar(ctx context.Context, db DB, modelID, revisionID string, entries []SelectorBarEntry) ([]SelectorBarEntry, error) {
	if len(entries) > MaxSelectorBarEntries {
		return nil, fmt.Errorf("selector_bar: at most %d dimensions", MaxSelectorBarEntries)
	}
	out := make([]SelectorBarEntry, 0, len(entries))
	seen := map[string]bool{}
	for i, e := range entries {
		ref := strings.TrimSpace(e.DimensionID)
		if ref == "" {
			return nil, fmt.Errorf("selector_bar[%d]: dimension_id is required", i)
		}
		var dimID, dimName string
		err := db.QueryRow(ctx, `
			SELECT id::text, name FROM model.dimension_def
			WHERE model_id = $1::uuid
			  AND (revision_id = NULLIF($2,'')::uuid OR revision_id IS NULL)
			  AND (id::text = $3 OR name = $3)
			ORDER BY (id::text = $3) DESC
			LIMIT 1`, modelID, revisionID, ref).Scan(&dimID, &dimName)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("selector_bar[%d]: dimension %q is not in this dashboard's revision", i, ref)
		}
		if err != nil {
			return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
		}
		if seen[dimID] {
			return nil, fmt.Errorf("selector_bar: dimension %q is listed twice", dimName)
		}
		seen[dimID] = true
		code := strings.TrimSpace(e.Default)
		if code != "" {
			var ok bool
			if err := db.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
				dimID, code).Scan(&ok); err != nil {
				return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
			}
			if !ok {
				return nil, fmt.Errorf("selector_bar: %q is not a member code of dimension %q", code, dimName)
			}
		}
		members, err := memberCodes(ctx, db, dimID, dimName, e.Members)
		if err != nil {
			return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
		}
		if code != "" && len(members) > 0 && !slices.Contains(members, code) {
			return nil, fmt.Errorf("selector_bar: the default %q of dimension %q is not among the members it offers", code, dimName)
		}
		entry := SelectorBarEntry{DimensionID: dimID, Default: code, Members: members}
		if ref := strings.TrimSpace(e.DefaultMetricID); ref != "" {
			if err := db.QueryRow(ctx, `
				SELECT id::text FROM model.metric_def
				WHERE model_id = $1::uuid
				  AND (revision_id = NULLIF($2,'')::uuid OR revision_id IS NULL)
				  AND (id::text = $3 OR name = $3)
				ORDER BY (id::text = $3) DESC
				LIMIT 1`, modelID, revisionID, ref).Scan(&entry.DefaultMetricID); errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("selector_bar[%d]: default_metric_id %q is not a metric of this dashboard's revision", i, ref)
			} else if err != nil {
				return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
			}
			if e.DefaultOffset < -MaxSelectorDefaultOffset || e.DefaultOffset > MaxSelectorDefaultOffset {
				return nil, fmt.Errorf("selector_bar[%d]: default_offset is between -%d and %d", i, MaxSelectorDefaultOffset, MaxSelectorDefaultOffset)
			}
			entry.DefaultOffset = e.DefaultOffset
		} else if e.DefaultOffset != 0 {
			return nil, fmt.Errorf("selector_bar[%d]: default_offset moves a default_metric_id's member; there is none", i)
		}
		out = append(out, entry)
	}
	return out, nil
}

// ResolveSelectorDefaults sets the Default of each entry of bar (stored
// JSON) that names a DefaultMetricID to the member it opens on in revisionID
// (SelectorBarEntry), and returns the bar as served. An entry whose metric
// holds no value at any member it offers keeps its stored Default.
func ResolveSelectorDefaults(ctx context.Context, db DB, revisionID string, bar []byte) []byte {
	var entries []map[string]any
	if len(bar) == 0 || json.Unmarshal(bar, &entries) != nil {
		return bar
	}
	changed := false
	for _, e := range entries {
		metricID, _ := e["default_metric_id"].(string)
		dimID, _ := e["dimension_id"].(string)
		if metricID == "" || dimID == "" {
			continue
		}
		offset := 0
		if f, ok := e["default_offset"].(float64); ok {
			offset = int(f)
		}
		var offered []string
		if ms, ok := e["members"].([]any); ok {
			for _, m := range ms {
				if c, ok := m.(string); ok {
					offered = append(offered, c)
				}
			}
		}
		if code := selectorDefaultFromMetric(ctx, db, revisionID, dimID, metricID, offered, offset); code != "" {
			e["default"] = code
			changed = true
		}
	}
	if !changed {
		return bar
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return bar
	}
	return out
}

// selectorDefaultFromMetric is the member of dimID a selector offering
// offered (its leaves when none) opens on: the last where metricID holds a
// value other than 0 at the member or a member under it, moved offset
// members on within offered. "" when none qualifies.
func selectorDefaultFromMetric(ctx context.Context, db DB, revisionID, dimID, metricID string, offered []string, offset int) string {
	// The dimension's members in order, each with its parent.
	rows, err := db.Query(ctx, `
		SELECT m.code, COALESCE(p.code, ''),
		       NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		FROM model.dimension_member m
		LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
		WHERE m.dimension_id = $1::uuid
		ORDER BY m.time_index NULLS LAST, m.sort_order, m.code`, dimID)
	if err != nil {
		return ""
	}
	parent := map[string]string{}
	var leaves []string
	for rows.Next() {
		var code, par string
		var leaf bool
		if rows.Scan(&code, &par, &leaf) != nil {
			rows.Close()
			return ""
		}
		parent[code] = par
		if leaf {
			leaves = append(leaves, code)
		}
	}
	rows.Close()
	if len(offered) == 0 {
		offered = leaves
	}
	// The leaves at which the metric holds a value other than 0, from its
	// stored inputs or its calculated results.
	held := map[string]bool{}
	vr, err := db.Query(ctx, `
		SELECT DISTINCT dim_members->>$3 FROM runtime.fact_input
		WHERE metric_id = $1::uuid AND revision_id = $2::uuid AND value <> 0 AND dim_members ? $3
		UNION
		SELECT DISTINCT dim_members->>$3 FROM runtime.calc_result
		WHERE metric_id = $1::uuid AND revision_id = $2::uuid AND value <> 0 AND dim_members ? $3`, metricID, revisionID, dimID)
	if err != nil {
		return ""
	}
	for vr.Next() {
		var code string
		if vr.Scan(&code) == nil {
			// The member and every member above it hold the value.
			for c, n := code, 0; c != "" && n < 64; c, n = parent[c], n+1 {
				held[c] = true
			}
		}
	}
	vr.Close()
	last := -1
	for i, c := range offered {
		if held[c] {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	return offered[min(len(offered)-1, max(0, last+offset))]
}

// memberCodes checks that every code names a member of dimID and returns
// them trimmed, without repeats, in their order.
func memberCodes(ctx context.Context, db DB, dimID, dimName string, codes []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		var ok bool
		if err := db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
			dimID, c).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%q is not a member code of dimension %q", c, dimName)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// SelectorBarJSON is the stored form of a normalized bar.
func SelectorBarJSON(entries []SelectorBarEntry) string {
	if len(entries) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(entries)
	return string(b)
}
