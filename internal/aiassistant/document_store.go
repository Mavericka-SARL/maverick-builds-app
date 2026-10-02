package aiassistant

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Document is the extracted-text record of a file uploaded into a session.
// A spreadsheet also keeps its original bytes (raw_data), so the assistant
// can import the whole file; every other type keeps only the text the LLM
// will see.
type Document struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	CharCount int    `json:"char_count"`
	Truncated bool   `json:"truncated"`
	// Importable: the original spreadsheet is kept, so import_file_data
	// can read it.
	Importable bool      `json:"importable"`
	CreatedAt  time.Time `json:"created_at"`
	// Content is omitted from list responses (can be large); loaded only when
	// building the LLM context.
	Content string `json:"-"`
}

type DocumentStore struct {
	pool *pgxpool.Pool
}

func NewDocumentStore(pool *pgxpool.Pool) *DocumentStore {
	return &DocumentStore{pool: pool}
}

// CreateDocument stores a document's extracted text and, for a spreadsheet,
// its original bytes (raw; nil for any other file).
func (s *DocumentStore) CreateDocument(ctx context.Context, sessionID, filename, mimeType, content string, truncated bool, raw []byte) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.document (session_id, filename, mime_type, char_count, truncated, content, raw_data)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, session_id::text, filename, mime_type, char_count, truncated, raw_data IS NOT NULL, created_at
	`, sessionID, filename, mimeType, len(content), truncated, content, raw).Scan(
		&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.Importable, &d.CreatedAt,
	)
	return d, err
}

// GetRaw returns a session document's original bytes, found by id or by
// file name (case-insensitive; the latest upload of that name wins).
func (s *DocumentStore) GetRaw(ctx context.Context, sessionID, ref string) (Document, []byte, error) {
	var d Document
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, session_id::text, filename, mime_type, char_count, truncated, raw_data IS NOT NULL, created_at, raw_data
		FROM ai_assistant.document
		WHERE session_id=$1::uuid AND (id::text = $2 OR lower(filename) = lower($2))
		ORDER BY (id::text = $2) DESC, created_at DESC LIMIT 1
	`, sessionID, ref).Scan(&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.Importable, &d.CreatedAt, &raw)
	return d, raw, err
}

// ListDocuments returns session documents without their content.
func (s *DocumentStore) ListDocuments(ctx context.Context, sessionID string) ([]Document, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, filename, mime_type, char_count, truncated, raw_data IS NOT NULL, created_at
		FROM ai_assistant.document
		WHERE session_id=$1::uuid ORDER BY created_at
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.Importable, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// ListDocumentsWithContent loads full document texts for LLM context injection.
func (s *DocumentStore) ListDocumentsWithContent(ctx context.Context, sessionID string) ([]Document, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, filename, mime_type, char_count, truncated, raw_data IS NOT NULL, content, created_at
		FROM ai_assistant.document
		WHERE session_id=$1::uuid ORDER BY created_at
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.Importable, &d.Content, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *DocumentStore) DeleteDocument(ctx context.Context, sessionID, documentID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM ai_assistant.document WHERE id=$1::uuid AND session_id=$2::uuid
	`, documentID, sessionID)
	return err
}
