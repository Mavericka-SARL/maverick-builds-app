package aiassistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

// ── describe_dashboard ────────────────────────────────────────────────────────

// selectorBarSQL is a dashboard d's selector bar in words: "Scenario = BUD,
// Month". list_dashboards and describe_dashboard both print it.
const selectorBarSQL = `COALESCE((SELECT string_agg(COALESCE(dim.name, b.e->>'dimension_id') ||
		                                   CASE WHEN COALESCE(b.e->>'default','') <> '' THEN ' = ' || (b.e->>'default') ELSE '' END,
		                                   ', ' ORDER BY b.o)
		                 FROM jsonb_array_elements(d.selector_bar) WITH ORDINALITY b(e, o)
		                 LEFT JOIN model.dimension_def dim ON dim.id::text = b.e->>'dimension_id'), '')`

// resolveDashboardRef turns a dashboard id from list_dashboards, or its
// name, into the id of that dashboard in the working revision. An id from
// the revision the draft was copied from means the draft's same-named copy.
func resolveDashboardRef(ctx context.Context, pool refQuerier, modelID, revID, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("dashboard_id is required — list_dashboards gives each dashboard's id and name")
	}
	name := ref
	if uuidShaped(ref) {
		var owner, rev string
		if err := pool.QueryRow(ctx, modelScopedResourceSQL["dashboard"].lookup, ref).Scan(&owner, &rev, &name); err != nil || owner != modelID {
			return "", fmt.Errorf("dashboard %s not found in this model — call list_dashboards", ref)
		}
		if rev == "" || rev == revID {
			return ref, nil
		}
	}
	var id string
	if err := pool.QueryRow(ctx, `
		SELECT id::text FROM model.dashboard_def
		WHERE model_id=$1::uuid AND (revision_id = $2::uuid OR revision_id IS NULL) AND lower(name) = lower($3)
		ORDER BY (name = $3) DESC LIMIT 1`, modelID, revID, name).Scan(&id); err != nil {
		return "", fmt.Errorf("dashboard %q not found in the working revision — call list_dashboards", name)
	}
	return id, nil
}

// describeDashboard reads one dashboard of the working revision as the page
// its users see: a text widget's whole Markdown with where each of its links
// leads, what every other widget reads, each widget's settings as
// update_dashboard_widget takes them, and the business roles that may open
// it. list_dashboards is every dashboard's outline; this is one in full.
func (e *ToolExecutor) describeDashboard(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		DashboardID string `json:"dashboard_id"`
	}
	_ = json.Unmarshal(raw, &p)
	dashID, err := resolveDashboardRef(ctx, e.pool, e.modelID, e.revID, p.DashboardID)
	if err != nil {
		return "", err
	}

	var name, folder, bar, roles, modelName string
	var tags []string
	if err := e.pool.QueryRow(ctx, `
		SELECT d.name, d.tags, COALESCE(f.name, ''), `+selectorBarSQL+`,
		       COALESCE((SELECT string_agg(br.name, ', ' ORDER BY br.name)
		                 FROM identity.business_role_dashboard brd
		                 JOIN identity.business_role br ON br.id = brd.role_id
		                 WHERE brd.dashboard_id = d.id), ''),
		       m.name
		FROM model.dashboard_def d
		JOIN core.model m ON m.id = d.model_id
		LEFT JOIN model.dashboard_folder f ON f.id = d.folder_id
		WHERE d.id = $1::uuid`, dashID).Scan(&name, &tags, &folder, &bar, &roles, &modelName); err != nil {
		return "", fmt.Errorf("load dashboard: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Dashboard '%s' (id:%s)", name, dashID)
	if folder != "" {
		fmt.Fprintf(&sb, " in folder %s", folder)
	}
	sb.WriteString(tagSuffix(tags) + "\n")
	if bar != "" {
		fmt.Fprintf(&sb, "Selector bar: %s\n", bar)
	}
	if roles != "" {
		fmt.Fprintf(&sb, "Business roles that may open it: %s\n", roles)
	} else {
		sb.WriteString("No business role is granted it yet: only developers see it.\n")
	}

	// A widget's ref_id names a metric, grid, table, integration or
	// automation rule by its type, as the developer endpoint checks it. An
	// image's content is the picture itself and is left out.
	rows, err := e.pool.Query(ctx, `
		SELECT w.id::text, w.widget_type, COALESCE(w.ref_id, ''),
		       COALESCE(md.name, gd.name, fd.name, idf.name, ar.name, ''),
		       COALESCE(w.title, ''), w.show_title,
		       CASE WHEN w.widget_type = 'image' THEN '' ELSE COALESCE(w.content, '') END,
		       COALESCE(w.widget_props, '{}'::jsonb),
		       w.pos_x, w.pos_y, w.size_w, w.size_h
		FROM model.dashboard_widget w
		LEFT JOIN model.metric_def md ON w.widget_type = 'metric_kpi' AND md.id::text = w.ref_id
		LEFT JOIN model.grid_def gd ON w.widget_type IN ('chart','grid','import') AND gd.id::text = w.ref_id
		LEFT JOIN model.form_def fd ON w.widget_type = 'form' AND fd.id::text = w.ref_id
		LEFT JOIN model.integration_def idf ON w.widget_type = 'integration_button' AND idf.id::text = w.ref_id
		LEFT JOIN workflow.automation_rule ar ON w.widget_type = 'automation_button' AND ar.id::text = w.ref_id
		WHERE w.dashboard_id = $1::uuid
		ORDER BY w.pos_y, w.pos_x`, dashID)
	if err != nil {
		return "", err
	}
	type widget struct {
		id, typ, ref, refName, title, content string
		showTitle                             bool
		props                                 []byte
		x, y, w, h                            int
	}
	var widgets []widget
	var ids []string
	for rows.Next() {
		var w widget
		if err := rows.Scan(&w.id, &w.typ, &w.ref, &w.refName, &w.title, &w.showTitle, &w.content, &w.props, &w.x, &w.y, &w.w, &w.h); err != nil {
			rows.Close()
			return "", err
		}
		widgets = append(widgets, w)
		ids = append(ids, idsIn(w.props)...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	names, err := e.idNames(ctx, ids)
	if err != nil {
		return "", err
	}

	if len(widgets) == 0 {
		sb.WriteString("No widgets yet.\n")
		return sb.String(), nil
	}
	fmt.Fprintf(&sb, "Widgets in reading order (top to bottom, left to right), %d:\n", len(widgets))
	for i, w := range widgets {
		fmt.Fprintf(&sb, "%d. %s (id:%s) at (%d,%d) size %dx%d", i+1, w.typ, w.id, w.x, w.y, w.w, w.h)
		if w.title != "" {
			shown := "shown"
			if !w.showTitle {
				shown = "not shown"
			}
			fmt.Fprintf(&sb, " title %q (%s)", w.title, shown)
		}
		sb.WriteString("\n")
		if w.ref != "" {
			if w.refName != "" {
				fmt.Fprintf(&sb, "   reads %s '%s' (id:%s)\n", widgetRefNoun(w.typ), w.refName, w.ref)
			} else {
				fmt.Fprintf(&sb, "   reads %s %s, which no longer exists — the widget shows nothing\n", widgetRefNoun(w.typ), w.ref)
			}
		}
		switch {
		case w.typ == "text":
			if strings.TrimSpace(w.content) == "" {
				sb.WriteString("   text: empty\n")
				break
			}
			sb.WriteString("   text (Markdown, exactly as stored):\n")
			for _, line := range strings.Split(w.content, "\n") {
				sb.WriteString("   | " + line + "\n")
			}
			e.describeTextLinks(ctx, &sb, w.content, modelName)
		case w.content != "":
			fmt.Fprintf(&sb, "   label %q\n", w.content)
		}
		if props := compactProps(w.props); props != "" {
			fmt.Fprintf(&sb, "   settings (widget_props): %s\n", props)
			if legend := idLegend(w.props, names); legend != "" {
				fmt.Fprintf(&sb, "   where %s\n", legend)
			}
		}
	}
	return sb.String(), nil
}

// describeTextLinks says where each link of a text widget leads. A link to
// a dashboard of this model is looked up in the working revision, by name,
// as the console follows it; one to another model is named as written.
func (e *ToolExecutor) describeTextLinks(ctx context.Context, sb *strings.Builder, text, modelName string) {
	links := reporting.TextLinks(text)
	if len(links) == 0 {
		return
	}
	sb.WriteString("   links:\n")
	for _, l := range links {
		switch {
		case !l.Dashboard:
			fmt.Fprintf(sb, "   - %q → %s\n", l.Label, l.Target)
		case l.Model != "" && l.Model != modelName:
			target := "its first dashboard"
			if l.Name != "" {
				target = fmt.Sprintf("dashboard '%s'", l.Name)
			}
			fmt.Fprintf(sb, "   - %q → %s of model '%s' (another model: not checked here)\n", l.Label, target, l.Model)
		case l.Name == "":
			fmt.Fprintf(sb, "   - %q → this model's first dashboard\n", l.Label)
		default:
			var id, found string
			err := e.pool.QueryRow(ctx, `
				SELECT id::text, name FROM model.dashboard_def
				WHERE model_id=$1::uuid AND (revision_id = $2::uuid OR revision_id IS NULL) AND name = $3
				LIMIT 1`, e.modelID, e.revID, l.Name).Scan(&id, &found)
			if err != nil {
				fmt.Fprintf(sb, "   - %q → dashboard '%s', which the working revision does not have: the link leads nowhere\n", l.Label, l.Name)
				continue
			}
			fmt.Fprintf(sb, "   - %q → dashboard '%s' (id:%s)\n", l.Label, found, id)
		}
	}
}

// widgetRefNoun is what a widget type's ref_id points at, in words.
func widgetRefNoun(widgetType string) string {
	switch widgetType {
	case "metric_kpi":
		return "metric"
	case "form":
		return "table"
	case "integration_button":
		return "integration"
	case "automation_button":
		return "automation rule"
	}
	return "grid"
}

// compactProps is a widget's settings on one line; "" when it has none.
func compactProps(props []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, props); err != nil || buf.String() == "{}" || buf.String() == "null" {
		return ""
	}
	return buf.String()
}

// idsIn collects every id in a widget's settings — values and keys, since
// a layout keys its filters by dimension id.
func idsIn(props []byte) []string {
	var v any
	if json.Unmarshal(props, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if uuidShaped(t) {
				out = append(out, t)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			for k, x := range t {
				if uuidShaped(k) {
					out = append(out, k)
				}
				walk(x)
			}
		}
	}
	walk(v)
	return out
}

// idNames names the metrics, dimensions and grids of this model among ids.
func (e *ToolExecutor) idNames(ctx context.Context, ids []string) (map[string]string, error) {
	names := map[string]string{}
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := e.pool.Query(ctx, `
		SELECT id::text, 'metric ' || name FROM model.metric_def WHERE model_id=$1::uuid AND id = ANY($2::uuid[])
		UNION ALL
		SELECT id::text, 'dimension ' || name FROM model.dimension_def WHERE model_id=$1::uuid AND id = ANY($2::uuid[])
		UNION ALL
		SELECT id::text, 'grid ' || name FROM model.grid_def WHERE model_id=$1::uuid AND id = ANY($2::uuid[])`, e.modelID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// idLegend names the ids a widget's settings hold, in the order they appear
// in them: "3f… = metric Revenue; 9a… = dimension Region".
func idLegend(props []byte, names map[string]string) string {
	compact := compactProps(props)
	ids := slices.Compact(slices.SortedFunc(slices.Values(idsIn(props)), func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Index(compact, a), strings.Index(compact, b)), strings.Compare(a, b))
	}))
	var parts []string
	for _, id := range ids {
		if n, ok := names[id]; ok {
			parts = append(parts, id+" = "+n)
		} else {
			parts = append(parts, id+" = not in this model (the setting points at nothing)")
		}
	}
	return strings.Join(parts, "; ")
}
