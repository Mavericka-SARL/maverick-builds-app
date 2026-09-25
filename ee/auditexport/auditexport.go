// Package auditexport is the enterprise audit-log export: the tenant's
// events as a CSV download or as JSON Lines a SIEM collector pulls with a
// cursor, and a retention policy that removes events older than the tenant
// chooses to keep. Licensed under ee/LICENSE; gated by
// license.FeatureAuditExport.
//
// The events themselves come from pkg/auditlog's single write path; nothing
// here writes an event except the export's own record of having happened.
package auditexport

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Query selects the events to export. ScopeCond is the tenant predicate the
// gateway builds — the same one its audit listing uses — over the aliases
// ae (event), u (actor), app (application); ScopeArgs are its parameters,
// numbered from $1. Everything else is optional.
type Query struct {
	ScopeCond string
	ScopeArgs []any
	Since     *time.Time
	Until     *time.Time
	After     string // cursor from a previous page
	Category  string
	EventType string
	Limit     int

	noHeader bool // set by StreamAll for every page after the first
}

// Format is "csv" or "jsonl".
type Format string

const (
	FormatCSV   Format = "csv"
	FormatJSONL Format = "jsonl"
	// MaxLimit bounds one response; a collector follows the cursor.
	MaxLimit     = 100000
	DefaultLimit = 10000
)

// Event is one exported row. JSON Lines carries exactly this shape.
type Event struct {
	ID              string          `json:"id"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Category        string          `json:"category"`
	EventType       string          `json:"event_type"`
	ActorUserID     string          `json:"actor_user_id,omitempty"`
	ActorName       string          `json:"actor_name"`
	ActorEmail      string          `json:"actor_email,omitempty"`
	ActorRole       string          `json:"actor_role,omitempty"`
	ApplicationID   string          `json:"application_id,omitempty"`
	ApplicationName string          `json:"application_name,omitempty"`
	RevisionID      string          `json:"revision_id,omitempty"`
	RevisionName    string          `json:"revision_name,omitempty"`
	ResourceType    string          `json:"resource_type,omitempty"`
	ResourceID      string          `json:"resource_id,omitempty"`
	Metadata        json.RawMessage `json:"metadata"`
	BeforeState     json.RawMessage `json:"before_state,omitempty"`
	AfterState      json.RawMessage `json:"after_state,omitempty"`
}

// csvHeader is the column order of a CSV export; stable, so a spreadsheet
// built on one export keeps working on the next.
var csvHeader = []string{"occurred_at", "category", "event_type", "actor_name", "actor_email", "actor_role",
	"application_id", "application_name", "revision_id", "revision_name", "resource_type", "resource_id", "metadata", "id"}

// EncodeCursor names the position after an event: its time and id.
func EncodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// DecodeCursor inverts EncodeCursor.
func DecodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(c))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || len(id) != 36 {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	return t, id, nil
}

// Result is what a page of export produced.
type Result struct {
	Count      int
	NextCursor string // set when more events may follow
}

// Stream writes the selected events, oldest first, to w in the given
// format, one row at a time. It returns how many were written and a cursor
// for the next page when the page was full.
func Stream(ctx context.Context, pool *pgxpool.Pool, q Query, format Format, w io.Writer) (Result, error) {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	args := append([]any{}, q.ScopeArgs...)
	var where []string
	if q.ScopeCond != "" {
		where = append(where, q.ScopeCond)
	}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Since != nil {
		add("ae.occurred_at >= $%d", *q.Since)
	}
	if q.Until != nil {
		add("ae.occurred_at < $%d", *q.Until)
	}
	if q.Category != "" {
		add("ae.category::text = $%d", q.Category)
	}
	if q.EventType != "" {
		add("ae.event_type = $%d", q.EventType)
	}
	if q.After != "" {
		t, id, err := DecodeCursor(q.After)
		if err != nil {
			return Result{}, err
		}
		args = append(args, t, id)
		where = append(where, fmt.Sprintf("(ae.occurred_at, ae.id) > ($%d, $%d::uuid)", len(args)-1, len(args)))
	}
	sql := `
		SELECT ae.id::text, ae.occurred_at, ae.category::text, ae.event_type,
		       COALESCE(ae.actor_user_id::text,''), COALESCE(u.display_name, 'system'), COALESCE(u.email,''), COALESCE(ae.actor_role,''),
		       COALESCE(ae.application_id::text,''), COALESCE(app.name,''),
		       COALESCE(ae.revision_id::text,''), COALESCE(s.name,''),
		       COALESCE(ae.resource_type,''), COALESCE(ae.resource_id,''),
		       ae.metadata, ae.before_state, ae.after_state
		FROM audit.audit_event ae
		LEFT JOIN identity."user" u ON u.id = ae.actor_user_id
		LEFT JOIN core.application app ON app.id = ae.application_id
		LEFT JOIN model.revision s ON s.id = ae.revision_id`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit+1) // one extra tells us whether a next page exists
	sql += fmt.Sprintf(" ORDER BY ae.occurred_at, ae.id LIMIT $%d", len(args))

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return Result{}, fmt.Errorf("audit export: %w", err)
	}
	defer rows.Close()

	var cw *csv.Writer
	var enc *json.Encoder
	switch format {
	case FormatCSV:
		cw = csv.NewWriter(w)
		if !q.noHeader {
			if err := cw.Write(csvHeader); err != nil {
				return Result{}, err
			}
		}
	case FormatJSONL:
		enc = json.NewEncoder(w)
	default:
		return Result{}, fmt.Errorf("format must be csv or jsonl")
	}
	res := Result{}
	var last Event
	for rows.Next() {
		var e Event
		var meta, before, after []byte
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Category, &e.EventType, &e.ActorUserID, &e.ActorName, &e.ActorEmail, &e.ActorRole,
			&e.ApplicationID, &e.ApplicationName, &e.RevisionID, &e.RevisionName, &e.ResourceType, &e.ResourceID, &meta, &before, &after); err != nil {
			return res, err
		}
		if res.Count == q.Limit {
			res.NextCursor = EncodeCursor(last.OccurredAt, last.ID)
			break
		}
		e.Metadata = jsonOrNull(meta)
		e.BeforeState, e.AfterState = jsonOrNil(before), jsonOrNil(after)
		if cw != nil {
			if err := cw.Write([]string{e.OccurredAt.UTC().Format(time.RFC3339), e.Category, e.EventType, e.ActorName, e.ActorEmail, e.ActorRole,
				e.ApplicationID, e.ApplicationName, e.RevisionID, e.RevisionName, e.ResourceType, e.ResourceID, string(e.Metadata), e.ID}); err != nil {
				return res, err
			}
		} else if err := enc.Encode(e); err != nil {
			return res, err
		}
		res.Count++
		last = e
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if cw != nil {
		cw.Flush()
		if err := cw.Error(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// StreamAll writes every matching event — one CSV header, then page after
// page of MaxLimit — for a person's download, which (unlike a collector) has
// no way to follow a cursor. Its Result carries the total count and no cursor.
func StreamAll(ctx context.Context, pool *pgxpool.Pool, q Query, format Format, w io.Writer) (Result, error) {
	total := Result{}
	q.Limit = MaxLimit
	for {
		res, err := Stream(ctx, pool, q, format, w)
		total.Count += res.Count
		if err != nil || res.NextCursor == "" {
			return total, err
		}
		q.After, q.noHeader = res.NextCursor, true
	}
}

// A metadata column holds "{}" for most events, but rows written with a
// nil map carry the JSON scalar null; both read as an empty object.
func jsonOrNull(b []byte) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}

func jsonOrNil(b []byte) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// ── retention ─────────────────────────────────────────────────────────────────

// Settings is one scope's row of audit.settings: a tenant's, or — with no
// tenant — the deployment's own, which a tenant inherits until it sets its
// own on an edition that includes deployment settings (migration 091).
type Settings struct {
	// RetentionDays is how long events are kept; 0 keeps them forever, and
	// any other value is at least MinRetentionDays.
	RetentionDays int       `json:"retention_days"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// MinRetentionDays is the floor a non-zero retention may not go under.
const MinRetentionDays = 30

// DeploymentScope is the customer id of the deployment's own row.
const DeploymentScope = ""

// GetSettings reads one scope's row; found is false — and the retention
// "forever" — when the scope has never saved one.
func GetSettings(ctx context.Context, pool *pgxpool.Pool, customerID string) (s Settings, found bool, err error) {
	err = pool.QueryRow(ctx, `SELECT retention_days, updated_at FROM audit.settings WHERE customer_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid`,
		customerID).Scan(&s.RetentionDays, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("read audit settings: %w", err)
	}
	return s, true, nil
}

// UpdateSettings stores one scope's retention, creating the row on first
// save. The floor is enforced here as well as by the table, so the message
// names the rule rather than a constraint.
func UpdateSettings(ctx context.Context, pool *pgxpool.Pool, customerID string, days int) (Settings, error) {
	if days < 0 {
		return Settings{}, fmt.Errorf("retention_days cannot be negative")
	}
	if days != 0 && days < MinRetentionDays {
		return Settings{}, fmt.Errorf("retention must be 0 (keep forever) or at least %d days", MinRetentionDays)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit.settings (customer_id, retention_days) VALUES (NULLIF($1, '')::uuid, $2)
		ON CONFLICT (customer_id) DO UPDATE SET retention_days = EXCLUDED.retention_days, updated_at = now()`, customerID, days); err != nil {
		return Settings{}, fmt.Errorf("update audit settings: %w", err)
	}
	s, _, err := GetSettings(ctx, pool, customerID)
	return s, err
}

// ClearSettings removes a tenant's own row, so it inherits again.
func ClearSettings(ctx context.Context, pool *pgxpool.Pool, customerID string) error {
	if customerID == DeploymentScope {
		return fmt.Errorf("the deployment's own settings cannot be cleared")
	}
	_, err := pool.Exec(ctx, `DELETE FROM audit.settings WHERE customer_id = $1::uuid`, customerID)
	return err
}

// Defaults supplies the deployment's row from the control plane, or reports
// that this edition has none. Set once by cmd/gateway; nil means no
// edition-level defaults.
var Defaults func(ctx context.Context) (Settings, bool)

// Effective is the retention that applies to a tenant: its own, else the
// deployment's when the edition includes one, else forever.
func Effective(ctx context.Context, pool *pgxpool.Pool, customerID string) (s Settings, inherited bool, err error) {
	s, found, err := GetSettings(ctx, pool, customerID)
	if err != nil || found || customerID == DeploymentScope {
		return s, false, err
	}
	if Defaults != nil {
		if d, ok := Defaults(ctx); ok {
			return d, true, nil
		}
	}
	return Settings{}, false, nil
}

// Sweep removes, for every tenant of this database, the events older than
// that tenant's retention, and returns how many went. An event belongs to
// the tenant of its application, else of its actor; events with neither
// are the platform's and are kept. A retention of 0 removes nothing.
func Sweep(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	rows, err := pool.Query(ctx, `SELECT id::text FROM core.customer`)
	if err != nil {
		return 0, fmt.Errorf("audit retention sweep: %w", err)
	}
	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		tenants = append(tenants, id)
	}
	rows.Close()
	var removed int64
	for _, cid := range tenants {
		s, _, err := Effective(ctx, pool, cid)
		if err != nil {
			return removed, err
		}
		if s.RetentionDays == 0 {
			continue
		}
		tag, err := pool.Exec(ctx, `
			DELETE FROM audit.audit_event ae
			WHERE ae.occurred_at < now() - ($2 || ' days')::interval
			  AND COALESCE(
			        (SELECT app.customer_id FROM core.application app WHERE app.id = ae.application_id),
			        (SELECT u.customer_id FROM identity.user u WHERE u.id = ae.actor_user_id)
			      ) = $1::uuid`, cid, fmt.Sprint(s.RetentionDays))
		if err != nil {
			return removed, fmt.Errorf("audit retention sweep: %w", err)
		}
		removed += tag.RowsAffected()
	}
	return removed, nil
}

// RunRetention sweeps once, then every interval, while licensed reports
// true. A lapsed licence stops the sweep rather than the deletions: keeping
// events is the safe failure.
func RunRetention(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger, interval time.Duration, licensed func() bool) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if licensed() {
			if n, err := Sweep(ctx, pool); err != nil {
				log.Warn().Err(err).Msg("audit retention sweep failed")
			} else if n > 0 {
				log.Info().Int64("removed", n).Msg("audit retention sweep")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
