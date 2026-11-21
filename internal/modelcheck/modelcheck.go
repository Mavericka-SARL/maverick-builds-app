// Package modelcheck finds what in a revision is legal but costs time later
// (2026-11-21, from an inventory model built by hand): a record field that
// posts nothing, a pick-list whose total is computed from blanks, a SUMIFS
// that adds the members of a dimension that are never added up, a dashboard
// that pins members of a dimension business users keep adding to, a role
// whose hidden members blank the figures built across them, an input left
// open to every role, a schedule that cannot start its workflow.
//
// One registry of rules serves Developer Models › Model check, the warnings a save
// returns for the object saved, the AI Developer's plan check (the findings
// a plan would add) and the summary before a revision is set active. A
// finding a developer decides to keep is ignored with a reason, per
// revision (model.check_ignore); its key names it by what it is about, never
// by ids, so a revision copy and a model export keep the ignores.
package modelcheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Severities, most serious first.
const (
	SeverityError   = "error"   // it fails when it runs
	SeverityWarning = "warning" // it runs, and shows something wrong
	SeverityInfo    = "info"    // worth a look
)

// Object is what a finding is about: Kind is one of metric, mapping,
// widget, dashboard, role, automation_rule; ID its id in the revision.
type Object struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Dashboard is the dashboard a widget is on.
	Dashboard string `json:"dashboard,omitempty"`
}

// Finding is one thing a rule found.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Object   Object `json:"object"`
	Message  string `json:"message"`
	Fix      string `json:"fix"`
	// Key names the finding across revision copies and exports.
	Key string `json:"key"`

	Ignored      bool       `json:"ignored"`
	IgnoreReason string     `json:"ignore_reason,omitempty"`
	IgnoredBy    string     `json:"ignored_by,omitempty"`
	IgnoredAt    *time.Time `json:"ignored_at,omitempty"`
}

// Report is a revision's findings, the ignored ones included (marked).
type Report struct {
	RevisionID string         `json:"revision_id"`
	Findings   []Finding      `json:"findings"`
	Counts     map[string]int `json:"counts"` // error, warning, info (not ignored), ignored
}

// Rule is one check: Run returns its findings in a revision.
type Rule struct {
	ID       string
	Title    string
	Severity string
	Run      func(ctx context.Context, rc *revisionContext) ([]Finding, error)
}

// Rules is the registry, in the order a report lists them.
func Rules() []Rule {
	return []Rule{
		ruleScheduleGap,
		ruleTextPosting,
		rulePicklistTotal,
		ruleUnsummedSpan,
		rulePinnedMembers,
		ruleHiddenReads,
		ruleOpenInput,
	}
}

// Run checks a revision with every rule and marks the ignored findings.
func Run(ctx context.Context, q Querier, modelID, revisionID string) (*Report, error) {
	rc, err := loadRevision(ctx, q, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	var all []Finding
	for _, r := range Rules() {
		found, err := r.Run(ctx, rc)
		if err != nil {
			return nil, fmt.Errorf("model check %s: %w", r.ID, err)
		}
		for i := range found {
			found[i].Rule, found[i].Title = r.ID, r.Title
			if found[i].Severity == "" {
				found[i].Severity = r.Severity
			}
		}
		all = append(all, found...)
	}
	if err := markIgnored(ctx, q, revisionID, all); err != nil {
		return nil, err
	}
	rank := map[string]int{SeverityError: 0, SeverityWarning: 1, SeverityInfo: 2}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Ignored != all[j].Ignored {
			return !all[i].Ignored
		}
		return rank[all[i].Severity] < rank[all[j].Severity]
	})
	rep := &Report{RevisionID: revisionID, Findings: all, Counts: map[string]int{SeverityError: 0, SeverityWarning: 0, SeverityInfo: 0, "ignored": 0}}
	if rep.Findings == nil {
		rep.Findings = []Finding{}
	}
	for _, f := range all {
		if f.Ignored {
			rep.Counts["ignored"]++
		} else {
			rep.Counts[f.Severity]++
		}
	}
	return rep, nil
}

// For is the report's findings about one object that are not ignored: the
// warnings a save of it answers with.
func (r *Report) For(kind, id string) []Finding {
	out := []Finding{}
	for _, f := range r.Findings {
		if !f.Ignored && f.Object.Kind == kind && f.Object.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// Open is the report's findings that are not ignored.
func (r *Report) Open() []Finding {
	out := []Finding{}
	for _, f := range r.Findings {
		if !f.Ignored {
			out = append(out, f)
		}
	}
	return out
}

// markIgnored marks the findings whose key the revision ignores.
func markIgnored(ctx context.Context, q Querier, revisionID string, findings []Finding) error {
	rows, err := q.Query(ctx, `
		SELECT ci.key, ci.reason, COALESCE(NULLIF(u.display_name, ''), u.email, ''), ci.ignored_at
		FROM model.check_ignore ci LEFT JOIN identity.user u ON u.id = ci.ignored_by
		WHERE ci.revision_id = $1::uuid`, revisionID)
	if err != nil {
		return err
	}
	type ignore struct {
		reason, by string
		at         time.Time
	}
	ignored := map[string]ignore{}
	for rows.Next() {
		var k string
		var ig ignore
		if err := rows.Scan(&k, &ig.reason, &ig.by, &ig.at); err != nil {
			rows.Close()
			return err
		}
		ignored[k] = ig
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range findings {
		if ig, ok := ignored[findings[i].Key]; ok {
			at := ig.at
			findings[i].Ignored, findings[i].IgnoreReason, findings[i].IgnoredBy, findings[i].IgnoredAt = true, ig.reason, ig.by, &at
		}
	}
	return nil
}

// key builds a finding key from its parts, without case.
func key(parts ...string) string {
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return strings.Join(parts, "|")
}

// list names up to five items, then how many more.
func list(items []string) string {
	if len(items) <= 5 {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:5], ", "), len(items)-5)
}
