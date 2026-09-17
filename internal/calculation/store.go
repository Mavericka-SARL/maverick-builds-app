package calculation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	calculationv1 "github.com/mavericks-engine/mavericks/gen/go/calculation/v1"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ── Partition state ───────────────────────────────────────────────────────────

func (s *Store) GetPartitionState(ctx context.Context, key string) (*calculationv1.PartitionState, error) {
	var status string
	var lastCalcAt *time.Time
	var errMsg *string

	err := s.pool.QueryRow(ctx, `
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
		_, err := s.pool.Exec(ctx, `
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

// ClaimForCalculation atomically transitions a partition from dirty → calculating.
// Returns false if already claimed by another worker.
func (s *Store) ClaimForCalculation(ctx context.Context, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'calculating', updated_at = now()
		WHERE partition_key = $1 AND status = 'dirty'
	`, key)
	return tag.RowsAffected() > 0, err
}

// MarkClean marks a partition as successfully calculated.
func (s *Store) MarkClean(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'clean', last_calc_at = now(), error = NULL, updated_at = now()
		WHERE partition_key = $1
	`, key)
	return err
}

// MarkError records a calculation failure for a partition.
func (s *Store) MarkError(ctx context.Context, key, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE runtime.metric_partition_state
		SET status = 'error', error = $2, updated_at = now()
		WHERE partition_key = $1
	`, key, errMsg)
	return err
}

// ListDirtyPartitions returns partition keys in order of updated_at (oldest first).
func (s *Store) ListDirtyPartitions(ctx context.Context, modelID string, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT partition_key FROM runtime.metric_partition_state
		WHERE model_id = $1 AND status = 'dirty'
		ORDER BY updated_at ASC
		LIMIT $2
	`, modelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
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
}

// LoadModelMetrics loads all metric definitions + dependency edges for one
// model/revision. Strictly revision-scoped (no NULL fallback): metric_def
// rows are always revision-specific copies (migrations 027/028), unlike
// dimension_def's genuine legacy/global-visibility case — mirrors
// internal/query/chart.go's loadAllMetricDefs exactly.
func (s *Store) LoadModelMetrics(ctx context.Context, modelID, revisionID string) (map[string]*MetricDef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, COALESCE(formula,''), is_input, agg_rule,
		       COALESCE(agg_numerator_metric_id::text,''), COALESCE(agg_denominator_metric_id::text,'')
		FROM model.metric_def WHERE model_id = $1::uuid AND revision_id = $2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	defs := make(map[string]*MetricDef)
	for rows.Next() {
		var d MetricDef
		if err := rows.Scan(&d.ID, &d.Name, &d.Formula, &d.IsInput, &d.AggRule, &d.AggNumeratorID, &d.AggDenominatorID); err != nil {
			return nil, err
		}
		defs[d.ID] = &d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load dependency edges
	edgeRows, err := s.pool.Query(ctx, `
		SELECT d.metric_id::text, d.depends_on_metric_id::text
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var from, to string
		if err := edgeRows.Scan(&from, &to); err != nil {
			return nil, err
		}
		if def, ok := defs[from]; ok {
			def.DependsOnID = append(def.DependsOnID, to)
		}
	}
	return defs, edgeRows.Err()
}

// GetCalcValue returns the latest calculated value for a metric + partition.
func (s *Store) GetCalcValue(ctx context.Context, modelID, revisionID, metricID string, dimMembers map[string]string) (float64, error) {
	dimJSON, _ := json.Marshal(dimMembers)

	var value *float64
	err := s.pool.QueryRow(ctx, `
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

// WriteCalcResult stores a calculated value for a metric partition.
func (s *Store) WriteCalcResult(ctx context.Context, modelID, revisionID, metricID, partitionKey string, dimMembers map[string]string, value float64) error {
	dimJSON, _ := json.Marshal(dimMembers)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runtime.calc_result
		    (model_id, revision_id, dim_members, metric_id, value, partition_key)
		VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6)
	`, modelID, revisionID, dimJSON, metricID, value, partitionKey)
	return err
}

// ── Dependency graph (mirrors model.graph but reads from DB) ──────────────────

func (s *Store) LoadDependencyGraph(ctx context.Context, modelID, revisionID string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.metric_id::text, d.depends_on_metric_id::text
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	graph := make(map[string][]string)
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		graph[from] = append(graph[from], to)
	}
	return graph, rows.Err()
}

// LoadDimIDToName returns a map of dimension_def.id → dimension_def.name for
// the given model/revision, used to translate stored {dim_id: member_code}
// combos into named variables that formulas can reference (e.g.
// "department"). Unlike metric_def, dimension_def has a genuine legacy/
// global-visibility case (revision_id IS NULL) — mirrors chart.go's
// loadAllDimensions exactly.
func (s *Store) LoadDimIDToName(ctx context.Context, modelID, revisionID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
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

// LoadAllDimensions loads every dimension in the revision (regardless of
// grid), in the shape rollup.Resolve/rollup.LeafCombos need. Unlike
// chart.go's version, there is no hidden-member filtering here: the
// scheduler has no per-user/access-rule concept (it runs from a
// NATS-triggered or directly-called background path with no acting user),
// and a computed value must exist regardless of who can currently see it —
// exactly like fact_input itself is never access-filtered at write time.
func (s *Store) LoadAllDimensions(ctx context.Context, modelID, revisionID string) (map[string]*rollup.Dimension, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id::text, COALESCE(d.parent_dimension_id::text,''),
		       COALESCE(d.source_dimension_id::text,''), COALESCE(d.source_property,''),
		       m.id::text, m.code, m.properties, COALESCE(pm.code,'') AS parent_code
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.name, m.sort_order, m.code
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dims := make(map[string]*rollup.Dimension)
	for rows.Next() {
		var dimID, parentDimID, sourceDimID, sourceProp string
		var memberID, code, parentCode string
		var properties []byte
		if err := rows.Scan(&dimID, &parentDimID, &sourceDimID, &sourceProp,
			&memberID, &code, &properties, &parentCode); err != nil {
			return nil, err
		}
		dim, ok := dims[dimID]
		if !ok {
			dim = &rollup.Dimension{ID: dimID, ParentDimensionID: parentDimID, SourceDimensionID: sourceDimID, SourceProperty: sourceProp}
			dims[dimID] = dim
		}
		var props map[string]string
		if len(properties) > 0 {
			_ = json.Unmarshal(properties, &props)
		}
		dim.Members = append(dim.Members, rollup.Member{ID: memberID, Code: code, ParentCode: parentCode, Properties: props})
	}
	return dims, rows.Err()
}

// LoadMetricDimensionIDs returns each metric's own native dimension IDs, via
// grid_metric -> grid_dimension on whatever grid it actually lives on.
// Unambiguous per metric: migration 051_grid_metric_unique.sql gives
// grid_metric a bare UNIQUE(metric_id) constraint, so a metric belongs to at
// most one grid. Mirrors chart.go's loadMetricDimensionIDs exactly.
func (s *Store) LoadMetricDimensionIDs(ctx context.Context, modelID, revisionID string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
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
	b, _ := json.Marshal(dimMembers)
	return string(b)
}

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
	rows, err := s.pool.Query(ctx, `
		SELECT dim_members::text, SUM(value)::float8
		FROM (
			SELECT direct.dim_members, direct.value FROM (
				SELECT DISTINCT ON (dim_members) dim_members, value
				FROM runtime.fact_input
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
				  AND source_ref IS NULL
				ORDER BY dim_members, entered_at DESC, id DESC
			) direct
			WHERE NOT EXISTS (
				SELECT 1 FROM runtime.fact_input fi2
				JOIN model.form_metric_mapping fmm ON fmm.id = fi2.source_ref
				WHERE fi2.model_id=$1::uuid AND fi2.revision_id=$2::uuid
				  AND fi2.metric_id=$3::uuid AND fi2.dim_members = direct.dim_members
				  AND fmm.aggregation IN ('replace', 'last')
			)
			UNION ALL
			SELECT dim_members, value FROM runtime.fact_input
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
			  AND source_ref IS NOT NULL
		) fi
		GROUP BY dim_members
	`, modelID, revisionID, metricID)
	return scanDimValueMap(rows, err)
}

// LoadCalcValueMap is LoadInputValueMap's runtime.calc_result analog.
func (s *Store) LoadCalcValueMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `
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
	rows, err := s.pool.Query(ctx, `
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
	rows, err := s.pool.Query(ctx, `
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
	rows, err := s.pool.Query(ctx, `
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
		var dm map[string]string
		if err := json.Unmarshal([]byte(raw), &dm); err != nil {
			continue
		}
		if out[metricID] == nil {
			out[metricID] = make(map[string]float64)
		}
		out[metricID][dimKey(dm)] = value
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
		var dm map[string]string
		if err := json.Unmarshal([]byte(raw), &dm); err != nil {
			continue
		}
		out[dimKey(dm)] = value
	}
	return out, rows.Err()
}

// ── Batched per-intersection write ──────────────────────────────────────────

// CalcResultRow is one leaf-combo's calculated value, for WriteCalcResults.
type CalcResultRow struct {
	DimMembers map[string]string
	Value      float64
}

// WriteCalcResults persists one calc_result row per entry in rows, in a
// single round trip via a batch. WriteCalcResult (singular, unchanged)
// remains what writes the '{}' aggregate row — this is purely additive.
// ClearPerComboResults deletes a metric's per-intersection calc_result rows
// (never the '{}' aggregate row), making each recompute's per-combo set
// authoritative. Without this, an intersection whose underlying facts were
// deleted (full_reload, member removal) kept serving its LAST computed
// value forever: the recompute skips the now-empty combo, the skip writes
// nothing, and nothing else ever garbage-collects the stale row — found
// live when a business user's junk test-writes (revenue 4, cost 3) kept
// showing margin_pct=25 at an intersection whose facts had been wiped.
func (s *Store) ClearPerComboResults(ctx context.Context, modelID, revisionID, metricID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		  AND dim_members::text <> '{}'
	`, modelID, revisionID, metricID)
	return err
}

func (s *Store) WriteCalcResults(ctx context.Context, modelID, revisionID, metricID, partitionKey string, rows []CalcResultRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		dimJSON, _ := json.Marshal(r.DimMembers)
		batch.Queue(`
			INSERT INTO runtime.calc_result
			    (model_id, revision_id, dim_members, metric_id, value, partition_key)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6)
		`, modelID, revisionID, string(dimJSON), metricID, r.Value, partitionKey)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck // any real failure already surfaces via br.Exec() below
	for range rows {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
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
