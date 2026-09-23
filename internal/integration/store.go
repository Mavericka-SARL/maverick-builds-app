package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the connector's persistence layer: credential connections,
// rest_api integration definitions (typed config), schedules, and the
// durable run queue the worker claims from.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ── Config hashing (activation gate) ─────────────────────────────────────────

// ConfigHash covers exactly the request-critical parts: changing any of them
// invalidates the last successful test. Mapping/limits/schedule changes do
// NOT invalidate a test — they don't change what is sent where.
func ConfigHash(c *Config) string {
	doc := struct {
		Direction  Direction        `json:"d"`
		TargetType TargetType       `json:"tt"`
		TargetID   string           `json:"t"`
		Request    RequestConfig    `json:"r"`
		Auth       AuthPlacement    `json:"a"`
		Response   ResponseConfig   `json:"re"`
		Pagination PaginationConfig `json:"p"`
	}{c.Direction, c.TargetType, c.TargetID, c.Request, c.Auth, c.Response, c.Pagination}
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// SanitizedHost is what run history and cards may show of a URL.
func SanitizedHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// SanitizedURL strips query (api keys ride there) and userinfo.
func SanitizedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.RawQuery, u.User, u.Fragment = "", nil, ""
	return u.String()
}

// ── Connections ──────────────────────────────────────────────────────────────

type Connection struct {
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	Name          string          `json:"name"`
	AuthType      string          `json:"auth_type"`
	Meta          json.RawMessage `json:"meta"`
	// HasSecret is all a client ever learns about the secret.
	HasSecret bool      `json:"has_secret"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func validAuthType(t string) bool {
	switch t {
	case "none", "api_key", "bearer", "basic", "oauth2_client_credentials", "oauth2_authorization_code":
		return true
	}
	return false
}

// CreateConnection stores public metadata and, when secret is non-nil, the
// sealed credential. The secret parameter is consumed here and never stored
// anywhere else.
func (s *Store) CreateConnection(ctx context.Context, appID, name, authType string, meta json.RawMessage, secret []byte, createdBy string) (*Connection, error) {
	if !validAuthType(authType) {
		return nil, fmt.Errorf("auth_type %q is not supported", authType)
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO model.integration_connection (application_id, name, auth_type, meta, created_by)
		VALUES ($1::uuid, $2, $3, $4::jsonb, NULLIF($5,'')::uuid) RETURNING id::text
	`, appID, name, authType, string(meta), createdBy).Scan(&id); err != nil {
		return nil, fmt.Errorf("create connection: %w", err)
	}
	if secret != nil {
		sealed, err := EncryptCredential(secret, appID, id)
		if err != nil {
			// Encryption unavailable ⇒ the row must not survive half-made.
			_, _ = s.pool.Exec(ctx, `DELETE FROM model.integration_connection WHERE id=$1::uuid`, id)
			return nil, err
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE model.integration_connection SET secret_enc=$2, updated_at=now() WHERE id=$1::uuid
		`, id, sealed); err != nil {
			return nil, fmt.Errorf("store credential: %w", err)
		}
	}
	return s.GetConnection(ctx, appID, id)
}

// UpdateConnection implements Keep / Replace / Remove:
//   - secret == nil        → keep the stored credential (metadata-only edit)
//   - len(secret) == 0     → remove the credential
//   - len(secret) > 0      → replace it
func (s *Store) UpdateConnection(ctx context.Context, appID, id, name, authType string, meta json.RawMessage, secret []byte) (*Connection, error) {
	if authType != "" && !validAuthType(authType) {
		return nil, fmt.Errorf("auth_type %q is not supported", authType)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE model.integration_connection
		SET name = COALESCE(NULLIF($3,''), name),
		    auth_type = COALESCE(NULLIF($4,''), auth_type),
		    meta = COALESCE($5::jsonb, meta),
		    updated_at = now()
		WHERE id=$2::uuid AND application_id=$1::uuid
	`, appID, id, name, authType, metaOrNil(meta))
	if err != nil {
		return nil, fmt.Errorf("update connection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}
	if secret != nil {
		sealed := ""
		if len(secret) > 0 {
			var eerr error
			sealed, eerr = EncryptCredential(secret, appID, id)
			if eerr != nil {
				return nil, eerr
			}
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE model.integration_connection SET secret_enc=$2, updated_at=now() WHERE id=$1::uuid
		`, id, sealed); err != nil {
			return nil, fmt.Errorf("store credential: %w", err)
		}
	}
	return s.GetConnection(ctx, appID, id)
}

func metaOrNil(m json.RawMessage) *string {
	if len(m) == 0 {
		return nil
	}
	s := string(m)
	return &s
}

func (s *Store) DeleteConnection(ctx context.Context, appID, id string) error {
	// Refuse while referenced: silently orphaning integrations to SET NULL
	// would flip them broken; the developer unbinds or deletes them first.
	var refs int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model.integration_def WHERE connection_id=$1::uuid
	`, id).Scan(&refs)
	if refs > 0 {
		return fmt.Errorf("connection is used by %d integration(s); unbind them first", refs)
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM model.integration_connection WHERE id=$1::uuid AND application_id=$2::uuid
	`, id, appID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) GetConnection(ctx context.Context, appID, id string) (*Connection, error) {
	var c Connection
	var meta string
	if err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, name, auth_type, meta::text,
		       secret_enc <> '', created_at, updated_at
		FROM model.integration_connection
		WHERE id=$1::uuid AND application_id=$2::uuid
	`, id, appID).Scan(&c.ID, &c.ApplicationID, &c.Name, &c.AuthType, &meta, &c.HasSecret, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Meta = json.RawMessage(meta)
	return &c, nil
}

func (s *Store) ListConnections(ctx context.Context, appID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, application_id::text, name, auth_type, meta::text,
		       secret_enc <> '', created_at, updated_at
		FROM model.integration_connection WHERE application_id=$1::uuid ORDER BY name
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var c Connection
		var meta string
		if err := rows.Scan(&c.ID, &c.ApplicationID, &c.Name, &c.AuthType, &meta, &c.HasSecret, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Meta = json.RawMessage(meta)
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenCredential returns the decrypted secret document — worker-side only;
// no HTTP handler may ever call this on a response path.
func (s *Store) OpenCredential(ctx context.Context, appID, id string) (authType string, meta json.RawMessage, secret []byte, err error) {
	var sealed, metaStr string
	if err = s.pool.QueryRow(ctx, `
		SELECT auth_type, meta::text, secret_enc FROM model.integration_connection
		WHERE id=$1::uuid AND application_id=$2::uuid
	`, id, appID).Scan(&authType, &metaStr, &sealed); err != nil {
		return "", nil, nil, err
	}
	meta = json.RawMessage(metaStr)
	if sealed == "" {
		return authType, meta, nil, nil
	}
	secret, err = DecryptCredential(sealed, appID, id)
	return authType, meta, secret, err
}

// ── Definitions ──────────────────────────────────────────────────────────────

type Definition struct {
	ID             string    `json:"id"`
	ModelID        string    `json:"model_id"`
	RevisionID     string    `json:"revision_id,omitempty"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Status         string    `json:"status"` // draft | active
	Tags           []string  `json:"tags"`
	Direction      Direction `json:"direction"`
	Enabled        bool      `json:"enabled"`
	ConnectionID   string    `json:"connection_id,omitempty"`
	Config         *Config   `json:"config,omitempty"`
	ConfigVersion  int       `json:"config_version"`
	LastTestedHash string    `json:"last_tested_hash,omitempty"`
	LastTestedAt   *time.Time `json:"last_tested_at,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Tested reports whether the CURRENT config passed its test.
func (d *Definition) Tested() bool {
	return d.Config != nil && d.LastTestedHash != "" && d.LastTestedHash == ConfigHash(d.Config)
}

// CreateDefinition writes metadata + typed config atomically (single INSERT —
// deliberately not the legacy create-then-PATCH-config sequence).
func (s *Store) CreateDefinition(ctx context.Context, modelID, revisionID, name, description string, tags []string, status string, connectionID string, cfg *Config, allowInsecure bool) (*Definition, error) {
	if status != "draft" && status != "active" {
		return nil, fmt.Errorf("status must be draft or active")
	}
	if status == "active" {
		if err := cfg.Validate(allowInsecure); err != nil {
			return nil, err
		}
	} else if cfg.Kind != ConfigKind {
		return nil, fmt.Errorf("config kind must be %s", ConfigKind)
	}
	raw, _ := json.Marshal(cfg)
	if tags == nil {
		tags = []string{}
	}
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO model.integration_def
		    (model_id, revision_id, name, description, type, target_type, target_id, config,
		     status, tags, direction, connection_id, enabled)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, 'rest_api', $5, NULLIF($6,'')::uuid, $7::jsonb,
		        $8, $9, $10, NULLIF($11,'')::uuid, true)
		RETURNING id::text
	`, modelID, revisionID, name, description, string(cfg.TargetType), cfg.TargetID, string(raw),
		status, tags, string(cfg.Direction), connectionID).Scan(&id); err != nil {
		return nil, fmt.Errorf("create integration: %w", err)
	}
	return s.GetDefinition(ctx, modelID, id)
}

// UpdateDefinition PATCHes metadata and/or config atomically. cfg == nil
// leaves the stored config untouched.
func (s *Store) UpdateDefinition(ctx context.Context, modelID, id string, name, description *string, tags []string, status *string, connectionID *string, cfg *Config, allowInsecure bool) (*Definition, error) {
	cur, err := s.GetDefinition(ctx, modelID, id)
	if err != nil {
		return nil, err
	}
	next := *cur
	if name != nil {
		next.Name = *name
	}
	if description != nil {
		next.Description = *description
	}
	if tags != nil {
		next.Tags = tags
	}
	if status != nil {
		if *status != "draft" && *status != "active" {
			return nil, fmt.Errorf("status must be draft or active")
		}
		next.Status = *status
	}
	if connectionID != nil {
		next.ConnectionID = *connectionID
	}
	if cfg != nil {
		next.Config = cfg
		next.ConfigVersion = cur.ConfigVersion + 1
	}
	if next.Status == "active" {
		if next.Config == nil {
			return nil, fmt.Errorf("cannot activate without configuration")
		}
		if err := next.Config.Validate(allowInsecure); err != nil {
			return nil, err
		}
		// Activation gate: the current config must have passed a test.
		if next.LastTestedHash == "" || next.LastTestedHash != ConfigHash(next.Config) {
			return nil, fmt.Errorf("activation requires a successful test of the current configuration")
		}
	}
	raw, _ := json.Marshal(next.Config)
	tag, err := s.pool.Exec(ctx, `
		UPDATE model.integration_def
		SET name=$3, description=$4, tags=$5, status=$6,
		    connection_id=NULLIF($7,'')::uuid, config=$8::jsonb,
		    config_version=$9, target_type=$10, target_id=NULLIF($11,'')::uuid,
		    direction=$12
		WHERE id=$2::uuid AND model_id=$1::uuid AND type='rest_api'
	`, modelID, id, next.Name, next.Description, next.Tags, next.Status,
		next.ConnectionID, string(raw), next.ConfigVersion,
		string(next.Config.TargetType), next.Config.TargetID, string(next.Config.Direction))
	if err != nil {
		return nil, fmt.Errorf("update integration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, pgx.ErrNoRows
	}
	return s.GetDefinition(ctx, modelID, id)
}

// MarkTested records a successful test of the config identified by hash.
func (s *Store) MarkTested(ctx context.Context, id, hash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE model.integration_def SET last_tested_hash=$2, last_tested_at=now() WHERE id=$1::uuid
	`, id, hash)
	return err
}

func (s *Store) GetDefinition(ctx context.Context, modelID, id string) (*Definition, error) {
	var d Definition
	var rawCfg string
	var revID, connID *string
	if err := s.pool.QueryRow(ctx, `
		SELECT id::text, model_id::text, COALESCE(revision_id::text,''), name, description,
		       status, tags, direction, enabled, connection_id::text,
		       config::text, config_version, last_tested_hash, last_tested_at, created_at
		FROM model.integration_def
		WHERE id=$1::uuid AND model_id=$2::uuid AND type='rest_api'
	`, id, modelID).Scan(&d.ID, &d.ModelID, &revID, &d.Name, &d.Description,
		&d.Status, &d.Tags, &d.Direction, &d.Enabled, &connID,
		&rawCfg, &d.ConfigVersion, &d.LastTestedHash, &d.LastTestedAt, &d.CreatedAt); err != nil {
		return nil, err
	}
	if revID != nil {
		d.RevisionID = *revID
	}
	if connID != nil {
		d.ConnectionID = *connID
	}
	var cfg Config
	if json.Unmarshal([]byte(rawCfg), &cfg) == nil && cfg.Kind == ConfigKind {
		d.Config = &cfg
	}
	return &d, nil
}

// ── Run queue ────────────────────────────────────────────────────────────────

type Run struct {
	ID            string     `json:"id"`
	IntegrationID string     `json:"integration_id"`
	Status        string     `json:"status"`
	TriggerType   string     `json:"trigger_type"`
	DryRun        bool       `json:"dry_run"`
	RunBy         string     `json:"run_by,omitempty"`
	ScheduledFor  *time.Time `json:"scheduled_for,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	DurationMS    int        `json:"duration_ms"`
	Pages         int        `json:"pages"`
	Requests      int        `json:"requests"`
	Retries       int        `json:"retries"`
	RecordsRead   int        `json:"records_read"`
	RecordsWritten int       `json:"records_written"`
	RecordsSkipped int       `json:"records_skipped"`
	ErrorCode     string     `json:"error_code,omitempty"`
	Message       string     `json:"message,omitempty"`
	Meta          json.RawMessage `json:"meta,omitempty"`
}

// Enqueue inserts a queued run and returns its ID. scheduledFor non-nil makes
// the insert idempotent per (integration, tick).
func (s *Store) Enqueue(ctx context.Context, integrationID, trigger, runBy string, dryRun bool, scheduledFor *time.Time) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO model.integration_run
		    (integration_id, status, trigger_type, dry_run, run_by, scheduled_for)
		VALUES ($1::uuid, 'queued', $2, $3, NULLIF($4,'')::uuid, $5)
		ON CONFLICT (integration_id, scheduled_for) WHERE scheduled_for IS NOT NULL DO NOTHING
		RETURNING id::text
	`, integrationID, trigger, dryRun, runBy, scheduledFor).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil // tick already enqueued by a sibling scheduler
	}
	return id, err
}

// Claim leases the oldest queued run for workerID. Returns nil when the
// queue is empty. FOR UPDATE SKIP LOCKED keeps concurrent workers from
// fighting over the same row; the lease lets a crashed worker's run be
// reclaimed after leaseFor.
func (s *Store) Claim(ctx context.Context, workerID string, leaseFor time.Duration) (*Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM model.integration_run
		WHERE (status='queued' AND (lease_until IS NULL OR lease_until < now()))
		   OR (status='running' AND lease_until < now())
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE model.integration_run
		SET status='running', claimed_by=$2, lease_until=now()+$3::interval,
		    attempt_count=attempt_count+1, started_at=COALESCE(started_at, now())
		WHERE id=$1::uuid
	`, id, workerID, fmt.Sprintf("%d seconds", int(leaseFor.Seconds()))); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetRun(ctx, id)
}

func (s *Store) Heartbeat(ctx context.Context, runID, workerID string, leaseFor time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE model.integration_run SET lease_until=now()+$3::interval
		WHERE id=$1::uuid AND claimed_by=$2 AND status='running'
	`, runID, workerID, fmt.Sprintf("%d seconds", int(leaseFor.Seconds())))
	return err
}

// RunResult carries everything Finish persists.
type RunResult struct {
	Status         string // success | partial | failed | cancelled
	HTTPStatus     int
	DurationMS     int
	Pages, Requests, Retries int
	RecordsRead, RecordsWritten, RecordsSkipped int
	ErrorCode      string
	Message        string // MUST already be sanitized by the caller
	Meta           map[string]string
}

func (s *Store) Finish(ctx context.Context, runID string, r RunResult) error {
	meta, _ := json.Marshal(r.Meta)
	if r.Meta == nil {
		meta = []byte(`{}`)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE model.integration_run
		SET status=$2, http_status=$3, duration_ms=$4, pages=$5, requests=$6, retries=$7,
		    records_read=$8, records_written=$9, records_skipped=$10,
		    rows_imported=$9, error_rows=$10,
		    error_code=$11, message=$12, meta=$13::jsonb, finished_at=now(), lease_until=NULL
		WHERE id=$1::uuid
	`, runID, r.Status, r.HTTPStatus, r.DurationMS, r.Pages, r.Requests, r.Retries,
		r.RecordsRead, r.RecordsWritten, r.RecordsSkipped, r.ErrorCode, r.Message, string(meta))
	return err
}

// Cancel marks a queued run cancelled, or requests cancellation of a running
// one (the worker checks IsCancelRequested between pages).
func (s *Store) Cancel(ctx context.Context, runID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `
		UPDATE model.integration_run
		SET status = CASE WHEN status='queued' THEN 'cancelled' ELSE status END,
		    meta = meta || '{"cancel_requested":"true"}'::jsonb,
		    finished_at = CASE WHEN status='queued' THEN now() ELSE finished_at END
		WHERE id=$1::uuid AND status IN ('queued','running')
		RETURNING status
	`, runID).Scan(&status)
	return status, err
}

func (s *Store) IsCancelRequested(ctx context.Context, runID string) bool {
	var v bool
	_ = s.pool.QueryRow(ctx, `
		SELECT meta->>'cancel_requested' = 'true' FROM model.integration_run WHERE id=$1::uuid
	`, runID).Scan(&v)
	return v
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	var r Run
	var runBy *string
	var meta string
	if err := s.pool.QueryRow(ctx, `
		SELECT id::text, integration_id::text, status, trigger_type, dry_run,
		       run_by::text, scheduled_for, created_at, started_at, finished_at,
		       http_status, duration_ms, pages, requests, retries,
		       records_read, records_written, records_skipped, error_code, message, meta::text
		FROM model.integration_run WHERE id=$1::uuid
	`, id).Scan(&r.ID, &r.IntegrationID, &r.Status, &r.TriggerType, &r.DryRun,
		&runBy, &r.ScheduledFor, &r.CreatedAt, &r.StartedAt, &r.FinishedAt,
		&r.HTTPStatus, &r.DurationMS, &r.Pages, &r.Requests, &r.Retries,
		&r.RecordsRead, &r.RecordsWritten, &r.RecordsSkipped, &r.ErrorCode, &r.Message, &meta); err != nil {
		return nil, err
	}
	if runBy != nil {
		r.RunBy = *runBy
	}
	r.Meta = json.RawMessage(meta)
	return &r, nil
}

func (s *Store) ListRuns(ctx context.Context, integrationID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM model.integration_run
		WHERE integration_id=$1::uuid ORDER BY created_at DESC LIMIT $2
	`, integrationID, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []Run{}
	for _, id := range ids {
		r, err := s.GetRun(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// RecordAttempt logs one outbound request (sanitized).
func (s *Store) RecordAttempt(ctx context.Context, runID string, seq, page int, method, rawURL string, httpStatus, durationMS int, errorCode, message string) {
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO model.integration_attempt (run_id, seq, page, method, url_sanitized, http_status, duration_ms, error_code, message)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (run_id, seq) DO NOTHING
	`, runID, seq, page, method, SanitizedURL(rawURL), httpStatus, durationMS, errorCode, message)
}

// ── Schedules ────────────────────────────────────────────────────────────────

type Schedule struct {
	IntegrationID   string     `json:"integration_id"`
	Kind            string     `json:"kind"` // manual | interval | cron
	IntervalSeconds int        `json:"interval_seconds,omitempty"`
	CronExpr        string     `json:"cron_expr,omitempty"`
	Timezone        string     `json:"timezone"`
	Enabled         bool       `json:"enabled"`
	OverlapPolicy   string     `json:"overlap_policy"`
	MisfirePolicy   string     `json:"misfire_policy"`
	EnabledBy       string     `json:"enabled_by,omitempty"`
	NextFireAt      *time.Time `json:"next_fire_at,omitempty"`
	LastFireAt      *time.Time `json:"last_fire_at,omitempty"`
}

func (s *Store) UpsertSchedule(ctx context.Context, sc *Schedule) error {
	switch sc.Kind {
	case "manual", "interval", "cron":
	default:
		return fmt.Errorf("schedule kind must be manual, interval or cron")
	}
	if sc.Kind == "interval" && sc.IntervalSeconds < 60 {
		return fmt.Errorf("interval must be at least 60 seconds")
	}
	if sc.OverlapPolicy == "" {
		sc.OverlapPolicy = "skip"
	}
	if sc.MisfirePolicy == "" {
		sc.MisfirePolicy = "skip"
	}
	if sc.Timezone == "" {
		sc.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q", sc.Timezone)
	}
	if sc.Kind == "cron" {
		if _, err := NextCron(sc.CronExpr, sc.Timezone, time.Now()); err != nil {
			return err
		}
	}
	next, err := s.computeNext(sc, time.Now())
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO model.integration_schedule
		    (integration_id, kind, interval_seconds, cron_expr, timezone, enabled,
		     overlap_policy, misfire_policy, enabled_by, next_fire_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,'')::uuid, $10, now())
		ON CONFLICT (integration_id) DO UPDATE SET
		    kind=EXCLUDED.kind, interval_seconds=EXCLUDED.interval_seconds,
		    cron_expr=EXCLUDED.cron_expr, timezone=EXCLUDED.timezone,
		    enabled=EXCLUDED.enabled, overlap_policy=EXCLUDED.overlap_policy,
		    misfire_policy=EXCLUDED.misfire_policy, enabled_by=EXCLUDED.enabled_by,
		    next_fire_at=EXCLUDED.next_fire_at, updated_at=now()
	`, sc.IntegrationID, sc.Kind, nilIfZero(sc.IntervalSeconds), sc.CronExpr, sc.Timezone,
		sc.Enabled, sc.OverlapPolicy, sc.MisfirePolicy, sc.EnabledBy, next)
	return err
}

func nilIfZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func (s *Store) computeNext(sc *Schedule, now time.Time) (*time.Time, error) {
	if !sc.Enabled || sc.Kind == "manual" {
		return nil, nil
	}
	if sc.Kind == "interval" {
		t := now.Add(time.Duration(sc.IntervalSeconds) * time.Second)
		return &t, nil
	}
	t, err := NextCron(sc.CronExpr, sc.Timezone, now)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) GetSchedule(ctx context.Context, integrationID string) (*Schedule, error) {
	var sc Schedule
	var interval *int
	var enabledBy *string
	err := s.pool.QueryRow(ctx, `
		SELECT integration_id::text, kind, interval_seconds, cron_expr, timezone, enabled,
		       overlap_policy, misfire_policy, enabled_by::text, next_fire_at, last_fire_at
		FROM model.integration_schedule WHERE integration_id=$1::uuid
	`, integrationID).Scan(&sc.IntegrationID, &sc.Kind, &interval, &sc.CronExpr, &sc.Timezone,
		&sc.Enabled, &sc.OverlapPolicy, &sc.MisfirePolicy, &enabledBy, &sc.NextFireAt, &sc.LastFireAt)
	if err == pgx.ErrNoRows {
		return &Schedule{IntegrationID: integrationID, Kind: "manual", Timezone: "UTC", OverlapPolicy: "skip", MisfirePolicy: "skip"}, nil
	}
	if err != nil {
		return nil, err
	}
	if interval != nil {
		sc.IntervalSeconds = *interval
	}
	if enabledBy != nil {
		sc.EnabledBy = *enabledBy
	}
	return &sc, nil
}

// DueSchedules returns enabled schedules whose next_fire_at has passed, and
// advances next_fire_at atomically so sibling schedulers don't double-fire
// (the unique (integration, scheduled_for) run index is the second guard).
func (s *Store) DueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT integration_id::text, kind, COALESCE(interval_seconds,0), cron_expr, timezone,
		       enabled, overlap_policy, misfire_policy, COALESCE(enabled_by::text,''), next_fire_at
		FROM model.integration_schedule
		WHERE enabled AND next_fire_at IS NOT NULL AND next_fire_at <= $1
		ORDER BY next_fire_at LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		if err := rows.Scan(&sc.IntegrationID, &sc.Kind, &sc.IntervalSeconds, &sc.CronExpr, &sc.Timezone,
			&sc.Enabled, &sc.OverlapPolicy, &sc.MisfirePolicy, &sc.EnabledBy, &sc.NextFireAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// AdvanceSchedule stamps last_fire_at and computes the following fire time.
func (s *Store) AdvanceSchedule(ctx context.Context, sc *Schedule, firedAt time.Time) error {
	next, err := s.computeNext(sc, firedAt)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE model.integration_schedule
		SET last_fire_at=$2, next_fire_at=$3, updated_at=now()
		WHERE integration_id=$1::uuid
	`, sc.IntegrationID, firedAt, next)
	return err
}

// HasActiveRun reports a queued/running run for overlap_policy=skip.
func (s *Store) HasActiveRun(ctx context.Context, integrationID string) bool {
	var n int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model.integration_run
		WHERE integration_id=$1::uuid AND status IN ('queued','running')
	`, integrationID).Scan(&n)
	return n > 0
}
