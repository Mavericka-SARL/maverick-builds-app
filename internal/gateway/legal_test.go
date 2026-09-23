package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/legal, which decides whether the front-door pages offer documents
// at all. The case that matters is the empty one: a deployment that has
// configured nothing must report published=false, because /signup shows its
// "you agree to" line from this and would otherwise claim agreement to
// documents nobody wrote.
func TestLegalInfo(t *testing.T) {
	configured := LegalConfig{
		Entity: "Acme Software SARL", Address: "1 Rue de Test, Luxembourg",
		Email: "legal@acme.test", Jurisdiction: "Luxembourg",
		Hosting: "Hetzner Online GmbH (Germany)", Updated: "2026-09-18",
	}

	for _, tc := range []struct {
		name      string
		cfg       LegalConfig
		published bool
		builtin   bool
		terms     string
		privacy   string
	}{
		{name: "nothing configured"},
		{
			name: "operator named and reachable", cfg: configured,
			published: true, builtin: true, terms: "/terms", privacy: "/privacy",
		},
		{
			// A name with no mailbox cannot carry a privacy notice: there
			// would be nowhere to exercise a data-subject right.
			name: "named but unreachable",
			cfg:  LegalConfig{Entity: "Acme Software SARL"},
		},
		{
			name: "operator's own documents elsewhere",
			cfg: LegalConfig{
				TermsURL:   "https://acme.test/terms",
				PrivacyURL: "https://acme.test/privacy",
			},
			published: true, builtin: false,
			terms: "https://acme.test/terms", privacy: "https://acme.test/privacy",
		},
		{
			// One external document does not stop the other being the
			// shipped one; both must exist for the sign-up claim.
			name: "external terms with the shipped notice",
			cfg: LegalConfig{
				Entity: "Acme Software SARL", Email: "legal@acme.test",
				TermsURL: "https://acme.test/terms",
			},
			published: true, builtin: true,
			terms: "https://acme.test/terms", privacy: "/privacy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handler{legalCfg: tc.cfg}
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/legal", nil)
			h.legalInfo(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			var out struct {
				Published  bool   `json:"published"`
				Builtin    bool   `json:"builtin"`
				TermsURL   string `json:"terms_url"`
				PrivacyURL string `json:"privacy_url"`
				Entity     string `json:"entity"`
				Updated    string `json:"updated"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Published != tc.published || out.Builtin != tc.builtin {
				t.Errorf("published=%v builtin=%v, want %v/%v", out.Published, out.Builtin, tc.published, tc.builtin)
			}
			if out.TermsURL != tc.terms || out.PrivacyURL != tc.privacy {
				t.Errorf("terms=%q privacy=%q, want %q/%q", out.TermsURL, out.PrivacyURL, tc.terms, tc.privacy)
			}
			if out.Entity != tc.cfg.Entity || out.Updated != tc.cfg.Updated {
				t.Errorf("entity=%q updated=%q not passed through", out.Entity, out.Updated)
			}
		})
	}
}
