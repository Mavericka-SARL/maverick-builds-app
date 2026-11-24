package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// GET /terms and GET /privacy: the two documents as finished HTML, rendered
// here rather than in the browser, so a reader that runs no JavaScript — a
// search engine, an AI assistant — reads the text itself and not an empty
// console shell. The Ingress (deploy/k8s/base/ingress.yaml) and the Compose
// Caddyfile send these two paths to the gateway; behind a proxy that does
// not, they reach the console, whose own page (LegalPage.tsx) renders the
// same document from GET /api/legal/documents/{doc}.
//
// The three states are the console page's: the operator publishes the
// document elsewhere (a redirect there), it publishes the shipped text (the
// page, with the operator filled in), or it publishes nothing (a page that
// says so, answered 404).

// publicProductName is the product's name as a visitor with no account sees
// it: the host's white-label brand where one is configured, else the
// product's own (the console's useBrand falls back the same way).
func (h *handler) publicProductName(ctx context.Context, r *http.Request) (string, string) {
	if v, ok := h.hostBrand(ctx, r); ok && v.ProductName != "" {
		return v.ProductName, v.LogoDataURL
	}
	return "maverickbuilds.app", ""
}

// signupPlanFacts is the edition the terms quote: the one sign-up hands out
// right now (signupOffer), or nil when sign-up is closed — the same offer,
// and the same condition, as GET /api/signup/options.
func (h *handler) signupPlanFacts(ctx context.Context) *legalPlanFacts {
	p, reason := h.signupOffer(ctx)
	if reason != "" {
		return nil
	}
	f := &legalPlanFacts{Name: p.Name, Allowance: planAllowanceProse(p.Limits), Next: p.LimitNote}
	if h.signupCfg.Playground && p.DormantWarnDays > 0 && p.DormantDeleteDays > 0 {
		backup := h.signupCfg.BackupRetentionDays
		if backup == 0 {
			backup = 14
		}
		f.Dormancy = &legalDormancy{WarnDays: p.DormantWarnDays, DeleteDays: p.DormantDeleteDays, BackupDays: backup}
	}
	return f
}

// legalTrailingSlash sends /terms/ and /privacy/ to the pages: the Ingress
// routes both paths here by prefix (Traefik ranks routers by rule length, so
// an exact path would lose to the console's "/"), and the console's own page
// accepted the slash.
func legalTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/terms/" || p == "/privacy/" {
			http.Redirect(w, r, strings.TrimSuffix(p, "/"), http.StatusMovedPermanently)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var httpURL = regexp.MustCompile(`(?i)^https?://`)

// externalLegalURL is where the operator publishes doc themselves, or "".
func (l LegalConfig) externalLegalURL(doc string) string {
	u := l.TermsURL
	if doc == "privacy" {
		u = l.PrivacyURL
	}
	if httpURL.MatchString(u) {
		return u
	}
	return ""
}

// builtinLegalDocument assembles doc for this request; ok is false when the
// deployment does not publish the shipped text of it.
func (h *handler) builtinLegalDocument(ctx context.Context, r *http.Request, doc, product string) (legalDocument, bool) {
	l := h.legalCfg
	if l.externalLegalURL(doc) != "" || !l.builtinAvailable() {
		return legalDocument{}, false
	}
	if doc == "privacy" {
		return privacyDocument(l, product, h.mcp.Enabled), true
	}
	return termsDocument(l, product, h.signupPlanFacts(ctx)), true
}

// legalDocumentJSON serves GET /api/legal/documents/{doc}: the document the
// console's /terms and /privacy render when a proxy sends those paths to it.
// 404 when the deployment publishes no shipped text for it — GET /api/legal
// says which of the three states applies.
func (h *handler) legalDocumentJSON(w http.ResponseWriter, r *http.Request) {
	doc := r.PathValue("doc")
	if doc != "terms" && doc != "privacy" {
		jsonErr(w, fmt.Errorf("no such document"), http.StatusNotFound)
		return
	}
	product, _ := h.publicProductName(r.Context(), r)
	d, ok := h.builtinLegalDocument(r.Context(), r, doc, product)
	if !ok {
		jsonErr(w, fmt.Errorf("this deployment does not publish the built-in %s document", doc), http.StatusNotFound)
		return
	}
	jsonOK(w, d)
}

// legalPage serves GET /terms or GET /privacy as HTML.
func (h *handler) legalPage(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if u := h.legalCfg.externalLegalURL(doc); u != "" {
			http.Redirect(w, r, u, http.StatusFound)
			return
		}
		product, logo := h.publicProductName(ctx, r)
		view := legalPageView{Product: product, Logo: safeLogo(logo), CSS: template.CSS(legalPageCSS)}
		status := http.StatusOK
		if d, ok := h.builtinLegalDocument(ctx, r, doc, product); ok {
			view.Doc = &d
			view.Updated, view.UpdatedText = h.legalCfg.Updated, legalDate(h.legalCfg.Updated)
			if doc == "terms" {
				view.OtherHref, view.OtherLabel = h.legalCfg.privacyHref(), "Privacy notice"
			} else {
				view.OtherHref, view.OtherLabel = h.legalCfg.termsHref(), "Terms of service"
			}
		} else {
			status = http.StatusNotFound
			view.Missing = "a privacy notice"
			if doc == "terms" {
				view.Missing = "terms of service"
			}
		}
		var buf bytes.Buffer
		if err := legalPageTemplate.Execute(&buf, view); err != nil {
			http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Type", "text/html; charset=utf-8")
		// The text follows the deployment's configuration and its open
		// edition, so a cached copy is revalidated, never trusted.
		hdr.Set("Cache-Control", "no-cache")
		hdr.Set("Content-Security-Policy", legalPageCSP)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.WriteHeader(status)
		_, _ = w.Write(buf.Bytes())
	}
}

type legalPageView struct {
	Product     string
	Logo        template.URL
	CSS         template.CSS
	Doc         *legalDocument
	Updated     string
	UpdatedText string
	OtherHref   string
	OtherLabel  string
	// Missing names the document a deployment has not published.
	Missing string
}

// safeLogo passes a white-label logo through only as an inline image, which
// is all the branding settings store; anything else falls back to the
// product mark.
var logoDataURL = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp|svg\+xml);base64,[A-Za-z0-9+/=]+$`)

func safeLogo(s string) template.URL {
	if logoDataURL.MatchString(s) {
		return template.URL(s)
	}
	return ""
}

// legalDate is "2026-09-18" as "18 September 2026", and unchanged if it is
// not a date (LegalPage.tsx formats it the same way).
func legalDate(s string) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.Format("2 January 2006")
}

// The look of the console's public pages (web/src/public/public.css): the
// dark ground with the warm glow, one wide column of prose. Inline, so the
// page needs nothing else from the server; the CSP admits exactly this
// stylesheet by its hash.
const legalPageCSS = `
:root{--bg:#0a0a0a;--line:#262626;--text:#fff;--muted:#b4b4b4;--quiet:#7c7c7c;--accent:#eeb900}
*{box-sizing:border-box}
html{background:var(--bg)}
body{margin:0;min-height:100vh;color:var(--text);-webkit-font-smoothing:antialiased;
font-family:"Avenir Next",Avenir,"Segoe UI Variable Display","Segoe UI",system-ui,-apple-system,"Helvetica Neue",Arial,sans-serif;
background:var(--bg);background-image:radial-gradient(60rem 40rem at 12% -10%,rgba(238,185,0,.14),transparent 60%),radial-gradient(45rem 30rem at 100% 0%,rgba(238,185,0,.05),transparent 55%);background-repeat:no-repeat}
main{width:100%;max-width:44rem;margin:0 auto;padding:min(14vh,6rem) 1.5rem 3rem}
.mark{display:inline-flex;align-items:center;gap:.625rem;margin-bottom:2.25rem;color:var(--text);font-size:1.25rem;font-weight:800;letter-spacing:-.01em;text-decoration:none}
.mark img{display:block;height:1.75rem;width:auto}
h1{margin:0 0 .5rem;font-size:clamp(1.5rem,1.1rem + 1.6vw,2rem);line-height:1.05;font-weight:800;letter-spacing:-.02em}
h2{margin:0 0 .75rem;font-size:1rem;font-weight:700;letter-spacing:.01em}
p,li{color:var(--muted);font-size:.9375rem;line-height:1.65}
p{margin:0 0 1rem}
.intro{font-size:1rem}
.date{margin:0 0 2.5rem;padding-bottom:1.25rem;border-bottom:1px solid var(--line);color:var(--quiet);font-size:.8125rem}
section{margin-bottom:2rem}
ul{list-style:disc outside;margin:0 0 1rem;padding-left:1.125rem}
li{margin-bottom:.5rem}
a{color:var(--accent);text-decoration:none}
a:hover,a:focus-visible{text-decoration:underline}
:focus-visible{outline:2px solid var(--accent);outline-offset:3px;border-radius:2px}
.footer{margin-top:2rem;padding-top:1.5rem;border-top:1px solid var(--line);color:var(--quiet);font-size:.8125rem}
@media print{html,body{background:#fff;color:#000}body{background-image:none}p,li,.date,.footer{color:#000}a{color:#000;text-decoration:underline}.mark{color:#000}}
`

var legalPageCSP = func() string {
	sum := sha256.Sum256([]byte(legalPageCSS))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

var legalPageTemplate = template.Must(template.New("legal").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{- if .Doc}}
<title>{{.Doc.Title}} · {{.Product}}</title>
<meta name="description" content="{{.Doc.Intro}}">
{{- else}}
<title>Not published · {{.Product}}</title>
<meta name="robots" content="noindex">
{{- end}}
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<style>{{.CSS}}</style>
</head>
<body>
<main>
<a class="mark" href="/">{{if .Logo}}<img src="{{.Logo}}" alt="{{.Product}}">{{else}}<img src="/favicon.svg" alt="" width="28" height="28"><span>{{.Product}}</span>{{end}}</a>
{{- with .Doc}}
<article>
<h1>{{.Title}}</h1>
<p class="intro">{{.Intro}}</p>
{{- if $.Updated}}
<p class="date">In force from <time datetime="{{$.Updated}}">{{$.UpdatedText}}</time></p>
{{- end}}
{{- range .Sections}}
<section>
<h2>{{.Heading}}</h2>
{{- range .Body}}
{{- if .Items}}
<ul>{{range .Items}}<li>{{.}}</li>{{end}}</ul>
{{- else}}
<p>{{.Text}}</p>
{{- end}}
{{- end}}
</section>
{{- end}}
<p class="footer">{{if $.OtherHref}}<a href="{{$.OtherHref}}">{{$.OtherLabel}}</a> · {{end}}<a href="/signup">Create a workspace</a> · <a href="/">Sign in</a></p>
</article>
{{- else}}
<h1>Not published</h1>
<p>This deployment of {{.Product}} has not published {{.Missing}}. Ask whoever operates it before you put anything into it.</p>
<p class="footer"><a href="/">Back to sign in</a></p>
{{- end}}
</main>
</body>
</html>
`))
