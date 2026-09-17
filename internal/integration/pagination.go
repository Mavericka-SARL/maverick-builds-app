package integration

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// pageState drives one pull run's iteration. Next() yields the template
// values for the upcoming request; Advance() consumes the response to decide
// whether another page follows.
type pageState struct {
	cfg     PaginationConfig
	pageNum int    // 1-based ordinal of the NEXT page to fetch
	cursor  string // cursor / link-header URL for the next request
	nextURL string // link_header mode: full URL override
}

func newPageState(cfg PaginationConfig) *pageState {
	start := cfg.StartPage
	if start <= 0 {
		start = 1
	}
	return &pageState{cfg: cfg, pageNum: start}
}

func (p *pageState) maxPages() int {
	if p.cfg.Mode == PageNone || p.cfg.Mode == "" {
		return 1
	}
	if p.cfg.MaxPages > 0 {
		return p.cfg.MaxPages
	}
	return 100
}

// templateVals returns {{page.number}} / {{page.cursor}} for the request.
// offset_limit mode exposes the OFFSET through page.number (that is what the
// URL/query template plugs in) — computed from ordinal × page size.
func (p *pageState) templateVals(ordinal int) map[string]string {
	switch p.cfg.Mode {
	case PageOffset:
		size := p.cfg.PageSize
		if size <= 0 {
			size = 100
		}
		return map[string]string{"number": strconv.Itoa((ordinal - 1) * size), "cursor": p.cursor}
	default:
		return map[string]string{"number": strconv.Itoa(p.pageNum), "cursor": p.cursor}
	}
}

// overrideURL returns a full replacement URL (link_header mode) or "".
func (p *pageState) overrideURL() string { return p.nextURL }

var linkNextRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// advance inspects the decoded response document (JSON modes) and response
// headers, returns false when iteration must stop. records is the extracted
// page record count (StopWhenEmpty semantics: zero records ends iteration).
func (p *pageState) advance(doc any, header http.Header, records int, allowInsecure bool) (bool, error) {
	if p.cfg.Mode == PageNone || p.cfg.Mode == "" {
		return false, nil
	}
	if records == 0 {
		return false, nil
	}
	switch p.cfg.Mode {
	case PageNone:
		return false, nil
	case PageNumber, PageOffset:
		p.pageNum++
		return true, nil
	case PageCursor:
		steps, err := ParsePath(p.cfg.CursorPath)
		if err != nil {
			return false, err
		}
		v, ok := LookupPath(doc, steps)
		if !ok || v == nil {
			return false, nil
		}
		next := ""
		switch t := v.(type) {
		case string:
			next = t
		case float64:
			next = strconv.FormatFloat(t, 'f', -1, 64)
		default:
			return false, fmt.Errorf("cursor at %s is neither string nor number", p.cfg.CursorPath)
		}
		if next == "" || next == p.cursor {
			return false, nil // exhausted or a loop — either way, stop
		}
		p.cursor = next
		p.pageNum++
		return true, nil
	case PageLink:
		m := linkNextRe.FindStringSubmatch(header.Get("Link"))
		if m == nil {
			return false, nil
		}
		nextURL := m[1]
		if nextURL == p.nextURL {
			return false, nil // self-referencing Link loop
		}
		// The remote controls this URL: it gets the FULL validation gauntlet
		// before it is ever fetched.
		if _, err := ValidateURL(nextURL, allowInsecure); err != nil {
			return false, err
		}
		p.nextURL = nextURL
		p.pageNum++
		return true, nil
	}
	return false, nil
}

// ── Response record extraction ───────────────────────────────────────────────

// extractRecords pulls the record collection out of a decoded JSON document
// using RecordsPath. A single object (not array) becomes one record; scalar
// collections are invalid.
func extractRecords(doc any, recordsPath string) ([]map[string]any, error) {
	target := doc
	if recordsPath != "" {
		steps, err := ParsePath(recordsPath)
		if err != nil {
			return nil, err
		}
		v, ok := LookupPath(doc, steps)
		if !ok {
			return nil, fmt.Errorf("records_path %s not present in response", recordsPath)
		}
		target = v
	}
	switch t := target.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for i, el := range t {
			obj, ok := el.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("record %d is not an object", i)
			}
			out = append(out, obj)
		}
		return out, nil
	case map[string]any:
		return []map[string]any{t}, nil
	default:
		return nil, fmt.Errorf("records collection is neither an array nor an object")
	}
}

// csvToRecords turns CSV text into records keyed by header.
func csvToRecords(header []string, rows [][]string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rec := make(map[string]any, len(header))
		for i, h := range header {
			if i < len(r) {
				rec[strings.TrimSpace(h)] = r[i]
			}
		}
		out = append(out, rec)
	}
	return out
}
