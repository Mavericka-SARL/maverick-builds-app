package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1 "github.com/mavericks-engine/mavericks/gen/go/model/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

var ErrNotFound = errors.New("not found")
var ErrDuplicate = errors.New("duplicate name")

// ── Dimensions ────────────────────────────────────────────────────────────────

func (s *Store) CreateDimension(ctx context.Context, modelID, name string, props []*modelv1.DimensionProperty) (*modelv1.Dimension, error) {
	propsJSON, err := json.Marshal(props)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt time.Time
	err = s.pool.QueryRow(ctx,
		`INSERT INTO model.dimension_def (model_id, name, properties)
		 VALUES ($1, $2, $3)
		 RETURNING id, created_at`,
		modelID, name, propsJSON,
	).Scan(&id, &createdAt)
	if isDuplicate(err) {
		return nil, fmt.Errorf("%w: dimension %q", ErrDuplicate, name)
	}
	if err != nil {
		return nil, err
	}

	return &modelv1.Dimension{
		Id: id, ModelId: modelID, Name: name, Properties: props,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetDimension(ctx context.Context, dimensionID string) (*modelv1.Dimension, error) {
	var d modelv1.Dimension
	var createdAt time.Time
	var propsJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, model_id, name, properties, created_at FROM model.dimension_def WHERE id = $1`,
		dimensionID,
	).Scan(&d.Id, &d.ModelId, &d.Name, &propsJSON, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(propsJSON, &d.Properties); err != nil {
		return nil, err
	}
	d.CreatedAt = timestamppb.New(createdAt)
	return &d, nil
}

func (s *Store) ListDimensions(ctx context.Context, modelID string, limit, offset int) ([]*modelv1.Dimension, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, model_id, name, properties, created_at FROM model.dimension_def
		 WHERE model_id = $1 ORDER BY name LIMIT $2 OFFSET $3`,
		modelID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dims []*modelv1.Dimension
	for rows.Next() {
		var d modelv1.Dimension
		var createdAt time.Time
		var propsJSON []byte
		if err := rows.Scan(&d.Id, &d.ModelId, &d.Name, &propsJSON, &createdAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(propsJSON, &d.Properties)
		d.CreatedAt = timestamppb.New(createdAt)
		dims = append(dims, &d)
	}
	return dims, rows.Err()
}

func (s *Store) CreateDimensionMember(ctx context.Context, dimensionID, code, label, parentID string, props map[string]string) (*modelv1.DimensionMember, error) {
	propsJSON, _ := json.Marshal(props)

	var id string
	var pid *string
	if parentID != "" {
		pid = &parentID
	}

	err := s.pool.QueryRow(ctx,
		`INSERT INTO model.dimension_member (dimension_id, code, label, parent_id, properties)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id`,
		dimensionID, code, label, pid, propsJSON,
	).Scan(&id)
	if isDuplicate(err) {
		return nil, fmt.Errorf("%w: member %q", ErrDuplicate, code)
	}
	if err != nil {
		return nil, err
	}

	return &modelv1.DimensionMember{
		Id: id, DimensionId: dimensionID, Code: code, Label: label,
		ParentId: parentID, Properties: props,
	}, nil
}

func (s *Store) ListDimensionMembers(ctx context.Context, dimensionID string, limit, offset int) ([]*modelv1.DimensionMember, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, dimension_id, code, label, COALESCE(parent_id::text, ''), properties
		 FROM model.dimension_member
		 WHERE dimension_id = $1 ORDER BY sort_order, code LIMIT $2 OFFSET $3`,
		dimensionID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []*modelv1.DimensionMember
	for rows.Next() {
		var m modelv1.DimensionMember
		var propsJSON []byte
		if err := rows.Scan(&m.Id, &m.DimensionId, &m.Code, &m.Label, &m.ParentId, &propsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(propsJSON, &m.Properties)
		members = append(members, &m)
	}
	return members, rows.Err()
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func (s *Store) CreateMetric(ctx context.Context, modelID, name, formula, storageType string, isInput bool) (*modelv1.Metric, error) {
	var id string
	var createdAt time.Time

	var formulaPtr *string
	if formula != "" {
		formulaPtr = &formula
	}

	err := s.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, formula, storage_type, is_input)
		 VALUES ($1, $2, $3, $4::core.storage_type, $5)
		 RETURNING id, created_at`,
		modelID, name, formulaPtr, storageType, isInput,
	).Scan(&id, &createdAt)
	if isDuplicate(err) {
		return nil, fmt.Errorf("%w: metric %q", ErrDuplicate, name)
	}
	if err != nil {
		return nil, err
	}

	return &modelv1.Metric{
		Id: id, ModelId: modelID, Name: name, Formula: formula,
		IsInput:   isInput,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetMetric(ctx context.Context, metricID string) (*modelv1.Metric, error) {
	var m modelv1.Metric
	var createdAt time.Time
	var formula *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, model_id, name, COALESCE(formula,''), storage_type::text, is_input, created_at
		 FROM model.metric_def WHERE id = $1`,
		metricID,
	).Scan(&m.Id, &m.ModelId, &m.Name, &formula, new(string), &m.IsInput, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if formula != nil {
		m.Formula = *formula
	}
	m.CreatedAt = timestamppb.New(createdAt)
	return &m, nil
}

func (s *Store) ListMetrics(ctx context.Context, modelID string, limit, offset int) ([]*modelv1.Metric, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, model_id, name, COALESCE(formula,''), storage_type::text, is_input, created_at
		 FROM model.metric_def WHERE model_id = $1 ORDER BY name LIMIT $2 OFFSET $3`,
		modelID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var metrics []*modelv1.Metric
	for rows.Next() {
		var m modelv1.Metric
		var createdAt time.Time
		var storageStr string
		if err := rows.Scan(&m.Id, &m.ModelId, &m.Name, &m.Formula, &storageStr, &m.IsInput, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = timestamppb.New(createdAt)
		metrics = append(metrics, &m)
	}
	return metrics, rows.Err()
}

// UpsertDependencies replaces all dependency edges for a metric.
func (s *Store) UpsertDependencies(ctx context.Context, metricID string, dependsOnIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`DELETE FROM model.calc_dependency WHERE metric_id = $1`, metricID,
	); err != nil {
		return err
	}

	for _, depID := range dependsOnIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
			 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			metricID, depID,
		); err != nil {
			return fmt.Errorf("insert dependency %s→%s: %w", metricID, depID, err)
		}
	}

	return tx.Commit(ctx)
}

// LoadDependencyGraph returns all edges for a model as adjacency list: metricID → []dependsOnID
func (s *Store) LoadDependencyGraph(ctx context.Context, modelID string) (map[string][]string, map[string]string, error) {
	// Load all metrics (id → name)
	metricRows, err := s.pool.Query(ctx,
		`SELECT id, name FROM model.metric_def WHERE model_id = $1`, modelID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer metricRows.Close()

	names := make(map[string]string) // id → name
	for metricRows.Next() {
		var id, name string
		if err := metricRows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		names[id] = name
	}
	if err := metricRows.Err(); err != nil {
		return nil, nil, err
	}

	// Load all dependency edges for metrics belonging to this model
	edgeRows, err := s.pool.Query(ctx, `
		SELECT d.metric_id, d.depends_on_metric_id
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		WHERE m.model_id = $1
	`, modelID)
	if err != nil {
		return nil, nil, err
	}
	defer edgeRows.Close()

	graph := make(map[string][]string)
	for edgeRows.Next() {
		var from, to string
		if err := edgeRows.Scan(&from, &to); err != nil {
			return nil, nil, err
		}
		graph[from] = append(graph[from], to)
	}
	return graph, names, edgeRows.Err()
}

// ── Hierarchies ───────────────────────────────────────────────────────────────

func (s *Store) CreateHierarchy(ctx context.Context, modelID, name, dimensionID string, levels []string) (*modelv1.Hierarchy, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO model.hierarchy (model_id, name, dimension_id, level_names)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		modelID, name, dimensionID, levels,
	).Scan(&id)
	if isDuplicate(err) {
		return nil, fmt.Errorf("%w: hierarchy %q", ErrDuplicate, name)
	}
	if err != nil {
		return nil, err
	}
	return &modelv1.Hierarchy{Id: id, ModelId: modelID, Name: name, DimensionId: dimensionID, LevelNames: levels}, nil
}

func (s *Store) ListHierarchies(ctx context.Context, modelID string) ([]*modelv1.Hierarchy, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, model_id, name, dimension_id, level_names FROM model.hierarchy WHERE model_id = $1 ORDER BY name`,
		modelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*modelv1.Hierarchy
	for rows.Next() {
		var h modelv1.Hierarchy
		if err := rows.Scan(&h.Id, &h.ModelId, &h.Name, &h.DimensionId, &h.LevelNames); err != nil {
			return nil, err
		}
		list = append(list, &h)
	}
	return list, rows.Err()
}

// ── Scenarios & Versions ──────────────────────────────────────────────────────

func (s *Store) CreateScenario(ctx context.Context, modelID, name, desc string) (*modelv1.Scenario, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO model.revision (model_id, name, description) VALUES ($1, $2, $3) RETURNING id`,
		modelID, name, desc,
	).Scan(&id)
	if isDuplicate(err) {
		return nil, fmt.Errorf("%w: scenario %q", ErrDuplicate, name)
	}
	if err != nil {
		return nil, err
	}
	return &modelv1.Scenario{Id: id, ModelId: modelID, Name: name, Description: desc}, nil
}

func (s *Store) ListScenarios(ctx context.Context, modelID string) ([]*modelv1.Scenario, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, model_id, name, description FROM model.revision WHERE model_id = $1 ORDER BY name`,
		modelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*modelv1.Scenario
	for rows.Next() {
		var sc modelv1.Scenario
		if err := rows.Scan(&sc.Id, &sc.ModelId, &sc.Name, &sc.Description); err != nil {
			return nil, err
		}
		list = append(list, &sc)
	}
	return list, rows.Err()
}

// ── Revisions ─────────────────────────────────────────────────────────────────

// PublishRevision computes a hash of the current model definition, increments
// the version counter, inserts a core.schema_version row, and marks it published.
func (s *Store) PublishRevision(ctx context.Context, modelID string) (*modelv1.Revision, error) {
	// Compute schema hash from metrics, dimensions, hierarchies
	hash, err := s.computeSchemaHash(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("compute schema hash: %w", err)
	}

	var id string
	var versionNum int32
	var publishedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO core.schema_version (model_id, version_number, schema_hash, published_at)
		SELECT $1::uuid,
		       COALESCE(MAX(version_number), 0) + 1,
		       $2,
		       now()
		FROM core.schema_version
		WHERE model_id = $1::uuid
		RETURNING id, version_number, published_at
	`, modelID, hash).Scan(&id, &versionNum, &publishedAt)
	if err != nil {
		return nil, fmt.Errorf("publish revision: %w", err)
	}

	return &modelv1.Revision{
		Id:            id,
		ModelId:       modelID,
		VersionNumber: versionNum,
		SchemaHash:    hash,
		PublishedAt:   timestamppb.New(publishedAt),
	}, nil
}

func (s *Store) GetRevision(ctx context.Context, revisionID string) (*modelv1.Revision, error) {
	var r modelv1.Revision
	var publishedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, model_id::text, version_number, schema_hash, published_at
		FROM core.schema_version WHERE id = $1
	`, revisionID).Scan(&r.Id, &r.ModelId, &r.VersionNumber, &r.SchemaHash, &publishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if publishedAt != nil {
		r.PublishedAt = timestamppb.New(*publishedAt)
	}
	return &r, nil
}

// computeSchemaHash produces a stable SHA-256 fingerprint of the model definition.
func (s *Store) computeSchemaHash(ctx context.Context, modelID string) (string, error) {
	type metricRow struct {
		Name    string `json:"name"`
		Formula string `json:"formula"`
		IsInput bool   `json:"is_input"`
	}
	type dimRow struct {
		Name string `json:"name"`
	}
	type hierRow struct {
		Name       string   `json:"name"`
		LevelNames []string `json:"level_names"`
	}

	mrows, err := s.pool.Query(ctx,
		`SELECT name, COALESCE(formula,''), is_input FROM model.metric_def WHERE model_id = $1 ORDER BY name`,
		modelID)
	if err != nil {
		return "", err
	}
	defer mrows.Close()
	var metrics []metricRow
	for mrows.Next() {
		var m metricRow
		if err := mrows.Scan(&m.Name, &m.Formula, &m.IsInput); err != nil {
			return "", err
		}
		metrics = append(metrics, m)
	}
	if err := mrows.Err(); err != nil {
		return "", err
	}

	drows, err := s.pool.Query(ctx,
		`SELECT d.name FROM model.dimension_def d WHERE d.model_id = $1 ORDER BY d.name`,
		modelID)
	if err != nil {
		return "", err
	}
	defer drows.Close()
	var dims []dimRow
	for drows.Next() {
		var d dimRow
		if err := drows.Scan(&d.Name); err != nil {
			return "", err
		}
		dims = append(dims, d)
	}
	if err := drows.Err(); err != nil {
		return "", err
	}

	hrows, err := s.pool.Query(ctx,
		`SELECT name, level_names FROM model.hierarchy WHERE model_id = $1 ORDER BY name`,
		modelID)
	if err != nil {
		return "", err
	}
	defer hrows.Close()
	var hiers []hierRow
	for hrows.Next() {
		var h hierRow
		if err := hrows.Scan(&h.Name, &h.LevelNames); err != nil {
			return "", err
		}
		hiers = append(hiers, h)
	}
	if err := hrows.Err(); err != nil {
		return "", err
	}

	snapshot := map[string]any{
		"model_id":    modelID,
		"metrics":     metrics,
		"dimensions":  dims,
		"hierarchies": hiers,
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// DeleteMetric removes a metric and its dependency edges.
func (s *Store) DeleteMetric(ctx context.Context, metricID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM model.metric_def WHERE id = $1`, metricID)
	return err
}

// UpdateMetric updates name and/or formula of a metric.
func (s *Store) UpdateMetric(ctx context.Context, metricID, name, formula string) error {
	var formulaPtr *string
	if formula != "" {
		formulaPtr = &formula
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE model.metric_def SET name = $2, formula = $3 WHERE id = $1
	`, metricID, name, formulaPtr)
	return err
}

// ── Dimension Properties ──────────────────────────────────────────────────────

// DimProperty represents a typed attribute schema for a dimension.
type DimProperty struct {
	ID          string `json:"id"`
	DimensionID string `json:"dimension_id"`
	Name        string `json:"name"`
	DataType    string `json:"data_type"`
}

func (s *Store) ListDimensionProperties(ctx context.Context, dimensionID string) ([]DimProperty, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, dimension_id::text, name, data_type
		FROM model.dimension_property WHERE dimension_id = $1 ORDER BY name
	`, dimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var props []DimProperty
	for rows.Next() {
		var p DimProperty
		if err := rows.Scan(&p.ID, &p.DimensionID, &p.Name, &p.DataType); err != nil {
			return nil, err
		}
		props = append(props, p)
	}
	return props, rows.Err()
}

func (s *Store) AddDimensionProperty(ctx context.Context, dimensionID, name, dataType string) (string, error) {
	if dataType == "" {
		dataType = "text"
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO model.dimension_property (dimension_id, name, data_type)
		VALUES ($1, $2, $3) RETURNING id::text
	`, dimensionID, name, dataType).Scan(&id)
	if isDuplicate(err) {
		return "", fmt.Errorf("%w: property %q", ErrDuplicate, name)
	}
	return id, err
}

func (s *Store) UpdateDimensionProperty(ctx context.Context, propID, name, dataType string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE model.dimension_property SET name = $2, data_type = $3 WHERE id = $1
	`, propID, name, dataType)
	return err
}

func (s *Store) DeleteDimensionProperty(ctx context.Context, propID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM model.dimension_property WHERE id = $1`, propID)
	return err
}

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	return containsCode(err, "23505")
}

func containsCode(err error, code string) bool {
	// pgconn.PgError implements this interface
	type sqlStater interface{ SQLState() string }
	var pe sqlStater
	if errors.As(err, &pe) {
		return pe.SQLState() == code
	}
	return false
}
