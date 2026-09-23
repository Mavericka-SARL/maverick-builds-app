package integration

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validate is the single source of truth for configuration health, run at
// save AND re-run by the worker before execution (a row edited by SQL or a
// stale revision copy must fail at claim time, not at request time).
// allowInsecure loosens URL rules exactly as in safehttp (dev only).
func (c *Config) Validate(allowInsecure bool) error {
	if c.Kind != ConfigKind {
		return fmt.Errorf("config kind %q is not %q", c.Kind, ConfigKind)
	}
	if c.Direction != DirectionPull && c.Direction != DirectionPush {
		return fmt.Errorf("direction must be pull or push")
	}
	switch c.TargetType {
	case TargetGrid, TargetForm, TargetDimension:
	default:
		return fmt.Errorf("target_type must be grid, form or dimension")
	}
	if c.TargetID == "" {
		return fmt.Errorf("target_id is required")
	}
	if c.Direction == DirectionPull {
		switch c.ImportMode {
		case ModeIncremental, ModeReplace, ModeFullReload:
		default:
			return fmt.Errorf("import_mode must be incremental, replace or full_reload for pull")
		}
		switch c.Response.Format {
		case FormatJSON, FormatCSV:
		default:
			return fmt.Errorf("response format must be json or csv")
		}
		if c.Response.RecordsPath != "" {
			if _, err := ParsePath(c.Response.RecordsPath); err != nil {
				return fmt.Errorf("records_path: %w", err)
			}
		}
	}

	// ── Request ──
	r := &c.Request
	switch r.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return fmt.Errorf("method %q is not allowed", r.Method)
	}
	if len(r.URL) > 4096 {
		return fmt.Errorf("URL exceeds 4096 characters")
	}
	if _, err := ValidateURL(r.URL, allowInsecure); err != nil {
		return err
	}
	rowAllowed := c.Direction == DirectionPush
	rowFields := map[string]bool{}
	for _, f := range c.Mapping.Fields {
		if c.Direction == DirectionPush {
			rowFields[f.Source] = true
		}
	}
	if err := ValidateTemplate(r.URL, false, nil); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if len(r.Query) > 64 || len(r.Headers) > 64 || len(r.BodyForm) > 256 {
		return fmt.Errorf("too many query/header/body entries")
	}
	for _, kv := range r.Query {
		if len(kv.Key) > 256 || len(kv.Value) > 4096 {
			return fmt.Errorf("query parameter %q too large", kv.Key)
		}
		if err := ValidateTemplate(kv.Value, rowAllowed, rowFields); err != nil {
			return fmt.Errorf("query %q: %w", kv.Key, err)
		}
	}
	for _, kv := range r.Headers {
		if err := CheckHeaderSafe(kv.Key, kv.Value); err != nil {
			return err
		}
		if len(kv.Value) > 4096 {
			return fmt.Errorf("header %q too large", kv.Key)
		}
		if err := ValidateTemplate(kv.Value, rowAllowed, rowFields); err != nil {
			return fmt.Errorf("header %q: %w", kv.Key, err)
		}
	}
	switch r.BodyMode {
	case BodyNone:
	case BodyJSON:
		if len(r.BodyJSON) > 1<<20 {
			return fmt.Errorf("JSON body exceeds 1MB")
		}
		if err := ValidateTemplate(r.BodyJSON, rowAllowed, rowFields); err != nil {
			return fmt.Errorf("body: %w", err)
		}
		// The template-substituted document must be valid JSON; validate the
		// SHAPE now by substituting benign placeholders.
		probe := templateVarRe.ReplaceAllString(r.BodyJSON, `"x"`)
		if probe != "" && !json.Valid([]byte(probe)) {
			// Template vars may be used as bare values ({{row.n}} without
			// quotes); retry with a numeric placeholder before failing.
			probe2 := templateVarRe.ReplaceAllString(r.BodyJSON, `0`)
			if !json.Valid([]byte(probe2)) {
				return fmt.Errorf("JSON body is not valid JSON")
			}
		}
	case BodyForm:
		for _, kv := range r.BodyForm {
			if err := ValidateTemplate(kv.Value, rowAllowed, rowFields); err != nil {
				return fmt.Errorf("form field %q: %w", kv.Key, err)
			}
		}
	case BodyRaw:
		if len(r.BodyRaw) > 1<<20 {
			return fmt.Errorf("raw body exceeds 1MB")
		}
		if err := ValidateTemplate(r.BodyRaw, rowAllowed, rowFields); err != nil {
			return fmt.Errorf("body: %w", err)
		}
	default:
		return fmt.Errorf("body_mode %q is not allowed", r.BodyMode)
	}
	if (r.Method == "GET" || r.Method == "DELETE") && r.BodyMode != BodyNone {
		return fmt.Errorf("%s requests must not carry a body", r.Method)
	}
	if r.TimeoutSeconds < 0 || r.TimeoutSeconds > 120 {
		return fmt.Errorf("timeout_seconds must be 0–120")
	}
	if r.MaxRetries < 0 || r.MaxRetries > 5 {
		return fmt.Errorf("max_retries must be 0–5")
	}
	if r.MaxRetries > 0 && r.Method != "GET" && r.IdempotencyKeyHeader == "" {
		return fmt.Errorf("retries on %s require an idempotency key header", r.Method)
	}
	if r.IdempotencyKeyHeader != "" {
		if err := CheckHeaderSafe(r.IdempotencyKeyHeader, ""); err != nil {
			return fmt.Errorf("idempotency key header: %w", err)
		}
	}

	// ── Auth placement ──
	switch c.Auth.Type {
	case "", "none":
	case "api_key":
		if (c.Auth.HeaderName == "") == (c.Auth.QueryParam == "") {
			return fmt.Errorf("api_key auth needs exactly one of header_name or query_param")
		}
		if c.Auth.HeaderName != "" {
			if err := CheckHeaderSafe(c.Auth.HeaderName, ""); err != nil {
				return fmt.Errorf("api_key header: %w", err)
			}
		}
	case "bearer", "basic", "oauth2_client_credentials", AuthTypeOAuthCode:
	default:
		return fmt.Errorf("auth type %q is not supported", c.Auth.Type)
	}

	// ── Pagination ──
	switch c.Pagination.Mode {
	case "", PageNone:
	case PageNumber, PageOffset, PageCursor, PageLink:
		if c.Direction != DirectionPull {
			return fmt.Errorf("pagination applies to pull only")
		}
		if c.Pagination.Mode == PageCursor {
			if c.Pagination.CursorPath == "" {
				return fmt.Errorf("cursor pagination requires cursor_path")
			}
			if _, err := ParsePath(c.Pagination.CursorPath); err != nil {
				return fmt.Errorf("cursor_path: %w", err)
			}
		}
		if c.Pagination.MaxPages < 0 || c.Pagination.MaxPages > 1000 {
			return fmt.Errorf("max_pages must be 0–1000")
		}
	default:
		return fmt.Errorf("pagination mode %q is not allowed", c.Pagination.Mode)
	}

	// ── Mapping ──
	if len(c.Mapping.Fields) == 0 && c.Direction == DirectionPush {
		return fmt.Errorf("push requires at least one mapped field")
	}
	if len(c.Mapping.Fields) > 256 {
		return fmt.Errorf("too many mapped fields")
	}
	seen := map[string]bool{}
	for i, f := range c.Mapping.Fields {
		if strings.TrimSpace(f.Source) == "" || strings.TrimSpace(f.Target) == "" {
			return fmt.Errorf("mapping row %d: source and target are required", i+1)
		}
		if c.Direction == DirectionPull {
			if _, err := ParsePath(f.Source); err != nil {
				return fmt.Errorf("mapping row %d: %w", i+1, err)
			}
		}
		key := f.Source + "→" + f.Target
		if seen[key] {
			return fmt.Errorf("duplicate mapping %s", key)
		}
		seen[key] = true
		for _, t := range f.Transforms {
			switch t.Kind {
			case TransformNone, TransformToString, TransformToNumber, TransformToBoolean,
				TransformToDate, TransformTrim, TransformDefault, TransformDateFormat:
			case TransformLookup:
				if len(t.Lookup) == 0 {
					return fmt.Errorf("mapping row %d: lookup transform needs a table", i+1)
				}
				if len(t.Lookup) > 500 {
					return fmt.Errorf("mapping row %d: lookup table exceeds 500 entries", i+1)
				}
			default:
				return fmt.Errorf("mapping row %d: transform %q is not allowed", i+1, t.Kind)
			}
		}
	}
	if c.Mapping.Shape == ShapeLong {
		if c.Mapping.MetricNameSource == "" || c.Mapping.ValueSource == "" {
			return fmt.Errorf("long shape requires metric_name_source and value_source")
		}
	}
	if c.Mapping.BatchSize < 0 || c.Mapping.BatchSize > 1000 {
		return fmt.Errorf("batch_size must be 0–1000")
	}

	// ── Limits ──
	if c.Limits.MaxRecords < 0 || c.Limits.MaxRecords > 1_000_000 {
		return fmt.Errorf("max_records must be 0–1000000")
	}
	if c.Limits.MaxRequests < 0 || c.Limits.MaxRequests > 10_000 {
		return fmt.Errorf("max_requests must be 0–10000")
	}
	return nil
}

// ParseConfig decodes and validates a stored config document.
func ParseConfig(raw []byte, allowInsecure bool) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config does not parse: %w", err)
	}
	if err := c.Validate(allowInsecure); err != nil {
		return nil, err
	}
	return &c, nil
}
