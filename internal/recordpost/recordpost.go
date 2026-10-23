// Package recordpost is what a person's access rules mean for the records
// of a table, and how records post into metrics — one implementation for
// the gateway (records saved through the console, the API, an import, a
// model-link run, a workflow's decision) and the integration worker (a
// scheduled REST API connector's rows).
//
// One AccessView per person and table decides three things the same way:
//
//   - which records they see (AccessView.Visibility → crudapp.Visibility,
//     which the records query applies in SQL);
//   - which values a record they save may hold (RecordValueProblems: a
//     member they cannot see is not a member, a metric they cannot see is
//     not a metric);
//   - whether a save may change what the record posts into metrics
//     (PostingRefusal). A record posts within its creator's access — the
//     owner's decision of 2026-10-06; an approver needs no write access
//     where it posts — so a save that would give the record a posting in a
//     cell its creator may not write, in any status it could post in, is
//     refused with a message saying which.
package recordpost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Poster checks and posts the records of one database's tables. Pool is
// the database a context's statements run on: the gateway's tenant router,
// or the worker's one pool.
type Poster struct {
	Pool func(context.Context) *pgxpool.Pool
	Log  zerolog.Logger
	// MembersChanged, when set, is told a table keeping a dimension added
	// members to it (an import of its records), after they are saved: what
	// reads the dimension's members is recalculated.
	MembersChanged func(ctx context.Context, form *crudapp.FormDef)
}

// OnPool is a Poster on one pool.
func OnPool(pool *pgxpool.Pool, log zerolog.Logger) *Poster {
	return &Poster{Pool: func(context.Context) *pgxpool.Pool { return pool }, Log: log}
}

func (ps *Poster) db(ctx context.Context) *pgxpool.Pool { return ps.Pool(ctx) }

// workflowStore is the workflow store a record's events dispatch through,
// with the hook that lets a run set the status of the record it is about.
func (ps *Poster) workflowStore(ctx context.Context) *workflow.Store {
	return workflow.NewStore(ps.db(ctx)).WithLogger(ps.Log).WithRecordStatus(ps.WorkflowRecordStatus)
}

// appOfForm is the application formID's model belongs to.
func (ps *Poster) appOfForm(ctx context.Context, formID string) (string, error) {
	var appID string
	err := ps.db(ctx).QueryRow(ctx, `
		SELECT m.application_id::text FROM model.form_def fd
		JOIN core.model m ON m.id = fd.model_id
		WHERE fd.id = $1::uuid`, formID).Scan(&appID)
	return appID, err
}

// recalc recalculates, per revision, the metrics a posting changed and
// what depends on them. It runs after its request has returned: bounded by
// calculation.RecalcTimeout, and a panic is a log line, not a crash.
func (ps *Poster) recalc(ctx context.Context, modelID string, affected []struct{ RevisionID, MetricID string }) {
	ctx, cancel := context.WithTimeout(ctx, calculation.RecalcTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			ps.Log.Error().Interface("panic", r).Str("stack", string(debug.Stack())).
				Msg("recalculation after a record posting panicked; carrying on")
		}
	}()
	if len(affected) == 0 {
		return
	}
	byRev := make(map[string][]string)
	for _, a := range affected {
		byRev[a.RevisionID] = append(byRev[a.RevisionID], a.MetricID)
	}
	sched := calculation.NewScheduler(ps.Log, calculation.NewStore(ps.db(ctx)))
	for revID, metricIDs := range byRev {
		if err := sched.RecalcAffected(ctx, modelID, revID, metricIDs); err != nil {
			ps.Log.Warn().Err(err).Str("model", modelID).Str("revision", revID).Msg("recalc after a record posting failed")
		}
	}
}

// pending marks the revisions formID's live mappings post into as
// awaiting a recalculation (calculation.RequestRecalc) and returns the func
// that marks them finished. A posting runs after its request answered,
// then recalculates what it posted into: the P&L of a model kept its old
// totals for half a minute after a ledger import, with nothing saying they
// were about to change (2026-10-20).
func (ps *Poster) pending(ctx context.Context, formID string) func() {
	mappings, err := ps.liveFormMappings(ctx, formID)
	if err != nil || len(mappings) == 0 {
		return func() {}
	}
	seqs := map[string]int64{}
	for _, m := range mappings {
		rev := ps.postingRevision(ctx, m)
		if _, done := seqs[rev]; rev == "" || done {
			continue
		}
		seq, err := calculation.RequestRecalc(ctx, ps.db(ctx), rev)
		if err != nil {
			ps.Log.Warn().Err(err).Str("revision", rev).Msg("record a pending recalculation")
			continue
		}
		seqs[rev] = seq
	}
	return func() {
		for rev, seq := range seqs {
			if err := calculation.RecalcFinished(context.WithoutCancel(ctx), ps.db(ctx), rev, seq); err != nil {
				ps.Log.Warn().Err(err).Str("revision", rev).Msg("record a finished recalculation")
			}
		}
	}
}

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

// MemberCache holds the members of the dimensions a file's records name,
// read once per dimension for the whole file.
type MemberCache struct {
	dims map[string]*dimMembers
}

type dimMembers struct {
	leaf    map[string]bool     // code → has no children
	byLabel map[string][]string // label → the codes of the members with it
}

// NewMemberCache is an empty MemberCache.
func NewMemberCache() *MemberCache { return &MemberCache{dims: map[string]*dimMembers{}} }

func (c *MemberCache) load(ctx context.Context, ps *Poster, dimID string) (*dimMembers, error) {
	if m, ok := c.dims[dimID]; ok {
		return m, nil
	}
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT m.code, COALESCE(m.label, ''), NOT EXISTS (SELECT 1 FROM model.dimension_member c
		                           WHERE c.parent_member_id = m.id AND c.dimension_id = m.dimension_id)
		FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid AND NULLIF(btrim(m.formula),'') IS NULL`, dimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := &dimMembers{leaf: map[string]bool{}, byLabel: map[string][]string{}}
	for rows.Next() {
		var code, label string
		var leaf bool
		if err := rows.Scan(&code, &label, &leaf); err != nil {
			return nil, err
		}
		m.leaf[code] = leaf
		if label != "" {
			m.byLabel[label] = append(m.byLabel[label], code)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.dims[dimID] = m
	return m, nil
}

// codeFor is the member code raw names in dimID: raw itself when a member
// has it as its code, else the code of the one member whose label it is
// exactly, else raw (no member: the caller says so).
func (c *MemberCache) codeFor(ctx context.Context, ps *Poster, dimID, raw string) (string, error) {
	m, err := c.load(ctx, ps, dimID)
	if err != nil {
		return "", err
	}
	if _, ok := m.leaf[raw]; ok {
		return raw, nil
	}
	if codes := m.byLabel[raw]; len(codes) == 1 {
		return codes[0], nil
	}
	return raw, nil
}

// AccessView is one person's access rules over one table's revision,
// resolved to the member codes and metric names records hold.
type AccessView struct {
	// hiddenCodes: dimension id → member code → hidden from the person,
	// by a rule of its own or of an ancestor.
	hiddenCodes map[string]map[string]bool
	// readCodes: dimension id → member code → read-only for the person (a
	// rule of the member's own; "read" does not cascade).
	readCodes map[string]map[string]bool
	// metricAccess: metric id → "hidden" or "read".
	metricAccess map[string]string
	// hiddenMetricNames: the names and labels of the metrics of the table's
	// revision hidden from the person.
	hiddenMetricNames []string
}

// AccessViewFor resolves userID's access rules against form's revision.
// It fails closed: an unreadable rule set is an error, not "no rules".
func (ps *Poster) AccessViewFor(ctx context.Context, userID string, form *crudapp.FormDef) (*AccessView, error) {
	v := &AccessView{hiddenCodes: map[string]map[string]bool{}, readCodes: map[string]map[string]bool{}, metricAccess: map[string]string{}}
	var formRevision string
	if err := ps.db(ctx).QueryRow(ctx,
		`SELECT COALESCE(revision_id::text, '') FROM model.form_def WHERE id=$1::uuid`, form.ID,
	).Scan(&formRevision); err != nil {
		return nil, fmt.Errorf("resolve table revision: %w", err)
	}
	dimRules, metricRules, err := writeguard.RuleMaps(ctx, ps.db(ctx), userID, formRevision)
	if err != nil {
		return nil, err
	}
	maps.Copy(v.metricAccess, metricRules)
	if len(dimRules) > 0 {
		rows, err := ps.db(ctx).Query(ctx, `
			SELECT m.id::text, m.dimension_id::text, m.code, COALESCE(m.parent_member_id::text,'')
			FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id = $1::uuid`, form.ModelID)
		if err != nil {
			return nil, err
		}
		type memberRow struct{ id, dimID, code, parentID string }
		var members []memberRow
		edges := make([]writeguard.MemberEdge, 0, 256)
		for rows.Next() {
			var mr memberRow
			if err := rows.Scan(&mr.id, &mr.dimID, &mr.code, &mr.parentID); err != nil {
				rows.Close()
				return nil, err
			}
			members = append(members, mr)
			edges = append(edges, writeguard.MemberEdge{ID: mr.id, ParentID: mr.parentID, DimID: mr.dimID})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		hidden := writeguard.ExpandHidden(edges, dimRules)
		mark := func(m map[string]map[string]bool, dimID, code string) {
			if m[dimID] == nil {
				m[dimID] = map[string]bool{}
			}
			m[dimID][code] = true
		}
		for _, mr := range members {
			switch {
			case hidden[mr.id] || dimRules[mr.id] == "hidden":
				mark(v.hiddenCodes, mr.dimID, mr.code)
			case dimRules[mr.id] == "read":
				mark(v.readCodes, mr.dimID, mr.code)
			}
		}
	}
	var hiddenMetrics []string
	for id, access := range metricRules {
		if access == "hidden" {
			hiddenMetrics = append(hiddenMetrics, id)
		}
	}
	if len(hiddenMetrics) > 0 {
		rows, err := ps.db(ctx).Query(ctx, `SELECT name, COALESCE(label, '') FROM model.metric_def WHERE id::text = ANY($1)`, hiddenMetrics)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, label string
			if err := rows.Scan(&name, &label); err != nil {
				rows.Close()
				return nil, err
			}
			v.hiddenMetricNames = append(v.hiddenMetricNames, name)
			if label != "" && label != name {
				v.hiddenMetricNames = append(v.hiddenMetricNames, label)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// Visibility is which of form's records userID sees: none outside their
// reach; only their own in a table showing each person their own, unless
// they administer it (admin); and never one naming a member or metric hidden
// from them. Coarser than redacting one field: the whole record is
// withheld, as the grid withholds a hidden row.
func (v *AccessView) Visibility(form *crudapp.FormDef, reach, admin bool, userID string) crudapp.Visibility {
	vis := crudapp.Visibility{Nothing: !reach}
	if form.OwnRecordsOnly && !admin {
		vis.OwnerID = userID
		if vis.OwnerID == "" {
			vis.Nothing = true
		}
	}
	// A table keeping a dimension: a record is the member its code names,
	// hidden with it.
	if form.KeepsDimension() {
		if codes := v.hiddenCodes[form.KeepsDimensionID]; len(codes) > 0 {
			vis.HiddenCodes = map[string][]string{form.CodeField().Name: slices.Sorted(maps.Keys(codes))}
		}
	}
	for _, f := range form.Fields {
		switch {
		case f.Type == "dimension" && f.DimensionID != "":
			if codes := v.hiddenCodes[f.DimensionID]; len(codes) > 0 {
				if vis.HiddenCodes == nil {
					vis.HiddenCodes = map[string][]string{}
				}
				vis.HiddenCodes[f.Name] = slices.Sorted(maps.Keys(codes))
			}
		case f.Type == "metric" && f.MetricID != "":
			if v.metricAccess[f.MetricID] == "hidden" {
				vis.WithheldFields = append(vis.WithheldFields, f.Name)
			}
		case f.Type == "metric":
			if len(v.hiddenMetricNames) > 0 {
				if vis.HiddenNames == nil {
					vis.HiddenNames = map[string][]string{}
				}
				vis.HiddenNames[f.Name] = v.hiddenMetricNames
			}
		}
	}
	return vis
}

// maxLinkDepth bounds how far VisibilityFor follows tables naming the
// members other tables keep (a task names a deal, which names an account).
const maxLinkDepth = 4

// VisibilityFor is which of form's records userID sees (AccessView.Visibility),
// and beyond it: a record that belongs to a record userID does not see is
// withheld too — a manager kept from an account sees neither its deals nor
// those deals' tasks. A record belongs to the record a dimension field of it
// names, of a dimension a table keeps, when that field's delete rule is block
// or cascade; a field emptied when its record is deleted (clear) is a mere
// reference — a deal stays visible when its primary contact has moved to an
// account hidden from the person. view is userID's access view of form's
// revision.
func (ps *Poster) VisibilityFor(ctx context.Context, view *AccessView, form *crudapp.FormDef, reach, admin bool, userID string) (crudapp.Visibility, error) {
	return ps.linkedVisibility(ctx, view, form, reach, admin, userID, map[string]bool{form.ID: true})
}

func (ps *Poster) linkedVisibility(ctx context.Context, view *AccessView, form *crudapp.FormDef, reach, admin bool, userID string, seen map[string]bool) (crudapp.Visibility, error) {
	vis := view.Visibility(form, reach, admin, userID)
	if vis.Nothing || len(seen) > maxLinkDepth {
		return vis, nil
	}
	keepers, err := ps.KeepersOf(ctx, form)
	if err != nil || len(keepers) == 0 {
		return vis, err
	}
	store := crudapp.NewStore(ps.db(ctx))
	for _, f := range form.Fields {
		keeper := keepers[f.DimensionID]
		if f.Type != "dimension" || keeper == nil || seen[keeper.ID] || f.DeleteRule() == crudapp.OnDeleteClear {
			continue
		}
		next := maps.Clone(seen)
		next[keeper.ID] = true
		kvis, err := ps.linkedVisibility(ctx, view, keeper, reach, admin, userID, next)
		if err != nil {
			return vis, err
		}
		withheld, err := store.WithheldCodes(ctx, keeper, kvis)
		if err != nil {
			return vis, err
		}
		if len(withheld) == 0 {
			continue
		}
		if vis.HiddenCodes == nil {
			vis.HiddenCodes = map[string][]string{}
		}
		merged := append(slices.Clone(vis.HiddenCodes[f.Name]), withheld...)
		slices.Sort(merged)
		vis.HiddenCodes[f.Name] = slices.Compact(merged)
	}
	return vis, nil
}

// KeepersOf maps each dimension form's dimension fields name to the table
// keeping its members, for the dimensions a table keeps (none for most).
func (ps *Poster) KeepersOf(ctx context.Context, form *crudapp.FormDef) (map[string]*crudapp.FormDef, error) {
	if !form.ModelKeeps && !form.KeepsDimension() {
		return nil, nil
	}
	var dims []string
	for _, f := range form.Fields {
		if f.Type == "dimension" && f.DimensionID != "" {
			dims = append(dims, f.DimensionID)
		}
	}
	if len(dims) == 0 {
		return nil, nil
	}
	rows, err := ps.db(ctx).Query(ctx,
		`SELECT id::text FROM model.form_def WHERE keeps_dimension_id = ANY($1::uuid[])`, dims)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return nil, err
	}
	store := crudapp.NewStore(ps.db(ctx))
	out := map[string]*crudapp.FormDef{}
	for _, id := range ids {
		k, err := store.GetForm(ctx, id)
		if err != nil {
			return nil, err
		}
		if k.KeepsDimension() {
			out[k.KeepsDimensionID] = k
		}
	}
	return out, nil
}

// ReadOnlyCode reports whether member code of dimension dimID is read-only
// for the view's person.
func (v *AccessView) ReadOnlyCode(dimID, code string) bool { return v.readCodes[dimID][code] }

// HiddenCode reports whether member code of dimension dimID is hidden from
// the view's person.
func (v *AccessView) HiddenCode(dimID, code string) bool { return v.hiddenCodes[dimID][code] }

// ReadOnlyMember is the label of a field of form whose value in data is a
// member read-only for the view's person ("" when none is): in a table
// everyone edits, such a record is theirs to see, not to change.
func (v *AccessView) ReadOnlyMember(form *crudapp.FormDef, data map[string]any) string {
	if len(v.readCodes) == 0 {
		return ""
	}
	if form.KeepsDimension() {
		if code := form.MemberCode(data); code != "" && v.readCodes[form.KeepsDimensionID][code] {
			return firstNonEmpty(form.CodeField().Label, form.CodeField().Name)
		}
	}
	for _, f := range form.Fields {
		if f.Type != "dimension" || f.DimensionID == "" {
			continue
		}
		if s, ok := data[f.Name].(string); ok && v.readCodes[f.DimensionID][s] {
			return firstNonEmpty(f.Label, f.Name)
		}
	}
	return ""
}

// FieldProblem is a value a record may not hold.
type FieldProblem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// RecordValueProblems checks the values a record of form saved by the
// view's person will hold, and puts each in its stored form (a number
// typed as text becomes a number). current is the record's data before the
// save (nil for a new record): a value the save leaves as it was is not
// checked again, so a record holding a value its table no longer allows
// can still have its other fields or its status changed. required checks
// that every required field has a value. leaves caches each dimension's
// members across a file import (NewMemberCache).
func (ps *Poster) RecordValueProblems(ctx context.Context, view *AccessView, form *crudapp.FormDef, current, data map[string]any, required bool, leaves *MemberCache) ([]FieldProblem, error) {
	var problems []FieldProblem
	bad := func(f crudapp.FormField, format string, args ...any) {
		problems = append(problems, FieldProblem{Field: f.Name, Message: fmt.Sprintf(format, args...)})
	}
	var metricNames map[string]bool // the table's revision's input metrics, by name and label
	for _, f := range form.Fields {
		if f.Type == crudapp.FieldAutoCode || f.Type == crudapp.FieldStatus {
			continue
		}
		label := firstNonEmpty(f.Label, f.Name)
		v, has := data[f.Name]
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			delete(data, f.Name)
			v, has = nil, false
		}
		if v == nil {
			if has {
				delete(data, f.Name)
			}
			if required && f.Required {
				bad(f, "%s is required", label)
			}
			continue
		}
		if old, ok := current[f.Name]; ok && current != nil && sameValue(old, v) {
			continue
		}
		switch {
		case f.Type == "number", f.Type == "metric" && f.MetricID != "":
			n, ok := numberOf(v)
			if !ok {
				bad(f, "%s is a number, not %v", label, v)
				continue
			}
			data[f.Name] = n
			if f.Type == "metric" && view.metricAccess[f.MetricID] == "hidden" {
				bad(f, "%s is not a field you can fill in", label)
			}
		case f.Type == "boolean":
			switch t := v.(type) {
			case bool:
			case string:
				b, err := strconv.ParseBool(strings.TrimSpace(t))
				if err != nil {
					bad(f, "%s is true or false, not %q", label, t)
					continue
				}
				data[f.Name] = b
			default:
				bad(f, "%s is true or false", label)
			}
		case f.Type == "date":
			s, _ := v.(string)
			day, ok := parseRecordDate(s)
			if !ok {
				bad(f, "%s is a date (YYYY-MM-DD), not %v", label, v)
				continue
			}
			data[f.Name] = day
		case f.Type == "select":
			s := fmt.Sprintf("%v", v)
			if len(f.Options) > 0 && !slices.Contains(f.Options, s) {
				bad(f, "%q is not one of the choices of %s", s, label)
			}
		case f.Type == crudapp.FieldURL:
			u, ok := crudapp.NormalizeURL(fmt.Sprintf("%v", v))
			if !ok {
				bad(f, "%s is a link (https://…), not %v", label, v)
				continue
			}
			data[f.Name] = u
		case f.Type == "dimension" && f.DimensionID != "":
			raw := fmt.Sprintf("%v", v)
			// A member is named by its code, or by its label when no code
			// is the text and exactly one member's label is: a business
			// user's file names members as people read them ("EdTech
			// revenue"), and every loader needed a label → code map kept in
			// step with the dimension (2026-10-20).
			code, err := leaves.codeFor(ctx, ps, f.DimensionID, raw)
			if err != nil {
				return nil, err
			}
			// A member hidden from the person is no member to them: the
			// message is the one an unknown code gets, confirming nothing.
			if view.hiddenCodes[f.DimensionID][code] {
				bad(f, "%q is not a member of %s (no member has this code or label)", raw, label)
				continue
			}
			msg, err := ps.FormMemberProblem(ctx, f, code, leaves)
			if err != nil {
				return nil, err
			}
			if msg != "" {
				bad(f, "%s", msg)
			}
			data[f.Name] = code
		case f.Type == "metric":
			name := fmt.Sprintf("%v", v)
			if metricNames == nil {
				var err error
				if metricNames, err = ps.tableInputMetrics(ctx, form); err != nil {
					return nil, err
				}
			}
			if !metricNames[name] || slices.Contains(view.hiddenMetricNames, name) {
				bad(f, "%q is not an input metric this table's %s can name", name, label)
			}
		}
	}
	// Fields filled in together: either holding a value asks for both.
	if required {
		for _, f := range form.Fields {
			if f.RequiredWith == "" {
				continue
			}
			other := form.Field(f.RequiredWith)
			if other == nil {
				continue
			}
			if hasValue(data[f.Name]) != hasValue(data[other.Name]) {
				missing, filled := f, *other
				if hasValue(data[f.Name]) {
					missing, filled = *other, f
				}
				bad(missing, "%s is required when %s is filled in", firstNonEmpty(missing.Label, missing.Name), firstNonEmpty(filled.Label, filled.Name))
			}
		}
	}
	return problems, nil
}

func hasValue(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// tableInputMetrics are the names and labels of the input metrics of
// form's revision: what a metric field chosen by name may hold.
func (ps *Poster) tableInputMetrics(ctx context.Context, form *crudapp.FormDef) (map[string]bool, error) {
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT md.name, COALESCE(md.label, '') FROM model.metric_def md
		JOIN model.form_def fd ON fd.model_id = md.model_id
		WHERE fd.id = $1::uuid AND md.is_input
		  AND (fd.revision_id IS NULL OR md.revision_id IS NULL OR md.revision_id = fd.revision_id)`, form.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			return nil, err
		}
		out[name] = true
		if label != "" {
			out[label] = true
		}
	}
	return out, rows.Err()
}

// recordDateLayouts are the ways a date is written that a record's date
// field takes — the date picker's, and what a spreadsheet shows — stored as
// YYYY-MM-DD. A date with slashes reads month first, one with dots day
// first, as the spreadsheets that show them do.
var recordDateLayouts = []string{
	"2006-01-02", time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006/01/02",
	"1/2/2006", "1/2/06", "2.1.2006", "02.01.2006", "2 Jan 2006", "Jan 2, 2006", "2-Jan-2006", "2-Jan-06",
}

// parseRecordDate is s as a record's date (YYYY-MM-DD).
func parseRecordDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range recordDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	return "", false
}

func numberOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		n, err := t.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return n, err == nil
	}
	return 0, false
}

func sameValue(a, b any) bool {
	an, aok := numberOf(a)
	bn, bok := numberOf(b)
	if aok && bok {
		return an == bn
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// formMapping is a live mapping of a table's records into a metric.
type formMapping struct {
	id, sourceField, targetMetricID string
	postingStatuses                 []string
	dimMappings                     map[string]string // dimension id → field name
	modelID, revisionID             string            // revisionID "" for a revision-global mapping
}

// liveFormMappings lists formID's live mappings.
func (ps *Poster) liveFormMappings(ctx context.Context, formID string) ([]formMapping, error) {
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT m.id::text, m.source_field, m.target_metric_id::text, m.posting_statuses, m.dimension_mappings,
		       m.model_id::text, COALESCE(m.revision_id::text,'')
		FROM model.form_metric_mapping m
		WHERE m.form_id=$1::uuid AND m.live_posting = TRUE
		  -- Only a mapping of the table's own model into one of its metrics:
		  -- one naming another model's table or metric posts nothing.
		  AND m.model_id = (SELECT fd.model_id FROM model.form_def fd WHERE fd.id = m.form_id)
		  AND EXISTS (SELECT 1 FROM model.metric_def md WHERE md.id = m.target_metric_id AND md.model_id = m.model_id)
		ORDER BY m.created_at, m.id`, formID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []formMapping
	for rows.Next() {
		var m formMapping
		var dimRaw []byte
		if err := rows.Scan(&m.id, &m.sourceField, &m.targetMetricID, &m.postingStatuses, &dimRaw, &m.modelID, &m.revisionID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(dimRaw, &m.dimMappings)
		out = append(out, m)
	}
	return out, rows.Err()
}

// postsAt reports whether a record in status posts through m, in a table
// whose records have a status (hasStatus): a record without one — every
// record of a table without a status field — always does.
func (m formMapping) postsAt(hasStatus bool, status string) bool {
	return !hasStatus || status == "" || slices.Contains(m.postingStatuses, status)
}

// value is the amount data holds in m's source field.
func (m formMapping) value(data map[string]any) (float64, bool) {
	switch v := data[m.sourceField].(type) {
	case float64:
		return v, true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case int:
		return float64(v), true
	}
	return 0, false
}

// members is the {dimension id: member code} coordinate data names for m.
// A date (YYYY-MM-DD) mapped to a time dimension names the period holding
// it (period, when not nil): a deal's close date posts into its month.
func (m formMapping) members(data map[string]any, period periodOf) map[string]string {
	out := map[string]string{}
	for dimID, fieldName := range m.dimMappings {
		if fv, ok := data[fieldName]; ok && fv != nil {
			code := fmt.Sprintf("%v", fv)
			if period != nil {
				if p, ok := period(dimID, code); ok {
					code = p
				}
			}
			out[dimID] = code
		}
	}
	return out
}

// periodOf names the period of time dimension dimID that holds a date
// value, false when value is not a date or the dimension has no period
// holding it (a dimension that is not a time dimension has none).
type periodOf func(dimID, value string) (string, bool)

// datedMember is a time dimension's period: its code and first and last
// days.
type datedMember struct {
	code       string
	start, end time.Time
}

// periodResolver is a periodOf reading each dimension's periods once, and
// only when a value is a date: a coordinate that is already a member code
// (2026-11) costs nothing.
func (ps *Poster) periodResolver(ctx context.Context) periodOf {
	periods := map[string][]datedMember{}
	loaded := map[string]bool{}
	return func(dimID, value string) (string, bool) {
		if len(value) != 10 {
			return "", false
		}
		day, err := time.Parse("2006-01-02", value)
		if err != nil {
			return "", false
		}
		if !loaded[dimID] {
			loaded[dimID] = true
			rows, err := ps.db(ctx).Query(ctx, `
				SELECT code, period_start, period_end FROM model.dimension_member
				WHERE dimension_id = $1::uuid AND period_start IS NOT NULL
				ORDER BY period_end - period_start, time_index`, dimID)
			if err != nil {
				return "", false
			}
			for rows.Next() {
				var dm datedMember
				if rows.Scan(&dm.code, &dm.start, &dm.end) == nil {
					periods[dimID] = append(periods[dimID], dm)
				}
			}
			rows.Close()
		}
		// The shortest period holding the day: a daily dimension's day, a
		// monthly one's month.
		for _, p := range periods[dimID] {
			if !day.Before(p.start) && !day.After(p.end) {
				return p.code, true
			}
		}
		return "", false
	}
}

// postingRevision is the revision m posts into: its own, or for a
// revision-global (legacy) mapping the model's active revision, else its
// most recent one.
func (ps *Poster) postingRevision(ctx context.Context, m formMapping) string {
	if m.revisionID != "" {
		return m.revisionID
	}
	var rev string
	_ = ps.db(ctx).QueryRow(ctx, `
		SELECT COALESCE(
			m.active_revision_id::text,
			(SELECT s.id::text FROM model.revision s WHERE s.model_id=m.id ORDER BY s.created_at DESC LIMIT 1)
		) FROM core.model m WHERE m.id=$1::uuid`, m.modelID,
	).Scan(&rev)
	return rev
}

// plannedPosting is what a record posts through one mapping.
type plannedPosting struct {
	value   float64
	members map[string]string
}

// planned is what a record (status, data) posts through m; nil when it
// posts nothing. anyStatus asks what it posts in whichever status m posts
// in: what it will post once approved, for a draft.
func (m formMapping) planned(hasStatus bool, status string, data map[string]any, anyStatus bool, period periodOf) *plannedPosting {
	if data == nil {
		return nil
	}
	if anyStatus {
		if hasStatus && len(m.postingStatuses) == 0 {
			return nil
		}
	} else if !m.postsAt(hasStatus, status) {
		return nil
	}
	v, ok := m.value(data)
	if !ok {
		return nil
	}
	return &plannedPosting{value: v, members: m.members(data, period)}
}

func samePosting(a, b *plannedPosting) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.value == b.value && maps.Equal(a.members, b.members)
}

// RecordState is a record's status and data after a change.
type RecordState struct {
	Status string
	Data   map[string]any
}

// PostingRefusal says why a change to record recordID of form ("" for a
// record being created) that leaves it as `to` (nil for a record being
// deleted) may not go ahead: every posting the record will hold must be in a
// cell creatorID — its creator — may write. For a record that stays, that is
// what it posts in any status its mappings post in, so a draft that could
// never post is refused when it is saved rather than dropped when it is
// approved; for a deleted one, the postings it withdraws. What the record
// posts now is read from its posting rows, and a posting the change leaves
// as it is is not checked again. forYou says the creator is the caller,
// which the message says. It answers the status code and message of the
// first refusal, or 0 and "" when the change may go ahead.
func (ps *Poster) PostingRefusal(ctx context.Context, creatorID string, forYou bool, form *crudapp.FormDef, recordID string, to *RecordState) (int, string, error) {
	tp, err := ps.newTablePosting(ctx, form)
	if err != nil {
		return 0, "", err
	}
	return ps.postingRefusal(ctx, tp, creatorID, forYou, recordID, to)
}

// postingRefusal is PostingRefusal with what tp has read and checked
// already: an import checks each of its rows with one tablePosting.
func (ps *Poster) postingRefusal(ctx context.Context, tp *tablePosting, creatorID string, forYou bool, recordID string, to *RecordState) (int, string, error) {
	if len(tp.mappings) == 0 {
		return 0, "", nil
	}
	posted := map[string]*plannedPosting{}
	if recordID != "" {
		rows, err := ps.db(ctx).Query(ctx, `
			SELECT mapping_id::text, dim_members, COALESCE(posted_value, 0)::float8
			FROM runtime.form_record_posting WHERE form_record_id = $1::uuid`, recordID)
		if err != nil {
			return 0, "", err
		}
		for rows.Next() {
			var id string
			var raw []byte
			p := &plannedPosting{}
			if err := rows.Scan(&id, &raw, &p.value); err != nil {
				rows.Close()
				return 0, "", err
			}
			_ = json.Unmarshal(raw, &p.members)
			posted[id] = p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, "", err
		}
	}
	hasStatus := tp.form.HasStatus()
	for i, m := range tp.mappings {
		before := posted[m.id]
		var after *plannedPosting
		if to != nil {
			after = m.planned(hasStatus, to.Status, to.Data, true, tp.period)
		}
		if samePosting(before, after) {
			continue
		}
		p := after
		if to == nil {
			p = before
		}
		if p == nil {
			continue
		}
		code, msg, err := ps.postingCellRefusal(ctx, tp, i, creatorID, forYou, p)
		if err != nil || code != 0 {
			return code, msg, err
		}
	}
	return 0, "", nil
}

// postingCellRefusal says why userID — the record's creator — may not
// write the cell posting p of tp's mapping i is in, once per cell: the
// metric, each member, then the write guard's own checks (a system-managed
// revision, a calculated member, a workflow lock). forYou says the creator
// is the caller.
func (ps *Poster) postingCellRefusal(ctx context.Context, tp *tablePosting, i int, userID string, forYou bool, p *plannedPosting) (int, string, error) {
	key := strconv.FormatBool(forYou) + "|" + cellKey(i, userID, p.members)
	if r, ok := tp.refusals[key]; ok {
		return r.code, r.msg, nil
	}
	code, msg, err := ps.cellRefusal(writeguard.WithMemo(ctx, tp.memo), userID, forYou, tp.form, tp.mappings[i], tp.revisions[i], p)
	if err == nil {
		tp.refusals[key] = cellRefusal{code: code, msg: msg}
	}
	return code, msg, err
}

// cellRefusal is postingCellRefusal's check of a cell of mapping m, which
// posts into revisionID.
func (ps *Poster) cellRefusal(ctx context.Context, userID string, forYou bool, form *crudapp.FormDef, m formMapping, revisionID string, p *plannedPosting) (int, string, error) {
	pool := ps.db(ctx)
	whose, readOnly := "your", "read-only for you, and this record posts into it"
	if !forYou {
		whose, readOnly = "its creator's", "read-only for the record's creator, so the record cannot post into it"
	}
	access, err := writeguard.MetricAccess(ctx, pool, userID, m.targetMetricID)
	if err != nil {
		return 0, "", err
	}
	switch access {
	case "hidden":
		// A metric hidden from the person is not named to them.
		return http.StatusForbidden, fmt.Sprintf("this record posts into a metric outside %s access", whose), nil
	case "read":
		var label string
		_ = ps.db(ctx).QueryRow(ctx, `SELECT COALESCE(NULLIF(label,''), name) FROM model.metric_def WHERE id=$1::uuid`, m.targetMetricID).Scan(&label)
		return http.StatusForbidden, fmt.Sprintf("%s is %s", label, readOnly), nil
	}
	memberIDs, err := writeguard.ResolveWriteMembers(ctx, pool, m.modelID, revisionID, p.members)
	if writeguard.IsMemberError(err) {
		return http.StatusBadRequest, fmt.Sprintf("this record posts into %s, which cannot hold it: %v", form.Label, err), nil
	}
	if err != nil {
		return 0, "", err
	}
	for _, id := range memberIDs {
		a, err := writeguard.HiddenAccess(ctx, pool, userID, id)
		if err != nil {
			return 0, "", err
		}
		if a == "read" {
			var dim, member string
			_ = ps.db(ctx).QueryRow(ctx, `
				SELECT d.name, COALESCE(NULLIF(dm.label,''), dm.code) FROM model.dimension_member dm
				JOIN model.dimension_def d ON d.id = dm.dimension_id WHERE dm.id=$1::uuid`, id).Scan(&dim, &member)
			return http.StatusForbidden, fmt.Sprintf("%s %s is %s", dim, member, readOnly), nil
		}
	}
	reason, err := writeguard.CheckWriteMetrics(ctx, pool, m.modelID, revisionID, userID, memberIDs, []string{m.targetMetricID})
	if err != nil {
		return 0, "", err
	}
	if reason != "" {
		if forYou {
			return http.StatusForbidden, "this record posts into a cell you cannot change: " + reason, nil
		}
		return http.StatusForbidden, "this record posts into a cell its creator cannot change: " + reason, nil
	}
	return 0, "", nil
}

// PostingFields are, for each field of form a live mapping takes a member
// from, the statuses in which a record posts ("" for a record without a
// status): where a member the person may only read cannot be picked.
func (ps *Poster) PostingFields(ctx context.Context, form *crudapp.FormDef) (map[string][]string, error) {
	fields, _, err := ps.PostingOf(ctx, form)
	return fields, err
}

// PostingOf is PostingFields, and whether the table posts into a grid at
// all: it has a live mapping (one may take no member from a field).
func (ps *Poster) PostingOf(ctx context.Context, form *crudapp.FormDef) (map[string][]string, bool, error) {
	mappings, err := ps.liveFormMappings(ctx, form.ID)
	if err != nil {
		return nil, false, err
	}
	out := map[string][]string{}
	statuses := []string{""}
	hasStatus := form.HasStatus()
	if hasStatus {
		statuses = append(statuses, crudapp.RecordStatuses...)
	}
	for _, m := range mappings {
		for _, field := range m.dimMappings {
			for _, st := range statuses {
				if m.postsAt(hasStatus, st) && !slices.Contains(out[field], st) {
					out[field] = append(out[field], st)
				}
			}
		}
	}
	return out, len(mappings) > 0, nil
}

// Apply evaluates all live form-metric mappings for a given record
// and posts (or retracts) values to runtime.fact_input based on status rules.
// RetryTransient runs op up to attempts times, pausing between tries.
//
// Apply and recomputeFactInput run in a goroutine detached from
// the request (`go func()` per record status-transition), so there is no
// caller to return an error to and nothing schedules a second attempt. Every
// failure path in them therefore used to `continue` or `return` — which left
// the mapping's aggregate PERMANENTLY wrong, not merely late: a posting that
// failed to insert is never re-posted, and a recompute that failed leaves
// whatever the previous one wrote. One transient error under load (a pool
// acquire timeout, a lock wait) silently under-counts a metric with nothing
// surfaced to any user.
//
// That is what CI caught in TestFormMappingConcurrentPostingsConverge: eight
// concurrent approvals settled at 300 instead of 360 and STAYED there for the
// full 10s the test polls — a stable wrong value, with rowCount=1 ruling out
// the duplicate-snapshot race the test was originally written for.
//
// Retrying is safe because every operation wrapped here is idempotent: the
// access checks are pure reads, the posting insert is ON CONFLICT DO UPDATE,
// and the recompute derives the aggregate from all posting rows rather than
// adjusting it incrementally.
func RetryTransient(ctx context.Context, attempts int, op func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * 100 * time.Millisecond):
			}
		}
		if err = op(); err == nil {
			return nil
		}
	}
	return err
}

// Apply posts one record through each of its table's live mappings and
// rebuilds the totals they post into.
func (ps *Poster) Apply(ctx context.Context, recordID, formID, status string, data map[string]any, userID string) error {
	defer ps.pending(ctx, formID)()
	// Every caller fires this from its own `go func()` per record
	// status-transition, so two calls for the SAME record (e.g. its
	// creation-as-draft call and a subsequent approve call) race with no
	// ordering guarantee — a stale call captured at spawn time can run
	// AFTER a chronologically later one and wrongly retract its posting.
	// Re-reading the record's current status/data here (rather than
	// trusting the possibly-stale status/data parameters) makes every
	// invocation reconcile against ground truth: a late-arriving stale
	// call sees the record's real current state and becomes a no-op or a
	// harmless re-post of the same value, instead of undoing a newer
	// call's work.
	rec := postingRecord{id: recordID}
	var rawData []byte
	if err := ps.db(ctx).QueryRow(ctx,
		`SELECT COALESCE(status::text, ''), data, COALESCE(created_by::text, ''), updated_at FROM runtime.form_record WHERE id=$1::uuid`, recordID,
	).Scan(&rec.status, &rawData, &rec.creator, &rec.updatedAt); err != nil {
		ps.Log.Warn().Err(err).Str("record_id", recordID).Msg("Apply: re-read current record state failed")
		return err
	}
	rec.data = map[string]any{}
	_ = json.Unmarshal(rawData, &rec.data)
	form, err := crudapp.NewStore(ps.db(ctx)).GetForm(ctx, formID)
	if err != nil {
		return err
	}
	tp, err := ps.newTablePosting(ctx, form)
	if err != nil {
		ps.Log.Warn().Err(err).Str("form_id", formID).Msg("Apply: read mappings")
		return err
	}
	ps.totals(ctx, tp, ps.post(ctx, tp, []postingRecord{rec}, userID), userID)
	return nil
}

// RebuildPostings gives every record of a revision's tables its postings
// through their live mappings: what a revision made wholesale needs —
// imported from a package, or copied from another revision. It arrives
// with its records and the totals they posted, but not the postings a
// total is rebuilt from, so the first record saved there rebuilt each of
// its table's totals from that one record, and every other record's value
// left the grid. Each record posts as Apply posts it (its status, its
// value, leaf members, within its creator's access); the totals stay as
// they arrived until the next save rebuilds them from every record, as in
// the revision they came from. It returns how many records it went through.
func (ps *Poster) RebuildPostings(ctx context.Context, modelID, revisionID, userID string) (int, error) {
	ids := func(sql string, args ...any) ([]string, error) {
		rows, err := ps.db(ctx).Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, rows.Err()
	}
	forms, err := ids(`
		SELECT DISTINCT form_id::text FROM model.form_metric_mapping
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND live_posting`, modelID, revisionID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, formID := range forms {
		form, err := crudapp.NewStore(ps.db(ctx)).GetForm(ctx, formID)
		if err != nil {
			return n, err
		}
		tp, err := ps.newTablePosting(ctx, form)
		if err != nil {
			return n, err
		}
		records, err := ps.recordsToPost(ctx, formID)
		if err != nil {
			return n, err
		}
		ps.post(ctx, tp, records, userID)
		n += len(records)
	}
	return n, nil
}

// recomputeFactInput aggregates all active postings for a mapping/metric
// and upserts a single fact_input row tagged with source_ref=mapping_id.
//
// Runs inside a transaction holding a Postgres advisory lock keyed on
// mappingID for its duration: Apply is invoked from an
// unguarded `go func()` per record status-transition (see recordAction/the
// /api/forms/{id}/records POST handler), so e.g. approving several records
// against the same mapping in quick succession schedules multiple
// concurrent calls here. Without serialization, two overlapping
// delete-then-insert passes can interleave (both DELETE, then both INSERT)
// and leave more than one stale snapshot behind — and since the grid read
// path sums every source_ref-tagged fact_input row for a cell (form
// postings are additive by design, unlike direct-entry rows which are
// latest-wins), a leftover stale snapshot silently gets counted again on
// top of the correct one. Found via cmd/qa-engine-test: 3 records posting
// 10/20/30 into a sum-aggregation mapping intermittently produced 2-3
// distinct fact_input rows for the one mapping (e.g. 30 AND 60, timestamps
// microseconds apart) instead of one settled 60, inflating the metric's
// total by the stale rows' values.
// recomputeFactInput rebuilds the mapping's aggregate from every posting row.
//
// Retried because a failed recompute is not a delayed one: nothing else
// recalculates this mapping until the next posting happens to arrive, so a
// single transient failure leaves the metric showing whatever the previous
// recompute wrote — silently stale, indistinguishable from a correct total.
// The whole operation is idempotent (it derives the value from ground truth
// inside one transaction), so re-running it is always safe.
//
// It takes the mapping's target, aggregation and model from the mapping row
// itself, not from its caller: a re-apply runs in the background, and a
// retarget or a delete can land between the moment its caller read the
// mapping and this write. revisionID is only the fallback for legacy
// postings that carry none.
func (ps *Poster) RecomputeFactInput(ctx context.Context, mappingID, revisionID, userID string) {
	if err := RetryTransient(ctx, 3, func() error {
		return ps.recomputeFactInputOnce(ctx, mappingID, revisionID, userID)
	}); err != nil {
		ps.Log.Error().Err(err).Str("mapping_id", mappingID).Msg("recomputeFactInput kept failing — this metric's form-posted total is now stale")
	}
}

func (ps *Poster) recomputeFactInputOnce(ctx context.Context, mappingID, revisionID, userID string) error {
	tx, err := ps.db(ctx).Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, mappingID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}

	// The mapping as it is now, held FOR SHARE until this commits, so its
	// delete (crudapp.DeleteMappings, which deletes the row before it
	// withdraws the totals) waits for this write instead of withdrawing
	// under it. A mapping already gone posts nothing: a re-apply still in
	// flight when it was deleted used to post its total back, under the id
	// of a mapping that no longer existed, where nothing could take it out.
	var targetMetricID, aggregation, modelID string
	err = tx.QueryRow(ctx, `
		SELECT target_metric_id::text, COALESCE(aggregation, ''), model_id::text
		FROM model.form_metric_mapping WHERE id=$1::uuid FOR SHARE`, mappingID,
	).Scan(&targetMetricID, &aggregation, &modelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mapping: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT revision_id::text, dim_members, SUM(posted_value), AVG(posted_value),
		       MIN(posted_value), MAX(posted_value), COUNT(*),
		       MAX(posted_value) FILTER (WHERE posted_at = (SELECT MAX(p2.posted_at) FROM runtime.form_record_posting p2 WHERE p2.mapping_id=$1::uuid))
		FROM runtime.form_record_posting
		WHERE mapping_id=$1::uuid
		GROUP BY revision_id, dim_members`, mappingID)
	if err != nil {
		return fmt.Errorf("query postings: %w", err)
	}

	type aggRow struct {
		revisionID                     string
		dimMembers                     []byte
		sumVal, avgVal, minVal, maxVal *float64
		cnt                            int64
		lastVal                        *float64
	}
	var aggs []aggRow
	for rows.Next() {
		var ag aggRow
		if err := rows.Scan(&ag.revisionID, &ag.dimMembers,
			&ag.sumVal, &ag.avgVal, &ag.minVal, &ag.maxVal, &ag.cnt, &ag.lastVal,
		); err == nil {
			aggs = append(aggs, ag)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postings: %w", err)
	}

	// Delete existing fact_input rows for this mapping source.
	// For "replace" aggregation also remove direct-entry rows for the same
	// (metric, dim) combos so the form value fully replaces them.
	// The reason travels with the transaction: the archive trigger on
	// runtime.fact_input records it against every row it keeps.
	_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'form_reposted'`)
	if _, err := tx.Exec(ctx,
		`DELETE FROM runtime.fact_input WHERE source_ref=$1::uuid AND metric_id=$2::uuid`,
		mappingID, targetMetricID); err != nil {
		return fmt.Errorf("delete existing fact rows: %w", err)
	}

	if len(aggs) > 0 {
		// Resolve user ID for entered_by FK — use passed userID if valid, else any real user.
		systemUserID := userID
		if systemUserID == "" {
			_ = tx.QueryRow(ctx, `SELECT id::text FROM identity.user LIMIT 1`).Scan(&systemUserID)
		}

		for _, ag := range aggs {
			var val float64
			switch aggregation {
			case "sum", "":
				if ag.sumVal != nil {
					val = *ag.sumVal
				}
			case "average":
				if ag.avgVal != nil {
					val = *ag.avgVal
				}
			case "min":
				if ag.minVal != nil {
					val = *ag.minVal
				}
			case "max":
				if ag.maxVal != nil {
					val = *ag.maxVal
				}
			case "count":
				val = float64(ag.cnt)
			case "last", "replace":
				if ag.lastVal != nil {
					val = *ag.lastVal
				} else if ag.sumVal != nil {
					val = *ag.sumVal
				}
			}

			useRevID := ag.revisionID
			if useRevID == "" {
				useRevID = revisionID
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime.fact_input
				  (model_id, revision_id, metric_id, dim_members, value, entered_by, source_ref)
				VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4, $5, $6::uuid, $7::uuid)`,
				modelID, useRevID, targetMetricID, ag.dimMembers, val, systemUserID, mappingID); err != nil {
				return fmt.Errorf("insert aggregate row: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Backfill posts every record of mappingID's table through the table's
// live mappings: what a new mapping needs for the records already there.
// It returns how many records it went through.
func (ps *Poster) Backfill(ctx context.Context, mappingID, userID string) (int, error) {
	var formID string
	var ownModel bool
	if err := ps.db(ctx).QueryRow(ctx, `
		SELECT m.form_id::text,
		       EXISTS (SELECT 1 FROM model.form_def fd WHERE fd.id = m.form_id AND fd.model_id = m.model_id)
		FROM model.form_metric_mapping m WHERE m.id=$1::uuid`, mappingID,
	).Scan(&formID, &ownModel); err != nil {
		return 0, fmt.Errorf("mapping not found: %w", err)
	}
	if !ownModel {
		return 0, nil // a mapping naming another model's table posts nothing
	}
	return ps.repost(ctx, formID, userID)
}

// repost posts every record of formID through its live mappings again,
// rebuilds each total they changed once and recalculates once. Each record
// used to post on its own — its totals rebuilt and a recalculation after
// every one, and a sync went through the table once per mapping. It
// returns how many records it went through.
func (ps *Poster) repost(ctx context.Context, formID, userID string) (int, error) {
	// Resolve a valid entered_by user — prefer the passed userID, fall back to first user in DB.
	if userID == "" {
		_ = ps.db(ctx).QueryRow(ctx, `SELECT id::text FROM identity.user LIMIT 1`).Scan(&userID)
	}
	form, err := crudapp.NewStore(ps.db(ctx)).GetForm(ctx, formID)
	if err != nil {
		return 0, err
	}
	tp, err := ps.newTablePosting(ctx, form)
	if err != nil {
		return 0, err
	}
	records, err := ps.recordsToPost(ctx, formID)
	if err != nil || len(records) == 0 {
		return 0, err
	}
	defer ps.pending(ctx, formID)()
	ps.totals(ctx, tp, ps.post(ctx, tp, records, userID), userID)
	return len(records), nil
}

// DispatchRecordCreated fires, in the background, the form_submit automation
// rules for a new record (DispatchEventRules fires them for a submitted
// record only), and form_approval for one created approved — for a record
// created through the API and one a form import creates alike.
func (ps *Poster) DispatchRecordCreated(ctx context.Context, appID, revisionID, formID, recordID, status, userID string) {
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		payload := map[string]string{"form_id": formID, "record_id": recordID, "status": status}
		ps.workflowStore(ctx).DispatchEventRules(bgCtx, appID, revisionID, "form_submit", formID, userID, payload)
		if status == crudapp.StatusApproved {
			ps.workflowStore(ctx).DispatchEventRules(bgCtx, appID, revisionID, "form_approval", formID, userID, payload)
		}
	}()
}

// WorkflowRecordStatus is the workflow engine's workflow.RecordStatusFunc:
// a run whose subject is a table of records moves the status of the record
// it is about. The record is re-posted through its table's mappings as
// byUserID, as a status set by hand is. No automation rule fires for it: a
// rule started the run, and a status the run set starting another run
// could loop.
func (ps *Poster) WorkflowRecordStatus(ctx context.Context, recordID, status, byUserID string) {
	var formID, previous string
	if err := ps.db(ctx).QueryRow(ctx, `
		WITH before AS (SELECT COALESCE(status::text, '') AS status FROM runtime.form_record WHERE id = $1::uuid)
		UPDATE runtime.form_record SET status = $2::runtime.record_status, updated_at = now()
		WHERE id = $1::uuid
		RETURNING form_id::text, (SELECT status FROM before)`, recordID, status).Scan(&formID, &previous); err != nil {
		ps.Log.Warn().Err(err).Str("record_id", recordID).Str("status", status).Msg("workflow could not set the record's status")
		return
	}
	bgCtx := context.WithoutCancel(ctx)
	go func() { _ = ps.Apply(bgCtx, recordID, formID, status, nil, byUserID) }()
	appID, _ := ps.appOfForm(ctx, formID)
	auditlog.Log(ctx, ps.db(ctx), ps.Log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordUpdated,
		ActorUserID: byUserID, ApplicationID: appID, ResourceType: "form_record", ResourceID: recordID,
		Metadata: map[string]string{"form_id": formID, "status": status, "previous_status": previous, "set_by": "workflow"},
	})
}

// FormMemberProblem says why raw is no member a dimension field of a record
// can hold — a code its dimension does not have, a parent member, one the
// field's allowed members leave out — or "" when it is one. The form's own
// member picker offers leaves only; a record naming another code was
// created but its posting went where no grid reads. leaves caches each
// dimension's codes (true for a leaf) across the file.
func (ps *Poster) FormMemberProblem(ctx context.Context, field crudapp.FormField, raw string, leaves *MemberCache) (string, error) {
	m, err := leaves.load(ctx, ps, field.DimensionID)
	if err != nil {
		return "", fmt.Errorf("read members of %s: %w", field.Label, err)
	}
	leaf, known := m.leaf[raw]
	switch {
	case !known:
		return fmt.Sprintf("%q is not a member of %s (no member has this code or label)", raw, field.Label), nil
	case !leaf:
		return fmt.Sprintf("%q is not a leaf member of %s (has child members)", raw, field.Label), nil
	case len(field.AllowedMembers) > 0 && !slices.Contains(field.AllowedMembers, raw):
		return fmt.Sprintf("%q is not one of the members %s allows", raw, field.Label), nil
	}
	return "", nil
}

// RepostTable posts every record of formID through each of its live
// mappings again, as userID — what a sync does, and what a table that
// gained or lost its status field needs, since which records count
// changed. It returns how many mappings and record postings it went
// through.
func (ps *Poster) RepostTable(ctx context.Context, formID, userID string) (mappings, processed int) {
	ms, err := ps.liveFormMappings(ctx, formID)
	if err != nil {
		ps.Log.Warn().Err(err).Str("form_id", formID).Msg("repost table: read mappings")
		return 0, 0
	}
	if len(ms) == 0 {
		return 0, 0
	}
	n, err := ps.repost(ctx, formID, userID)
	if err != nil {
		ps.Log.Warn().Err(err).Str("form_id", formID).Msg("repost table failed")
		return len(ms), 0
	}
	return len(ms), n * len(ms)
}
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
