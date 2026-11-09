package importpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// JobDetails is what a line of Recent Imports says about an import beyond
// its counts: what it wrote into — a grid, a dimension or a table of
// records, named as it was then — how (a grid's import mode), and from which
// of the person's files. StartedAt, when set, is when the import began, so
// the line's duration covers reading and checking the file, not only the
// commit. The job's times stay on the database's clock — the time spent
// since StartedAt is taken off the database's now — as the gateway's may
// differ from it. Taken off the job's insert time instead, it moved a job
// described after a long commit before the import made just before it, and
// Recent Imports listed the two out of order (2026-11-02).
type JobDetails struct {
	TargetType string // "grid", "dimension" or "table"
	TargetName string
	Mode       string
	FileName   string
	StartedAt  time.Time
}

// DescribeJob records d on jobID.
func (s *Store) DescribeJob(ctx context.Context, jobID string, d JobDetails) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE import.import_job
		SET target_type = $2, target_name = $3, import_mode = $4, file_name = $5,
		    created_at = LEAST(created_at, now() - make_interval(secs => $6))
		WHERE id = $1::uuid
	`, jobID, d.TargetType, d.TargetName, d.Mode, JobFileName(d.FileName), spentSince(d.StartedAt))
	return err
}

// FinishedJob is an import that writes without staging — a dimension's
// members, a table's records — recorded once it is over, so Recent Imports
// lists every file import and not only a grid's. Rows is what was written,
// ErrorRows the rows that failed; Errors (kept with the job, the first shown
// on its line) say why.
type FinishedJob struct {
	ModelID, RevisionID, Source, UserID string
	Details                             JobDetails
	Rows, ErrorRows                     int
	Failed                              bool
	Errors                              []*importpkgv1.ImportError
}

// RecordJob writes j as a committed import job — or, Failed, a failed one —
// and answers its id.
func (s *Store) RecordJob(ctx context.Context, j FinishedJob) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, err := RecordJobTx(ctx, tx, j, "")
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// RecordJobTx is RecordJob on the caller's transaction, holding the
// Idempotency-Key the import was sent under (none when key is ""): an import
// that writes without staging — a table's records — records its job in the
// transaction that writes, so the key commits with what it wrote, or not at
// all. A key one of the person's imports of the model holds already is
// ErrImportKeyInUse, and the caller's transaction is then to roll back.
func RecordJobTx(ctx context.Context, tx pgx.Tx, j FinishedJob, key string) (string, error) {
	status := "committed"
	if j.Failed {
		status = "failed"
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO import.import_job
		    (model_id, revision_id, file_url, created_by, status, total_rows, valid_rows, error_rows,
		     created_at, completed_at, target_type, target_name, import_mode, file_name, idempotency_key)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, NULLIF($4,'')::uuid, $5::import.import_status, $6, $7, $8,
		        now() - make_interval(secs => $9), now(), $10, $11, $12, $13, NULLIF($14, ''))
		RETURNING id::text
	`, j.ModelID, j.RevisionID, j.Source, j.UserID, status, int32(j.Rows+j.ErrorRows), int32(j.Rows), int32(j.ErrorRows),
		spentSince(j.Details.StartedAt), j.Details.TargetType, j.Details.TargetName, j.Details.Mode, JobFileName(j.Details.FileName), key).Scan(&id)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return "", ErrImportKeyInUse
	}
	if err != nil {
		return "", err
	}
	if len(j.Errors) > 0 {
		// One statement: a refused file of 10,000 records has 10,000.
		rowNumbers := make([]int32, len(j.Errors))
		columns, codes, messages, raws := make([]string, len(j.Errors)), make([]string, len(j.Errors)), make([]string, len(j.Errors)), make([]string, len(j.Errors))
		for i, e := range j.Errors {
			rowNumbers[i], columns[i], codes[i], messages[i], raws[i] = e.RowNumber, e.Column, e.ErrorCode, e.Message, e.RawValue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO import.import_error (job_id, row_number, column_name, error_code, message, raw_value)
			SELECT $1::uuid, r, c, k, m, v
			FROM unnest($2::int4[], $3::text[], $4::text[], $5::text[], $6::text[]) AS u(r, c, k, m, v)
		`, id, rowNumbers, columns, codes, messages, raws); err != nil {
			return "", fmt.Errorf("record the job's errors: %w", err)
		}
	}
	return id, nil
}

// JobFileName is the name of the person's file as a job keeps it: the base
// name only (a browser may send a path), at most 255 characters.
func JobFileName(name string) string {
	name = strings.TrimSpace(name[strings.LastIndexAny(name, `/\`)+1:])
	if r := []rune(name); len(r) > 255 {
		name = string(r[:255])
	}
	return name
}

// spentSince is the seconds since started, 0 when it is not set.
func spentSince(started time.Time) float64 {
	if started.IsZero() {
		return 0
	}
	return time.Since(started).Seconds()
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

// ListImportJobs answers a model's newest import jobs — with revisionID,
// that revision's and those of no revision only: Recent Imports listed
// another revision's imports, and a deleted one's, as the open revision's
// history (2026-10-23).
func (s *Store) ListImportJobs(ctx context.Context, modelID, revisionID string, limit int32) ([]*importpkgv1.ImportJob, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, model_id::text, COALESCE(revision_id::text,''), status::text,
		       total_rows, valid_rows, error_rows, created_at, completed_at
		FROM import.import_job
		WHERE model_id = $1::uuid
		  AND ($3 = '' OR revision_id IS NULL OR revision_id::text = $3)
		ORDER BY created_at DESC LIMIT $2
	`, modelID, limit, revisionID)
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
	// Text is a text metric's cell (migration 111); nil for a number.
	Text      *string
	RowNumber int
}

// StageRows inserts validated rows into import.import_staging and records errors.
// It then updates job stats and transitions to 'staged' or 'failed'.
func (s *Store) StageRows(ctx context.Context, jobID string, rows []StagingRow, errs []*importpkgv1.ImportError) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// A statement per batch, not per row: 8,000 rows of two metrics were
	// 16,000 round trips to the database. Values go as numeric, every digit
	// of the float64 kept (a float8 cast keeps 15): a pick-list option's key
	// is a 16-digit number.
	const stageBatch = 5000
	for start := 0; start < len(rows); start += stageBatch {
		batch := rows[start:min(start+stageBatch, len(rows))]
		metricIDs := make([]string, len(batch))
		dims := make([]string, len(batch))
		values := make([]float64, len(batch))
		rowNumbers := make([]int32, len(batch))
		texts := make([]*string, len(batch))
		for i, row := range batch {
			dimJSON, _ := json.Marshal(row.DimMembers)
			metricIDs[i], dims[i], values[i], rowNumbers[i], texts[i] = row.MetricID, string(dimJSON), row.Value, int32(row.RowNumber), row.Text
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO import.import_staging (job_id, metric_id, dim_members, value, row_number, text_value)
			SELECT $1::uuid, m::uuid, d::jsonb, v, r, t
			FROM unnest($2::text[], $3::text[], $4::numeric[], $5::int4[], $6::text[]) AS u(m, d, v, r, t)
		`, jobID, metricIDs, dims, values, rowNumbers, texts)
		if err != nil {
			return fmt.Errorf("stage rows %d-%d: %w", batch[0].RowNumber, batch[len(batch)-1].RowNumber, err)
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
	// ModeFullReload first deletes the typed values of the grids the file
	// writes into — every input metric of each (a metric is on at most one
	// grid), plus a file metric on no grid — then inserts the file: those
	// grids become the file. Other grids keep their values (it used to wipe
	// the whole revision, 2026-11-09). Values the caller may not write and
	// values a table's records post stay.
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
// revisionID/userID/those members. Every import commit flows through this
// one place, so none can commit a write cells() itself would reject — see
// internal/writeguard's package doc for why this used to be bypassable.
func (s *Store) CommitImport(ctx context.Context, jobID, modelID, revisionID, userID string, mode ImportMode) ([]string, error) {
	metricIDs, _, err := s.CommitImportWith(ctx, jobID, modelID, revisionID, userID, mode, CommitOptions{})
	return metricIDs, err
}

// CommitOptions is what a commit does beyond writing its staged rows.
type CommitOptions struct {
	// LinkRef is the model link (integration_def.link_id) whose run this
	// is: each row the commit writes carries it (fact_input.link_ref,
	// migration 169). "" for any other import.
	LinkRef string
	// Reconcile, with LinkRef in replace mode, also clears the cells whose
	// latest typed value the link wrote and the job does not hold: values
	// its source no longer has. A source cell emptied since the last run
	// gives no record, so replace alone left the target's copy (2026-10-09).
	// A cell someone typed over since the link wrote it is kept.
	Reconcile bool
}

// CommitImportWith is CommitImport with opts; it also answers how many
// cells it cleared (CommitOptions.Reconcile), whose metrics are among those
// it returns for recalculation.
func (s *Store) CommitImportWith(ctx context.Context, jobID, modelID, revisionID, userID string, mode ImportMode, opts CommitOptions) ([]string, int, error) {
	if mode == "" {
		mode = ModeIncremental
	}
	reconcile := opts.Reconcile && opts.LinkRef != "" && mode == ModeReplace

	// Resolve referenced dimension members (best-effort) and run the shared
	// write guard BEFORE opening the commit transaction — on its own
	// connection (s.pool, not a tx), since a staged dim_members key that
	// isn't a real UUID (e.g. a legacy name-keyed fallback row) fails the
	// ::uuid cast, and inside a transaction a single failed statement
	// poisons every statement after it until rollback.
	memberIDs, err := s.resolveStagedMemberIDs(ctx, jobID)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve staged members: %w", err)
	}
	// The staged metrics, so metric-scoped workflow locks apply to exactly
	// the member×metric crossings this import writes (the same list feeds
	// the per-metric access check just below).
	stagedMetricIDs, err := s.resolveStagedMetricIDs(ctx, jobID)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve staged metrics: %w", err)
	}
	if reason, gErr := writeguard.CheckWriteMetrics(ctx, s.pool, modelID, revisionID, userID, memberIDs, stagedMetricIDs); gErr != nil {
		return nil, 0, fmt.Errorf("write guard: %w", gErr)
	} else if reason != "" {
		return nil, 0, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	}

	// A cell its metric's closed rules close takes no input (migration 147):
	// the import is refused as a typed cell would be.
	if closedCells, cErr := s.resolveStagedCells(ctx, jobID); cErr != nil {
		return nil, 0, fmt.Errorf("resolve staged cells: %w", cErr)
	} else if reason, gErr := writeguard.ClosedReason(ctx, s.pool, revisionID, closedCells); gErr != nil {
		return nil, 0, fmt.Errorf("write guard: %w", gErr)
	} else if reason != "" {
		return nil, 0, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	} else if reason, gErr := writeguard.PostedReason(ctx, s.pool, revisionID, closedCells); gErr != nil {
		return nil, 0, fmt.Errorf("write guard: %w", gErr)
	} else if reason != "" {
		// A cell a table of records posts into takes its value from them.
		return nil, 0, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	}

	// Metric-level access — CheckWriteMetrics above covers the workflow
	// lock, mirroring HTTP /api/cells' own two separate checks
	// (MetricAccess + CheckWrite). Without this, a user hidden/
	// read-restricted from a metric could still write it by importing a
	// CSV, even though the identical direct cell write is rejected.
	for _, metricID := range stagedMetricIDs {
		access, maErr := writeguard.MetricAccess(ctx, s.pool, userID, metricID)
		if maErr != nil {
			return nil, 0, fmt.Errorf("check metric access: %w", maErr)
		}
		if access == "hidden" || access == "read" {
			return nil, 0, fmt.Errorf("%w: access denied: the import references a metric outside your access scope", ErrWriteDenied)
		}
	}

	// The cells a model link's run clears, checked as writing them would be:
	// a cell the runner may not write is left alone, and a locked or closed
	// one refuses the run.
	var clearCells []linkCell
	if reconcile {
		if clearCells, err = s.linkCellsToClear(ctx, jobID, modelID, revisionID, userID, opts.LinkRef); err != nil {
			return nil, 0, fmt.Errorf("cells the link no longer holds: %w", err)
		}
		if reason, gErr := s.guardLinkCells(ctx, modelID, revisionID, userID, clearCells); gErr != nil {
			return nil, 0, fmt.Errorf("write guard: %w", gErr)
		} else if reason != "" {
			return nil, 0, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
		}
	}

	// A full reload deletes more than it stages: the values it clears are
	// checked against a running approval's locks too, as Clear values
	// checks them — the guard above covers only the file's own cells.
	var reload *fullReloadScope
	if mode == ModeFullReload {
		if reload, err = s.fullReloadScope(ctx, modelID, revisionID, userID, stagedMetricIDs); err != nil {
			return nil, 0, err
		}
		if err := reload.checkLocks(ctx, s.pool, modelID, revisionID, userID); err != nil {
			return nil, 0, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Verify job is staged
	var jobStatus string
	err = tx.QueryRow(ctx, `
		SELECT status::text FROM import.import_job WHERE id = $1::uuid
	`, jobID).Scan(&jobStatus)
	if err != nil {
		return nil, 0, fmt.Errorf("get job status: %w", err)
	}
	if jobStatus != "staged" {
		return nil, 0, fmt.Errorf("job %s cannot be committed (status: %s)", jobID, jobStatus)
	}

	// Collect distinct metric IDs before flushing
	mrows, err := tx.Query(ctx, `
		SELECT DISTINCT metric_id::text FROM import.import_staging WHERE job_id = $1::uuid
	`, jobID)
	if err != nil {
		return nil, 0, fmt.Errorf("collect metric ids: %w", err)
	}
	var metricIDs []string
	for mrows.Next() {
		var id string
		if err := mrows.Scan(&id); err != nil {
			mrows.Close()
			return nil, 0, err
		}
		metricIDs = append(metricIDs, id)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, 0, err
	}

	if reload != nil {
		// Declared for the archive trigger (see migration 081): a full
		// reload is the one import that erases prior values.
		_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'import_full_reload'`)
		// The metrics it cleared join the staged ones, so recalcAfterImport
		// recalculates their dependents too: a calculated metric of a
		// cleared one kept its old value (2026-11-09).
		crows, err := tx.Query(ctx, `
			WITH d AS (DELETE FROM runtime.fact_input fi WHERE `+reload.where+` RETURNING fi.metric_id)
			SELECT DISTINCT metric_id::text FROM d`, reload.args...)
		if err != nil {
			return nil, 0, fmt.Errorf("full reload delete: %w", err)
		}
		staged := make(map[string]bool, len(metricIDs))
		for _, id := range metricIDs {
			staged[id] = true
		}
		for crows.Next() {
			var id string
			if err := crows.Scan(&id); err != nil {
				crows.Close()
				return nil, 0, err
			}
			if !staged[id] {
				metricIDs = append(metricIDs, id)
			}
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return nil, 0, fmt.Errorf("full reload delete: %w", err)
		}
	}

	// A file is its rows entered top to bottom, so rows naming the same cell
	// combine as entering them one after another would, and each cell gets
	// one new row. Every row used to be inserted with the same entered_at,
	// so which of a cell's rows the reads took was the random id's tie-break.
	switch mode {
	case ModeFullReload, ModeReplace:
		// Both insert the cell's last row as-is; full_reload already deleted
		// every prior row above, replace leaves untouched cells alone and
		// relies on latest-wins to make the new row authoritative.
		// A model link's rows name the link (CommitOptions.LinkRef).
		_, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by, text_value, link_ref)
			SELECT DISTINCT ON (metric_id, dim_members)
			       $1::uuid, NULLIF($2,'')::uuid, metric_id, dim_members, value, $3::uuid, text_value, NULLIF($5,'')::uuid
			FROM import.import_staging WHERE job_id = $4::uuid
			ORDER BY metric_id, dim_members, row_number DESC
		`, modelID, revisionID, userID, jobID, opts.LinkRef)
	default: // ModeIncremental
		// Numbers add to the cell's latest value. A text or pick-list cell
		// holds a word or a member, not an amount: it takes the last row's,
		// as a typed value would (a pick-list's stored member key used to be
		// added to the new one, naming no member at all). The latest values
		// are read in one pass over the staged cells' history: a lookup per
		// row scanned the metric's whole history each time, so every load
		// of the same file took longer than the one before (8,000 rows: 5 s,
		// then 25, 46, 95, then past the gateway's 2-minute answer).
		//
		// The cells go into a table of this transaction's own, analysed, so
		// the plan knows how many there are: rows staged a moment ago are
		// absent from the statistics, and a plan made for one cell read the
		// whole history once per cell.
		if _, err = tx.Exec(ctx, `
			CREATE TEMP TABLE import_cells (
			    metric_id uuid, dim_members jsonb, replaces boolean,
			    added numeric, last_value numeric, last_text text
			) ON COMMIT DROP`); err != nil {
			return nil, 0, fmt.Errorf("stage cells: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO import_cells
			SELECT s.metric_id, s.dim_members,
			       bool_or(s.text_value IS NOT NULL OR md.picklist_dimension_id IS NOT NULL),
			       sum(s.value),
			       (array_agg(s.value ORDER BY s.row_number DESC))[1],
			       (array_agg(s.text_value ORDER BY s.row_number DESC))[1]
			FROM import.import_staging s
			JOIN model.metric_def md ON md.id = s.metric_id
			WHERE s.job_id = $1::uuid
			GROUP BY s.metric_id, s.dim_members
		`, jobID); err != nil {
			return nil, 0, fmt.Errorf("stage cells: %w", err)
		}
		if _, err = tx.Exec(ctx, `ANALYZE import_cells`); err != nil {
			return nil, 0, fmt.Errorf("stage cells: %w", err)
		}
		_, err = tx.Exec(ctx, `
			WITH latest AS (
			    SELECT DISTINCT ON (fi.metric_id, fi.dim_members) fi.metric_id, fi.dim_members, fi.value
			    FROM runtime.fact_input fi
			    JOIN import_cells c ON c.metric_id = fi.metric_id AND c.dim_members = fi.dim_members AND NOT c.replaces
			    WHERE fi.model_id = $1::uuid
			      AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
			    ORDER BY fi.metric_id, fi.dim_members, fi.entered_at DESC, fi.id DESC
			)
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by, text_value)
			SELECT $1::uuid, NULLIF($2,'')::uuid, c.metric_id, c.dim_members,
			       CASE WHEN c.replaces THEN c.last_value ELSE c.added + COALESCE(l.value, 0) END,
			       $3::uuid, CASE WHEN c.replaces THEN c.last_text END
			FROM import_cells c
			LEFT JOIN latest l ON l.metric_id = c.metric_id AND l.dim_members = c.dim_members
		`, modelID, revisionID, userID)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("flush staging: %w", err)
	}
	cleared := 0
	if len(clearCells) > 0 {
		var clearedMetrics []string
		if cleared, clearedMetrics, err = clearLinkCells(ctx, tx, modelID, revisionID, opts.LinkRef, clearCells); err != nil {
			return nil, 0, fmt.Errorf("clear what the source emptied: %w", err)
		}
		for _, id := range clearedMetrics {
			if !slices.Contains(metricIDs, id) {
				metricIDs = append(metricIDs, id)
			}
		}
	}
	// The staged rows are facts now and nothing reads them again; kept, they
	// counted towards the workspace's storage for an import that was done.
	if _, err = tx.Exec(ctx, `DELETE FROM import.import_staging WHERE job_id = $1::uuid`, jobID); err != nil {
		return nil, 0, fmt.Errorf("clear staging: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE import.import_job
		SET status = 'committed', completed_at = now()
		WHERE id = $1::uuid
	`, jobID)
	if err != nil {
		return nil, 0, err
	}

	return metricIDs, cleared, tx.Commit(ctx)
}

// linkCell is a cell a model link wrote: its metric and coordinate.
type linkCell struct {
	MetricID   string            `json:"metric_id"`
	DimMembers map[string]string `json:"dim_members"`
}

// linkCellsToClear lists the cells of revisionID whose latest typed value
// link wrote and job jobID does not hold, among the metrics the link has
// written: what its source no longer has. A cell someone typed over since
// has a latest row without the link and is not listed; nor is a cell userID
// may not write (a metric or member hidden or read-only for them), as a
// full reload leaves those.
func (s *Store) linkCellsToClear(ctx context.Context, jobID, modelID, revisionID, userID, link string) ([]linkCell, error) {
	restrictedMembers, restrictedMetrics, err := restrictedIDs(ctx, s.pool, modelID, revisionID, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve restricted members: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (fi.metric_id, fi.dim_members) fi.metric_id, fi.dim_members, fi.link_ref
			FROM runtime.fact_input fi
			WHERE fi.model_id = $1::uuid AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid AND fi.source_ref IS NULL
			  AND fi.metric_id IN (SELECT DISTINCT metric_id FROM runtime.fact_input
				WHERE link_ref = $3::uuid AND model_id = $1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid)
			ORDER BY fi.metric_id, fi.dim_members, fi.entered_at DESC, fi.id DESC
		)
		SELECT l.metric_id::text, l.dim_members FROM latest l
		WHERE l.link_ref = $3::uuid
		  AND NOT EXISTS (SELECT 1 FROM import.import_staging st
			WHERE st.job_id = $4::uuid AND st.metric_id = l.metric_id AND st.dim_members = l.dim_members)
		  AND NOT (l.metric_id::text = ANY($5::text[]))
		  AND NOT EXISTS (
			SELECT 1 FROM jsonb_each_text(l.dim_members) AS dm(dim_id, code)
			JOIN model.dimension_member m ON m.dimension_id::text = dm.dim_id AND m.code = dm.code
			WHERE m.id::text = ANY($6::text[]))
		ORDER BY 1, 2`, modelID, revisionID, link, jobID, restrictedMetrics, restrictedMembers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linkCell
	for rows.Next() {
		var c linkCell
		var raw []byte
		if err := rows.Scan(&c.MetricID, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &c.DimMembers) != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// guardLinkCells says why userID may not clear cells ("" when they may):
// the write guard's workflow locks and a system-managed revision, and the
// metrics' closed rules — what writing them would be refused for.
func (s *Store) guardLinkCells(ctx context.Context, modelID, revisionID, userID string, cells []linkCell) (string, error) {
	if len(cells) == 0 {
		return "", nil
	}
	var metricIDs, memberIDs []string
	seenMetric, seenMember := map[string]bool{}, map[string]bool{}
	guardCells := make([]writeguard.Cell, 0, len(cells))
	for _, c := range cells {
		if !seenMetric[c.MetricID] {
			seenMetric[c.MetricID] = true
			metricIDs = append(metricIDs, c.MetricID)
		}
		guardCells = append(guardCells, writeguard.Cell{MetricID: c.MetricID, DimCodes: c.DimMembers})
		for dimID, code := range c.DimMembers {
			if !stagedDimUUID.MatchString(dimID) || seenMember[dimID+"|"+code] {
				continue
			}
			seenMember[dimID+"|"+code] = true
			var id string
			if err := s.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&id); err == nil {
				memberIDs = append(memberIDs, id)
			}
		}
	}
	reason, err := writeguard.CheckWriteMetrics(ctx, s.pool, modelID, revisionID, userID, memberIDs, metricIDs)
	if err != nil || reason != "" {
		return reason, err
	}
	// Only the metrics with closed rules have cells to check, as for the
	// staged rows (resolveStagedCells).
	closing := map[string]bool{}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM model.metric_def WHERE id::text = ANY($1::text[]) AND closed_when <> '[]'::jsonb`, metricIDs)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		closing[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(closing) == 0 {
		return "", err
	}
	closed := guardCells[:0]
	for _, c := range guardCells {
		if closing[c.MetricID] {
			closed = append(closed, c)
		}
	}
	return writeguard.ClosedReason(ctx, s.pool, revisionID, closed)
}

// clearLinkCells deletes every typed row of cells whose latest typed value
// link still wrote — re-read in tx, so a value typed since the cells were
// listed stays — declared to cell history as link_source_emptied. It
// answers how many cells it cleared and their metrics.
func clearLinkCells(ctx context.Context, tx pgx.Tx, modelID, revisionID, link string, cells []linkCell) (int, []string, error) {
	raw, err := json.Marshal(cells)
	if err != nil {
		return 0, nil, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'link_source_emptied'`); err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH target AS (
			SELECT (c->>'metric_id')::uuid AS metric_id, c->'dim_members' AS dim_members
			FROM jsonb_array_elements($4::jsonb) c
		), latest AS (
			SELECT DISTINCT ON (fi.metric_id, fi.dim_members) fi.metric_id, fi.dim_members, fi.link_ref
			FROM runtime.fact_input fi
			JOIN target t ON t.metric_id = fi.metric_id AND t.dim_members = fi.dim_members
			WHERE fi.model_id = $1::uuid AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid AND fi.source_ref IS NULL
			ORDER BY fi.metric_id, fi.dim_members, fi.entered_at DESC, fi.id DESC
		), gone AS (
			DELETE FROM runtime.fact_input fi USING latest l
			WHERE fi.model_id = $1::uuid AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid AND fi.source_ref IS NULL
			  AND fi.metric_id = l.metric_id AND fi.dim_members = l.dim_members AND l.link_ref = $3::uuid
			RETURNING fi.metric_id, fi.dim_members
		)
		SELECT metric_id::text, count(DISTINCT dim_members)::int FROM gone GROUP BY metric_id`,
		modelID, revisionID, link, string(raw))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	n := 0
	var metricIDs []string
	for rows.Next() {
		var id string
		var cells int
		if err := rows.Scan(&id, &cells); err != nil {
			return 0, nil, err
		}
		n += cells
		metricIDs = append(metricIDs, id)
	}
	return n, metricIDs, rows.Err()
}

// StageNothing marks job jobID staged with no rows: a model link's run
// whose source holds no value at all, committed only to clear what the link
// wrote before (CommitOptions.Reconcile). StageRows fails a job with no
// rows, as a file with none is.
func (s *Store) StageNothing(ctx context.Context, jobID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE import.import_job SET status = 'staged', total_rows = 0, valid_rows = 0, error_rows = 0
		WHERE id = $1::uuid`, jobID)
	return err
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

// fullReloadScope is the typed values a full reload deletes, as a WHERE
// over runtime.fact_input fi and its arguments.
type fullReloadScope struct {
	where string
	args  []any
}

// fullReloadScope scopes a full reload to the grids its file writes into:
// the input metrics of every grid holding a staged metric (a metric is on at
// most one grid, migration 051), and a staged metric on no grid. It used to
// delete every typed value of the revision, so a file of one grid emptied
// the others (2026-11-09).
//
// Within them it deletes only what this caller could have written
// themselves: no metric they're hidden/read-restricted from, no combination
// touching a restricted dimension member (cascaded to descendants exactly as
// the read paths do), and never a form-posted value — those belong to the
// form-mapping pipeline, and dropping them here would leave the records that
// produced them with nothing on the grid. A narrowly-scoped user (one
// department, say) could otherwise destroy every other department's data by
// uploading a single row.
func (s *Store) fullReloadScope(ctx context.Context, modelID, revisionID, userID string, stagedMetricIDs []string) (*fullReloadScope, error) {
	restrictedMembers, restrictedMetrics, err := restrictedIDs(ctx, s.pool, modelID, revisionID, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve restricted members: %w", err)
	}
	if stagedMetricIDs == nil {
		stagedMetricIDs = []string{}
	}
	sc := &fullReloadScope{
		where: `fi.model_id = $1::uuid
		  AND fi.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		  AND fi.source_ref IS NULL
		  AND NOT (fi.metric_id::text = ANY($3::text[]))
		  AND fi.metric_id IN (
		      SELECT gm.metric_id FROM model.grid_metric gm
		      WHERE gm.grid_id IN (SELECT g.grid_id FROM model.grid_metric g WHERE g.metric_id = ANY($4::uuid[]))
		      UNION ALL SELECT unnest($4::uuid[]))`,
		args: []any{modelID, revisionID, restrictedMetrics, stagedMetricIDs},
	}
	// A member is found by its dimension's uuid, as Clear values finds it; a
	// caller with nothing restricted needs no check at all.
	if len(restrictedMembers) > 0 {
		sc.args = append(sc.args, restrictedMembers)
		sc.where += `
		  AND NOT EXISTS (
		      SELECT 1
		      FROM jsonb_each_text(fi.dim_members) AS dm(dim_id, code)
		      JOIN model.dimension_member m
		        ON m.dimension_id = dm.dim_id::uuid AND m.code = dm.code
		      WHERE m.id::text = ANY($5::text[]))`
	}
	return sc, nil
}

// checkLocks refuses the reload (ErrWriteDenied) when a running approval
// locks a value it would delete — its members, crossed with its metrics —
// as Clear values does. Nothing to delete, nothing to check.
func (sc *fullReloadScope) checkLocks(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID, userID string) error {
	rows, err := pool.Query(ctx, `SELECT DISTINCT fi.metric_id::text FROM runtime.fact_input fi WHERE `+sc.where, sc.args...)
	if err != nil {
		return fmt.Errorf("full reload scope: %w", err)
	}
	var metricIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		metricIDs = append(metricIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("full reload scope: %w", err)
	}
	if len(metricIDs) == 0 {
		return nil
	}
	memberIDs, err := clearedMemberIDs(ctx, pool, sc.where, sc.args)
	if err != nil {
		return fmt.Errorf("full reload members: %w", err)
	}
	if reason, err := writeguard.CheckWriteMetrics(ctx, pool, modelID, revisionID, userID, memberIDs, metricIDs); err != nil {
		return fmt.Errorf("write guard: %w", err)
	} else if reason != "" {
		return fmt.Errorf("%w: %s (a full reload clears every value of the grids its file writes into)", ErrWriteDenied, reason)
	}
	if reason, err := closedCellsReason(ctx, pool, revisionID, sc.where, sc.args); err != nil {
		return fmt.Errorf("write guard: %w", err)
	} else if reason != "" {
		return fmt.Errorf("%w: %s (a full reload clears every value of the grids its file writes into)", ErrWriteDenied, reason)
	}
	return nil
}

// closedCellsReason is writeguard.ClosedReason for the cells a full reload
// would clear — the fact_input rows matching where (alias fi) whose metric
// has closed rules — or "" when none of them is closed. A closed cell
// refused a file's value, but a full reload deleted the one it held
// (2026-11-09). A developer's Clear values is not checked: like a revision
// copy or a model package, it moves values as a whole (manual, Closed cells).
func closedCellsReason(ctx context.Context, pool *pgxpool.Pool, revisionID, where string, args []any) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT fi.metric_id::text, fi.dim_members FROM runtime.fact_input fi
		JOIN model.metric_def cm ON cm.id = fi.metric_id AND cm.closed_when <> '[]'::jsonb
		WHERE `+where, args...)
	if err != nil {
		return "", fmt.Errorf("cells to clear: %w", err)
	}
	var cells []writeguard.Cell
	for rows.Next() {
		var metricID string
		var raw []byte
		if err := rows.Scan(&metricID, &raw); err != nil {
			rows.Close()
			return "", err
		}
		var codes map[string]string
		if json.Unmarshal(raw, &codes) == nil {
			cells = append(cells, writeguard.Cell{MetricID: metricID, DimCodes: codes})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	return writeguard.ClosedReason(ctx, pool, revisionID, cells)
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

// resolveStagedCells lists the staged cells of the job's metrics that carry
// closed rules, for writeguard.ClosedReason (none: no query of the cells).
func (s *Store) resolveStagedCells(ctx context.Context, jobID string) ([]writeguard.Cell, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT st.metric_id::text, st.dim_members FROM import.import_staging st
		JOIN model.metric_def m ON m.id = st.metric_id
		WHERE st.job_id=$1::uuid AND m.closed_when <> '[]'::jsonb`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cells []writeguard.Cell
	for rows.Next() {
		var metricID string
		var raw []byte
		if err := rows.Scan(&metricID, &raw); err != nil {
			return nil, err
		}
		var codes map[string]string
		if json.Unmarshal(raw, &codes) == nil {
			cells = append(cells, writeguard.Cell{MetricID: metricID, DimCodes: codes})
		}
	}
	return cells, rows.Err()
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

// restrictedIDs returns every dimension member and every metric of this
// model/revision the user may not write — their own hidden/read rules plus,
// for members, everything those cascade onto down the parent_member_id
// chain, the same expansion the grid and chart read paths apply. The rules
// resolve by lineage against revisionID (writeguard.RulesForRevision), so
// a full reload into an older revision spares what the active one
// protects. Returns empty slices for an unrestricted user, which makes the
// ANY() clauses no-ops.
func restrictedIDs(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID, userID string) (members, metrics []string, err error) {
	out := []string{}
	metrics = []string{}
	if userID == "" {
		return out, metrics, nil
	}

	all, err := writeguard.RulesForRevision(ctx, pool, userID, revisionID)
	if err != nil {
		return nil, nil, err
	}
	rules := map[string]string{}
	for _, ar := range all {
		if ar.Access != "hidden" && ar.Access != "read" {
			continue
		}
		switch ar.Type {
		case "dimension_member":
			// ExpandHidden cascades on the literal "hidden" marker; read-only
			// members are equally undeletable, so they enter the same
			// expansion.
			rules[ar.RefID] = "hidden"
		case "metric":
			metrics = append(metrics, ar.RefID)
		}
	}
	if len(rules) == 0 {
		return out, metrics, nil
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
		return nil, nil, err
	}
	var edges []writeguard.MemberEdge
	for edgeRows.Next() {
		var e writeguard.MemberEdge
		if err := edgeRows.Scan(&e.ID, &e.ParentID); err != nil {
			edgeRows.Close()
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	edgeRows.Close()
	if err := edgeRows.Err(); err != nil {
		return nil, nil, err
	}

	for id := range writeguard.ExpandHidden(edges, rules) {
		out = append(out, id)
	}
	for id := range rules {
		out = append(out, id)
	}
	return out, metrics, nil
}
