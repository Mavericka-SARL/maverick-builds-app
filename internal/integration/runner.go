package integration

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Runner executes one claimed run end to end: build requests through the
// safe client, paginate, extract, map, and hand rows to the Committer. Every
// failure is classified into the machine error taxonomy so run history can
// distinguish DNS, blocked-host, TLS, timeout, auth, rate-limit, HTTP,
// invalid-data, and oversized-response failures.
type Runner struct {
	Store         *Store
	Log           zerolog.Logger
	AllowInsecure bool
	// Committer applies mapped rows to the target. Injected so the worker
	// wires importpkg/crudapp/writeguard while tests use a recorder.
	Committer Committer
	// Now is injectable for tests.
	Now func() time.Time
	// OnFinished, when set, runs after a real (non-dry) run's result is
	// persisted — the worker uses it to fire integration_completed /
	// integration_failed automation rules. Nil in tests and dry runs.
	OnFinished func(ctx context.Context, run *Run, res RunResult)
}

// Committer is the write side of a pull (and the data source of a push).
type Committer interface {
	// CommitPull applies header+rows to the integration's target as the run's
	// acting principal (runBy — the manual runner, or the developer who
	// enabled the schedule). Returns written/skipped counts. Dry runs must
	// not write.
	CommitPull(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool, runBy string) (written, skipped int, err error)
	// LoadPushRows returns the source rows for a push, each keyed by the
	// mapped Source field names.
	LoadPushRows(ctx context.Context, def *Definition) ([]map[string]string, error)
}

// Error taxonomy.
const (
	ErrCodeDNS         = "dns"
	ErrCodeBlockedHost = "blocked_host"
	ErrCodeTLS         = "tls"
	ErrCodeTimeout     = "timeout"
	ErrCodeAuth        = "auth"
	ErrCodeRateLimit   = "rate_limit"
	ErrCodeHTTP        = "http_error"
	ErrCodeInvalidData = "invalid_data"
	ErrCodeTooLarge    = "too_large"
	ErrCodeCancelled   = "cancelled"
	ErrCodeInternal    = "internal"
)

func classifyErr(err error) string {
	var blocked *BlockedDestinationError
	if errors.As(err, &blocked) {
		return ErrCodeBlockedHost
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrCodeDNS
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return ErrCodeTLS
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return ErrCodeTimeout
	}
	if strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:") {
		return ErrCodeTLS
	}
	return ErrCodeInternal
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func classifyStatus(status int) string {
	switch {
	case status == 401 || status == 403:
		return ErrCodeAuth
	case status == 429:
		return ErrCodeRateLimit
	case status >= 400:
		return ErrCodeHTTP
	}
	return ""
}

// sanitizeMsg strips anything that might carry credentials (query strings,
// Authorization echoes) out of error text before it is persisted.
func sanitizeMsg(msg string) string {
	if i := strings.Index(msg, "?"); i > 0 && strings.Contains(msg[:i], "://") {
		if j := strings.IndexAny(msg[i:], " \"'"); j > 0 {
			msg = msg[:i] + msg[i+j:]
		} else {
			msg = msg[:i]
		}
	}
	for _, needle := range []string{"Authorization:", "Bearer ", "Basic "} {
		if i := strings.Index(msg, needle); i >= 0 {
			msg = msg[:i] + needle + "[redacted]"
			break
		}
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

// safeRespHeaders is the allowlist echoed into test previews.
var safeRespHeaders = []string{"Content-Type", "Content-Length", "Date", "Server", "X-Request-Id", "X-Ratelimit-Remaining", "Retry-After", "Link"}

// Execute runs one claimed run to completion and persists the result.
func (rn *Runner) Execute(ctx context.Context, run *Run) {
	res := rn.execute(ctx, run)
	res.Message = sanitizeMsg(res.Message)
	if err := rn.Store.Finish(ctx, run.ID, res); err != nil {
		rn.Log.Error().Err(err).Str("run", run.ID).Msg("finish run")
	}
	if rn.OnFinished != nil && !run.DryRun && run.TriggerType != "test" {
		rn.OnFinished(ctx, run, res)
	}
}

func (rn *Runner) now() time.Time {
	if rn.Now != nil {
		return rn.Now()
	}
	return time.Now()
}

// The result MUST be named: the deferred duration stamp below runs after
// every `return res` copies into the result slot, so with an unnamed result
// it would mutate a dead local and every run would persist duration_ms=0.
func (rn *Runner) execute(ctx context.Context, run *Run) (res RunResult) {
	started := rn.now()
	res = RunResult{Status: "failed", Meta: map[string]string{}}
	defer func() { res.DurationMS = int(rn.now().Sub(started).Milliseconds()) }()

	// Load + re-validate: an edited row, foreign target, or stale revision
	// copy fails at claim time, not mid-request (validated at save AND here).
	var modelID, appID string
	if err := rn.Store.pool.QueryRow(ctx, `
		SELECT i.model_id::text, m.application_id::text
		FROM model.integration_def i JOIN core.model m ON m.id=i.model_id WHERE i.id=$1::uuid
	`, run.IntegrationID).Scan(&modelID, &appID); err != nil {
		res.ErrorCode, res.Message = ErrCodeInternal, "integration no longer exists"
		return res
	}
	def, err := rn.Store.GetDefinition(ctx, modelID, run.IntegrationID)
	if err != nil || def.Config == nil {
		res.ErrorCode, res.Message = ErrCodeInternal, "configuration missing"
		return res
	}
	cfg := def.Config
	if verr := cfg.Validate(rn.AllowInsecure); verr != nil {
		res.ErrorCode, res.Message = ErrCodeInvalidData, "configuration invalid: "+verr.Error()
		return res
	}
	res.Meta["host"] = SanitizedHost(cfg.Request.URL)

	// Credential.
	authType, authMeta, secret, cerr := rn.openAuth(ctx, appID, def)
	if cerr != nil {
		res.ErrorCode, res.Message = ErrCodeAuth, cerr.Error()
		return res
	}

	limits := &Limits{}
	if cfg.Request.TimeoutSeconds > 0 {
		limits.RequestTimeout = time.Duration(cfg.Request.TimeoutSeconds) * time.Second
	}
	client := NewSafeClient(limits, rn.AllowInsecure)

	tctx := TemplateContext{
		Context: map[string]string{"application_id": appID, "model_id": modelID, "revision_id": def.RevisionID},
		Run:     map[string]string{"id": run.ID, "started_at": started.UTC().Format(time.RFC3339)},
	}

	if cfg.Direction == DirectionPush {
		return rn.executePush(ctx, run, def, appID, client, tctx, authType, authMeta, secret, res)
	}
	return rn.executePull(ctx, run, def, appID, client, tctx, authType, authMeta, secret, res)
}

// openAuth resolves the connection's credential (client-credentials tokens
// are fetched per run through the SAME safe client rules).
func (rn *Runner) openAuth(ctx context.Context, appID string, def *Definition) (string, map[string]string, map[string]string, error) {
	if def.Config.Auth.Type == "" || def.Config.Auth.Type == "none" {
		return "none", nil, nil, nil
	}
	if def.ConnectionID == "" {
		return "", nil, nil, fmt.Errorf("auth type %s requires a connection", def.Config.Auth.Type)
	}
	authType, metaRaw, secretRaw, err := rn.Store.OpenCredential(ctx, appID, def.ConnectionID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("credential cannot be opened")
	}
	if authType != def.Config.Auth.Type {
		return "", nil, nil, fmt.Errorf("connection auth type %s does not match integration auth %s", authType, def.Config.Auth.Type)
	}
	meta := map[string]string{}
	_ = json.Unmarshal(metaRaw, &meta)
	secret := map[string]string{}
	if len(secretRaw) > 0 {
		if uerr := json.Unmarshal(secretRaw, &secret); uerr != nil {
			return "", nil, nil, fmt.Errorf("credential payload corrupt")
		}
	}
	return authType, meta, secret, nil
}

// applyAuth mutates one outbound request. OAuth client-credentials exchanges
// happen in fetchOAuthToken before the page loop.
func applyAuth(req *http.Request, placement AuthPlacement, authType string, secret map[string]string, bearerToken string) {
	switch authType {
	case "api_key":
		if placement.HeaderName != "" {
			req.Header.Set(placement.HeaderName, secret["value"])
		} else if placement.QueryParam != "" {
			q := req.URL.Query()
			q.Set(placement.QueryParam, secret["value"])
			req.URL.RawQuery = q.Encode()
		}
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+secret["token"])
	case "basic":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(secret["username"]+":"+secret["password"])))
	case "oauth2_client_credentials", AuthTypeOAuthCode:
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
}

// bearerFor obtains the per-run bearer for the two OAuth auth types:
// a client-credentials exchange, or the connection's authorised access
// token (refreshed and re-stored when expired).
func (rn *Runner) bearerFor(ctx context.Context, client *http.Client, appID, connectionID, authType string, meta, secret map[string]string) (string, error) {
	switch authType {
	case "oauth2_client_credentials":
		return rn.fetchOAuthToken(ctx, client, meta, secret)
	case AuthTypeOAuthCode:
		metaJSON, _ := json.Marshal(meta)
		return rn.Store.OAuthBearer(ctx, client, appID, connectionID, metaJSON, secret, rn.AllowInsecure)
	}
	return "", nil
}

// fetchOAuthToken performs the client-credentials exchange against the
// connection's token_url (validated like any destination).
func (rn *Runner) fetchOAuthToken(ctx context.Context, client *http.Client, meta, secret map[string]string) (string, error) {
	tokenURL := meta["token_url"]
	if tokenURL == "" {
		tokenURL = secret["token_url"]
	}
	if _, err := ValidateURL(tokenURL, rn.AllowInsecure); err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if sc := meta["scope"]; sc != "" {
		form.Set("scope", sc)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(secret["client_id"]), url.QueryEscape(secret["client_secret"]))
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token")
	}
	return tok.AccessToken, nil
}

// buildRequest renders one page's request from config + templates.
func buildRequest(ctx context.Context, cfg *Config, tctx TemplateContext, overrideURL string) (*http.Request, error) {
	rawURL := cfg.Request.URL
	if overrideURL != "" {
		rawURL = overrideURL
	}
	rendered, err := Render(rawURL, tctx)
	if err != nil {
		return nil, err
	}
	u, err := ValidateURL(rendered, false)
	if err != nil {
		// Re-check in caller's insecure mode happens at ValidateURL below;
		// this pre-parse keeps template errors distinct.
		u2, err2 := url.Parse(rendered)
		if err2 != nil {
			return nil, err
		}
		u = u2
	}
	q := u.Query()
	for _, kv := range cfg.Request.Query {
		if !kv.Enabled {
			continue
		}
		v, rerr := Render(kv.Value, tctx)
		if rerr != nil {
			return nil, rerr
		}
		q.Set(kv.Key, v)
	}
	u.RawQuery = q.Encode()

	var body io.Reader
	contentType := ""
	switch cfg.Request.BodyMode {
	case BodyNone:
		// no body
	case BodyJSON:
		rendered, rerr := Render(cfg.Request.BodyJSON, tctx)
		if rerr != nil {
			return nil, rerr
		}
		body = strings.NewReader(rendered)
		contentType = "application/json"
	case BodyForm:
		form := url.Values{}
		for _, kv := range cfg.Request.BodyForm {
			if !kv.Enabled {
				continue
			}
			v, rerr := Render(kv.Value, tctx)
			if rerr != nil {
				return nil, rerr
			}
			form.Set(kv.Key, v)
		}
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case BodyRaw:
		rendered, rerr := Render(cfg.Request.BodyRaw, tctx)
		if rerr != nil {
			return nil, rerr
		}
		body = strings.NewReader(rendered)
		contentType = cfg.Request.ContentType
		if contentType == "" {
			contentType = "text/plain"
		}
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Request.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, kv := range cfg.Request.Headers {
		if !kv.Enabled {
			continue
		}
		if herr := CheckHeaderSafe(kv.Key, kv.Value); herr != nil {
			return nil, herr
		}
		v, rerr := Render(kv.Value, tctx)
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set(kv.Key, v)
	}
	if cfg.Request.IdempotencyKeyHeader != "" && cfg.Request.Method != "GET" {
		req.Header.Set(cfg.Request.IdempotencyKeyHeader, uuid.NewString())
	}
	req.Header.Set("Accept-Encoding", "gzip")
	return req, nil
}

// readBody enforces the decompressed-size ceiling.
func readBody(resp *http.Response, maxSize int64) ([]byte, bool, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck
		r = gz
	}
	limited := io.LimitReader(r, maxSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maxSize {
		return data[:maxSize], true, nil
	}
	return data, false, nil
}

// doRequestWithRetry performs one page request with the retry policy: safe
// methods retry on 5xx/429/transport errors; mutation methods only with an
// idempotency key configured (enforced at validation).
func (rn *Runner) doRequestWithRetry(ctx context.Context, client *http.Client, cfg *Config, build func() (*http.Request, error), retries *int) (*http.Response, error) {
	max := cfg.Request.MaxRetries
	backoff := time.Duration(cfg.Request.RetryBackoffMS) * time.Millisecond
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return resp, nil
		}
		if err == nil {
			if attempt >= max {
				return resp, nil // caller classifies the status
			}
			resp.Body.Close() //nolint:errcheck
		} else {
			lastErr = err
			var blocked *BlockedDestinationError
			if errors.As(err, &blocked) {
				return nil, err // never retry a policy refusal
			}
			if attempt >= max {
				return nil, lastErr
			}
		}
		*retries++
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff * time.Duration(attempt+1)):
		}
	}
}

// executePull: page loop → extract → map → commit (or dry-run report).
func (rn *Runner) executePull(ctx context.Context, run *Run, def *Definition, appID string, client *http.Client, tctx TemplateContext, authType string, authMeta, secret map[string]string, res RunResult) RunResult {
	cfg := def.Config
	bearer, terr := rn.bearerFor(ctx, client, appID, def.ConnectionID, authType, authMeta, secret)
	if terr != nil {
		res.ErrorCode, res.Message = classifyAuthErr(terr), "OAuth token: "+terr.Error()
		return res
	}

	maxRecords := cfg.Limits.MaxRecords
	if maxRecords <= 0 {
		maxRecords = 100_000
	}
	maxRequests := cfg.Limits.MaxRequests
	if maxRequests <= 0 {
		maxRequests = 1_000
	}
	failThreshold := cfg.Limits.FailureThreshold
	if failThreshold <= 0 {
		failThreshold = 100
	}
	rate := newRateGate(cfg.Request.RateLimitRPS)

	ps := newPageState(cfg.Pagination)
	var header []string
	var allRows [][]string
	var recordErrs []RecordError
	seq := 0

	for page := 1; page <= ps.maxPages(); page++ {
		if rn.Store.IsCancelRequested(ctx, run.ID) {
			res.Status, res.ErrorCode = "cancelled", ErrCodeCancelled
			return res
		}
		if res.Requests >= maxRequests {
			res.Meta["stopped"] = "max_requests"
			break
		}
		tctx.Page = ps.templateVals(page)
		rate.wait(ctx)
		seq++
		reqStart := rn.now()
		var reqURL string
		resp, err := rn.doRequestWithRetry(ctx, client, cfg, func() (*http.Request, error) {
			req, berr := buildRequest(ctx, cfg, tctx, ps.overrideURL())
			if berr == nil {
				reqURL = req.URL.String()
				applyAuth(req, cfg.Auth, authType, secret, bearer)
			}
			return req, berr
		}, &res.Retries)
		res.Requests++
		durMS := int(rn.now().Sub(reqStart).Milliseconds())
		if err != nil {
			code := classifyErr(err)
			rn.Store.RecordAttempt(ctx, run.ID, seq, page, cfg.Request.Method, reqURL, 0, durMS, code, sanitizeMsg(err.Error()))
			res.ErrorCode, res.Message = code, err.Error()
			return res
		}
		res.HTTPStatus = resp.StatusCode
		body, truncated, rerr := readBody(resp, (&Limits{}).withDefaults().MaxResponseSize)
		resp.Body.Close() //nolint:errcheck
		rn.Store.RecordAttempt(ctx, run.ID, seq, page, cfg.Request.Method, reqURL, resp.StatusCode, durMS, classifyStatus(resp.StatusCode), "")
		if rerr != nil {
			res.ErrorCode, res.Message = ErrCodeInternal, rerr.Error()
			return res
		}
		if truncated {
			res.ErrorCode, res.Message = ErrCodeTooLarge, "response exceeded the size ceiling"
			return res
		}
		if code := classifyStatus(resp.StatusCode); code != "" {
			res.ErrorCode, res.Message = code, fmt.Sprintf("remote returned %d", resp.StatusCode)
			return res
		}

		// Test runs capture the sanitized preview and stop after page 1.
		if run.TriggerType == "test" {
			rn.attachPreview(&res, resp, body, cfg)
		}

		var records []map[string]any
		var doc any
		if cfg.Response.Format == FormatCSV {
			rdr := csv.NewReader(strings.NewReader(string(body)))
			rows, cerr := rdr.ReadAll()
			if cerr != nil || len(rows) == 0 {
				res.ErrorCode, res.Message = ErrCodeInvalidData, "response is not parseable CSV"
				return res
			}
			records = csvToRecords(rows[0], rows[1:])
		} else {
			if jerr := json.Unmarshal(body, &doc); jerr != nil {
				res.ErrorCode, res.Message = ErrCodeInvalidData, "response is not valid JSON"
				return res
			}
			var exErr error
			records, exErr = extractRecords(doc, cfg.Response.RecordsPath)
			if exErr != nil {
				res.ErrorCode, res.Message = ErrCodeInvalidData, exErr.Error()
				return res
			}
		}
		res.Pages++
		res.RecordsRead += len(records)

		h, rows, errs := MapPullRecords(cfg, records, res.RecordsRead-len(records))
		if header == nil {
			header = h
		}
		allRows = append(allRows, rows...)
		recordErrs = append(recordErrs, errs...)
		if len(recordErrs) > failThreshold {
			res.ErrorCode, res.Message = ErrCodeInvalidData, fmt.Sprintf("%d record(s) failed mapping (threshold %d)", len(recordErrs), failThreshold)
			res.RecordsSkipped = len(recordErrs)
			return res
		}
		if res.RecordsRead >= maxRecords {
			res.Meta["stopped"] = "max_records"
			break
		}
		if run.TriggerType == "test" {
			break
		}
		more, aerr := ps.advance(doc, resp.Header, len(records), rn.AllowInsecure)
		if aerr != nil {
			res.ErrorCode, res.Message = classifyErr(aerr), aerr.Error()
			return res
		}
		if !more {
			break
		}
	}

	if cfg.Mapping.Shape == ShapeLong && header != nil {
		header, allRows = LongToWide(header, allRows)
	}

	// Test runs verify + preview only — never write.
	if run.TriggerType == "test" {
		res.Status = "success"
		res.RecordsSkipped = len(recordErrs)
		if err := rn.Store.MarkTested(ctx, def.ID, ConfigHash(cfg)); err != nil {
			rn.Log.Warn().Err(err).Msg("mark tested")
		}
		return res
	}

	written, skipped, cerr := rn.Committer.CommitPull(ctx, def, header, allRows, run.DryRun, run.RunBy)
	if cerr != nil {
		res.ErrorCode, res.Message = ErrCodeInvalidData, cerr.Error()
		return res
	}
	res.RecordsWritten = written
	res.RecordsSkipped = skipped + len(recordErrs)
	res.Status = "success"
	if len(recordErrs) > 0 || skipped > 0 {
		res.Status = "partial"
		res.Message = fmt.Sprintf("%d record(s) skipped", res.RecordsSkipped)
	}
	if run.DryRun {
		res.Meta["dry_run"] = "true"
	}
	return res
}

func classifyAuthErr(err error) string {
	if code := classifyErr(err); code != ErrCodeInternal {
		return code
	}
	return ErrCodeAuth
}

// attachPreview stores the sanitized test preview in run meta: status,
// safe headers, truncated body (≤32KB), content type.
func (rn *Runner) attachPreview(res *RunResult, resp *http.Response, body []byte, cfg *Config) {
	preview := body
	previewTruncated := false
	if len(preview) > 32<<10 {
		preview = preview[:32<<10]
		previewTruncated = true
	}
	headers := map[string]string{}
	for _, h := range safeRespHeaders {
		if v := resp.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	hb, _ := json.Marshal(headers)
	res.Meta["preview_status"] = fmt.Sprintf("%d", resp.StatusCode)
	res.Meta["preview_content_type"] = resp.Header.Get("Content-Type")
	res.Meta["preview_truncated"] = fmt.Sprintf("%v", previewTruncated)
	res.Meta["preview_headers"] = string(hb)
	res.Meta["preview_body"] = string(preview)
	_ = cfg
}

// executePush: load rows → per-record or batched requests.
func (rn *Runner) executePush(ctx context.Context, run *Run, def *Definition, appID string, client *http.Client, tctx TemplateContext, authType string, authMeta, secret map[string]string, res RunResult) RunResult {
	cfg := def.Config
	bearer, terr := rn.bearerFor(ctx, client, appID, def.ConnectionID, authType, authMeta, secret)
	if terr != nil {
		res.ErrorCode, res.Message = classifyAuthErr(terr), "OAuth token: "+terr.Error()
		return res
	}

	rows, lerr := rn.Committer.LoadPushRows(ctx, def)
	if lerr != nil {
		res.ErrorCode, res.Message = ErrCodeInternal, lerr.Error()
		return res
	}
	res.RecordsRead = len(rows)
	if run.DryRun || run.TriggerType == "test" {
		// A push test/dry-run reports what WOULD be sent without sending —
		// unless the developer acknowledged side effects on a test, which
		// still sends only the FIRST record/batch.
		res.Meta["dry_run"] = "true"
	}

	mapped := make([]map[string]string, 0, len(rows))
	var recordErrs []RecordError
	for i, src := range rows {
		m, merr := MapPushRecord(cfg, src)
		if merr != nil {
			recordErrs = append(recordErrs, RecordError{Record: i + 1, Message: merr.Error()})
			continue
		}
		mapped = append(mapped, m)
	}

	rate := newRateGate(cfg.Request.RateLimitRPS)
	seq := 0
	send := func(rowCtx map[string]string, bodyOverride string) error {
		tc := tctx
		tc.Row = rowCtx
		rate.wait(ctx)
		seq++
		start := rn.now()
		var reqURL string
		resp, err := rn.doRequestWithRetry(ctx, client, cfg, func() (*http.Request, error) {
			c := *cfg
			if bodyOverride != "" {
				c.Request.BodyMode = BodyJSON
				c.Request.BodyJSON = bodyOverride
			}
			req, berr := buildRequest(ctx, &c, tc, "")
			if berr == nil {
				reqURL = req.URL.String()
				applyAuth(req, cfg.Auth, authType, secret, bearer)
			}
			return req, berr
		}, &res.Retries)
		res.Requests++
		durMS := int(rn.now().Sub(start).Milliseconds())
		if err != nil {
			code := classifyErr(err)
			rn.Store.RecordAttempt(ctx, run.ID, seq, 0, cfg.Request.Method, reqURL, 0, durMS, code, sanitizeMsg(err.Error()))
			return fmt.Errorf("%s: %w", code, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		_, _, _ = readBody(resp, 1<<20)
		res.HTTPStatus = resp.StatusCode
		rn.Store.RecordAttempt(ctx, run.ID, seq, 0, cfg.Request.Method, reqURL, resp.StatusCode, durMS, classifyStatus(resp.StatusCode), "")
		if code := classifyStatus(resp.StatusCode); code != "" {
			return fmt.Errorf("%s: remote returned %d", code, resp.StatusCode)
		}
		return nil
	}

	dryish := run.DryRun || run.TriggerType == "test"
	if cfg.Mapping.Batch {
		size := cfg.Mapping.BatchSize
		if size <= 0 {
			size = 100
		}
		for off := 0; off < len(mapped); off += size {
			if rn.Store.IsCancelRequested(ctx, run.ID) {
				res.Status, res.ErrorCode = "cancelled", ErrCodeCancelled
				return res
			}
			end := off + size
			if end > len(mapped) {
				end = len(mapped)
			}
			body, berr := BuildPushBatchBody(cfg, mapped[off:end])
			if berr != nil {
				res.ErrorCode, res.Message = ErrCodeInvalidData, berr.Error()
				return res
			}
			if dryish && off > 0 {
				break
			}
			if run.DryRun {
				break // dry-run: send nothing at all
			}
			if serr := send(nil, body); serr != nil {
				res.ErrorCode, res.Message = strings.SplitN(serr.Error(), ":", 2)[0], serr.Error()
				res.RecordsSkipped = len(mapped) - off
				if res.RecordsWritten > 0 {
					res.Status = "partial"
				}
				return res
			}
			res.RecordsWritten += end - off
			if dryish {
				break
			}
		}
	} else {
		for i, row := range mapped {
			if rn.Store.IsCancelRequested(ctx, run.ID) {
				res.Status, res.ErrorCode = "cancelled", ErrCodeCancelled
				return res
			}
			if run.DryRun {
				break
			}
			if serr := send(row, ""); serr != nil {
				recordErrs = append(recordErrs, RecordError{Record: i + 1, Message: serr.Error()})
				if len(recordErrs) > 100 {
					break
				}
				continue
			}
			res.RecordsWritten++
			if dryish {
				break // test: first record only
			}
		}
	}

	res.RecordsSkipped = len(recordErrs)
	res.Status = "success"
	if len(recordErrs) > 0 {
		res.Status = "partial"
		res.Message = fmt.Sprintf("%d record(s) failed", len(recordErrs))
		if res.RecordsWritten == 0 && !dryish {
			res.Status = "failed"
			res.ErrorCode = ErrCodeHTTP
		}
	}
	if run.TriggerType == "test" && res.Status == "success" {
		_ = rn.Store.MarkTested(ctx, def.ID, ConfigHash(cfg))
	}
	return res
}

// rateGate is a minimal per-run RPS ceiling.
type rateGate struct{ interval time.Duration; last time.Time }

func newRateGate(rps int) *rateGate {
	if rps <= 0 {
		return &rateGate{}
	}
	return &rateGate{interval: time.Second / time.Duration(rps)}
}

func (g *rateGate) wait(ctx context.Context) {
	if g.interval == 0 {
		return
	}
	if elapsed := time.Since(g.last); elapsed < g.interval {
		select {
		case <-time.After(g.interval - elapsed):
		case <-ctx.Done():
		}
	}
	g.last = time.Now()
}
