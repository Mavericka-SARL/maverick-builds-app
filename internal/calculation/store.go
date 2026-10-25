package calculation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/pkg/dbx"

	"google.golang.org/protobuf/types/known/timestamppb"

	calculationv1 "github.com/mavericks-engine/mavericks/gen/go/calculation/v1"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

type Store struct {
	pool *pgxpool.Pool
	db   dbx.DB // what the store\'s statements run on: the pool, or a transaction
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, db: pool} }

// NewStoreOn is NewStore on a pool or a transaction (dbx.DB): built on a
// transaction it writes nothing the transaction does not keep. Pool() is
// nil unless db is a pool.
func NewStoreOn(db dbx.DB) *Store {
	s := &Store{db: db}
	if p, ok := db.(*pgxpool.Pool); ok {
		s.pool = p
	}
	return s
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ── Partition state ───────────────────────────────────────────────────────────

func (s *Store) GetPartitionState(ctx context.Context, key string) (*calculationv1.PartitionState, error) {
	var status string
	var lastCalcAt *time.Time
	var errMsg *string

	err := s.db.QueryRow(ctx, `
		SELECT status::text, last_calc_at, error
		FROM runtime.metric_partition_state
		WHERE partition_key = $1
	`, key).Scan(&status, &lastCalcAt, &errMsg)

	if errors.Is(err, pgx.ErrNoRows) {
		return &calculationv1.PartitionState{
			Status: calculationv1.PartitionStatus_PARTITION_STATUS_DIRTY,
		}, nil
	}
	if err != nil {
		return nil, err
	}

	state := &calculationv1.PartitionState{
		Status: partitionStatusFromString(status),
	}
	if lastCalcAt != nil {
		state.LastCalcAt = timestamppb.New(*lastCalcAt)
	}
	if errMsg != nil {
		state.Error = *errMsg
	}
	return state, nil
}

// MarkDirty marks the given partition keys as dirty.
func (s *Store) MarkDirty(ctx context.Context, keys []string, modelID, metricID, revisionID, timePartition string) error {
	for _, key := range keys {
		_, err := s.db.Exec(ctx, `
			INSERT INTO runtime.metric_partition_state
			    (partition_key, model_id, metric_id, revision_id, time_partition, status)
			VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, 'dirty')
			ON CONFLICT (partition_key) DO UPDATE SET status = 'dirty', updated_at = now()
		`, key, modelID, metricID, revisionID, timePartition)
		if err != nil {
			return fmt.Errorf("mark dirty %s: %w", key, err)
		}
	}
	return nil
}

// MarkDirtyTx is MarkDirty run through a caller-owned transaction instead of
// s.pool, so a caller can mark partitions dirty atomically with whatever
// wrote the underlying facts. This closes a real gap: without it, a crash in
// the window between that caller's commit and its own (necessarily
// post-commit, pool-based) trigger of the actual claim-and-execute pass
// would leave newly-written facts un-recalculated with nothing ever
// noticing — there's no idempotency ledger anywhere in this codebase to
// catch it later. With the partition already durably dirty inside the same
// transaction as the write, any later recalculation pass safely picks it up;
// ClaimForCalculation's WHERE status='dirty' guard is what makes that safe.
func (s *Store) MarkDirtyTx(ctx context.Context, tx pgx.Tx, keys []string, modelID, metricID, revisionID, timePartition string) error {
	for _, key := range keys {
		_, err := tx.Exec(ctx, `
			INSERT INTO runtime.metric_partition_state
			    (partition_key, model_id, metric_id, revision_id, time_partition, status)
			VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, 'dirty')
			ON CONFLICT (partition_key) DO UPDATE SET status = 'dirty', updated_at = now()
		`, key, modelID, metricID, revisionID, timePartition)
		if err != nil {
			return fmt.Errorf("mark dirty %s: %w", key, err)
		}
	}
	return nil
}

// MarkCalculating is MarkDirty and ClaimForCalculation in one statement,
// for a pass that holds its revision's lock (lockRevision): the partition
// is created or set calculating. One round trip less per metric of a pass.
func (s *Store) MarkCalculating(ctx context.Context, key, modelID, metricID, revisionID, timePartition string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO runtime.metric_partition_state
		    (partition_key, model_id, metric_id, revision_id, time_partition, status)
		VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, 'calculating')
		ON CONFLICT (partition_key) DO UPDATE SET status = 'calculating', updated_at = now()
	`, key, modelID, metricID, revisionID, timePartition)
	if err != nil {
		return fmt.Errorf("mark calculating %s: %w", key, err)
	}
	return nil
}

// ClaimForCalculation atomically transitions a partition from dirty → calculating.
// Returns false if already claimed by another worker.
func (s *Store) ClaimForCalculation(ctx context.Context, key string) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'calculating', updated_at = now()
		WHERE partition_key = $1 AND status = 'dirty'
	`, key)
	return tag.RowsAffected() > 0, err
}

// MarkClean marks a partition as successfully calculated.
func (s *Store) MarkClean(ctx context.Context, key string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'clean', last_calc_at = now(), error = NULL, updated_at = now()
		WHERE partition_key = $1
	`, key)
	return err
}

// MarkError records a calculation failure for a partition.
func (s *Store) MarkError(ctx context.Context, key, errMsg string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'error', error = $2, updated_at = now()
		WHERE partition_key = $1
	`, key, errMsg)
	return err
}

// ── Metric values for calculation ─────────────────────────────────────────────

// MetricDef holds what the calculation engine needs about a metric.
type MetricDef struct {
	ID          string
	Name        string
	Formula     string
	IsInput     bool
	AggRule     string   // how to aggregate across members: sum | average | count | formula | rate
	DependsOnID []string // metric IDs this metric depends on

	// Operands for agg_rule 'rate' (Anaplan's Ratio summary): the total is
	// numerator_total / denominator_total rather than a combination of this
	// metric's own member values. Empty for every other rule.
	AggNumeratorID   string
	AggDenominatorID string

	// TimeSummary is how the metric aggregates ACROSS its time dimension
	// (sum | average | min | max | first | last | none); AggRule stays the
	// rule for every other dimension. Only meaningful for a metric
	// dimensioned by a time dimension.
	TimeSummary string
	// DependsOn carries each dependency with the time offsets it is read at
	// (the same IDs as DependsOnID, in the same order).
	DependsOn []metricformula.Edge
	// PicklistDimID is the dimension a pick-list metric's cells hold
	// members of (format "picklist"); "" for every other metric.
	PicklistDimID string
	// Text: format "text". A calculated one stores its formula's text in
	// calc_result.text_value (executeText); formulas never read it.
	Text bool
}

// LoadModelMetrics loads all metric definitions + dependency edges for one
// model/revision. Strictly revision-scoped (no NULL fallback): metric_def
// rows are always revision-specific copies (migrations 027/028), unlike
// dimension_def's genuine legacy/global-visibility case — mirrors
// internal/query/chart.go's loadAllMetricDefs exactly.
func (s *Store) LoadModelMetrics(ctx context.Context, modelID, revisionID string) (map[string]*MetricDef, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, name, COALESCE(formula,''), is_input, agg_rule,
		       COALESCE(agg_numerator_metric_id::text,''), COALESCE(agg_denominator_metric_id::text,''),
		       time_summary, COALESCE(picklist_dimension_id::text,''), format = 'text'
		FROM model.metric_def WHERE model_id = $1::uuid AND revision_id = $2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	defs := make(map[string]*MetricDef)
	for rows.Next() {
		var d MetricDef
		if err := rows.Scan(&d.ID, &d.Name, &d.Formula, &d.IsInput, &d.AggRule, &d.AggNumeratorID, &d.AggDenominatorID, &d.TimeSummary, &d.PicklistDimID, &d.Text); err != nil {
			return nil, err
		}
		defs[d.ID] = &d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load dependency edges
	edgeRows, err := s.db.Query(ctx, `
		SELECT d.metric_id::text, d.depends_on_metric_id::text,
		       d.min_time_offset, d.max_time_offset, d.unbounded_past, d.unbounded_future
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid
		ORDER BY d.metric_id, d.depends_on_metric_id
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var from string
		var e metricformula.Edge
		if err := edgeRows.Scan(&from, &e.To, &e.MinTimeOffset, &e.MaxTimeOffset, &e.UnboundedPast, &e.UnboundedFuture); err != nil {
			return nil, err
		}
		if def, ok := defs[from]; ok {
			def.DependsOnID = append(def.DependsOnID, e.To)
			def.DependsOn = append(def.DependsOn, e)
		}
	}
	return defs, edgeRows.Err()
}

// GetCalcValue returns the latest calculated value for a metric + partition.
func (s *Store) GetCalcValue(ctx context.Context, modelID, revisionID, metricID string, dimMembers map[string]string) (float64, error) {
	dimJSON, _ := json.Marshal(dimMembers)

	var value *float64
	err := s.db.QueryRow(ctx, `
		SELECT value FROM runtime.calc_result
		WHERE model_id = $1 AND revision_id = $2::uuid
		  AND metric_id = $3::uuid AND dim_members = $4
		ORDER BY calc_at DESC LIMIT 1
	`, modelID, revisionID, metricID, dimJSON).Scan(&value)

	if errors.Is(err, pgx.ErrNoRows) || value == nil {
		return 0, nil
	}
	return *value, err
}

// ── Dependency graph (mirrors model.graph but reads from DB) ──────────────────

// LoadDimIDToName returns a map of dimension_def.id → dimension_def.name for
// the given model/revision, used to translate stored {dim_id: member_code}
// combos into named variables that formulas can reference (e.g.
// "department"). Unlike metric_def, dimension_def has a genuine legacy/
// global-visibility case (revision_id IS NULL) — mirrors chart.go's
// loadAllDimensions exactly.
func (s *Store) LoadDimIDToName(ctx context.Context, modelID, revisionID string) (map[string]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id::text, name FROM model.dimension_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL)`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[id] = name
	}
	return m, rows.Err()
}

// ── Config-driven dimension/combo discovery ───────────────────────────────────
//
// Mirrors internal/query/chart.go's loadAllDimensions/loadMetricDimensionIDs
// exactly (both already revision-scoped there) — a metric's applicable
// intersections are its own DECLARED dimensions (grid_metric ⋈
// grid_dimension), enumerated to their full leaf-member set via
// rollup.LeafCombos, not whatever dim_members happen to already exist in
// fact_input (the old ListDistinctDimCombos*, removed: a department with no
// fact yet is still a real, zero-valued intersection, not an absent one).

// PropertyDecl is one declared member property of a dimension
// (model.dimension_property): its name as declared and its data_type.
type PropertyDecl struct {
	Name     string
	DataType string
}

// DimensionSchema is the declared shape of a revision's dimensions beyond
// what rollup.Dimension carries: each dimension's property declarations,
// and which dimension rows belong to the revision itself.
type DimensionSchema struct {
	// Properties maps dimension ID -> UPPER-CASE property name -> its
	// declaration. Property names match case-insensitively (contract C1).
	Properties map[string]map[string]PropertyDecl
	// RevisionOwned holds the IDs of dimension rows whose revision_id is the
	// revision itself. A revision-less legacy row may share a name with one
	// of them; a formula's dimension name then means the revision's own row.
	RevisionOwned map[string]bool
}

// LoadDimensionSchema loads the property declarations of every dimension in
// scope for the revision, scoped exactly like LoadAllDimensions (the
// revision's own rows and revision-less ones). Loaded once per
// recalculation pass.
func (s *Store) LoadDimensionSchema(ctx context.Context, modelID, revisionID string) (*DimensionSchema, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.id::text, d.revision_id IS NOT NULL, COALESCE(p.name,''), COALESCE(p.data_type,'')
		FROM model.dimension_def d
		LEFT JOIN model.dimension_property p ON p.dimension_id = d.id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.id, p.name
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schema := &DimensionSchema{Properties: map[string]map[string]PropertyDecl{}, RevisionOwned: map[string]bool{}}
	for rows.Next() {
		var dimID, name, dataType string
		var owned bool
		if err := rows.Scan(&dimID, &owned, &name, &dataType); err != nil {
			return nil, err
		}
		if owned {
			schema.RevisionOwned[dimID] = true
		}
		if name == "" {
			continue
		}
		props := schema.Properties[dimID]
		if props == nil {
			props = map[string]PropertyDecl{}
			schema.Properties[dimID] = props
		}
		key := strings.ToUpper(name)
		if _, dup := props[key]; !dup { // names differing only in case: the first (sorted) declaration wins
			props[key] = PropertyDecl{Name: name, DataType: dataType}
		}
	}
	return schema, rows.Err()
}

// LoadAllDimensions loads every dimension in the revision (regardless of
// grid), in the shape rollup.Resolve/rollup.LeafCombos need. Unlike
// chart.go's version, there is no hidden-member filtering here: the
// scheduler has no per-user/access-rule concept (it runs on a background
// path with no acting user),
// and a computed value must exist regardless of who can currently see it —
// exactly like fact_input itself is never access-filtered at write time.
func (s *Store) LoadAllDimensions(ctx context.Context, modelID, revisionID string) (map[string]*rollup.Dimension, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.id::text, COALESCE(d.parent_dimension_id::text,''),
		       COALESCE(d.source_dimension_id::text,''), COALESCE(d.source_property,''),
		       d.dimension_type = 'time', COALESCE(d.time_granularity,''), COALESCE(d.fiscal_year_start_month,0),
		       m.id::text, m.code, m.properties, COALESCE(pm.code,'') AS parent_code,
		       COALESCE(m.time_index,-1), m.period_start, m.period_end, COALESCE(btrim(m.formula),'')
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.name, m.time_index NULLS LAST, m.sort_order, m.code
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dims := make(map[string]*rollup.Dimension)
	for rows.Next() {
		var dimID, parentDimID, sourceDimID, sourceProp string
		var isTime bool
		var granularity string
		var fiscalStart int
		var memberID, code, parentCode string
		var properties []byte
		var timeIndex int
		var periodStart, periodEnd *time.Time
		var memberFormula string
		if err := rows.Scan(&dimID, &parentDimID, &sourceDimID, &sourceProp, &isTime, &granularity, &fiscalStart,
			&memberID, &code, &properties, &parentCode, &timeIndex, &periodStart, &periodEnd, &memberFormula); err != nil {
			return nil, err
		}
		dim, ok := dims[dimID]
		if !ok {
			dim = &rollup.Dimension{ID: dimID, ParentDimensionID: parentDimID, SourceDimensionID: sourceDimID, SourceProperty: sourceProp,
				IsTime: isTime, TimeGranularity: granularity, FiscalYearStartMonth: fiscalStart}
			dims[dimID] = dim
		}
		if memberFormula != "" {
			// A calculated member is no leaf and no child: nothing is
			// computed or stored at it, and no total includes it.
			dim.Calculated = append(dim.Calculated, rollup.CalculatedMember{Code: code, Formula: memberFormula})
			continue
		}
		var props map[string]string
		if len(properties) > 0 {
			_ = json.Unmarshal(properties, &props)
		}
		m := rollup.Member{ID: memberID, Code: code, ParentCode: parentCode, Properties: props, TimeIndex: timeIndex}
		if periodStart != nil {
			m.PeriodStart = periodStart.UTC()
		}
		if periodEnd != nil {
			m.PeriodEnd = periodEnd.UTC()
		}
		dim.Members = append(dim.Members, m)
	}
	return dims, rows.Err()
}

// LoadMetricDimensionIDs returns each metric's own native dimension IDs, via
// grid_metric -> grid_dimension on whatever grid it actually lives on.
// Unambiguous per metric: migration 051_grid_metric_unique.sql gives
// grid_metric a bare UNIQUE(metric_id) constraint, so a metric belongs to at
// most one grid. Mirrors chart.go's loadMetricDimensionIDs exactly.
func (s *Store) LoadMetricDimensionIDs(ctx context.Context, modelID, revisionID string) (map[string][]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT gm.metric_id::text, gd.dimension_id::text
		FROM model.grid_metric gm
		JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
		ORDER BY gm.metric_id, d.name
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dims := make(map[string][]string)
	for rows.Next() {
		var metricID, dimID string
		if err := rows.Scan(&metricID, &dimID); err != nil {
			return nil, err
		}
		dims[metricID] = append(dims[metricID], dimID)
	}
	return dims, rows.Err()
}

// ── Bulk value prefetch ────────────────────────────────────────────────────────
//
// One query per dependency metric (not one per leaf combo) — load-bearing
// for making full leaf-combo enumeration viable instead of the fact-driven
// "only combos that already have data" shortcut. Row keys are built by
// parsing dim_members::text back into a map and re-marshaling it via Go's
// own json.Marshal (dimKey) — not by using Postgres's own JSONB text form
// directly as the map key. Postgres's JSONB key ordering is not guaranteed
// to match Go's encoding/json (which sorts map keys deterministically), so
// comparing across the two would silently produce always-missing lookups.

// dimKey canonicalizes a combo into a stable map key — used both when
// building the maps below from DB rows and when looking a combo up in them,
// so both sides always go through the same Go json.Marshal call.
func dimKey(dimMembers map[string]string) string {
	// json.Marshal's own output, built directly when no key or code needs
	// escaping (dimension IDs are UUIDs, codes are mostly plain): every
	// read of a dependency value builds one, and json.Marshal's reflection
	// was a tenth of a recalculation's time (2026-10-20).
	var keys [8]string
	if dimMembers == nil || len(dimMembers) > len(keys) {
		b, _ := json.Marshal(dimMembers)
		return string(b)
	}
	n, size := 0, 2
	for k, v := range dimMembers {
		if !plainJSON(k) || !plainJSON(v) {
			b, _ := json.Marshal(dimMembers)
			return string(b)
		}
		i := n
		for i > 0 && keys[i-1] > k {
			keys[i] = keys[i-1]
			i--
		}
		keys[i] = k
		n++
		size += len(k) + len(v) + 6
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteByte('{')
	for i, k := range keys[:n] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(k)
		b.WriteString(`":"`)
		b.WriteString(dimMembers[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// keyOf is dimKey of the combo a row's dim_members::text holds; false when
// it is no object of strings. PostgreSQL writes a jsonb object as
// {"k": "v", "k2": "v2"}, which is read here directly when no string needs
// an escape — every calculated value a pass or a grid read loads goes
// through it, and json.Unmarshal took most of a 19,500-row read
// (2026-10-20).
func keyOf(raw string) (string, bool) {
	if k, ok := plainObjectKey(raw); ok {
		return k, true
	}
	var dm map[string]string
	if err := json.Unmarshal([]byte(raw), &dm); err != nil {
		return "", false
	}
	return dimKey(dm), true
}

// plainObjectKey is keyOf's direct path: raw is a flat object of at most
// eight string members, none with an escape or a character json.Marshal
// writes escaped.
func plainObjectKey(raw string) (string, bool) {
	var keys, vals [8]string
	n, i := 0, 0
	skip := func() {
		for i < len(raw) && raw[i] == ' ' {
			i++
		}
	}
	str := func() (string, bool) {
		if i >= len(raw) || raw[i] != '"' {
			return "", false
		}
		j := i + 1
		for j < len(raw) && raw[j] != '"' {
			j++
		}
		if j >= len(raw) {
			return "", false
		}
		v := raw[i+1 : j]
		if !plainJSON(v) {
			return "", false
		}
		i = j + 1
		return v, true
	}
	if len(raw) < 2 || raw[0] != '{' {
		return "", false
	}
	i = 1
	skip()
	if i < len(raw) && raw[i] == '}' {
		return "{}", i == len(raw)-1
	}
	for {
		skip()
		k, ok := str()
		if !ok {
			return "", false
		}
		skip()
		if i >= len(raw) || raw[i] != ':' {
			return "", false
		}
		i++
		skip()
		v, ok := str()
		if !ok || n == len(keys) {
			return "", false
		}
		at := n
		for at > 0 && keys[at-1] > k {
			keys[at], vals[at] = keys[at-1], vals[at-1]
			at--
		}
		if at > 0 && keys[at-1] == k {
			return "", false // a repeated key: json.Unmarshal's rules
		}
		keys[at], vals[at] = k, v
		n++
		skip()
		if i >= len(raw) {
			return "", false
		}
		if raw[i] == '}' {
			if i != len(raw)-1 {
				return "", false
			}
			break
		}
		if raw[i] != ',' {
			return "", false
		}
		i++
	}
	var b strings.Builder
	b.Grow(len(raw))
	b.WriteByte('{')
	for j := 0; j < n; j++ {
		if j > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(keys[j])
		b.WriteString(`":"`)
		b.WriteString(vals[j])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), true
}

// plainJSON reports whether s is written by encoding/json as itself between
// quotes: printable ASCII other than the characters it escapes.
func plainJSON(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}

// inputValueMapSQL is LoadInputValueMap's statement ($1 model, $2 revision,
// $3 metric). The form-covered combos (normally none) are read once, as the
// grid's own read does (817d927): a NOT EXISTS correlated to fact_input
// itself was planned, under the prepared statement's generic plan, as a
// nested loop rescanning every row of the metric per combo — 315ms instead
// of 4ms for 504 combos, and a recalculation reads every input it needs
// (actual-through-month in the CPG model: 4.4s → 0.8s, 2026-10-19).
const inputValueMapSQL = `
		WITH covered AS MATERIALIZED (
			SELECT DISTINCT fi.dim_members
			FROM runtime.fact_input fi
			JOIN model.form_metric_mapping fmm ON fmm.id = fi.source_ref
			WHERE fi.model_id=$1::uuid AND fi.revision_id=$2::uuid AND fi.metric_id=$3::uuid
			  AND fmm.aggregation IN ('replace', 'last')
		)
		SELECT dim_members::text, SUM(value)::float8
		FROM (
			SELECT direct.dim_members, direct.value FROM (
				SELECT DISTINCT ON (dim_members) dim_members, value
				FROM runtime.fact_input
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
				  AND source_ref IS NULL
				ORDER BY dim_members, entered_at DESC, id DESC
			) direct
			WHERE NOT EXISTS (SELECT 1 FROM covered c WHERE c.dim_members = direct.dim_members)
			UNION ALL
			SELECT dim_members, value FROM runtime.fact_input
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
			  AND source_ref IS NOT NULL
		) fi
		GROUP BY dim_members
	`

// LoadInputValueMap returns metricID's latest recorded value at every
// dim_members combo it has ever been entered against (including '{}', a
// legitimate combo for a dimensionless metric), keyed by dimKey.
// LoadInputValueMap reads an input metric's effective value per combo using
// the SAME semantics as the grid's own cell query (grid() in the gateway) and
// the chart's loadInputFactMap: directly-typed rows are latest-wins per combo
// and excluded where a form 'replace'/'last' mapping covers the cell; ALL
// form/import-sourced rows (source_ref set) are summed additively on top.
//
// It used to be a plain DISTINCT ON over every row regardless of source —
// one latest row per combo — which silently diverged from what the grid
// displays wherever source_ref rows exist: an incremental import appending
// five rows at one combo showed their SUM in the grid, while every calc
// metric depending on it was computed from just the newest single row.
// Found live on the 500-product Test model (margin off by 22k from the
// grid's own revenue−cost) once precomputed slice totals were compared
// against the grid's scoped recompute.
func (s *Store) LoadInputValueMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]float64, error) {
	rows, err := s.db.Query(ctx, inputValueMapSQL, modelID, revisionID, metricID)
	return scanDimValueMap(rows, err)
}

// LoadInputTextMap is LoadInputValueMap for a text input: each cell's
// latest typed note, keyed by dimKey, as the grid shows it (loadTextCells).
func (s *Store) LoadInputTextMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (dim_members) dim_members::text, COALESCE(text_value, '')
		FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND source_ref IS NULL
		ORDER BY dim_members, entered_at DESC, id DESC
	`, modelID, revisionID, metricID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var raw, text string
		if err := rows.Scan(&raw, &text); err != nil {
			return nil, err
		}
		k, ok := keyOf(raw)
		if !ok || text == "" {
			continue
		}
		out[k] = text
	}
	return out, rows.Err()
}

// LoadCalcTextMap is LoadInputTextMap for a calculated text: the text its
// formula gave at every cell, keyed by dimKey.
func (s *Store) LoadCalcTextMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT dim_members::text, text_value FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND text_value IS NOT NULL
	`, modelID, revisionID, metricID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var raw, text string
		if err := rows.Scan(&raw, &text); err != nil {
			return nil, err
		}
		if k, ok := keyOf(raw); ok {
			out[k] = text
		}
	}
	return out, rows.Err()
}

// LoadCalcValueMap is LoadInputValueMap's runtime.calc_result analog.
func (s *Store) LoadCalcValueMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]float64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (dim_members) dim_members::text, value
		FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		ORDER BY dim_members, calc_at DESC
	`, modelID, revisionID, metricID)
	return scanDimValueMap(rows, err)
}

// LoadAllCalcValueMaps is LoadCalcValueMap generalized across every metric in
// the revision in one round trip, instead of one query per metric — the
// read-path analog of executePartition's own bulk-prefetch-then-resolve
// pattern (see the fetch map built in executePartition), for a caller (grid())
// that needs every calc metric's full per-combo history at once rather than
// one dependency at a time. Same DISTINCT ON + calc_at DESC "latest wins"
// semantics and the same dimKey canonicalization as LoadCalcValueMap.
func (s *Store) LoadAllCalcValueMaps(ctx context.Context, modelID, revisionID string) (map[string]map[string]float64, error) {
	return s.LoadAllCalcValueMapsScoped(ctx, modelID, revisionID, "", nil)
}

// LoadCalcTotals returns just each calc metric's '{}' company-wide aggregate
// row (metricID -> value), skipping the per-combo lattice entirely. For a
// totals-only, unscoped grid read this is ~one row per metric instead of the
// tens of thousands LoadAllCalcValueMaps pulls back on a large model.
func (s *Store) LoadCalcTotals(ctx context.Context, modelID, revisionID string) (map[string]float64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (metric_id) metric_id::text, value
		FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members = '{}'::jsonb
		ORDER BY metric_id, calc_at DESC
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var metricID string
		var value float64
		if err := rows.Scan(&metricID, &value); err != nil {
			return nil, err
		}
		out[metricID] = value
	}
	return out, rows.Err()
}

// LoadCalcSlice returns metricID -> value for every calc_result row whose
// dim_members exactly equals sliceJSON — the persisted single-dimension slice
// rows ({oneDim: member}). A totals-only grid read that pins exactly that one
// dimension serves each calc metric's total from here in O(1) instead of
// re-resolving the slice. A metric absent from the result has no slice row
// (an eval-skipped combo, or a revision mid-recompute) and the caller must
// fall back to the live recompute for the whole read.
func (s *Store) LoadCalcSlice(ctx context.Context, modelID, revisionID, sliceJSON string) (map[string]float64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (metric_id) metric_id::text, value
		FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members=$3::jsonb
		ORDER BY metric_id, calc_at DESC
	`, modelID, revisionID, sliceJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var metricID string
		var value float64
		if err := rows.Scan(&metricID, &value); err != nil {
			return nil, err
		}
		out[metricID] = value
	}
	return out, rows.Err()
}

// LoadAllCalcValueMapsScoped is LoadAllCalcValueMaps with an optional context
// filter: scopeSQL is an " AND (...)" fragment referencing dim_members and
// bind params $3+ (supplied in scopeArgs), so the grid can load only the
// calc rows for a pinned context instead of the whole model's rollup lattice
// — the difference between a sub-second and a multi-second read at 500
// members. scopeSQL == "" loads everything (the unscoped call above).
func (s *Store) LoadAllCalcValueMapsScoped(ctx context.Context, modelID, revisionID, scopeSQL string, scopeArgs []any) (map[string]map[string]float64, error) {
	args := append([]any{modelID, revisionID}, scopeArgs...)
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (metric_id, dim_members) metric_id::text, dim_members::text, value
		FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid`+scopeSQL+`
		ORDER BY metric_id, dim_members, calc_at DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]float64)
	for rows.Next() {
		var metricID, raw string
		var value float64
		if err := rows.Scan(&metricID, &raw, &value); err != nil {
			return nil, err
		}
		k, ok := keyOf(raw)
		if !ok {
			continue
		}
		if out[metricID] == nil {
			out[metricID] = make(map[string]float64)
		}
		out[metricID][k] = value
	}
	return out, rows.Err()
}

func scanDimValueMap(rows pgx.Rows, err error) (map[string]float64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var raw string
		var value float64
		if err := rows.Scan(&raw, &value); err != nil {
			return nil, err
		}
		k, ok := keyOf(raw)
		if !ok {
			continue
		}
		out[k] = value
	}
	return out, rows.Err()
}

// ── Batched per-intersection write ──────────────────────────────────────────

// CalcResultRow is one intersection's calculated value, for ReplaceResultsTx.
type CalcResultRow struct {
	DimMembers map[string]string
	Value      float64
	Text       *string // a text calculation's result, beside a 0 Value
}

// ReplaceResultsTx makes a metric's calc_result rows — its '{}' total and
// every intersection — the given set, inside tx, writing only what
// changed: a cell whose value is unchanged keeps its row, a changed one
// gets a new row, and every other row of the metric goes, earlier rows of
// a cell included. The set is authoritative: an intersection whose facts
// were deleted (full_reload, member removal) and that a pass therefore
// skips must stop serving its last value — found live when junk
// test-writes (revenue 4, cost 3) kept showing margin_pct=25 at an
// intersection whose facts had been wiped. A pass with no total leaves no
// total either (time_summary 'none', a blank total).
//
// Each pass used to delete every cell and insert it again, and to add a
// '{}' total without removing the last one, so a busy workspace's
// calc_result was mostly rows nobody would read again: one tenant held
// 260 MB for 200,000 live rows after 7.3 million deletes.
func (s *Store) ReplaceResultsTx(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID string, rows []CalcResultRow) error {
	if len(rows) >= copyRowsAt {
		return replaceResultsByCopy(ctx, tx, modelID, revisionID, metricID, rowsSource(rows))
	}
	dims := make([]string, len(rows))
	values := make([]float64, len(rows))
	texts := make([]*string, len(rows))
	for i, r := range rows {
		dimJSON, _ := json.Marshal(r.DimMembers)
		dims[i], values[i], texts[i] = string(dimJSON), r.Value, r.Text
	}
	// Values go as numeric, every digit of the float64 kept (a float8 cast
	// keeps 15): a pick-list option's code is a 16-digit number.
	//
	// Planned for these arrays each time (QueryExecModeCacheDescribe): a
	// statement prepared once and run again gets Postgres's generic plan
	// after five runs, which takes an array parameter for 10 rows and keep
	// for 1, and so checked keep once per result row — 108,000 results
	// took minutes instead of a second, and a pass after every import
	// held the revision's calculations up behind it.
	//
	// gone tests keep with NOT IN, not NOT EXISTS: keep is a CTE whose size
	// the planner guesses as one row, and NOT EXISTS against it was planned
	// as a nested-loop anti join — every row of the metric compared with
	// every kept row, 190 million comparisons and 30 s for a 19,503-cell
	// metric, on every recalculation. NOT IN is a hashed subplan whatever
	// the guess (keep's id and calc_at are never NULL).
	_, err := tx.Exec(ctx, `
		WITH new AS (
		    SELECT d::jsonb AS dims, v AS value, t AS text
		    FROM unnest($4::text[], $5::numeric[], $6::text[]) AS n(d, v, t)),
		keep AS (
		    SELECT DISTINCT ON (c.dim_members) c.id, c.calc_at, c.dim_members
		    FROM runtime.calc_result c JOIN new n ON n.dims = c.dim_members
		    WHERE c.model_id = $1::uuid AND c.revision_id = $2::uuid AND c.metric_id = $3::uuid
		      AND c.value = n.value AND c.text_value IS NOT DISTINCT FROM n.text
		    ORDER BY c.dim_members, c.calc_at DESC),
		gone AS (
		    DELETE FROM runtime.calc_result c
		    WHERE c.model_id = $1::uuid AND c.revision_id = $2::uuid AND c.metric_id = $3::uuid
		      AND (c.id, c.calc_at) NOT IN (SELECT k.id, k.calc_at FROM keep k))
		INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, text_value)
		SELECT $1::uuid, $2::uuid, n.dims, $3::uuid, n.value, n.text
		FROM new n WHERE NOT EXISTS (SELECT 1 FROM keep k WHERE k.dim_members = n.dims)
	`, pgx.QueryExecModeCacheDescribe, modelID, revisionID, metricID, dims, values, texts)
	return err
}

// resultSource is a result set read one row at a time: rows already built
// (rowsSource), or a metric's leaves held as member indices (lean.go),
// whose rows are built only as they are written.
type resultSource interface {
	Len() int
	Row(i int) (dims map[string]string, value float64, text *string)
}

// rowsSource is resultSource over rows.
type rowsSource []CalcResultRow

func (r rowsSource) Len() int { return len(r) }
func (r rowsSource) Row(i int) (map[string]string, float64, *string) {
	return r[i].DimMembers, r[i].Value, r[i].Text
}

// sources is several result sets read as one, in order.
type sources []resultSource

func (ss sources) Len() int {
	n := 0
	for _, s := range ss {
		n += s.Len()
	}
	return n
}
func (ss sources) Row(i int) (map[string]string, float64, *string) {
	for _, s := range ss {
		if i < s.Len() {
			return s.Row(i)
		}
		i -= s.Len()
	}
	panic("sources: row out of range")
}

// ReplaceResultSourceTx is ReplaceResultsTx for a result set read row by
// row: always written through COPY.
func (s *Store) ReplaceResultSourceTx(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID string, rows resultSource) error {
	return replaceResultsByCopy(ctx, tx, modelID, revisionID, metricID, rows)
}

// copyRowsAt is the result set size from which ReplaceResultsTx streams
// the rows with COPY instead of sending them as array parameters.
var copyRowsAt = 20_000

// replaceResultsByCopy is ReplaceResultsTx for a large set. As one
// statement, the set went as array parameters: every row JSON-encoded, then
// encoded again by the driver — 2.2 GB of the gateway for two metrics of
// 2.5 million cells, and a Postgres backend that grew past 5 GB unnesting
// them (a tester's model, 2026-10-25). Here the rows are encoded one at a
// time into COPY, and the replace runs from temporary tables Postgres has
// statistics for, so its joins spill to disk instead of holding the set in
// memory. Same result as the statement: unchanged cells keep their row,
// every other row of the metric goes, new and changed cells are inserted.
func replaceResultsByCopy(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID string, rows resultSource) error {
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE calc_result_new (dims jsonb NOT NULL, value numeric NOT NULL, text text) ON COMMIT DROP;
		CREATE TEMP TABLE calc_result_old (id uuid NOT NULL, calc_at timestamptz NOT NULL, dims jsonb NOT NULL, value numeric, text text) ON COMMIT DROP;
		CREATE TEMP TABLE calc_result_keep (id uuid NOT NULL, calc_at timestamptz NOT NULL, dims jsonb NOT NULL) ON COMMIT DROP`); err != nil {
		return err
	}
	var dimJSON []byte
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"calc_result_new"}, []string{"dims", "value", "text"},
		pgx.CopyFromSlice(rows.Len(), func(i int) ([]any, error) {
			dims, value, text := rows.Row(i)
			var err error
			if dimJSON, err = json.Marshal(dims); err != nil {
				return nil, err
			}
			return []any{string(dimJSON), value, text}, nil
		})); err != nil {
		return fmt.Errorf("copy the results: %w", err)
	}
	// Every join is between temporary tables just analyzed: the metric's
	// rows read straight from calc_result were estimated at one row (its
	// statistics lag a pass's writes), and the anti join against the kept
	// rows ran as a nested loop — 7 s for 20,000 cells, quadratic.
	for _, stmt := range []string{
		`ANALYZE calc_result_new`,
		`INSERT INTO calc_result_old (id, calc_at, dims, value, text)
		 SELECT c.id, c.calc_at, c.dim_members, c.value, c.text_value FROM runtime.calc_result c
		 WHERE c.model_id = $1::uuid AND c.revision_id = $2::uuid AND c.metric_id = $3::uuid`,
		`ANALYZE calc_result_old`,
		`INSERT INTO calc_result_keep (id, calc_at, dims)
		 SELECT DISTINCT ON (o.dims) o.id, o.calc_at, o.dims
		 FROM calc_result_old o JOIN calc_result_new n ON n.dims = o.dims
		 WHERE o.value = n.value AND o.text IS NOT DISTINCT FROM n.text
		 ORDER BY o.dims, o.calc_at DESC`,
		`ANALYZE calc_result_keep`,
		`DELETE FROM runtime.calc_result c USING calc_result_old o
		 WHERE c.id = o.id AND c.calc_at = o.calc_at
		   AND c.model_id = $1::uuid AND c.revision_id = $2::uuid AND c.metric_id = $3::uuid
		   AND NOT EXISTS (SELECT 1 FROM calc_result_keep k WHERE k.id = o.id AND k.calc_at = o.calc_at)`,
		`INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, text_value)
		 SELECT $1::uuid, $2::uuid, n.dims, $3::uuid, n.value, n.text
		 FROM calc_result_new n WHERE NOT EXISTS (SELECT 1 FROM calc_result_keep k WHERE k.dims = n.dims)`,
		`DROP TABLE calc_result_new, calc_result_old, calc_result_keep`,
	} {
		var err error
		if strings.Contains(stmt, "$1") {
			_, err = tx.Exec(ctx, stmt, modelID, revisionID, metricID)
		} else {
			_, err = tx.Exec(ctx, stmt)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ApplyResultsDiffTx changes a metric's result set by a difference: it
// deletes the rows at del (dimKeys) and inserts ins. It reports whether
// the metric then holds want rows — the size of the set the difference
// was taken against; when it does not, the caller replaces the whole set.
// Planned for its arrays each time, as ReplaceResultsTx is.
func (s *Store) ApplyResultsDiffTx(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID string, del []string, ins []CalcResultRow, want int) (bool, error) {
	if len(del) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM runtime.calc_result
			WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id = $3::uuid
			  AND dim_members IN (SELECT k::jsonb FROM unnest($4::text[]) AS u(k))
		`, pgx.QueryExecModeCacheDescribe, modelID, revisionID, metricID, del); err != nil {
			return false, err
		}
	}
	if len(ins) > 0 {
		dims := make([]string, len(ins))
		values := make([]float64, len(ins))
		texts := make([]*string, len(ins))
		for i, r := range ins {
			dims[i], values[i], texts[i] = dimKey(r.DimMembers), r.Value, r.Text
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, text_value)
			SELECT $1::uuid, $2::uuid, d::jsonb, $3::uuid, v, t
			FROM unnest($4::text[], $5::numeric[], $6::text[]) AS n(d, v, t)
		`, pgx.QueryExecModeCacheDescribe, modelID, revisionID, metricID, dims, values, texts); err != nil {
			return false, err
		}
	}
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM runtime.calc_result
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id = $3::uuid
	`, modelID, revisionID, metricID).Scan(&n); err != nil {
		return false, err
	}
	return n == want, nil
}

// LoadPartitionStatuses returns each metric's partition status in
// timePartition (dirty, calculating, clean, error); a metric with no
// partition there is absent.
func (s *Store) LoadPartitionStatuses(ctx context.Context, modelID, revisionID, timePartition string) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT metric_id::text, status FROM runtime.metric_partition_state
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND time_partition = $3
	`, modelID, revisionID, timePartition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

// ClearAllResults deletes every calc_result row of a metric, the '{}'
// total included: the state of a metric whose every intersection has no
// data (all of it deleted) — absent, never its last total.
func (s *Store) ClearAllResults(ctx context.Context, modelID, revisionID, metricID string) error {
	return clearResults(ctx, s.pool, modelID, revisionID, metricID)
}

// ClearAllResultsTx is ClearAllResults inside tx.
func (s *Store) ClearAllResultsTx(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID string) error {
	return clearResults(ctx, tx, modelID, revisionID, metricID)
}

func clearResults(ctx context.Context, w resultWriter, modelID, revisionID, metricID string) error {
	_, err := w.Exec(ctx, `
		DELETE FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid`,
		modelID, revisionID, metricID)
	return err
}

// InTx runs fn in one transaction, committed when fn returns nil and
// rolled back otherwise. A metric's result set is replaced through it
// (ReplaceResultsTx), so a concurrent reader sees either the previous set
// or the new one, and a failed write keeps the last good rows.
func (s *Store) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resultWriter is the subset of pgx shared by a pool and a transaction.
type resultWriter interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func partitionStatusFromString(s string) calculationv1.PartitionStatus {
	switch s {
	case "clean":
		return calculationv1.PartitionStatus_PARTITION_STATUS_CLEAN
	case "dirty":
		return calculationv1.PartitionStatus_PARTITION_STATUS_DIRTY
	case "calculating":
		return calculationv1.PartitionStatus_PARTITION_STATUS_CALCULATING
	case "error":
		return calculationv1.PartitionStatus_PARTITION_STATUS_ERROR
	default:
		return calculationv1.PartitionStatus_PARTITION_STATUS_DIRTY
	}
}
