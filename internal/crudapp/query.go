package crudapp

// Querying a table's records: one engine for the records widget's list
// (filter, search, sort, pages), the record list and export routes, a
// single record's read, and the read-only records query the chat connector
// and the AI assistant answer questions with (count, sum, average, group
// by a field or a period). What one person may see of a table is a
// Visibility, applied in the same SQL statement as everything else, so no
// page, count or total ever includes a record the person may not see.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Visibility is which of a table's records one person may see.
type Visibility struct {
	// Nothing: the person sees no record of the table.
	Nothing bool
	// OwnerID, when set, keeps only the records this user created (a table
	// showing each person their own records, to someone who does not
	// administer it).
	OwnerID string
	// HiddenCodes: a dimension field's name → the member codes hidden from
	// the person. A record naming one is withheld.
	HiddenCodes map[string][]string
	// HiddenNames: a metric field chosen by name → the names of the metrics
	// hidden from the person. A record naming one is withheld.
	HiddenNames map[string][]string
	// WithheldFields: fields bound to a metric hidden from the person. A
	// record holding a value in one is withheld.
	WithheldFields []string
}

// Column kinds: how a column's values compare, sort and add up.
const (
	KindText     = "text"
	KindNumber   = "number"
	KindDate     = "date"
	KindBoolean  = "boolean"
	KindDateTime = "datetime"
	KindStatus   = "status"
	KindUser     = "user"
)

// Column is a field of a table, or one of the columns every record has
// (id, created_at, updated_at, created_by), as a query names it.
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Type is the field's type ("" for a column every record has).
	Type string `json:"type,omitempty"`
	// DimensionID: a dimension field's dimension, whose member labels name
	// its values.
	DimensionID string `json:"dimension_id,omitempty"`
	system      string // id, created_at, updated_at, created_by, status: a column of the record row
}

// Columns lists what a query can name in the table: its fields, then the
// columns every record has unless a field of the same name hides them.
// "status" names the table's status field whatever its name, unless a
// field of its own is called "status".
func (f *FormDef) Columns() []Column {
	cols := make([]Column, 0, len(f.Fields)+4)
	names := map[string]bool{}
	for _, fd := range f.Fields {
		c := Column{Key: fd.Name, Label: firstNonEmpty(fd.Label, fd.Name), Kind: KindText, Type: fd.Type}
		switch {
		case fd.Type == FieldStatus:
			c.Kind, c.system = KindStatus, "status"
		case fd.Type == "number", fd.Type == "metric" && fd.MetricID != "":
			c.Kind = KindNumber
		case fd.Type == "date":
			c.Kind = KindDate
		case fd.Type == "boolean":
			c.Kind = KindBoolean
		case fd.Type == "dimension":
			c.DimensionID = fd.DimensionID
		}
		cols = append(cols, c)
		names[fd.Name] = true
	}
	for _, sys := range []Column{
		{Key: "id", Label: "Record id", Kind: KindText, system: "id"},
		{Key: "created_at", Label: "Created", Kind: KindDateTime, system: "created_at"},
		{Key: "updated_at", Label: "Updated", Kind: KindDateTime, system: "updated_at"},
		{Key: "created_by", Label: "Created by", Kind: KindUser, system: "created_by"},
	} {
		if !names[sys.Key] {
			cols = append(cols, sys)
		}
	}
	return cols
}

// column finds key among the table's columns.
func (f *FormDef) column(key string) (Column, bool) {
	cols := f.Columns()
	for _, c := range cols {
		if c.Key == key {
			return c, true
		}
	}
	if key == "status" {
		for _, c := range cols {
			if c.Kind == KindStatus {
				return c, true
			}
		}
	}
	return Column{}, false
}

// Query asks for a table's records, or for totals over them.
type Query struct {
	// Filters all apply (AND).
	Filters []Filter `json:"filters,omitempty"`
	// Search keeps records any of whose fields, member labels or status
	// contain the text (case-insensitive).
	Search string `json:"search,omitempty"`
	// Sort orders the records or groups; records end newest first.
	Sort []Sort `json:"sort,omitempty"`
	// GroupBy and Aggregates make it a totals query: one row per group (or
	// a single row without GroupBy) holding each aggregate. Without either
	// it lists records.
	GroupBy    []Group     `json:"group_by,omitempty"`
	Aggregates []Aggregate `json:"aggregates,omitempty"`
	// Limit and Offset page the records or groups.
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
	// Today is the asker's day (YYYY-MM-DD) that the relative date filters
	// (before_today, next_days, …) count from; the server's day (UTC) when
	// omitted.
	Today string `json:"today,omitempty"`
}

// Relative date filter operations: a date compared with the query's day.
// next_days and last_days take a number of days (Value) and include today.
var relativeDateOps = map[string]bool{
	"before_today": true, "on_or_before_today": true, "today": true,
	"on_or_after_today": true, "after_today": true, "next_days": true, "last_days": true,
}

// Filter keeps the records whose Field compares to Value (or Values) by Op.
type Filter struct {
	Field string `json:"field"`
	// Op: eq, ne, in, not_in, gt, gte, lt, lte, between, contains,
	// starts_with, empty, not_empty; on a date: before_today,
	// on_or_before_today, today, on_or_after_today, after_today, next_days
	// and last_days (Value: a number of days).
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
	Values []any  `json:"values,omitempty"`
}

// Sort orders by a column, or in a totals query by a group or aggregate key.
type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Group groups by a column; a date or datetime column may be grouped by
// Period: day, week, month, quarter or year.
type Group struct {
	Field  string `json:"field"`
	Period string `json:"period,omitempty"`
}

// Aggregate is a total over each group: count (records), count_distinct,
// sum, avg, min or max of a field.
type Aggregate struct {
	Op    string `json:"op"`
	Field string `json:"field,omitempty"`
}

// Key is the name of the aggregate's value in a result row: "count", or
// the operation and field ("sum_amount").
func (a Aggregate) Key() string {
	if a.Field == "" {
		return a.Op
	}
	return a.Op + "_" + a.Field
}

// QueryError is a query that cannot be answered as asked; its message says
// why, in terms of the table.
type QueryError struct{ msg string }

func (e *QueryError) Error() string { return e.msg }

func queryErr(format string, args ...any) error {
	return &QueryError{msg: fmt.Sprintf(format, args...)}
}

// IsQueryError reports whether err is a QueryError.
func IsQueryError(err error) bool {
	var q *QueryError
	return errors.As(err, &q)
}

// Record and group page bounds.
const (
	DefaultRecordLimit = 100
	MaxRecordLimit     = 1000
	DefaultGroupLimit  = 1000
	MaxGroupLimit      = 5000
	maxOffset          = 1_000_000
	// MaxExportRecords bounds one export.
	MaxExportRecords = 100_000
)

// RecordsResult is a page of records and how many records match in all.
type RecordsResult struct {
	Records []*FormRecord `json:"records"`
	Total   int           `json:"total"`
}

// TotalsResult is a totals query's answer: its columns (the groups, each
// dimension or person group followed by its label, then the aggregates),
// one row per group, and how many groups there are in all.
type TotalsResult struct {
	Columns     []ResultColumn   `json:"columns"`
	Rows        []map[string]any `json:"rows"`
	TotalGroups int              `json:"total_groups"`
}

// ResultColumn is a column of a totals result.
type ResultColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// IsTotals reports whether q asks for totals rather than records.
func (q Query) IsTotals() bool { return len(q.GroupBy) > 0 || len(q.Aggregates) > 0 }

// qb builds one statement's WHERE clause and arguments.
type qb struct {
	form   *FormDef
	userID string
	today  string
	args   []any
	conds  []string
}

func (b *qb) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// expr is the SQL value of column c in record row r.
func (b *qb) expr(c Column) string {
	switch c.system {
	case "id":
		return "r.id::text"
	case "created_at":
		return "r.created_at"
	case "updated_at":
		return "r.updated_at"
	case "created_by":
		return "r.created_by::text"
	case "status":
		return "r.status::text"
	}
	p := b.arg(c.Key) + "::text"
	switch c.Kind {
	case KindNumber:
		return "(CASE WHEN jsonb_typeof(r.data->" + p + ") = 'number' THEN (r.data->>" + p + ")::numeric" +
			" WHEN pg_input_is_valid(btrim(r.data->>" + p + "), 'numeric') THEN btrim(r.data->>" + p + ")::numeric END)"
	case KindDate:
		return "(CASE WHEN pg_input_is_valid(left(r.data->>" + p + ", 10), 'date') THEN left(r.data->>" + p + ", 10)::date END)"
	case KindBoolean:
		return "(CASE WHEN jsonb_typeof(r.data->" + p + ") = 'boolean' THEN (r.data->>" + p + ")::boolean" +
			" WHEN lower(btrim(r.data->>" + p + ")) IN ('true', 'false') THEN lower(btrim(r.data->>" + p + "))::boolean END)"
	}
	return "(r.data->>" + p + ")"
}

// visibility adds what the person may see.
func (b *qb) visibility(v Visibility) {
	b.conds = append(b.conds, "r.form_id = "+b.arg(b.form.ID)+"::uuid")
	if v.Nothing {
		b.conds = append(b.conds, "false")
	}
	if v.OwnerID != "" {
		b.conds = append(b.conds, "r.created_by = "+b.arg(v.OwnerID)+"::uuid")
	}
	for _, field := range sortedKeys(v.HiddenCodes) {
		if codes := v.HiddenCodes[field]; len(codes) > 0 {
			b.conds = append(b.conds, "COALESCE(r.data->>"+b.arg(field)+"::text, '') <> ALL("+b.arg(codes)+"::text[])")
		}
	}
	for _, field := range sortedKeys(v.HiddenNames) {
		if names := v.HiddenNames[field]; len(names) > 0 {
			b.conds = append(b.conds, "COALESCE(r.data->>"+b.arg(field)+"::text, '') <> ALL("+b.arg(names)+"::text[])")
		}
	}
	for _, field := range v.WithheldFields {
		b.conds = append(b.conds, "NOT (r.data ? "+b.arg(field)+"::text)")
	}
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// search adds q.Search.
func (b *qb) search(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	pat := b.arg("%"+likeEscape(text)+"%") + "::text"
	keys := make([]string, 0, len(b.form.Fields))
	var ors []string
	for _, f := range b.form.Fields {
		if f.Type == FieldStatus {
			ors = append(ors, "COALESCE(r.status::text, '') ILIKE "+pat)
			continue
		}
		keys = append(keys, f.Name)
		if f.Type == "dimension" && f.DimensionID != "" {
			ors = append(ors, "EXISTS (SELECT 1 FROM model.dimension_member m WHERE m.dimension_id = "+
				b.arg(f.DimensionID)+"::uuid AND m.code = r.data->>"+b.arg(f.Name)+"::text AND m.label ILIKE "+pat+")")
		}
	}
	ors = append([]string{"EXISTS (SELECT 1 FROM jsonb_each_text(r.data) e WHERE e.key = ANY(" +
		b.arg(keys) + "::text[]) AND e.value ILIKE " + pat + ")"}, ors...)
	b.conds = append(b.conds, "("+strings.Join(ors, " OR ")+")")
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// filter adds f.
func (b *qb) filter(f Filter) error {
	c, ok := b.form.column(f.Field)
	if !ok {
		return queryErr("%q is not a field of %s (its fields: %s)", f.Field, b.form.Label, columnKeys(b.form))
	}
	op := strings.ToLower(strings.TrimSpace(f.Op))
	if op == "" {
		op = "eq"
	}
	e := b.expr(c)
	if relativeDateOps[op] {
		return b.relativeDate(c, e, op, f.Value)
	}
	switch op {
	case "empty", "not_empty":
		cond := e + " IS NULL"
		if c.Kind == KindText {
			cond = "COALESCE(btrim(" + e + "), '') = ''"
		}
		if op == "not_empty" {
			cond = "NOT (" + cond + ")"
		}
		b.conds = append(b.conds, cond)
		return nil
	case "contains", "starts_with":
		if c.Kind != KindText && c.Kind != KindStatus {
			return queryErr("%s compares %s, not text: use eq, gt, lt or between", op, c.Label)
		}
		s, err := textValue(f.Value)
		if err != nil {
			return queryErr("%s of %s: %v", op, c.Label, err)
		}
		pat := likeEscape(s) + "%"
		if op == "contains" {
			pat = "%" + pat
		}
		b.conds = append(b.conds, "COALESCE("+e+", '') ILIKE "+b.arg(pat)+"::text")
		return nil
	}

	vals := f.Values
	if f.Value != nil && op != "in" && op != "not_in" && op != "between" {
		vals = []any{f.Value}
	} else if f.Value != nil && len(vals) == 0 {
		if list, ok := f.Value.([]any); ok {
			vals = list
		} else {
			vals = []any{f.Value}
		}
	}
	switch op {
	case "eq", "ne", "gt", "gte", "lt", "lte":
		if len(vals) != 1 {
			return queryErr("%s of %s takes one value", op, c.Label)
		}
	case "between":
		if len(vals) != 2 {
			return queryErr("between of %s takes two values, the lowest and the highest", c.Label)
		}
	case "in", "not_in":
		if len(vals) == 0 {
			return queryErr("%s of %s takes a list of values", op, c.Label)
		}
	default:
		return queryErr("%q is not a filter operation (eq, ne, in, not_in, gt, gte, lt, lte, between, contains, starts_with, empty, not_empty, before_today, on_or_before_today, today, on_or_after_today, after_today, next_days, last_days)", f.Op)
	}
	ordered := c.Kind == KindNumber || c.Kind == KindDate || c.Kind == KindDateTime
	if (op == "gt" || op == "gte" || op == "lt" || op == "lte" || op == "between") && !ordered {
		return queryErr("%s compares numbers and dates; %s is %s", op, c.Label, c.Kind)
	}

	// A status filter for "no status" is an empty test.
	if c.Kind == KindStatus && (op == "eq" || op == "ne") {
		if s, _ := textValue(vals[0]); s == "" || s == "none" {
			cond := e + " IS NULL"
			if op == "ne" {
				cond = e + " IS NOT NULL"
			}
			b.conds = append(b.conds, cond)
			return nil
		}
	}

	lhs := e
	if c.Kind == KindText {
		lhs = "lower(" + e + ")"
	}
	if c.Kind == KindDateTime {
		// A day compares with the record's day (UTC); a moment, with its moment.
		allDays := true
		for _, v := range vals {
			s, _ := textValue(v)
			if _, err := time.Parse("2006-01-02", s); err != nil {
				allDays = false
			}
		}
		if allDays {
			lhs = "(" + e + " AT TIME ZONE 'UTC')::date"
		}
	}
	params := make([]string, len(vals))
	for i, v := range vals {
		p, err := b.value(c, v)
		if err != nil {
			return err
		}
		params[i] = p
	}
	switch op {
	case "eq":
		b.conds = append(b.conds, lhs+" = "+params[0])
	case "ne":
		b.conds = append(b.conds, lhs+" IS DISTINCT FROM "+params[0])
	case "gt":
		b.conds = append(b.conds, lhs+" > "+params[0])
	case "gte":
		b.conds = append(b.conds, lhs+" >= "+params[0])
	case "lt":
		b.conds = append(b.conds, lhs+" < "+params[0])
	case "lte":
		b.conds = append(b.conds, lhs+" <= "+params[0])
	case "between":
		b.conds = append(b.conds, lhs+" BETWEEN "+params[0]+" AND "+params[1])
	case "in":
		b.conds = append(b.conds, lhs+" IN ("+strings.Join(params, ", ")+")")
	case "not_in":
		b.conds = append(b.conds, "NOT COALESCE("+lhs+" IN ("+strings.Join(params, ", ")+"), false)")
	}
	return nil
}

// relativeDate adds a date filter counted from the query's day: e is
// column c's value.
func (b *qb) relativeDate(c Column, e, op string, v any) error {
	if c.Kind != KindDate && c.Kind != KindDateTime {
		return queryErr("%s compares dates; %s is %s", op, c.Label, c.Kind)
	}
	lhs := e
	if c.Kind == KindDateTime {
		lhs = "(" + e + " AT TIME ZONE 'UTC')::date"
	}
	day := b.arg(b.today) + "::date"
	switch op {
	case "before_today":
		b.conds = append(b.conds, lhs+" < "+day)
	case "on_or_before_today":
		b.conds = append(b.conds, lhs+" <= "+day)
	case "today":
		b.conds = append(b.conds, lhs+" = "+day)
	case "on_or_after_today":
		b.conds = append(b.conds, lhs+" >= "+day)
	case "after_today":
		b.conds = append(b.conds, lhs+" > "+day)
	case "next_days", "last_days":
		n, err := numberValue(v)
		if err != nil || n < 0 || n > 3660 || n != math.Trunc(n) {
			return queryErr("%s of %s takes a whole number of days (0 to 3660)", op, c.Label)
		}
		days := b.arg(int(n)) + "::int"
		if op == "next_days" {
			b.conds = append(b.conds, lhs+" BETWEEN "+day+" AND "+day+" + "+days)
		} else {
			b.conds = append(b.conds, lhs+" BETWEEN "+day+" - "+days+" AND "+day)
		}
	}
	return nil
}

// value binds v as a value of column c and returns its placeholder.
func (b *qb) value(c Column, v any) (string, error) {
	switch c.Kind {
	case KindNumber:
		n, err := numberValue(v)
		if err != nil {
			return "", queryErr("%s is a number: %v", c.Label, err)
		}
		return b.arg(n) + "::numeric", nil
	case KindDate:
		s, _ := textValue(v)
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return "", queryErr("%s is a date: give it as YYYY-MM-DD, not %q", c.Label, s)
		}
		return b.arg(s) + "::date", nil
	case KindDateTime:
		s, _ := textValue(v)
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return b.arg(s) + "::date", nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return "", queryErr("%s is a date and time: give it as YYYY-MM-DD or RFC 3339, not %q", c.Label, s)
		}
		return b.arg(t) + "::timestamptz", nil
	case KindBoolean:
		switch t := v.(type) {
		case bool:
			return b.arg(t) + "::boolean", nil
		case string:
			if bv, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
				return b.arg(bv) + "::boolean", nil
			}
		}
		return "", queryErr("%s is true or false", c.Label)
	case KindStatus:
		s, _ := textValue(v)
		s = strings.ToLower(s)
		if !ValidRecordStatus(s) {
			return "", queryErr("%q is not a status (draft, submitted, approved, rejected, or none)", s)
		}
		return b.arg(s) + "::text", nil
	case KindUser:
		s, _ := textValue(v)
		if strings.EqualFold(s, "me") {
			s = b.userID
		}
		return b.arg(s) + "::text", nil
	}
	s, err := textValue(v)
	if err != nil {
		return "", queryErr("%s: %v", c.Label, err)
	}
	return "lower(" + b.arg(s) + "::text)", nil
}

func textValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	}
	return "", fmt.Errorf("give a single value, not %v", v)
}

func numberValue(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case json.Number:
		return t.Float64()
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("%q is not a number", t)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%v is not a number", v)
}

func columnKeys(f *FormDef) string {
	cols := f.Columns()
	keys := make([]string, len(cols))
	for i, c := range cols {
		keys[i] = c.Key
	}
	return strings.Join(keys, ", ")
}

// where builds the WHERE clause of q for the person vis describes.
func (s *Store) where(form *FormDef, vis Visibility, userID string, q Query) (*qb, error) {
	today := strings.TrimSpace(q.Today)
	if today == "" {
		today = time.Now().UTC().Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", today); err != nil {
		return nil, queryErr("today is a date (YYYY-MM-DD), not %q", q.Today)
	}
	b := &qb{form: form, userID: userID, today: today}
	b.visibility(vis)
	for _, f := range q.Filters {
		if err := b.filter(f); err != nil {
			return nil, err
		}
	}
	b.search(q.Search)
	return b, nil
}

// HasColumn reports whether a query may name key in the table.
func (f *FormDef) HasColumn(key string) bool {
	_, ok := f.column(key)
	return ok
}

// CheckFilters says what is wrong with filters on form's records, nil
// when the records query answers them: a widget's fixed filters are
// checked when they are saved, not when someone opens the dashboard.
func CheckFilters(form *FormDef, filters []Filter) error {
	b := &qb{form: form, today: time.Now().UTC().Format("2006-01-02")}
	for _, f := range filters {
		if err := b.filter(f); err != nil {
			return err
		}
	}
	return nil
}

// WithheldCodes are the codes of the records of form, a table keeping a
// dimension, that the person vis describes does not see: the members whose
// records are hidden from them. Nil when vis hides nothing.
func (s *Store) WithheldCodes(ctx context.Context, form *FormDef, vis Visibility) ([]string, error) {
	if !form.KeepsDimension() {
		return nil, nil
	}
	b := &qb{form: form}
	b.visibility(vis)
	scope, rest := b.conds[0], b.conds[1:]
	if len(rest) == 0 {
		return nil, nil
	}
	code := "r.data->>" + b.arg(form.CodeField().Name) + "::text"
	rows, err := s.db.Query(ctx, "SELECT DISTINCT "+code+" FROM runtime.form_record r WHERE "+scope+
		" AND COALESCE("+code+", '') <> '' AND NOT COALESCE(("+strings.Join(rest, " AND ")+"), false)", b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const recordColumns = `r.id::text, r.form_id::text, r.data, COALESCE(r.status::text, ''), r.created_by::text, r.created_at, r.updated_at`

// QueryRecords lists the records of form the person vis describes may see
// that match q, a page at a time, with how many match in all.
func (s *Store) QueryRecords(ctx context.Context, form *FormDef, vis Visibility, userID string, q Query) (*RecordsResult, error) {
	if q.IsTotals() {
		return nil, queryErr("a records list has no group_by or aggregates")
	}
	limit, err := pageLimit(q.Limit, DefaultRecordLimit, MaxRecordLimit, q.Offset)
	if err != nil {
		return nil, err
	}
	return s.queryRecords(ctx, form, vis, userID, q, limit)
}

// ExportRecords is QueryRecords without pages: every matching record, up
// to MaxExportRecords.
func (s *Store) ExportRecords(ctx context.Context, form *FormDef, vis Visibility, userID string, q Query) ([]*FormRecord, error) {
	q.Offset = 0
	res, err := s.queryRecords(ctx, form, vis, userID, q, MaxExportRecords)
	if err != nil {
		return nil, err
	}
	return res.Records, nil
}

func (s *Store) queryRecords(ctx context.Context, form *FormDef, vis Visibility, userID string, q Query, limit int) (*RecordsResult, error) {
	b, err := s.where(form, vis, userID, q)
	if err != nil {
		return nil, err
	}
	where := strings.Join(b.conds, " AND ")
	countArgs := slices.Clone(b.args)
	order := make([]string, 0, len(q.Sort)+2)
	for _, so := range q.Sort {
		c, ok := form.column(so.Field)
		if !ok {
			return nil, queryErr("cannot sort by %q: it is not a field of %s (its fields: %s)", so.Field, form.Label, columnKeys(form))
		}
		order = append(order, b.expr(c)+direction(so.Desc))
	}
	order = append(order, "r.created_at DESC", "r.id DESC")

	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record r WHERE `+where, countArgs...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count records: %w", err)
	}
	sql := `SELECT ` + recordColumns + ` FROM runtime.form_record r WHERE ` + where +
		` ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ` + b.arg(limit) + ` OFFSET ` + b.arg(q.Offset)
	rows, err := s.db.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, fmt.Errorf("query records: %w", err)
	}
	defer rows.Close()
	res := &RecordsResult{Records: []*FormRecord{}, Total: total}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		res.Records = append(res.Records, r)
	}
	return res, rows.Err()
}

func direction(desc bool) string {
	if desc {
		return " DESC NULLS LAST"
	}
	return " ASC NULLS LAST"
}

func pageLimit(limit, def, max, offset int) (int, error) {
	if offset < 0 || offset > maxOffset {
		return 0, queryErr("offset must be between 0 and %d", maxOffset)
	}
	if limit == 0 {
		return def, nil
	}
	if limit < 1 || limit > max {
		return 0, queryErr("limit must be between 1 and %d", max)
	}
	return limit, nil
}

// VisibleRecord is record id of form, when the person vis describes may
// see it; ErrRecordNotFound otherwise.
func (s *Store) VisibleRecord(ctx context.Context, form *FormDef, vis Visibility, id string) (*FormRecord, error) {
	b := &qb{form: form}
	b.visibility(vis)
	b.conds = append(b.conds, "r.id = "+b.arg(id)+"::uuid")
	r, err := scanRecord(s.db.QueryRow(ctx, `SELECT `+recordColumns+` FROM runtime.form_record r WHERE `+strings.Join(b.conds, " AND "), b.args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("record %s: %w", id, ErrRecordNotFound)
	}
	return r, err
}

// VisibleRecordsPage lists up to limit of the records of form the person
// vis describes may see, newest first, after `after` (from the start when
// nil): the record list's pages. A record withheld from the person neither
// fills a page nor ends the list early.
func (s *Store) VisibleRecordsPage(ctx context.Context, form *FormDef, vis Visibility, after *RecordPosition, limit int) ([]*FormRecord, error) {
	b := &qb{form: form}
	b.visibility(vis)
	if after != nil {
		b.conds = append(b.conds, "(r.created_at, r.id) < ("+b.arg(after.CreatedAt)+"::timestamptz, "+b.arg(after.ID)+"::uuid)")
	}
	rows, err := s.db.Query(ctx, `SELECT `+recordColumns+` FROM runtime.form_record r WHERE `+strings.Join(b.conds, " AND ")+
		` ORDER BY r.created_at DESC, r.id DESC LIMIT `+b.arg(limit), b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FormRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// group periods: the to_char format a date is grouped by.
var periodFormats = map[string]string{
	"day": "YYYY-MM-DD", "week": `IYYY-"W"IW`, "month": "YYYY-MM", "quarter": `YYYY-"Q"Q`, "year": "YYYY",
}

// QueryTotals answers a totals query over the records of form the person
// vis describes may see: one row per group of q.GroupBy (one row in all
// without it) holding each of q.Aggregates (a count without any).
func (s *Store) QueryTotals(ctx context.Context, form *FormDef, vis Visibility, userID string, q Query) (*TotalsResult, error) {
	limit, err := pageLimit(q.Limit, DefaultGroupLimit, MaxGroupLimit, q.Offset)
	if err != nil {
		return nil, err
	}
	b, err := s.where(form, vis, userID, q)
	if err != nil {
		return nil, err
	}
	aggs := q.Aggregates
	if len(aggs) == 0 {
		aggs = []Aggregate{{Op: "count"}}
	}
	if len(q.GroupBy) > 4 {
		return nil, queryErr("group by at most 4 fields")
	}

	var selects []string
	res := &TotalsResult{Rows: []map[string]any{}}
	type labelled struct {
		key, dimensionID string
		user             bool
	}
	var labels []labelled
	keys := map[string]int{} // result key → 1-based position in the SELECT
	for _, g := range q.GroupBy {
		c, ok := form.column(g.Field)
		if !ok {
			return nil, queryErr("cannot group by %q: it is not a field of %s (its fields: %s)", g.Field, form.Label, columnKeys(form))
		}
		e := b.expr(c)
		key := c.Key
		kind := c.Kind
		if g.Period != "" {
			format, ok := periodFormats[strings.ToLower(g.Period)]
			if !ok {
				return nil, queryErr("%q is not a period (day, week, month, quarter, year)", g.Period)
			}
			if c.Kind != KindDate && c.Kind != KindDateTime {
				return nil, queryErr("only a date is grouped by %s; %s is %s", g.Period, c.Label, c.Kind)
			}
			if c.Kind == KindDateTime {
				e = "(" + e + " AT TIME ZONE 'UTC')"
			}
			e = "to_char(" + e + ", '" + format + "')"
			key = c.Key + "_" + strings.ToLower(g.Period)
			kind = KindText
		} else {
			switch c.Kind {
			case KindNumber:
				e += "::float8"
			case KindDate, KindDateTime:
				e = "to_char(" + e + ", 'YYYY-MM-DD')"
				kind = KindText
			}
		}
		if _, dup := keys[key]; dup {
			return nil, queryErr("%s is grouped by twice", key)
		}
		selects = append(selects, e)
		keys[key] = len(selects)
		res.Columns = append(res.Columns, ResultColumn{Key: key, Label: c.Label, Kind: kind})
		if c.DimensionID != "" || c.Kind == KindUser {
			labels = append(labels, labelled{key: key, dimensionID: c.DimensionID, user: c.Kind == KindUser})
			res.Columns = append(res.Columns, ResultColumn{Key: key + "_label", Label: c.Label + " (name)", Kind: KindText})
		}
	}
	nGroups := len(selects)
	for _, a := range aggs {
		op := strings.ToLower(strings.TrimSpace(a.Op))
		a.Op = op
		var e string
		kind := KindNumber
		switch op {
		case "count":
			if a.Field != "" {
				return nil, queryErr("count counts records; to count the different values of %s use count_distinct", a.Field)
			}
			e = "count(*)"
		case "count_distinct", "sum", "avg", "min", "max":
			c, ok := form.column(a.Field)
			if !ok {
				return nil, queryErr("%s needs a field of %s (its fields: %s)", op, form.Label, columnKeys(form))
			}
			fe := b.expr(c)
			switch op {
			case "count_distinct":
				e = "count(DISTINCT " + fe + ")"
			case "sum", "avg":
				if c.Kind != KindNumber {
					return nil, queryErr("%s adds up numbers; %s is %s", op, c.Label, c.Kind)
				}
				e = op + "(" + fe + ")::float8"
			default:
				switch c.Kind {
				case KindNumber:
					e = op + "(" + fe + ")::float8"
				case KindDate, KindDateTime:
					e = "to_char(" + op + "(" + fe + "), 'YYYY-MM-DD')"
					kind = KindText
				default:
					return nil, queryErr("%s compares numbers and dates; %s is %s", op, c.Label, c.Kind)
				}
			}
		default:
			return nil, queryErr("%q is not an aggregate (count, count_distinct, sum, avg, min, max)", a.Op)
		}
		key := a.Key()
		if _, dup := keys[key]; dup {
			return nil, queryErr("%s is asked for twice", key)
		}
		selects = append(selects, e)
		keys[key] = len(selects)
		label := strings.ReplaceAll(key, "_", " ")
		res.Columns = append(res.Columns, ResultColumn{Key: key, Label: label, Kind: kind})
	}

	order := make([]string, 0, len(q.Sort)+nGroups)
	for _, so := range q.Sort {
		pos, ok := keys[so.Field]
		if !ok {
			return nil, queryErr("cannot sort by %q: sort a totals query by one of its groups or aggregates", so.Field)
		}
		order = append(order, strconv.Itoa(pos)+direction(so.Desc))
	}
	groupBy := make([]string, nGroups)
	for i := range groupBy {
		groupBy[i] = strconv.Itoa(i + 1)
		order = append(order, strconv.Itoa(i+1)+" ASC NULLS LAST")
	}
	sql := `SELECT ` + strings.Join(selects, ", ") + `, count(*) OVER () FROM runtime.form_record r WHERE ` + strings.Join(b.conds, " AND ")
	if nGroups > 0 {
		sql += ` GROUP BY ` + strings.Join(groupBy, ", ")
	}
	if len(order) > 0 {
		sql += ` ORDER BY ` + strings.Join(order, ", ")
	}
	sql += ` LIMIT ` + b.arg(limit) + ` OFFSET ` + b.arg(q.Offset)
	rows, err := s.db.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, fmt.Errorf("query totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := map[string]any{}
		for key, pos := range keys {
			row[key] = vals[pos-1]
		}
		if n, ok := vals[len(vals)-1].(int64); ok {
			res.TotalGroups = int(n)
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Name each dimension member and person grouped by.
	for _, l := range labels {
		names := map[string]string{}
		var ids []string
		for _, row := range res.Rows {
			if v, ok := row[l.key].(string); ok && v != "" {
				ids = append(ids, v)
			}
		}
		if len(ids) > 0 {
			var lrows pgx.Rows
			if l.user {
				lrows, err = s.db.Query(ctx, `SELECT id::text, COALESCE(NULLIF(display_name, ''), email) FROM identity.user WHERE id::text = ANY($1)`, ids)
			} else {
				lrows, err = s.db.Query(ctx, `SELECT code, COALESCE(NULLIF(label, ''), code) FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = ANY($2)`, l.dimensionID, ids)
			}
			if err != nil {
				return nil, err
			}
			for lrows.Next() {
				var k, v string
				if err := lrows.Scan(&k, &v); err != nil {
					lrows.Close()
					return nil, err
				}
				names[k] = v
			}
			lrows.Close()
			if err := lrows.Err(); err != nil {
				return nil, err
			}
		}
		for _, row := range res.Rows {
			v, _ := row[l.key].(string)
			row[l.key+"_label"] = names[v]
		}
	}
	return res, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
