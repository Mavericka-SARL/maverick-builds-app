package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/plan"
)

var acmeLegal = LegalConfig{
	Entity: "Acme Software SARL", Address: "1 Rue de Test, L-1000 Luxembourg",
	Email: "legal@acme.test", Jurisdiction: "Luxembourg",
	Hosting: "Example Hosting GmbH (Germany)", Updated: "2026-09-18",
}

// docText is a document as one string, for "does it say X" checks.
func docText(d legalDocument) string {
	var b strings.Builder
	b.WriteString(d.Title + "\n" + d.Intro + "\n")
	for _, s := range d.Sections {
		b.WriteString(s.Heading + "\n")
		for _, blk := range s.Body {
			b.WriteString(blk.Text + "\n")
			for _, it := range blk.Items {
				b.WriteString(it + "\n")
			}
		}
	}
	return b.String()
}

func mustContain(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q", w)
		}
	}
}

func mustNotContain(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			t.Errorf("unexpected %q", w)
		}
	}
}

// The terms name the operator from the configuration and quote the edition
// the server offers — never anything from the bundle.
func TestTermsDocumentQuotesOperatorAndEdition(t *testing.T) {
	community := &legalPlanFacts{
		Name: "Community", Allowance: "100 MB of data",
		Next: "To run the platform on your own infrastructure, install the Community edition.",
	}
	text := docText(termsDocument(acmeLegal, "maverickbuilds.app", community))
	mustContain(t, text,
		"Acme Software SARL", "1 Rue de Test, L-1000 Luxembourg", "legal@acme.test",
		"The Community Edition costs nothing and has no end date. It is limited to 100 MB of data",
		"becomes read-only", "run the platform on your own infrastructure",
		"This agreement is governed by the law of Luxembourg",
		"The Community Edition needs no payment details",
	)
	mustNotContain(t, text, "an edition is agreed", "the free edition")

	small := &legalPlanFacts{Name: "Small", Allowance: planAllowanceProse(plan.Limits{MaxUsers: 5, MaxApplications: 2, MaxModels: 3})}
	text = docText(termsDocument(acmeLegal, "maverickbuilds.app", small))
	mustContain(t, text, "Small Edition costs nothing and has no end date", "5 users, 2 applications and 3 models")
	if regexp.MustCompile(`(?i)trial`).MatchString(text) {
		t.Error("an edition clause mentions a trial")
	}
}

// With sign-up closed there is no edition to quote: the generic clause, and
// "the free edition" capitalised where it opens a sentence.
func TestTermsDocumentWithoutAnEditionOrJurisdiction(t *testing.T) {
	cfg := acmeLegal
	cfg.Jurisdiction = ""
	d := termsDocument(cfg, "maverickbuilds.app", nil)
	text := docText(d)
	mustContain(t, text, "The edition in force for your workspace sets what it may hold", "The free edition needs no payment details")
	mustNotContain(t, text, "11. Law and courts", "governed by the law")
	if got := d.Sections[len(d.Sections)-1].Heading; got != "10. Liability" {
		t.Errorf("last section = %q, want the law section left out", got)
	}
}

// The Playground's retirement clause quotes the edition's own day counts.
func TestTermsDocumentPlaygroundDormancy(t *testing.T) {
	p := &legalPlanFacts{Name: "Playground", Dormancy: &legalDormancy{WarnDays: 30, DeleteDays: 45, BackupDays: 14}}
	mustContain(t, docText(termsDocument(acmeLegal, "maverickbuilds.app", p)),
		"a workspace nobody uses for 30 days", "within 15 days of that message", "Backups keep a copy for 14 days")
}

func TestPrivacyDocument(t *testing.T) {
	text := docText(privacyDocument(acmeLegal, "maverickbuilds.app", false))
	mustContain(t, text,
		"Acme Software SARL, 1 Rue de Test, L-1000 Luxembourg is the controller of the personal data",
		"There is no analytics, tracking or advertising technology",
		"Example Hosting GmbH (Germany), which hosts the servers",
		"kept for fourteen days", "complain to a data-protection supervisory authority",
		"9. Where it is processed",
	)
	// The chat connector is described only where it runs.
	mustNotContain(t, text, "ChatGPT")
	mustContain(t, docText(privacyDocument(acmeLegal, "maverickbuilds.app", true)),
		"You can also connect ChatGPT or Claude to your account.",
		"the text of the dashboards and workflows you may open",
		"your workspace's administrator can turn this off")

	// No hosting provider configured: the clauses that would name it go.
	cfg := acmeLegal
	cfg.Hosting = ""
	text = docText(privacyDocument(cfg, "maverickbuilds.app", false))
	mustNotContain(t, text, "9. Where it is processed", "which hosts the servers")
	mustContain(t, text, "the e-mail relay that delivers invitations")
}

func TestPlanAllowanceProse(t *testing.T) {
	for _, tc := range []struct {
		l    plan.Limits
		want string
	}{
		{plan.Limits{}, ""},
		{plan.Limits{MaxStorageMB: 100}, "100 MB of data"},
		{plan.Limits{MaxUsers: 1, MaxModels: 3}, "1 user and 3 models"},
		{plan.Limits{MaxUsers: 5, MaxApplications: 2, MaxModels: 3, MaxStorageMB: 50}, "5 users, 2 applications, 3 models and 50 MB of data"},
	} {
		if got := planAllowanceProse(tc.l); got != tc.want {
			t.Errorf("planAllowanceProse(%+v) = %q, want %q", tc.l, got, tc.want)
		}
	}
}

// The console renders body blocks as a string (paragraph) or an array of
// strings (a list): the JSON keeps that shape.
func TestLegalDocumentJSONShape(t *testing.T) {
	raw, err := json.Marshal(privacyDocument(acmeLegal, "maverickbuilds.app", false))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Sections []struct {
			Heading string            `json:"heading"`
			Body    []json.RawMessage `json:"body"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	second := d.Sections[1] // "2. What we hold": paragraph, list, paragraph
	if second.Heading != "2. What we hold" || len(second.Body) != 3 {
		t.Fatalf("section 2 = %q with %d blocks", second.Heading, len(second.Body))
	}
	if second.Body[0][0] != '"' || second.Body[1][0] != '[' {
		t.Errorf("blocks = %s / %s, want a string then an array", second.Body[0], second.Body[1])
	}
}

func legalServer(t *testing.T, cfg LegalConfig) *httptest.Server {
	t.Helper()
	h := &handler{legalCfg: cfg}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// fetched is one response, read and closed.
type fetched struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, url string) fetched {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fetched{res.StatusCode, res.Header, string(b)}
}

// GET /terms and /privacy answer the document itself as HTML: no script,
// the text in the markup, and a CSP that admits exactly the page's stylesheet.
func TestLegalPagesServeHTML(t *testing.T) {
	cfg := acmeLegal
	cfg.Entity = `Acme <Software> SARL`
	srv := legalServer(t, cfg)

	r := get(t, srv.URL+"/terms")
	if r.status != http.StatusOK {
		t.Fatalf("GET /terms = %d", r.status)
	}
	if ct := r.header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	mustContain(t, r.body,
		"<title>Terms of service · maverickbuilds.app</title>",
		"<h1>Terms of service</h1>",
		`In force from <time datetime="2026-09-18">18 September 2026</time>`,
		"Acme &lt;Software&gt; SARL", // configuration is text, never markup
		`<a href="/privacy">Privacy notice</a>`,
		"<ul><li>do not use it to store or process unlawful content",
		"The free edition needs no payment details", // sign-up is closed here
	)
	mustNotContain(t, r.body, "<script", "Acme <Software> SARL")

	m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatal("no stylesheet")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP %q does not admit the page's own stylesheet", csp)
	}

	r = get(t, srv.URL+"/privacy")
	if r.status != http.StatusOK {
		t.Fatalf("GET /privacy = %d", r.status)
	}
	mustContain(t, r.body, "<h1>Privacy notice</h1>", `<a href="/terms">Terms of service</a>`, "is the controller of the personal data")
}

// The operator's own documents elsewhere: a real redirect, no page.
func TestLegalPagesRedirectToOperatorsOwn(t *testing.T) {
	cfg := acmeLegal
	cfg.PrivacyURL = "https://acme.test/legal/privacy"
	srv := legalServer(t, cfg)

	r := get(t, srv.URL+"/privacy")
	if r.status != http.StatusFound || r.header.Get("Location") != "https://acme.test/legal/privacy" {
		t.Errorf("GET /privacy = %d to %q", r.status, r.header.Get("Location"))
	}
	// The terms still render, and link to the notice where it lives.
	r = get(t, srv.URL+"/terms")
	mustContain(t, r.body, `<a href="https://acme.test/legal/privacy">Privacy notice</a>`)
	r = get(t, srv.URL+"/api/legal/documents/privacy")
	if r.status != http.StatusNotFound {
		t.Errorf("JSON of a document published elsewhere = %d, want 404", r.status)
	}
}

// A deployment that configured nothing publishes nothing, and says so.
func TestLegalPagesUnpublished(t *testing.T) {
	srv := legalServer(t, LegalConfig{})
	r := get(t, srv.URL+"/terms")
	if r.status != http.StatusNotFound {
		t.Errorf("GET /terms = %d, want 404", r.status)
	}
	mustContain(t, r.body, "This deployment of maverickbuilds.app has not published terms of service.", `<meta name="robots" content="noindex">`)
	r = get(t, srv.URL+"/privacy")
	mustContain(t, r.body, "has not published a privacy notice")
}

func TestLegalDocumentJSON(t *testing.T) {
	srv := legalServer(t, acmeLegal)
	r := get(t, srv.URL+"/api/legal/documents/terms")
	if r.status != http.StatusOK {
		t.Fatalf("GET terms JSON = %d", r.status)
	}
	var d legalDocumentWire
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Title != "Terms of service" || len(d.Sections) != 11 {
		t.Errorf("terms JSON = %q with %d sections", d.Title, len(d.Sections))
	}
	if r := get(t, srv.URL+"/api/legal/documents/cookies"); r.status != http.StatusNotFound {
		t.Errorf("unknown document = %d, want 404", r.status)
	}
	empty := legalServer(t, LegalConfig{})
	if r := get(t, empty.URL+"/api/legal/documents/terms"); r.status != http.StatusNotFound {
		t.Errorf("unpublished document = %d, want 404", r.status)
	}
}

type legalDocumentWire struct {
	Title    string            `json:"title"`
	Sections []json.RawMessage `json:"sections"`
}

// The Ingress routes /terms/ here too; it goes to the page.
func TestLegalTrailingSlash(t *testing.T) {
	h := legalTrailingSlash(http.NotFoundHandler())
	for path, want := range map[string]string{"/terms/": "/terms", "/privacy/": "/privacy"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != want {
			t.Errorf("%s = %d to %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/terms", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/terms itself was redirected (%d)", rec.Code)
	}
}
