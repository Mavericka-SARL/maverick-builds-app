package integration

// Typed configuration for a rest_api integration. This is what
// integration_def.config holds (JSON) for source="rest_api" — validated by
// Validate() at save AND again by the worker at execution. Credentials are
// NEVER part of this document; auth is a reference to an
// integration_connection row plus non-secret placement metadata.

type Direction string

const (
	DirectionPull Direction = "pull" // external API → grid/form/dimension
	DirectionPush Direction = "push" // grid/form/dimension → external API
)

type TargetType string

const (
	TargetGrid      TargetType = "grid"
	TargetForm      TargetType = "form"
	TargetDimension TargetType = "dimension"
)

// KV is one visual key/value row (query param or header).
type KV struct {
	Key     string `json:"key"`
	Value   string `json:"value"` // may contain template variables
	Enabled bool   `json:"enabled"`
}

type BodyMode string

const (
	BodyNone BodyMode = "none"
	BodyJSON BodyMode = "json"
	BodyForm BodyMode = "form" // application/x-www-form-urlencoded
	BodyRaw  BodyMode = "raw"
)

type RequestConfig struct {
	Method  string `json:"method"` // GET POST PUT PATCH DELETE
	URL     string `json:"url"`
	Query   []KV   `json:"query,omitempty"`
	Headers []KV   `json:"headers,omitempty"`

	BodyMode BodyMode `json:"body_mode"`
	// BodyJSON holds the JSON document (as raw text, template-bearing) when
	// BodyMode==json; BodyForm the urlencoded pairs; BodyRaw the raw text.
	BodyJSON    string `json:"body_json,omitempty"`
	BodyForm    []KV   `json:"body_form,omitempty"`
	BodyRaw     string `json:"body_raw,omitempty"`
	ContentType string `json:"content_type,omitempty"` // raw mode only

	TimeoutSeconds int  `json:"timeout_seconds,omitempty"` // default 30, max 120
	MaxRetries     int  `json:"max_retries,omitempty"`     // safe methods only unless idempotency key set
	RetryBackoffMS int  `json:"retry_backoff_ms,omitempty"`
	RateLimitRPS   int  `json:"rate_limit_rps,omitempty"` // requests/second ceiling within a run
	// IdempotencyKeyHeader, when set on a mutation method, is sent with a
	// per-request UUID and unlocks retries for that method.
	IdempotencyKeyHeader string `json:"idempotency_key_header,omitempty"`
}

type PaginationMode string

const (
	PageNone   PaginationMode = "none"
	PageNumber PaginationMode = "page_number"
	PageOffset PaginationMode = "offset_limit"
	PageCursor PaginationMode = "cursor"
	PageLink   PaginationMode = "link_header"
)

type PaginationConfig struct {
	Mode PaginationMode `json:"mode"`
	// Parameter names live in Query/Body via {{page.number}}/{{page.cursor}};
	// these settings drive the iteration itself.
	StartPage  int    `json:"start_page,omitempty"`  // page_number: first page (default 1)
	PageSize   int    `json:"page_size,omitempty"`   // offset_limit: limit per page
	CursorPath string `json:"cursor_path,omitempty"` // cursor: response path of next cursor, e.g. $.meta.next
	// StopWhenEmpty defaults true: iteration ends when a page yields zero
	// records (in addition to MaxPages and cursor/link exhaustion).
	MaxPages int `json:"max_pages,omitempty"` // default 100, hard cap 1000
}

type ResponseFormat string

const (
	FormatJSON ResponseFormat = "json"
	FormatCSV  ResponseFormat = "csv"
)

type ResponseConfig struct {
	Format ResponseFormat `json:"format"`
	// RecordsPath selects the record collection from a JSON response with the
	// constrained path syntax of paths.go ($.data.items). Empty = the root
	// (array), or the whole document as a single record.
	RecordsPath string `json:"records_path,omitempty"`
}

// TransformKind is the closed set of declarative transforms.
type TransformKind string

const (
	TransformNone       TransformKind = ""
	TransformToString   TransformKind = "to_string"
	TransformToNumber   TransformKind = "to_number"
	TransformToBoolean  TransformKind = "to_boolean"
	TransformToDate     TransformKind = "to_date"
	TransformTrim       TransformKind = "trim"
	TransformDefault    TransformKind = "default"
	TransformDateFormat TransformKind = "date_format"
	TransformLookup     TransformKind = "lookup"
)

type Transform struct {
	Kind TransformKind `json:"kind"`
	// Default value, date layout (Go reference layout or "iso"), or lookup table.
	Value  string            `json:"value,omitempty"`
	Lookup map[string]string `json:"lookup,omitempty"`
}

// FieldMap is one mapping row.
// Pull: Source is a response path relative to a record ($.attributes.amount);
// Target names a metric/dimension/form field.
// Push: Source names a Mavericks field; Target is the outbound JSON property.
type FieldMap struct {
	Source     string      `json:"source"`
	Target     string      `json:"target"`
	TargetKind string      `json:"target_kind,omitempty"` // pull: metric|dimension|form_field|member_code|member_label|member_parent|property:<name>
	Transforms []Transform `json:"transforms,omitempty"`
}

// GridShape distinguishes long (one metric column keyed by a metric-name
// field) from wide (one column per metric) responses.
type GridShape string

const (
	ShapeWide GridShape = "wide"
	ShapeLong GridShape = "long"
)

type MappingConfig struct {
	Fields []FieldMap `json:"fields"`
	Shape  GridShape  `json:"shape,omitempty"` // pull→grid only
	// Long shape: which response fields carry the metric name and the value.
	MetricNameSource string `json:"metric_name_source,omitempty"`
	ValueSource      string `json:"value_source,omitempty"`
	// Push batch mode wraps records into this JSON property (default "items")
	// of a single request; per-record mode sends one request per record.
	Batch         bool   `json:"batch,omitempty"`
	BatchProperty string `json:"batch_property,omitempty"`
	BatchSize     int    `json:"batch_size,omitempty"` // default 100, max 1000
}

type ImportMode string

const (
	ModeIncremental ImportMode = "incremental"
	ModeReplace     ImportMode = "replace"
	ModeFullReload  ImportMode = "full_reload"
)

type RunLimits struct {
	MaxRecords  int `json:"max_records,omitempty"`  // default 100_000
	MaxRequests int `json:"max_requests,omitempty"` // default 1_000
	// FailureThreshold aborts the run when this many record-level errors
	// accumulate (0 = default 100).
	FailureThreshold int `json:"failure_threshold,omitempty"`
}

// AuthPlacement is the NON-secret half of authentication: where the
// credential goes. The secret itself lives in integration_connection.
type AuthPlacement struct {
	// Type mirrors the connection's type for validation: none | api_key |
	// bearer | basic | oauth2_client_credentials.
	Type string `json:"type"`
	// api_key placement:
	HeaderName string `json:"header_name,omitempty"`
	QueryParam string `json:"query_param,omitempty"`
}

// Config is the full typed document.
type Config struct {
	Kind       string           `json:"kind"` // always "rest_api/v1"
	Direction  Direction        `json:"direction"`
	TargetType TargetType       `json:"target_type"`
	TargetID   string           `json:"target_id"`
	ImportMode ImportMode       `json:"import_mode,omitempty"` // pull only
	Request    RequestConfig    `json:"request"`
	Auth       AuthPlacement    `json:"auth"`
	Response   ResponseConfig   `json:"response,omitempty"` // pull only
	Pagination PaginationConfig `json:"pagination,omitempty"`
	Mapping    MappingConfig    `json:"mapping"`
	Limits     RunLimits        `json:"limits,omitempty"`
}

// ConfigKind is the discriminator value stored in Config.Kind.
const ConfigKind = "rest_api/v1"
