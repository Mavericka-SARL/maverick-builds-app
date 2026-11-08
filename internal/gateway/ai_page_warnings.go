package gateway

// Page mistakes of the AI Developer that run but leave a page wrong
// (2026-11-08, the AI-built production model reviewed for testers): a guide
// rewritten whole when two lines were to change, a page no role could open,
// grids showing metric names turned into words and rates as 0.17. Each is a
// plan warning: the plan goes back once with it.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

var mdHeading = regexp.MustCompile(`(?m)^#{1,4}\s+(.+?)\s*$`)

// textRewriteWarnings names each step replacing a text widget's content that
// drops sections of it (headings) or most of its text: asked to change two
// lines, the AI rewrote a whole Start here guide. db holds the content as it
// is, before the plan.
func textRewriteWarnings(ctx context.Context, db dbx.DB, steps []aiassistant.ProposalStep) []string {
	var out []string
	for i, step := range steps {
		if step.Tool != "update_dashboard_widget" {
			continue
		}
		var p struct {
			WidgetID string  `json:"widget_id"`
			Content  *string `json:"content"`
		}
		if json.Unmarshal(step.Params, &p) != nil || p.Content == nil || p.WidgetID == "" {
			continue
		}
		var old string
		if db.QueryRow(ctx, `SELECT COALESCE(content,'') FROM model.dashboard_widget WHERE id::text=$1`, p.WidgetID).Scan(&old) != nil || len(old) < 200 {
			continue
		}
		var dropped []string
		for _, m := range mdHeading.FindAllStringSubmatch(old, -1) {
			if !strings.Contains(*p.Content, m[1]) {
				dropped = append(dropped, `"`+m[1]+`"`)
			}
		}
		shrunk := len(*p.Content) < len(old)*6/10
		if len(dropped) == 0 && !shrunk {
			continue
		}
		what := fmt.Sprintf("keeps %d%% of its text", len(*p.Content)*100/len(old))
		if len(dropped) > 0 {
			what = "drops the sections " + strings.Join(dropped, ", ")
		}
		out = append(out, fmt.Sprintf("step %d (update_dashboard_widget) replaces the widget's whole text and %s — keep its text and change only what was asked", i+1, what))
	}
	return out
}

// pageRoleWarnings names each page the plan creates that no role may open
// once it has run, while the model's application has business roles: a page
// given to no role is opened by developers and business admins only.
func pageRoleWarnings(ctx context.Context, tx pgx.Tx, modelID string, steps []aiassistant.ProposalStep, created []string) []string {
	var out []string
	for i, step := range steps {
		if step.Tool != "create_dashboard" || i >= len(created) || created[i] == "" {
			continue
		}
		var name string
		var granted, roles int
		if tx.QueryRow(ctx, `
			SELECT d.name,
			       (SELECT count(*) FROM identity.business_role_dashboard brd WHERE brd.dashboard_id = d.id),
			       (SELECT count(*) FROM identity.business_role br JOIN core.workspace w ON w.id = br.workspace_id
			        WHERE w.customer_id = (SELECT COALESCE(a.customer_id, ws.customer_id) FROM core.model mo
			                               JOIN core.application a ON a.id = mo.application_id
			                               LEFT JOIN core.workspace ws ON ws.id = a.workspace_id WHERE mo.id = $2::uuid))
			FROM model.dashboard_def d WHERE d.id::text = $1`, created[i], modelID).Scan(&name, &granted, &roles) != nil {
			continue
		}
		if granted == 0 && roles > 0 {
			out = append(out, fmt.Sprintf("step %d creates the page %q, which no role may open: give it to the roles that use it in this plan (set_role_dashboards keeps each role's pages and adds it)", i+1, name))
		}
	}
	return out
}

// rateName: a metric whose name says it is a rate or a share.
var rateName = regexp.MustCompile(`(?i)(margin|share|utili[sz]|rate|ratio|pct|percent|service_level|fill)`)

// pageMetricWarnings names, for each grid widget the plan puts on a page, the
// metrics it shows with no label (the grid shows their names turned into
// words: "Pnl Ebitda") and the rates in number format (a 17.5% margin reads
// 0.17).
func pageMetricWarnings(ctx context.Context, tx pgx.Tx, steps []aiassistant.ProposalStep, created []string) []string {
	var out []string
	for i, step := range steps {
		if step.Tool != "add_dashboard_widget" || i >= len(created) || created[i] == "" {
			continue
		}
		var widgetType, gridID string
		var props []byte
		if tx.QueryRow(ctx, `SELECT widget_type, COALESCE(ref_id,''), COALESCE(widget_props,'{}'::jsonb) FROM model.dashboard_widget WHERE id::text=$1`,
			created[i]).Scan(&widgetType, &gridID, &props) != nil || widgetType != "grid" {
			continue
		}
		var wp struct {
			MetricIDs []string `json:"metric_ids"`
		}
		_ = json.Unmarshal(props, &wp)
		rows, err := tx.Query(ctx, `
			SELECT md.name, COALESCE(md.label,''), COALESCE(md.format,'number'), COALESCE(md.formula,'')
			FROM model.grid_metric gm JOIN model.metric_def md ON md.id = gm.metric_id
			WHERE gm.grid_id::text = $1 AND (COALESCE(cardinality($2::text[]), 0) = 0 OR md.id::text = ANY($2::text[]))
			ORDER BY md.name`, gridID, wp.MetricIDs)
		if err != nil {
			continue
		}
		var unlabelled, fractions []string
		for rows.Next() {
			var name, label, format, formula string
			if rows.Scan(&name, &label, &format, &formula) != nil {
				continue
			}
			if strings.TrimSpace(label) == "" {
				unlabelled = append(unlabelled, name)
			}
			if format == "number" && rateName.MatchString(name) && strings.Contains(formula, "/") {
				fractions = append(fractions, name)
			}
		}
		rows.Close()
		if len(unlabelled) > 0 {
			out = append(out, fmt.Sprintf("step %d puts a grid on a page whose metrics %s have no label: the page shows their names turned into words — label each with the workbook's row text (update_metric \"label\")",
				i+1, listSome(unlabelled, 8)))
		}
		if len(fractions) > 0 {
			out = append(out, fmt.Sprintf("step %d shows the rates %s as plain numbers (a 17.5%% margin reads 0.17): make each a Percentage metric in percent units (the formula times 100, format \"percentage\") and change what reads it",
				i+1, listSome(fractions, 8)))
		}
	}
	return out
}

func listSome(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
}

// splitWidgetWarnings names each grid widget the plan puts on a page that
// already shows (or the plan also shows) the same grid with the same rows,
// columns, selectors and number scale, where one of the two shows at most
// three metrics: one widget shows them together (owner, 2026-11-08: "three
// grid widgets with the same dimensions and view must be one grid widget
// with 3 metrics"). A different scale, or a group that needs its own total
// row, is the reason to keep two.
func splitWidgetWarnings(ctx context.Context, tx pgx.Tx, steps []aiassistant.ProposalStep, created []string) []string {
	var out []string
	for i, step := range steps {
		if step.Tool != "add_dashboard_widget" || i >= len(created) || created[i] == "" {
			continue
		}
		var page, grid, other string
		if tx.QueryRow(ctx, `
			WITH w AS (
				SELECT w.id, w.dashboard_id, w.ref_id, COALESCE(w.title, '') AS title,
				       COALESCE(w.widget_props->'default_view', '{}'::jsonb) AS view,
				       COALESCE(w.widget_props->>'number_scale', '') AS scale,
				       COALESCE(NULLIF(jsonb_array_length(COALESCE(w.widget_props->'metric_ids', '[]'::jsonb)), 0),
				                (SELECT count(*) FROM model.grid_metric gm WHERE gm.grid_id::text = w.ref_id)) AS metrics
				FROM model.dashboard_widget w WHERE w.widget_type = 'grid')
			SELECT d.name, g.name, COALESCE(NULLIF(o.title, ''), o.id::text)
			FROM w n JOIN w o ON o.dashboard_id = n.dashboard_id AND o.id <> n.id AND o.ref_id = n.ref_id
			                 AND o.view = n.view AND o.scale = n.scale AND LEAST(o.metrics, n.metrics) <= 3
			JOIN model.dashboard_def d ON d.id = n.dashboard_id
			JOIN model.grid_def g ON g.id::text = n.ref_id
			WHERE n.id::text = $1 ORDER BY o.title LIMIT 1`, created[i]).Scan(&page, &grid, &other) != nil {
			continue
		}
		out = append(out, fmt.Sprintf("step %d shows the grid %q on the page %q again, with the same rows, columns and scale as the widget %q: show them in one widget (their metrics, or the rows' members, together) — keep two only for a different number scale or a group that needs its own total row",
			i+1, grid, page, other))
	}
	return out
}

// taggedModelSize is the number of metrics from which a model's metrics and
// grids are found by tag rather than by scrolling.
const taggedModelSize = 30

// untaggedWarnings names the metrics and grids the plan creates without
// tags, in a revision of taggedModelSize metrics or more (owner,
// 2026-11-08: "we have a lot of metrics, use tags to navigate"): Developer ›
// Metrics and Grids filter by tag.
func untaggedWarnings(ctx context.Context, tx pgx.Tx, revID string, steps []aiassistant.ProposalStep, created []string) []string {
	var metrics int
	if tx.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE revision_id = $1::uuid`, revID).Scan(&metrics) != nil || metrics < taggedModelSize {
		return nil
	}
	var names []string
	for i, step := range steps {
		if i >= len(created) || created[i] == "" {
			continue
		}
		table := map[string]string{"create_metric": "model.metric_def", "create_grid": "model.grid_def"}[step.Tool]
		if table == "" {
			continue
		}
		var name string
		if tx.QueryRow(ctx, `SELECT name FROM `+table+` WHERE id::text = $1 AND COALESCE(cardinality(tags), 0) = 0`, created[i]).Scan(&name) == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	var known []string
	_ = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(t ORDER BY n DESC, t), '{}') FROM (
		SELECT t, count(*) AS n FROM (
			SELECT unnest(tags) AS t FROM model.metric_def WHERE revision_id = $1::uuid
			UNION ALL SELECT unnest(tags) FROM model.grid_def WHERE revision_id = $1::uuid) x
		GROUP BY t ORDER BY n DESC, t LIMIT 12) y`, revID).Scan(&known)
	hint := "a tag of its area, as the workbook sheet it comes from (demand, capacity, p-and-l)"
	if len(known) > 0 {
		hint = "the tag of its area — the model's tags are " + strings.Join(known, ", ")
	}
	return []string{fmt.Sprintf("the plan creates %s with no tags, in a model of %d metrics the developer finds by tag: give each %s (\"tags\" on create, set_tags for what exists)",
		listSome(names, 8), metrics, hint)}
}
