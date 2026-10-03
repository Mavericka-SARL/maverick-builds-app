package aiassistant

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// Conversion is an attachment converted to the layout an import reads
// (prepare_converted_file): the attachment, how it is reshaped and
// column-mapped, and what that made. The file is rebuilt on each download.
type Conversion struct {
	ID         string             `json:"id"`
	SessionID  string             `json:"session_id"`
	DocumentID string             `json:"document_id"`
	Sheet      string             `json:"sheet,omitempty"`
	Reshape    *importpkg.Reshape `json:"reshape,omitempty"`
	ColumnMap  map[string]string  `json:"column_map,omitempty"`
	Filename   string             `json:"filename"`
	RowCount   int                `json:"row_count"`
	Columns    []string           `json:"columns"`
	CreatedAt  time.Time          `json:"created_at"`
}

// ConversionStore keeps a session's conversions.
type ConversionStore struct{ pool *pgxpool.Pool }

func NewConversionStore(pool *pgxpool.Pool) *ConversionStore { return &ConversionStore{pool: pool} }

// Save records a conversion and returns it with its id.
func (s *ConversionStore) Save(ctx context.Context, c Conversion) (Conversion, error) {
	reshape, _ := json.Marshal(c.Reshape)
	columnMap, _ := json.Marshal(c.ColumnMap)
	if c.Columns == nil {
		c.Columns = []string{}
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.conversion (session_id, document_id, sheet, reshape, column_map, filename, row_count, columns)
		VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,'null')::jsonb, NULLIF($5,'null')::jsonb, $6, $7, $8)
		RETURNING id::text, created_at`,
		c.SessionID, c.DocumentID, c.Sheet, string(reshape), string(columnMap), c.Filename, c.RowCount, c.Columns,
	).Scan(&c.ID, &c.CreatedAt)
	return c, err
}

// List returns a session's conversions, oldest first.
func (s *ConversionStore) List(ctx context.Context, sessionID string) ([]Conversion, error) {
	rows, err := s.pool.Query(ctx, conversionSelect+` WHERE session_id=$1::uuid ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversion{}
	for rows.Next() {
		c, err := scanConversion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns one of a session's conversions.
func (s *ConversionStore) Get(ctx context.Context, sessionID, id string) (Conversion, error) {
	return scanConversion(s.pool.QueryRow(ctx, conversionSelect+` WHERE session_id=$1::uuid AND id::text=$2`, sessionID, id))
}

const conversionSelect = `
	SELECT id::text, session_id::text, document_id::text, sheet, COALESCE(reshape,'null'::jsonb), COALESCE(column_map,'null'::jsonb),
	       filename, row_count, columns, created_at
	FROM ai_assistant.conversion`

func scanConversion(row interface{ Scan(...any) error }) (Conversion, error) {
	var c Conversion
	var reshape, columnMap []byte
	if err := row.Scan(&c.ID, &c.SessionID, &c.DocumentID, &c.Sheet, &reshape, &columnMap, &c.Filename, &c.RowCount, &c.Columns, &c.CreatedAt); err != nil {
		return Conversion{}, err
	}
	_ = json.Unmarshal(reshape, &c.Reshape)
	_ = json.Unmarshal(columnMap, &c.ColumnMap)
	return c, nil
}
