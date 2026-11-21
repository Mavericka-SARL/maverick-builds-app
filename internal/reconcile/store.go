package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Saved is a revision's reconciliation without its workbook's bytes.
type Saved struct {
	RevisionID   string     `json:"revision_id"`
	FileName     string     `json:"file_name"`
	WorkbookSize int        `json:"workbook_size"`
	Sheets       []string   `json:"sheets,omitempty"`
	UploadedAt   *time.Time `json:"uploaded_at,omitempty"`
	Map          Map        `json:"map"`
	Cases        []Case     `json:"cases"`
	Status       string     `json:"status"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	Report       *Report    `json:"report,omitempty"`
}

// Load reads a revision's reconciliation; nil when it has none.
func Load(ctx context.Context, q DB, revisionID string) (*Saved, error) {
	s := &Saved{RevisionID: revisionID}
	var mapRaw, casesRaw, reportRaw []byte
	var errText *string
	err := q.QueryRow(ctx, `
		SELECT file_name, COALESCE(octet_length(workbook), 0), uploaded_at, map, cases, status, started_at, finished_at, error, report
		FROM model.reconciliation WHERE revision_id = $1::uuid`, revisionID).Scan(
		&s.FileName, &s.WorkbookSize, &s.UploadedAt, &mapRaw, &casesRaw, &s.Status, &s.StartedAt, &s.FinishedAt, &errText, &reportRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(mapRaw, &s.Map)
	_ = json.Unmarshal(casesRaw, &s.Cases)
	if s.Map.Rules == nil {
		s.Map.Rules = []Rule{}
	}
	if s.Cases == nil {
		s.Cases = []Case{}
	}
	if errText != nil {
		s.Error = *errText
	}
	if len(reportRaw) > 0 {
		s.Report = &Report{}
		_ = json.Unmarshal(reportRaw, s.Report)
	}
	return s, nil
}

// Workbook is the stored workbook and its file name (nil without one).
func Workbook(ctx context.Context, q DB, revisionID string) ([]byte, string, error) {
	var data []byte
	var name string
	err := q.QueryRow(ctx, `SELECT workbook, file_name FROM model.reconciliation WHERE revision_id = $1::uuid`, revisionID).Scan(&data, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	return data, name, err
}

// SaveWorkbook stores (or replaces) the workbook.
func SaveWorkbook(ctx context.Context, q DB, revisionID, fileName string, data []byte, userID string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO model.reconciliation (revision_id, file_name, workbook, uploaded_by, uploaded_at)
		VALUES ($1::uuid, $2, $3, NULLIF($4, '')::uuid, now())
		ON CONFLICT (revision_id) DO UPDATE SET file_name = EXCLUDED.file_name, workbook = EXCLUDED.workbook,
		    uploaded_by = EXCLUDED.uploaded_by, uploaded_at = now()`, revisionID, fileName, data, userID)
	return err
}

// SaveMap stores the map and the cases.
func SaveMap(ctx context.Context, q DB, revisionID string, m Map, cases []Case) error {
	if cases == nil {
		cases = []Case{}
	}
	mj, _ := json.Marshal(m)
	cj, _ := json.Marshal(cases)
	_, err := q.Exec(ctx, `
		INSERT INTO model.reconciliation (revision_id, map, cases) VALUES ($1::uuid, $2::jsonb, $3::jsonb)
		ON CONFLICT (revision_id) DO UPDATE SET map = EXCLUDED.map, cases = EXCLUDED.cases`, revisionID, string(mj), string(cj))
	return err
}

// ErrRunning: a run of the revision is under way.
var ErrRunning = errors.New("a reconciliation of this revision is running")

// Begin marks a run started; ErrRunning while one is (a run left running
// for an hour is taken as gone).
func Begin(ctx context.Context, q DB, revisionID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE model.reconciliation SET status = 'running', started_at = now(), finished_at = NULL, error = NULL
		WHERE revision_id = $1::uuid AND (status <> 'running' OR started_at < now() - interval '1 hour')`, revisionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRunning
	}
	return nil
}

// Finish keeps a run's report, or its error.
func Finish(ctx context.Context, q DB, revisionID string, rep *Report, runErr error) error {
	if runErr != nil {
		_, err := q.Exec(ctx, `UPDATE model.reconciliation SET status = 'failed', finished_at = now(), error = $2 WHERE revision_id = $1::uuid`,
			revisionID, runErr.Error())
		return err
	}
	rj, _ := json.Marshal(rep)
	_, err := q.Exec(ctx, `UPDATE model.reconciliation SET status = 'done', finished_at = now(), error = NULL, report = $2::jsonb WHERE revision_id = $1::uuid`,
		revisionID, string(rj))
	return err
}

// Delete removes a revision's reconciliation.
func Delete(ctx context.Context, q DB, revisionID string) error {
	_, err := q.Exec(ctx, `DELETE FROM model.reconciliation WHERE revision_id = $1::uuid`, revisionID)
	return err
}
