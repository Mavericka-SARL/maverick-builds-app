package aiassistant

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Document is the extracted-text record of a file uploaded into a session.
// The original binary is not retained — only the text the LLM will see.
type Document struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mime_type"`
	CharCount int       `json:"char_count"`
	Truncated bool      `json:"truncated"`
	CreatedAt time.Time `json:"created_at"`
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

func (s *DocumentStore) CreateDocument(ctx context.Context, sessionID, filename, mimeType, content string, truncated bool) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.document (session_id, filename, mime_type, char_count, truncated, content)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)
		RETURNING id::text, session_id::text, filename, mime_type, char_count, truncated, created_at
	`, sessionID, filename, mimeType, len(content), truncated, content).Scan(
		&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.CreatedAt,
	)
	return d, err
}

// ListDocuments returns session documents without their content.
func (s *DocumentStore) ListDocuments(ctx context.Context, sessionID string) ([]Document, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, filename, mime_type, char_count, truncated, created_at
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
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// ListDocumentsWithContent loads full document texts for LLM context injection.
func (s *DocumentStore) ListDocumentsWithContent(ctx context.Context, sessionID string) ([]Document, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, filename, mime_type, char_count, truncated, content, created_at
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
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Filename, &d.MimeType, &d.CharCount, &d.Truncated, &d.Content, &d.CreatedAt); err != nil {
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
