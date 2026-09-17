package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// ErrWriteDenied wraps a business-rule rejection (not a writable input
// metric, or a writeguard.CheckWrite denial: system-managed revision,
// hidden/read-only access, workflow lock) so the gRPC server can map it to
// PermissionDenied instead of a generic Internal error — the same
// distinction HTTP /api/cells already makes (403 vs 500).
var ErrWriteDenied = errors.New("write denied")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WritebackResult is the outcome of a single writeback operation.
type WritebackResult struct {
	CellsWritten int
	MetricIDs    []string // affected input metric IDs (for recalc trigger)
}

// Writeback writes one or more updates to runtime.fact_input inside a single transaction.
func (s *Store) Writeback(ctx context.Context, modelID, revisionID, userID string, updates []*queryv1.WritebackUpdate) (*WritebackResult, error) {
	// Business-rule checks run against the pool, before the write
	// transaction begins — matching internal/importpkg/store.go's own
	// documented reasoning: a failed statement inside a tx poisons it for
	// everything after until rollback, so checks belong outside. This is
	// what HTTP /api/cells already enforces per-write and gRPC Writeback did
	// not: is_input, plus writeguard.CheckWrite's system-managed-revision,
	// hidden-member (with ancestor cascade), and workflow-lock checks —
	// previously the only authorization here was a per-metric Policy
	// Service permission check, a materially weaker and different surface
	// than the HTTP path.
	var allMemberIDs []string
	for _, u := range updates {
		var isInput bool
		if err := s.pool.QueryRow(ctx,
			`SELECT is_input FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid`,
			u.MetricId, modelID,
		).Scan(&isInput); err != nil || !isInput {
			return nil, fmt.Errorf("%w: metric %s is not a writable input metric in this model", ErrWriteDenied, u.MetricId)
		}
		// Metric-level access, mirroring HTTP /api/cells' own two checks
		// (MetricAccess + CheckWrite) — this used to only run CheckWrite
		// below, so a user hidden/read-restricted from a metric could
		// still write it via gRPC Writeback even though the identical
		// HTTP write was rejected.
		if access, maErr := writeguard.MetricAccess(ctx, s.pool, userID, u.MetricId); maErr != nil {
			return nil, fmt.Errorf("check metric access: %w", maErr)
		} else if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("%w: access denied: the write references a metric outside your access scope", ErrWriteDenied)
		}
		for dimID, code := range u.DimMembers {
			var memberID string
			if err := s.pool.QueryRow(ctx,
				`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
				dimID, code,
			).Scan(&memberID); err != nil {
				continue // unknown member: nothing to restrict — matches HTTP cells()'s exact behavior
			}
			allMemberIDs = append(allMemberIDs, memberID)
		}
	}
	writtenMetricIDs := make([]string, 0, len(updates))
	seenMetric := map[string]bool{}
	for _, u := range updates {
		if u.MetricId != "" && !seenMetric[u.MetricId] {
			seenMetric[u.MetricId] = true
			writtenMetricIDs = append(writtenMetricIDs, u.MetricId)
		}
	}
	if reason, err := writeguard.CheckWriteMetrics(ctx, s.pool, modelID, revisionID, userID, allMemberIDs, writtenMetricIDs); err != nil {
		return nil, fmt.Errorf("check write: %w", err)
	} else if reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	seen := make(map[string]bool)
	for _, u := range updates {
		dimJSON, err := json.Marshal(u.DimMembers)
		if err != nil {
			return nil, fmt.Errorf("marshal dim_members: %w", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input
			    (model_id, revision_id, dim_members, metric_id, value, entered_by)
			VALUES ($1, $2::uuid, $3, $4::uuid, $5, $6::uuid)
		`, modelID, revisionID, dimJSON, u.MetricId, u.Value, userID)
		if err != nil {
			return nil, fmt.Errorf("insert fact_input metric %s: %w", u.MetricId, err)
		}
		seen[u.MetricId] = true
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	metricIDs := make([]string, 0, len(seen))
	for id := range seen {
		metricIDs = append(metricIDs, id)
	}
	return &WritebackResult{CellsWritten: len(updates), MetricIDs: metricIDs}, nil
}

// QueryCells reads calc_result rows (or fact_input for input metrics) matching the query.
// Policy filtering of dimension members and metrics is applied by the caller before this.
func (s *Store) QueryCells(ctx context.Context, modelID, revisionID string, metricIDs, allowedDimCodes []string) ([]*queryv1.CellValue, error) {
	// Read latest calc_result per (dim_members, metric_id) partition key
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (metric_id, dim_members)
		       metric_id::text, dim_members, value
		FROM runtime.calc_result
		WHERE model_id = $1
		  AND revision_id = $2::uuid
		  AND metric_id = ANY($3::uuid[])
		ORDER BY metric_id, dim_members, calc_at DESC
	`, modelID, revisionID, metricIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cells []*queryv1.CellValue
	for rows.Next() {
		var metricID string
		var dimJSON []byte
		var value float64
		if err := rows.Scan(&metricID, &dimJSON, &value); err != nil {
			return nil, err
		}
		var dimMembers map[string]string
		_ = json.Unmarshal(dimJSON, &dimMembers)

		if !dimMembersAllowed(dimMembers, allowedDimCodes) {
			continue
		}

		cells = append(cells, &queryv1.CellValue{
			DimMembers: dimMembers,
			MetricId:   metricID,
			Value:      value,
			IsInput:    false,
			IsWritable: false,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Also include latest input values from fact_input
	inputRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (metric_id, dim_members)
		       metric_id::text, dim_members, value
		FROM runtime.fact_input
		WHERE model_id = $1
		  AND revision_id = $2::uuid
		  AND metric_id = ANY($3::uuid[])
		ORDER BY metric_id, dim_members, entered_at DESC, id DESC
	`, modelID, revisionID, metricIDs)
	if err != nil {
		return nil, err
	}
	defer inputRows.Close()

	for inputRows.Next() {
		var metricID string
		var dimJSON []byte
		var value float64
		if err := inputRows.Scan(&metricID, &dimJSON, &value); err != nil {
			return nil, err
		}
		var dimMembers map[string]string
		_ = json.Unmarshal(dimJSON, &dimMembers)

		if !dimMembersAllowed(dimMembers, allowedDimCodes) {
			continue
		}

		cells = append(cells, &queryv1.CellValue{
			DimMembers: dimMembers,
			MetricId:   metricID,
			Value:      value,
			IsInput:    true,
			IsWritable: true,
		})
	}
	return cells, inputRows.Err()
}

// GetCell returns the latest value for a specific cell (calc or input).
func (s *Store) GetCell(ctx context.Context, modelID, revisionID, metricID string, dimMembers map[string]string) (*queryv1.CellValue, error) {
	dimJSON, _ := json.Marshal(dimMembers)

	// Try calc_result first
	var value float64
	var calcAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT value, calc_at FROM runtime.calc_result
		WHERE model_id = $1 AND revision_id = $2::uuid AND metric_id = $3::uuid
		  AND dim_members = $4
		ORDER BY calc_at DESC LIMIT 1
	`, modelID, revisionID, metricID, dimJSON).Scan(&value, &calcAt)

	if err == nil {
		return &queryv1.CellValue{
			DimMembers: dimMembers,
			MetricId:   metricID,
			Value:      value,
			IsInput:    false,
		}, nil
	}

	// Fall back to fact_input
	err = s.pool.QueryRow(ctx, `
		SELECT value FROM runtime.fact_input
		WHERE model_id = $1 AND revision_id = $2::uuid AND metric_id = $3::uuid
		  AND dim_members = $4
		ORDER BY entered_at DESC LIMIT 1
	`, modelID, revisionID, metricID, dimJSON).Scan(&value)
	if err != nil {
		return nil, fmt.Errorf("cell not found")
	}

	return &queryv1.CellValue{
		DimMembers: dimMembers,
		MetricId:   metricID,
		Value:      value,
		IsInput:    true,
		IsWritable: true,
	}, nil
}

func dimMembersAllowed(dimMembers map[string]string, allowedCodes []string) bool {
	if len(allowedCodes) == 0 {
		return true // no restriction
	}
	allowed := make(map[string]bool, len(allowedCodes))
	for _, c := range allowedCodes {
		allowed[c] = true
	}
	for _, code := range dimMembers {
		if !allowed[code] {
			return false
		}
	}
	return true
}
