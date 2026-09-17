package importpkg

// Google Sheets as an import source, sharing the exact same RawRow →
// ResolveRows → CommitImport pipeline as CSV/XLSX uploads (see xlsx.go for
// why that matters). Only link-shared sheets are supported: the fetch uses
// Google's unauthenticated CSV export endpoint, so there are no credentials,
// no OAuth consent flow, and nothing to configure — a sheet shared as
// "anyone with the link can view" works, a private one is reported as such.
//
// The caller's pasted URL is never fetched verbatim: ParseSheetURL extracts
// the spreadsheet ID (and optional gid) and FetchCSV constructs the export
// URL itself, so the only thing this package ever requests is a
// docs.google.com CSV export for a validated ID — a pasted URL pointing
// anywhere else is rejected up front rather than proxied.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// maxSheetBytes caps the fetched export at the same 16 MB the upload
// endpoint enforces via MaxBytesReader — a sheet is not a way around it.
const maxSheetBytes = 16 << 20

var (
	// Spreadsheet IDs are base64url-ish tokens (typically 44 chars); the
	// 20-char minimum keeps a bare pasted ID distinguishable from ordinary
	// words while never rejecting a real ID.
	sheetIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`)
	sheetGidPattern = regexp.MustCompile(`^[0-9]{1,19}$`)
)

// ErrSheetNotAccessible marks fetch failures caused by the sheet itself —
// not link-shared, deleted, or a wrong URL — as opposed to network trouble.
// Callers map it to a client error (the user must fix their sheet/URL) and
// everything else to an upstream error.
var ErrSheetNotAccessible = errors.New("sheet not accessible")

// ParseSheetURL extracts the spreadsheet ID and optional worksheet gid from
// a docs.google.com URL ("…/spreadsheets/d/<id>/edit?gid=0#gid=0" and
// variants), or accepts a bare spreadsheet ID pasted on its own.
func ParseSheetURL(raw string) (spreadsheetID, gid string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("sheet URL is empty")
	}
	if sheetIDPattern.MatchString(raw) {
		return raw, "", nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" {
		return "", "", fmt.Errorf("%q is not a valid URL — paste the sheet's docs.google.com link", raw)
	}
	if !strings.EqualFold(u.Hostname(), "docs.google.com") {
		return "", "", fmt.Errorf("only docs.google.com spreadsheet URLs are supported, got host %q", u.Hostname())
	}
	// Path shapes: /spreadsheets/d/<id>/edit, /spreadsheets/u/0/d/<id>/…
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	idIdx := -1
	for i, p := range parts {
		if p == "d" && i+1 < len(parts) {
			idIdx = i + 1
			break
		}
	}
	if len(parts) == 0 || parts[0] != "spreadsheets" || idIdx == -1 {
		return "", "", fmt.Errorf("URL is not a Google Sheets link (expected …/spreadsheets/d/<id>/…)")
	}
	spreadsheetID = parts[idIdx]
	if !sheetIDPattern.MatchString(spreadsheetID) {
		return "", "", fmt.Errorf("%q does not look like a spreadsheet ID", spreadsheetID)
	}
	// The worksheet gid lives in the query on newer share links and in the
	// fragment on older ones; check both, query first.
	gid = u.Query().Get("gid")
	if gid == "" {
		if fv, ferr := url.ParseQuery(u.Fragment); ferr == nil {
			gid = fv.Get("gid")
		}
	}
	if gid != "" && !sheetGidPattern.MatchString(gid) {
		return "", "", fmt.Errorf("%q is not a valid worksheet gid", gid)
	}
	return spreadsheetID, gid, nil
}

// SheetFetcher fetches a link-shared Google Sheet as CSV. The zero value is
// ready to use against the real docs.google.com; both fields exist only so
// tests can point it at an httptest server.
type SheetFetcher struct {
	BaseURL string       // defaults to "https://docs.google.com"
	Client  *http.Client // defaults to a client with a 30s timeout
}

// FetchCSV downloads the sheet's CSV export. An empty gid exports the first
// worksheet. Failures the user can fix (sheet not link-shared, wrong URL)
// wrap ErrSheetNotAccessible.
func (f *SheetFetcher) FetchCSV(ctx context.Context, spreadsheetID, gid string) ([]byte, error) {
	if !sheetIDPattern.MatchString(spreadsheetID) {
		return nil, fmt.Errorf("invalid spreadsheet ID %q", spreadsheetID)
	}
	if gid != "" && !sheetGidPattern.MatchString(gid) {
		return nil, fmt.Errorf("invalid worksheet gid %q", gid)
	}
	base := f.BaseURL
	if base == "" {
		base = "https://docs.google.com"
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	endpoint := fmt.Sprintf("%s/spreadsheets/d/%s/export?format=csv", base, spreadsheetID)
	if gid != "" {
		endpoint += "&gid=" + gid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch sheet: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("spreadsheet not found — check the URL: %w", ErrSheetNotAccessible)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the sheet is not link-shared — in Google Sheets set Share → \"Anyone with the link\" → Viewer: %w", ErrSheetNotAccessible)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("google returned HTTP %d for the sheet export", resp.StatusCode)
	}
	// A private sheet doesn't 403: the export redirects to a sign-in page
	// that answers 200 with HTML. Content type is the reliable signal.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		return nil, fmt.Errorf("the sheet is not link-shared (Google answered with a sign-in page) — in Google Sheets set Share → \"Anyone with the link\" → Viewer: %w", ErrSheetNotAccessible)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSheetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read sheet export: %w", err)
	}
	if len(data) > maxSheetBytes {
		return nil, fmt.Errorf("sheet export exceeds the %d MB import limit", maxSheetBytes>>20)
	}
	// Google's CSV export starts with a UTF-8 BOM; left in place it would
	// become part of the first column's header text and fail classification.
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), nil
}

// FetchCSVFromURL is ParseSheetURL + FetchCSV in one call, for callers that
// hold a stored sheet URL (saved integrations) rather than pre-split parts.
func (f *SheetFetcher) FetchCSVFromURL(ctx context.Context, rawURL string) ([]byte, error) {
	id, gid, err := ParseSheetURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", err.Error(), ErrSheetNotAccessible)
	}
	return f.FetchCSV(ctx, id, gid)
}
