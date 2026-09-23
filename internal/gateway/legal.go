package gateway

import "net/http"

// The terms a deployment publishes, and who is accountable for them.
//
// The engine ships the *text* of a terms of service and a privacy notice
// (web/src/public/legal) because it describes what this software does with
// data, which is the same wherever it runs. Who answers for it is not: that is
// whoever operates the deployment, and it is configuration. A deployment that
// already has its own documents points at them with LEGAL_TERMS_URL and
// LEGAL_PRIVACY_URL, and the built-in pages step aside.
//
// None of this is legal advice and the shipped text is a starting point an
// operator is expected to have reviewed. What the code guarantees is narrower,
// and is the part that matters here: the sign-up page claims agreement to
// documents only when the deployment actually publishes some. An install that
// has configured nothing never asserts a contract that does not exist.
type LegalConfig struct {
	// Entity is the operator's legal name — the counterparty of the terms
	// and the controller of the personal data. Without it the built-in
	// documents have no author and are not offered.
	Entity string
	// Address is the operator's postal address, which a privacy notice is
	// required to carry in the EU.
	Address string
	// Email receives legal and data-protection correspondence. It has to be
	// a mailbox someone reads, not a no-reply sender.
	Email string
	// Jurisdiction is the governing law and the courts, as prose
	// ("Luxembourg"). Empty leaves the clause out rather than guessing.
	Jurisdiction string
	// Hosting names the infrastructure provider and where it runs, for the
	// privacy notice's recipients section.
	Hosting string
	// Updated is the date the documents last changed, as YYYY-MM-DD. The
	// pages show it, because a policy with no date cannot be relied on.
	Updated string
	// TermsURL and PrivacyURL point at documents the operator publishes
	// elsewhere. Either one set replaces the built-in page for that
	// document, so a company with a single legal site keeps one copy.
	TermsURL   string
	PrivacyURL string
}

// termsHref is where "terms of service" leads, or "" when the deployment
// publishes none.
func (l LegalConfig) termsHref() string {
	if l.TermsURL != "" {
		return l.TermsURL
	}
	if l.builtinAvailable() {
		return "/terms"
	}
	return ""
}

// privacyHref is where "privacy notice" leads, or "" when there is none.
func (l LegalConfig) privacyHref() string {
	if l.PrivacyURL != "" {
		return l.PrivacyURL
	}
	if l.builtinAvailable() {
		return "/privacy"
	}
	return ""
}

// builtinAvailable reports whether the shipped documents can be rendered:
// they name the operator and give an address to reach them at, and a document
// missing either is worse than none.
func (l LegalConfig) builtinAvailable() bool {
	return l.Entity != "" && l.Email != ""
}

// Published reports whether both documents exist. The sign-up page's "you
// agree to" line appears only then.
func (l LegalConfig) Published() bool {
	return l.termsHref() != "" && l.privacyHref() != ""
}

// legalInfo serves GET /api/legal: what the front-door pages need to render
// the documents, and what to link to. Public — a visitor reads the terms
// before they have an account, which is the whole point of them.
func (h *handler) legalInfo(w http.ResponseWriter, r *http.Request) {
	l := h.legalCfg
	jsonOK(w, map[string]any{
		"published":    l.Published(),
		"builtin":      l.builtinAvailable(),
		"terms_url":    l.termsHref(),
		"privacy_url":  l.privacyHref(),
		"entity":       l.Entity,
		"address":      l.Address,
		"email":        l.Email,
		"jurisdiction": l.Jurisdiction,
		"hosting":      l.Hosting,
		"updated":      l.Updated,
	})
}
