package importpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// ErrWriteDenied wraps a writeguard rejection (system-managed revision,
// hidden/read-only access rule, or workflow lock) so callers can map it to
// 403/PermissionDenied instead of a generic 500/Internal error.
var ErrWriteDenied = errors.New("write denied")

// ErrImportJobNotFound means the job either never existed or doesn't belong
// to the caller's model — deliberately indistinguishable, so a caller can't
// use this endpoint to probe which job IDs exist in other tenants' models.
var ErrImportJobNotFound = errors.New("import job not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) CreateImportJob(ctx context.Context, modelID, revisionID, fileURL, createdByUserID string, mappings []*importpkgv1.ColumnMapping) (*importpkgv1.ImportJob, error) {
	mappingsJSON, _ := json.Marshal(mappings)

	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO import.import_job
		    (model_id, revision_id, file_url, mappings, created_by)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5::uuid)
		RETURNING id::text, created_at
	`, modelID, revisionID, fileURL, mappingsJSON, createdByUserID).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}

	return &importpkgv1.ImportJob{
		Id:         id,
		ModelId:    modelID,
		RevisionId: revisionID,
		Status:     importpkgv1.ImportStatus_IMPORT_STATUS_VALIDATING,
		Mappings:   mappings,
		CreatedAt:  timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetImportJob(ctx context.Context, jobID string) (*importpkgv1.ImportJob, error) {
	var id, modelID, fileURL, statusStr string
	var revisionID *string
	var mappingsJSON []byte
	var totalRows, validRows, errorRows int32
	var createdAt time.Time
	var completedAt *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, model_id::text, COALESCE(revision_id::text,''), file_url, status::text,
		       mappings, total_rows, valid_rows, error_rows, created_at, completed_at
		FROM import.import_job WHERE id = $1::uuid
	`, jobID).Scan(&id, &modelID, &revisionID, &fileURL, &statusStr,
		&mappingsJSON, &totalRows, &validRows, &errorRows, &createdAt, &completedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("import job %s not found", jobID)
	}
	if err != nil {
		return nil, err
	}

	var mappings []*importpkgv1.ColumnMapping
	_ = json.Unmarshal(mappingsJSON, &mappings)

	revID := ""
	if revisionID != nil {
		revID = *revisionID
	}

	job := &importpkgv1.ImportJob{
		Id:         id,
		ModelId:    modelID,
		RevisionId: revID,
		Status:     importStatusFromString(statusStr),
		Mappings:   mappings,
		TotalRows:  totalRows,
		ValidRows:  validRows,
		ErrorRows:  errorRows,
		CreatedAt:  timestamppb.New(createdAt),
	}
	if completedAt != nil {
		job.CompletedAt = timestamppb.New(*completedAt)
	}
	return job, nil
}

func (s *Store) ListImportJobs(ctx context.Context, modelID string, limit int32) ([]*importpkgv1.ImportJob, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, model_id::text, COALESCE(revision_id::text,''), status::text,
		       total_rows, valid_rows, error_rows, created_at, completed_at
		FROM import.import_job
		WHERE model_id = $1::uuid
		ORDER BY created_at DESC LIMIT $2
	`, modelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*importpkgv1.ImportJob
	for rows.Next() {
		var id, mID, revID, statusStr string
		var totalRows, validRows, errorRows int32
		var createdAt time.Time
		var completedAt *time.Time

		if err := rows.Scan(&id, &mID, &revID, &statusStr,
			&totalRows, &validRows, &errorRows, &createdAt, &completedAt); err != nil {
			return nil, err
		}
		job := &importpkgv1.ImportJob{
			Id:         id,
			ModelId:    mID,
			RevisionId: revID,
			Status:     importStatusFromString(statusStr),
			TotalRows:  totalRows,
			ValidRows:  validRows,
			ErrorRows:  errorRows,
			CreatedAt:  timestamppb.New(createdAt),
		}
		if completedAt != nil {
			job.CompletedAt = timestamppb.New(*completedAt)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) DeleteImportJob(ctx context.Context, jobID, modelID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM import.import_job WHERE id = $1::uuid AND model_id = $2::uuid`, jobID, modelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrImportJobNotFound
	}
	return nil
}

// StagingRow is a validated row ready for staging.
type StagingRow struct {
	MetricID   string
	DimMembers map[string]string
	Value      float64
	RowNumber  int
}

// StageRows inserts validated rows into import.import_staging and records errors.
// It then updates job stats and transitions to 'staged' or 'failed'.
func (s *Store) StageRows(ctx context.Context, jobID string, rows []StagingRow, errs []*importpkgv1.ImportError) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, row := range rows {
		dimJSON, _ := json.Marshal(row.DimMembers)
		_, err = tx.Exec(ctx, `
			INSERT INTO import.import_staging (job_id, metric_id, dim_members, value, row_number)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
		`, jobID, row.MetricID, dimJSON, row.Value, row.RowNumber)
		if err != nil {
			return fmt.Errorf("stage row %d: %w", row.RowNumber, err)
		}
	}

	for _, e := range errs {
		_, err = tx.Exec(ctx, `
			INSERT INTO import.import_error (job_id, row_number, column_name, error_code, message, raw_value)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
		`, jobID, e.RowNumber, e.Column, e.ErrorCode, e.Message, e.RawValue)
		if err != nil {
			return fmt.Errorf("record error row %d: %w", e.RowNumber, err)
		}
	}

	finalStatus := "staged"
	if len(rows) == 0 {
		finalStatus = "failed"
	}

	total := int32(len(rows) + len(errs))
	_, err = tx.Exec(ctx, `
		UPDATE import.import_job
		SET status = $2::import.import_status,
		    total_rows = $3,
		    valid_rows = $4,
		    error_rows = $5
		WHERE id = $1::uuid
	`, jobID, finalStatus, total, int32(len(rows)), int32(len(errs)))
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ImportMode selects how CommitImport reconciles staged rows against
// existing runtime.fact_input rows for the same cells. fact_input is
// append-only/latest-wins (see runtime.fact_input in migrations) — no mode
// ever deletes a cell's history except FullReload's explicit wipe; the other
// two modes simply insert a new "latest" row and rely on every read path's
// existing DISTINCT ON (metric_id, dim_members) ORDER BY entered_at DESC to
// make it authoritative.
type ImportMode string

const (
	// ModeIncremental adds the staged value to the current latest value per
	// cell (e.g. accumulating partial CSV exports over a period).
	ModeIncremental ImportMode = "incremental"
	// ModeReplace inserts the staged value as-is for exactly the cells
	// present in the file — untouched cells keep their prior latest value.
	// This is "replace/upsert semantics for the listed cells": the file is
	// authoritative for what it contains, silent for what it doesn't.
	ModeReplace ImportMode = "replace"
	// ModeFullReload deletes every fact_input row for the model+revision
	// before inserting — a clean full-dataset replacement, not scoped to the
	// file's cells.
	ModeFullReload ImportMode = "full_reload"
)

// CommitImport flushes staged rows to runtime.fact_input and marks the job
// committed. Returns the distinct metric IDs that were committed (for recalc
// event publishing).
//
// Before writing anything, it resolves the staged rows' dim_members into
// dimension_member UUIDs (best-effort — a key that isn't a resolvable UUID,
// e.g. a legacy name-keyed row from an older caller, is skipped rather than
// failing the whole commit) and runs writeguard.CheckWrite against modelID/
// revisionID/userID/those members. This is the ONE place both the gRPC
// ImportService and the HTTP import handler ultimately flow through, so
// neither can commit a write cells() itself would reject — see
// internal/writeguard's package doc for why this used to be bypassable.
func (s *Store) CommitImport(ctx context.Context, jobID, modelID, revisionID, userID string, mode ImportMode) ([]string, error) {
	if mode == "" {
		mode = ModeIncremental
	}

	// Resolve referenced dimension members (best-effort) and run the shared
	// write guard BEFORE opening the commit transaction — on its own
	// connection (s.pool, not a tx), since a staged dim_members key that
	// isn't a real UUID (e.g. a legacy name-keyed fallback row) fails the
	// ::uuid cast, and inside a transaction a single failed statement
	// poisons every statement after it until rollback.
	memberIDs, err := s.resolveStagedMemberIDs(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("resolve staged members: %w", err)
	}
	// The staged metrics, so metric-scoped workflow locks apply to exactly
	// the member×metric crossings this import writes (the same list feeds
	// the per-metric access check just below).
	stagedMetricIDs, err := s.resolveStagedMetricIDs(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("resolve staged metrics: %w", err)
	}
	if reason, gErr := writeguard.CheckWriteMetrics(ctx, s.pool, modelID, revisionID, userID, memberIDs, stagedMetricIDs); gErr != nil {
		return nil, fmt.Errorf("write guard: %w", gErr)
	} else if reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	}

	// Metric-level access — CheckWriteMetrics above covers the workflow
	// lock, mirroring HTTP /api/cells' own two separate checks
	// (MetricAccess + CheckWrite). Without this, a user hidden/
	// read-restricted from a metric could still write it by importing a
	// CSV, even though the identical direct cell write is rejected.
	for _, metricID := range stagedMetricIDs {
		access, maErr := writeguard.MetricAccess(ctx, s.pool, userID, metricID)
		if maErr != nil {
			return nil, fmt.Errorf("check metric access: %w", maErr)
		}
		if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("%w: access denied: the import references a metric outside your access scope", ErrWriteDenied)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Verify job is staged
	var jobStatus string
	err = tx.QueryRow(ctx, `
		SELECT status::text FROM import.import_job WHERE id = $1::uuid
	`, jobID).Scan(&jobStatus)
	if err != nil {
		return nil, fmt.Errorf("get job status: %w", err)
	}
	if jobStatus != "staged" {
		return nil, fmt.Errorf("job %s cannot be committed (status: %s)", jobID, jobStatus)
	}

	// Collect distinct metric IDs before flushing
	mrows, err := tx.Query(ctx, `
		SELECT DISTINCT metric_id::text FROM import.import_staging WHERE job_id = $1::uuid
	`, jobID)
	if err != nil {
		return nil, fmt.Errorf("collect metric ids: %w", err)
	}
	var metricIDs []string
	for mrows.Next() {
		var id string
		if err := mrows.Scan(&id); err != nil {
			mrows.Close()
			return nil, err
		}
		metricIDs = append(metricIDs, id)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	if mode == ModeFullReload {
		// full_reload wipes the model/revision's facts, but the guards above
		// only authorize the STAGED rows. Deleting unconditionally therefore
		// let a narrowly-scoped user (one department, say) destroy every
		// other department's data — including members and metrics they can't
		// even see — by uploading a single row.
		//
		// The delete is instead constrained to facts this caller could have
		// written themselves: no metric they're hidden/read-restricted from,
		// no combo touching a restricted dimension member (cascaded to
		// descendants exactly as the read paths do), and never a form-posted
		// fact — those belong to the form-mapping pipeline, and dropping them
		// here would leave the form records that produced them with nothing
		// on the grid. An unrestricted caller still gets a true full reload
		// of every direct-entry fact.
		restrictedMembers, rErr := restrictedMemberIDs(ctx, s.pool, modelID, userID)
		if rErr != nil {
			return nil, fmt.Errorf("resolve restricted members: %w", rErr)
		}
		// Declared for the archive trigger (see migration 081): a full
		// reload is the one import that erases prior values.
		_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'import_full_reload'`)
		_, err = tx.Exec(ctx, `
			DELETE FROM runtime.fact_input fi
			WHERE fi.model_id = $1::uuid
			  AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
			  AND fi.source_ref IS NULL
			  AND NOT EXISTS (
			      SELECT 1 FROM identity.user_access_rule r
			      WHERE r.user_id = $3::uuid
			        AND r.rule_type = 'metric'
			        AND r.ref_id = fi.metric_id::text
			        AND r.access IN ('hidden', 'read')
			  )
			  AND NOT EXISTS (
			      SELECT 1
			      FROM jsonb_each_text(fi.dim_members) AS dm(dim_id, code)
			      JOIN model.dimension_member m
			        ON m.dimension_id = dm.dim_id::uuid AND m.code = dm.code
			      WHERE m.id::text = ANY($4)
			  )
		`, modelID, revisionID, userID, restrictedMembers)
		if err != nil {
			return nil, fmt.Errorf("full reload delete: %w", err)
		}
	}

	switch mode {
	case ModeFullReload, ModeReplace:
		// Both insert the staged value as-is; full_reload already deleted
		// every prior row above, replace leaves untouched cells alone and
		// relies on latest-wins to make the new row authoritative.
		_, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			SELECT $1::uuid, NULLIF($2,'')::uuid, metric_id, dim_members, value, $3::uuid
			FROM import.import_staging WHERE job_id = $4::uuid
		`, modelID, revisionID, userID, jobID)
	default: // ModeIncremental
		_, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			SELECT $1::uuid, NULLIF($2,'')::uuid, s.metric_id, s.dim_members,
			       s.value + COALESCE((
			           SELECT fi.value FROM runtime.fact_input fi
			           WHERE fi.model_id   = $1::uuid
			             AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
			             AND fi.metric_id  = s.metric_id
			             AND fi.dim_members = s.dim_members
			           ORDER BY fi.entered_at DESC
			           LIMIT 1
			       ), 0),
			       $3::uuid
			FROM import.import_staging s
			WHERE s.job_id = $4::uuid
		`, modelID, revisionID, userID, jobID)
	}
	if err != nil {
		return nil, fmt.Errorf("flush staging: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE import.import_job
		SET status = 'committed', completed_at = now()
		WHERE id = $1::uuid
	`, jobID)
	if err != nil {
		return nil, err
	}

	return metricIDs, tx.Commit(ctx)
}

// stagedDimUUID matches a well-formed UUID so a staged dim_members key can
// be checked before attempting a ::uuid cast in SQL — a failed cast inside a
// transaction poisons every statement after it until rollback, so this must
// be filtered in Go, not left for Postgres to reject per-row.
var stagedDimUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveStagedMemberIDs collects the distinct dimension_member UUIDs
// referenced by a staged batch's dim_members maps, for the write-guard
// check. dim_members keys are dimension IDs (UUIDs); a key that isn't a
// valid UUID (a legacy name-keyed fallback — see dimNameToID in the gateway
// HTTP handler) is skipped rather than failing resolution, matching the
// pre-existing lenient behavior of the inline check this replaces. Runs on
// s.pool (not a transaction) — read-only and logically prior to any write.
func (s *Store) resolveStagedMemberIDs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT dim_members FROM import.import_staging WHERE job_id=$1::uuid`, jobID)
	if err != nil {
		return nil, err
	}
	type dimCode struct{ dimID, code string }
	seen := map[dimCode]bool{}
	var pairs []dimCode
	for rows.Next() {
		var dmJSON []byte
		if err := rows.Scan(&dmJSON); err != nil {
			rows.Close()
			return nil, err
		}
		var dm map[string]string
		if json.Unmarshal(dmJSON, &dm) != nil {
			continue
		}
		for dimID, code := range dm {
			if !stagedDimUUID.MatchString(dimID) {
				continue // name-keyed fallback row: nothing to resolve
			}
			key := dimCode{dimID, code}
			if !seen[key] {
				seen[key] = true
				pairs = append(pairs, key)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var memberIDs []string
	for _, p := range pairs {
		var memberID string
		if err := s.pool.QueryRow(ctx,
			`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			p.dimID, p.code,
		).Scan(&memberID); err == nil {
			memberIDs = append(memberIDs, memberID)
		}
		// no matching member: skip (best-effort).
	}
	return memberIDs, nil
}

// resolveStagedMetricIDs returns the distinct metric_id values staged for
// jobID, for the per-metric writeguard.MetricAccess check.
func (s *Store) resolveStagedMetricIDs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT metric_id::text FROM import.import_staging WHERE job_id=$1::uuid`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metricIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		metricIDs = append(metricIDs, id)
	}
	return metricIDs, rows.Err()
}

func (s *Store) AbortImport(ctx context.Context, jobID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE import.import_job
		SET status = 'aborted', completed_at = now()
		WHERE id = $1::uuid AND status NOT IN ('committed', 'aborted')
	`, jobID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %s cannot be aborted", jobID)
	}
	return nil
}

func (s *Store) GetImportErrors(ctx context.Context, jobID string, limit int32) ([]*importpkgv1.ImportError, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT row_number, COALESCE(column_name,''), error_code, message, COALESCE(raw_value,'')
		FROM import.import_error
		WHERE job_id = $1::uuid
		ORDER BY row_number ASC LIMIT $2
	`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var errs []*importpkgv1.ImportError
	for rows.Next() {
		var e importpkgv1.ImportError
		if err := rows.Scan(&e.RowNumber, &e.Column, &e.ErrorCode, &e.Message, &e.RawValue); err != nil {
			return nil, err
		}
		errs = append(errs, &e)
	}
	return errs, rows.Err()
}

func importStatusFromString(s string) importpkgv1.ImportStatus {
	switch s {
	case "validating":
		return importpkgv1.ImportStatus_IMPORT_STATUS_VALIDATING
	case "staged":
		return importpkgv1.ImportStatus_IMPORT_STATUS_STAGED
	case "committed":
		return importpkgv1.ImportStatus_IMPORT_STATUS_COMMITTED
	case "failed":
		return importpkgv1.ImportStatus_IMPORT_STATUS_FAILED
	case "aborted":
		return importpkgv1.ImportStatus_IMPORT_STATUS_ABORTED
	default:
		return importpkgv1.ImportStatus_IMPORT_STATUS_UNSPECIFIED
	}
}

// restrictedMemberIDs returns every dimension member of this model/revision
// the user may not write — their own hidden/read rules plus everything those
// cascade onto down the parent_member_id chain, the same expansion the grid
// and chart read paths apply. Returns an empty slice for an unrestricted
// user, which makes the ANY($4) clause a no-op.
func restrictedMemberIDs(ctx context.Context, pool *pgxpool.Pool, modelID, userID string) ([]string, error) {
	out := []string{}
	if userID == "" {
		return out, nil
	}

	rules := map[string]string{}
	ruleRows, err := pool.Query(ctx, `
		SELECT ref_id, access FROM identity.user_access_rule
		WHERE user_id=$1::uuid AND rule_type='dimension_member' AND access IN ('hidden','read')
	`, userID)
	if err != nil {
		return nil, err
	}
	for ruleRows.Next() {
		var refID, access string
		if err := ruleRows.Scan(&refID, &access); err != nil {
			ruleRows.Close()
			return nil, err
		}
		// ExpandHidden cascades on the literal "hidden" marker; read-only
		// members are equally undeletable, so they enter the same expansion.
		rules[refID] = "hidden"
		_ = access
	}
	ruleRows.Close()
	if err := ruleRows.Err(); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return out, nil
	}

	// Every member of the model, across revisions: rules name specific member
	// IDs, so a wider edge set can only make the cascade more complete — a
	// member is restricted only if a rule reaches it. Avoids depending on
	// dimension_def.revision_id, which not every deployment of this schema has.
	edgeRows, err := pool.Query(ctx, `
		SELECT m.id::text, COALESCE(m.parent_member_id::text, '')
		FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.model_id=$1::uuid
	`, modelID)
	if err != nil {
		return nil, err
	}
	var edges []writeguard.MemberEdge
	for edgeRows.Next() {
		var e writeguard.MemberEdge
		if err := edgeRows.Scan(&e.ID, &e.ParentID); err != nil {
			edgeRows.Close()
			return nil, err
		}
		edges = append(edges, e)
	}
	edgeRows.Close()
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}

	for id := range writeguard.ExpandHidden(edges, rules) {
		out = append(out, id)
	}
	for id := range rules {
		out = append(out, id)
	}
	return out, nil
}
