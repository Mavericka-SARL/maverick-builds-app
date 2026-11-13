package aiassistant

// The task plan of a delegated goal (migration 162): the AI Developer plans
// a goal the developer leaves to it before building it, works it one stage
// per proposal, and marks each stage as it goes. Every turn of the session is
// shown the plan; the developer sees it in the chat.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskPlan is a delegated goal split into stages.
type TaskPlan struct {
	Goal      string      `json:"goal"`
	Stages    []TaskStage `json:"stages"`
	UpdatedAt time.Time   `json:"updated_at"`
	// CarriedFrom is the earlier session this plan was carried over from
	// (CarryTaskPlan): a session ends at its call limit, the goal does not.
	CarriedFrom string `json:"carried_from,omitempty"`
	// CarriedChecked: the carried plan's stages were checked against the
	// model on the session's first turn (the gateway's carried-plan check).
	CarriedChecked bool `json:"carried_checked,omitempty"`
	// Decisions are what the developer decided in answer to the AI — "store
	// percentages in percent units", "an opening balance is the last
	// closing" — kept for every later turn and session, never asked again.
	// Session 2 of the High-Tech build asked the percent question answered
	// in session 1, and redid the inputs as fractions (2026-11-11).
	Decisions []string `json:"decisions,omitempty"`
	// Target is the TO-BE model of a build from a workbook (target_model.go),
	// recorded before the plan and kept with it.
	Target *TargetModel `json:"target,omitempty"`
}

// maxDecisions bounds the decisions a plan keeps (the oldest go first).
const maxDecisions = 30

// AddDecision keeps decision (trimmed, at most 500 characters) unless the
// plan holds it already; it reports whether the plan changed.
func (p *TaskPlan) AddDecision(decision string) bool {
	d := strings.Join(strings.Fields(decision), " ")
	if d == "" {
		return false
	}
	if r := []rune(d); len(r) > 500 {
		d = string(r[:500]) + "…"
	}
	for _, have := range p.Decisions {
		if strings.EqualFold(have, d) {
			return false
		}
	}
	p.Decisions = append(p.Decisions, d)
	if len(p.Decisions) > maxDecisions {
		p.Decisions = p.Decisions[len(p.Decisions)-maxDecisions:]
	}
	return true
}

// MarkCovered notes that proposalID does every item of the current stage
// (the coverage check found none missing): confirmed whole, its items count
// as done (RecordConfirmed).
func (p *TaskPlan) MarkCovered(proposalID string) bool {
	cur := p.Current()
	if cur < 0 {
		return false
	}
	return p.NoteCoverage(proposalID, p.Stages[cur].Items)
}

// NoteCoverage notes which items of the current stage proposalID does (the
// coverage check's answer). Confirmed whole, they count as done; a stage is
// finished once its confirmed plans did every item between them — a stage
// is often several plans (2026-11-12: every plan of an eleven-sheet stage
// was sent back as incomplete, and the AI looped on four-step plans).
func (p *TaskPlan) NoteCoverage(proposalID string, items []string) bool {
	cur := p.Current()
	if cur < 0 || proposalID == "" || len(p.Stages[cur].Items) == 0 {
		return false
	}
	st := &p.Stages[cur]
	st.PendingCover = &StageCover{Proposal: proposalID, Items: items}
	if containsAllItems(items, st.Items) {
		st.CoveredBy = proposalID
	}
	return true
}

// OpenItems are the stage's items no confirmed plan has done yet.
func (s TaskStage) OpenItems() []string {
	var out []string
	for _, it := range s.Items {
		if !containsItem(s.Covered, it) {
			out = append(out, it)
		}
	}
	return out
}

// needsProofRE: a stage that loads or matches values is finished by its
// numbers, not by its steps having been confirmed (2026-11-12: a plan
// writing 9 of 72 driver values was judged to "load the workbook's values
// for all driver inputs", and the stage was closed).
var needsProofRE = regexp.MustCompile(`(?i)\b(values?|load(s|ed|ing)?|import(s|ed|ing)?|match(es|ed|ing)?|reconcil\w*|compar\w*|actuals?)\b`)

// NeedsProof reports whether the stage's numbers decide when it is done.
func (s TaskStage) NeedsProof() bool {
	if len(s.StageSheets()) > 0 {
		return true
	}
	if needsProofRE.MatchString(s.Title) {
		return true
	}
	for _, it := range s.Items {
		if needsProofRE.MatchString(it) {
			return true
		}
	}
	return false
}

func normItem(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r <= 127
	}), " ")
}

func containsItem(list []string, item string) bool {
	for _, x := range list {
		if SameItem(x, item) {
			return true
		}
	}
	return false
}

func containsAllItems(have, want []string) bool {
	for _, w := range want {
		if !containsItem(have, w) {
			return false
		}
	}
	return true
}

// SameItem reports whether a coverage answer's item names the stage item
// (the checker repeats items in its own words at times).
func SameItem(answer, item string) bool {
	a, b := normItem(answer), normItem(item)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= 12 && strings.Contains(long, short)
}

// stageNumberRE strips a "Stage 3 —" numbering, to recognise the same
// stage proposed again.
var stageNumberRE = regexp.MustCompile(`(?i)^\s*(stage|step|phase)\s*\d+\s*[—–:.\-]*\s*`)

func stageTitleKey(t string) string { return normItem(stageNumberRE.ReplaceAllString(t, "")) }

// StageCover is the coverage check's answer for one proposal, kept until
// the developer confirms it.
type StageCover struct {
	Proposal string   `json:"proposal"`
	Items    []string `json:"items"`
}

// TaskStage is one stage: what it delivers (named, so a plan for it can be
// checked against them) and how the AI checks it once confirmed.
type TaskStage struct {
	Title  string   `json:"title"`
	Items  []string `json:"items"`
	Check  string   `json:"check,omitempty"`
	Status string   `json:"status"` // pending | in_progress | done | skipped
	Note   string   `json:"note,omitempty"`
	// Confirmed counts the plans the developer confirmed while the stage
	// was current, and their steps (RecordConfirmed).
	Confirmed      int `json:"confirmed,omitempty"`
	ConfirmedSteps int `json:"confirmed_steps,omitempty"`
	// CoveredBy is the proposal the coverage check found doing every item
	// of the stage (MarkCovered).
	CoveredBy string `json:"covered_by,omitempty"`
	// Covered are the items confirmed plans of the stage did, between them;
	// PendingCover is the latest checked proposal's share, until confirmed.
	Covered      []string    `json:"covered,omitempty"`
	PendingCover *StageCover `json:"pending_cover,omitempty"`
	// ProofAsked: a "done" the workbook map could not confirm was sent back
	// once; marked done again, it is taken with the note (update_task_plan).
	ProofAsked bool `json:"proof_asked,omitempty"`
	// Sheet is the workbook sheet the stage builds (plans before modules);
	// Sheets the module of dependent sheets it builds together (owner,
	// 2026-11-12: one coherent module per stage, e.g. Hiring Plan →
	// Headcount → payroll). The stage is done when the workbook map shows
	// every one of them matching.
	Sheet  string   `json:"sheet,omitempty"`
	Sheets []string `json:"sheets,omitempty"`
	// Evidence is how the stage ended: what was built or repaired, the rows
	// and periods compared, what still differs and why, the next stage.
	Evidence *StageEvidence `json:"evidence,omitempty"`
	// Measure is what the stage took, turn by turn (AddTurn).
	Measure *StageMeasure `json:"measure,omitempty"`
}

// StageEvidence ends a stage (update_task_plan "done").
type StageEvidence struct {
	Built       string `json:"built"`
	Compared    string `json:"compared,omitempty"` // the server's count from the workbook map
	Differences string `json:"differences,omitempty"`
	Next        string `json:"next,omitempty"`
}

// StageMeasure is what a stage took: the developer's minutes waiting on the
// AI, its model calls and tokens, the tools it ran, and the developer's
// corrections (messages other than confirming and continuing).
type StageMeasure struct {
	Turns        int     `json:"turns"`
	Minutes      float64 `json:"minutes"`
	Calls        int     `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	ToolCalls    int     `json:"tool_calls"`
	Corrections  int     `json:"corrections"`
}

// StageSheets are the workbook sheets the stage builds.
func (s TaskStage) StageSheets() []string {
	if len(s.Sheets) > 0 {
		return s.Sheets
	}
	if s.Sheet != "" {
		return []string{s.Sheet}
	}
	return nil
}

// TurnMeasure is one turn's share of a stage's measure.
type TurnMeasure struct {
	Minutes                                 float64
	Calls, ToolCalls                        int
	InputTokens, CachedTokens, OutputTokens int64
	Correction                              bool
}

// AddTurn adds a turn to stage i (0-based) of the plan; out of range is
// ignored.
func (p *TaskPlan) AddTurn(i int, t TurnMeasure) {
	if p == nil || i < 0 || i >= len(p.Stages) {
		return
	}
	st := &p.Stages[i]
	if st.Measure == nil {
		st.Measure = &StageMeasure{}
	}
	m := st.Measure
	m.Turns++
	m.Minutes += t.Minutes
	m.Calls += t.Calls
	m.ToolCalls += t.ToolCalls
	m.InputTokens += t.InputTokens
	m.CachedTokens += t.CachedTokens
	m.OutputTokens += t.OutputTokens
	if t.Correction {
		m.Corrections++
	}
}

// String is the measure in one line.
func (m *StageMeasure) String() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%.0f min, %d turns, %d calls, %s tokens, %d tool calls, %d corrections",
		m.Minutes, m.Turns, m.Calls, shortCount(m.InputTokens+m.CachedTokens+m.OutputTokens), m.ToolCalls, m.Corrections)
}

func shortCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// UnmarshalJSON takes "items" as a list or, as models send it at times, one
// string (2026-11-12: plan_task failed on "items": "Time dimension …").
func (s *TaskStage) UnmarshalJSON(b []byte) error {
	type plain TaskStage
	var raw struct {
		plain
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = TaskStage(raw.plain)
	s.Items = nil
	if len(raw.Items) == 0 || string(raw.Items) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw.Items, &s.Items); err == nil {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw.Items, &one); err != nil {
		return fmt.Errorf("items: a list of strings")
	}
	for _, part := range strings.Split(one, ";") {
		if part = strings.TrimSpace(part); part != "" {
			s.Items = append(s.Items, part)
		}
	}
	return nil
}

// Stage statuses.
const (
	StagePending    = "pending"
	StageInProgress = "in_progress"
	StageDone       = "done"
	StageSkipped    = "skipped"
)

// Current is the first stage not done or skipped (-1 when none is left).
func (p *TaskPlan) Current() int {
	if p == nil {
		return -1
	}
	for i, s := range p.Stages {
		if s.Status != StageDone && s.Status != StageSkipped {
			return i
		}
	}
	return -1
}

// Render is the plan as the AI and the developer read it.
func (p *TaskPlan) Render() string {
	if p == nil || (len(p.Stages) == 0 && len(p.Decisions) == 0 && p.Target == nil) {
		return ""
	}
	return p.Target.RenderFor(p.currentSheets()...) + p.renderStages() + p.renderDecisions()
}

// renderDecisions is the decisions part of Render.
func (p *TaskPlan) renderDecisions() string {
	if len(p.Decisions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Decisions the developer made (in this or an earlier session) — they stand; never ask them again:\n")
	for _, d := range p.Decisions {
		b.WriteString("- " + d + "\n")
	}
	return b.String()
}

// renderStages is the stages part of Render.
func (p *TaskPlan) renderStages() string {
	if len(p.Stages) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", p.Goal)
	if p.CarriedFrom != "" {
		b.WriteString("(Carried over from an earlier session in this draft: go on from the CURRENT stage.)\n")
	}
	cur := p.Current()
	// A long plan shows its counts, the stages around the current one and
	// the current stage whole: the 80-stage plan of 2026-11-12 was 50k
	// characters, re-sent with every call.
	compact := len(p.Stages) > compactStagesAbove
	if compact {
		done, skipped := 0, 0
		for _, s := range p.Stages {
			switch s.Status {
			case StageDone:
				done++
			case StageSkipped:
				skipped++
			}
		}
		fmt.Fprintf(&b, "%d stages: %d done, %d skipped (titles of the others below; their items are shown when each is CURRENT).\n", len(p.Stages), done, skipped)
	}
	for i, s := range p.Stages {
		if compact && i != cur {
			if cur >= 0 && (i < cur-2 || i > cur+6) {
				continue
			}
			fmt.Fprintf(&b, "%d. [%s] %s", i+1, map[string]string{StageDone: "done", StageSkipped: "skipped", StageInProgress: "in progress", StagePending: "to do", "": "to do"}[s.Status], s.Title)
			if s.Status == StageDone && s.Evidence != nil && s.Evidence.Compared != "" {
				fmt.Fprintf(&b, " — compared: %s", s.Evidence.Compared)
			}
			b.WriteByte('\n')
			continue
		}
		mark := map[string]string{StageDone: "done", StageSkipped: "skipped", StageInProgress: "in progress"}[s.Status]
		if mark == "" {
			mark = "to do"
		}
		if i == cur {
			mark += ", CURRENT"
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, mark, s.Title)
		if len(s.Items) > 0 {
			fmt.Fprintf(&b, " — items: %s", strings.Join(s.Items, "; "))
		}
		if open := s.OpenItems(); i == cur && len(s.Covered) > 0 && len(open) > 0 {
			fmt.Fprintf(&b, " — done by confirmed plans: %s; still open: %s", strings.Join(s.Covered, "; "), strings.Join(open, "; "))
		}
		if s.Check != "" {
			fmt.Fprintf(&b, " — check: %s", s.Check)
		}
		if s.Note != "" {
			fmt.Fprintf(&b, " — note: %s", s.Note)
		}
		if s.Confirmed > 0 {
			fmt.Fprintf(&b, " — confirmed: %d plan(s), %d step(s)", s.Confirmed, s.ConfirmedSteps)
		}
		if e := s.Evidence; e != nil {
			fmt.Fprintf(&b, " — built: %s", e.Built)
			if e.Compared != "" {
				fmt.Fprintf(&b, "; compared: %s", e.Compared)
			}
			if e.Differences != "" {
				fmt.Fprintf(&b, "; differences: %s", e.Differences)
			}
		}
		if s.Measure != nil && (s.Status == StageDone || i == cur) {
			fmt.Fprintf(&b, " — took %s", s.Measure.String())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TaskPlan is sessionID's plan (nil when it has none).
func (s *ChatStore) TaskPlan(ctx context.Context, sessionID string) (*TaskPlan, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT task_plan FROM ai_assistant.session WHERE id = $1::uuid`, sessionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p TaskPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil
	}
	return &p, nil
}

// SetTaskPlan replaces sessionID's plan (nil clears it).
func (s *ChatStore) SetTaskPlan(ctx context.Context, sessionID string, p *TaskPlan) error {
	var raw any
	if p != nil {
		p.UpdatedAt = time.Now().UTC()
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.pool.Exec(ctx, `UPDATE ai_assistant.session SET task_plan = $2::jsonb WHERE id = $1::uuid`, sessionID, raw)
	return err
}

// RecordConfirmed notes a confirmed plan of steps steps on the current
// stage, which is then in progress. Stage 1 stayed "in progress" after three
// stages were confirmed (2026-11-08): the AI marks a stage done, but the
// server knows a plan for it was confirmed. It reports whether the plan
// changed.
//
// A plan the coverage check found doing every item of the stage
// (MarkCovered), confirmed with every step run, finishes it: stages were
// left "pending" for 29 turns and ~210 executed steps because only the AI
// marked them (2026-11-11), and the next session redid finished work.
func (p *TaskPlan) RecordConfirmed(proposalID string, steps int, whole bool) bool {
	cur := p.Current()
	if cur < 0 {
		return false
	}
	st := &p.Stages[cur]
	if st.Status == StagePending {
		st.Status = StageInProgress
	}
	st.Confirmed++
	st.ConfirmedSteps += steps
	if whole && proposalID != "" && st.PendingCover != nil && st.PendingCover.Proposal == proposalID {
		for _, it := range st.PendingCover.Items {
			if !containsItem(st.Covered, it) {
				st.Covered = append(st.Covered, it)
			}
		}
		st.PendingCover = nil
	}
	if whole && proposalID != "" && st.CoveredBy == proposalID {
		for _, it := range st.Items {
			if !containsItem(st.Covered, it) {
				st.Covered = append(st.Covered, it)
			}
		}
	}
	if len(st.Items) > 0 && len(st.OpenItems()) == 0 {
		if st.NeedsProof() {
			// Its steps are all confirmed; its numbers decide (update_task_plan
			// "done" is checked against the workbook map).
			st.Note = "every item has a confirmed step; the values are not proven yet — compare the stage's sheets, then mark it done"
		} else {
			st.Status = StageDone
			st.Note = "done by the server: confirmed plans did every item"
		}
	}
	return true
}

// MarkFound notes on the stages which items the model already holds — the
// carried-plan check's answer, by 0-based stage. It finishes no stage: one
// reading by the session's model found "every item" of stages with nothing
// built — an eleven-sheet calculation stage, then Headcount, SaaS, Sites
// Logistics, Hardware, Operating Expenses and Checks (2026-11-12). The AI
// marks a stage done (update_task_plan, checked against the workbook map
// for a sheet's stage); confirmed plans finish theirs (RecordConfirmed).
func (p *TaskPlan) MarkFound(found map[int][]string) bool {
	changed := false
	for i, items := range found {
		if i < 0 || i >= len(p.Stages) || len(items) == 0 {
			continue
		}
		st := &p.Stages[i]
		if st.Status == StageDone || st.Status == StageSkipped {
			continue
		}
		st.Note = "the carried-plan check read these as already in the model (unverified — check, then mark the stage done): " + strings.Join(items, "; ")
		changed = true
	}
	return changed
}

// Append adds stages after the plan's own, for a new goal given while a
// carried plan is unfinished: the carried stages and decisions stay (owner,
// 2026-11-11: a new session must read the earlier sessions' plan, never
// lose it).
//
// A stage the plan already has (same title, numbering aside) is not added
// again: a new session re-sent the carried plan's five stages and the plan
// held each twice (2026-11-12). It returns the titles left out.
func (p *TaskPlan) Append(goal string, stages []TaskStage) []string {
	if g := strings.TrimSpace(goal); g != "" && !strings.Contains(p.Goal, g) {
		p.Goal = strings.TrimSpace(p.Goal + "; then: " + g)
	}
	var dup []string
	for _, st := range stages {
		if p.hasStage(st.Title) {
			dup = append(dup, st.Title)
			continue
		}
		st.Status, st.Note, st.CoveredBy, st.Covered, st.PendingCover, st.ProofAsked = StagePending, "", "", nil, nil, false
		p.Stages = append(p.Stages, st)
	}
	return dup
}

// Replace re-cuts the plan's open stages: done and skipped stages stay as
// they are (what was built stays recorded), the open ones give way to
// stages — the developer asked for a stage per sheet and the AI could not
// change the plan (2026-11-12). The goal and decisions stay.
func (p *TaskPlan) Replace(goal string, stages []TaskStage) {
	var kept []TaskStage
	for _, st := range p.Stages {
		if st.Status == StageDone || st.Status == StageSkipped {
			kept = append(kept, st)
		}
	}
	p.Stages = kept
	if g := strings.TrimSpace(goal); g != "" && p.Goal == "" {
		p.Goal = g
	}
	for _, st := range stages {
		if p.hasStage(st.Title) {
			continue
		}
		st.Status, st.Note, st.CoveredBy, st.Covered, st.PendingCover, st.ProofAsked = StagePending, "", "", nil, nil, false
		p.Stages = append(p.Stages, st)
	}
}

func (p *TaskPlan) hasStage(title string) bool {
	k := stageTitleKey(title)
	for _, s := range p.Stages {
		if stageTitleKey(s.Title) == k {
			return true
		}
	}
	return false
}

// Finished reports whether every stage is done or skipped.
func (p *TaskPlan) Finished() bool { return p != nil && len(p.Stages) > 0 && p.Current() < 0 }

// CarryTaskPlan gives sessionID, new and without a plan, the plan of the
// developer's latest earlier session in the model that has one, when that
// plan is unfinished and the session worked in the same draft (draftID) or
// was promoted — its draft became the developer's working revision. The
// Creator & Agency build ran thirteen sessions (200 calls each) and every
// new one started without its plan (2026-11-08). It returns the plan
// carried, or nil.
func (s *ChatStore) CarryTaskPlan(ctx context.Context, sessionID, modelID, userID, draftID string) (*TaskPlan, error) {
	var from string
	var raw []byte
	var sameDraft, promoted bool
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, task_plan, COALESCE(draft_revision_id::text, '') = $4 AND $4 <> '', promoted_at IS NOT NULL
		FROM ai_assistant.session
		WHERE model_id = $1::uuid AND user_id = $2::uuid AND id <> $3::uuid AND task_plan IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, modelID, userID, sessionID, draftID).Scan(&from, &raw, &sameDraft, &promoted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p TaskPlan
	if json.Unmarshal(raw, &p) != nil || (!sameDraft && !promoted) {
		return nil, nil
	}
	if p.Finished() || len(p.Stages) == 0 {
		// A finished goal is not carried; its decisions and target are.
		if len(p.Decisions) == 0 && p.Target == nil {
			return nil, nil
		}
		p = TaskPlan{Decisions: p.Decisions, Target: p.Target}
	}
	p.CarriedFrom = from
	p.CarriedChecked = false
	if err := s.SetTaskPlan(ctx, sessionID, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// NewTaskPlan checks a plan the AI wrote and gives each stage its status.
func NewTaskPlan(goal string, stages []TaskStage) (*TaskPlan, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, fmt.Errorf("plan_task needs the goal, in the developer's words")
	}
	if len(stages) < 2 {
		return nil, fmt.Errorf("plan_task needs two stages or more: a goal one plan can do needs no task plan — propose it")
	}
	if err := CheckStages(stages); err != nil {
		return nil, err
	}
	return &TaskPlan{Goal: goal, Stages: stages}, nil
}

// CheckStages checks stages the AI wrote — for a new plan, or to add to or
// re-cut a plan under way, where one stage is enough.
func CheckStages(stages []TaskStage) error {
	if len(stages) == 0 {
		return fmt.Errorf("plan_task needs at least one stage")
	}
	// 80: a workbook build cut one stage per sheet and per page ran to 75
	// (2026-11-12); a stage is a line of the plan, not a cost.
	if len(stages) > 80 {
		return fmt.Errorf("plan_task: %d stages; group them into 80 or fewer", len(stages))
	}
	for i := range stages {
		st := &stages[i]
		st.Title = strings.TrimSpace(st.Title)
		if st.Title == "" || len(st.Items) == 0 {
			return fmt.Errorf("plan_task: stage %d needs a title and the items it delivers (metrics, grids, pages, postings — named)", i+1)
		}
		st.Status, st.Note = StagePending, ""
	}
	return nil
}

// UpdateStage marks stage (1-based) with status and an optional note.
func (p *TaskPlan) UpdateStage(stage int, status, note string) error {
	if stage < 1 || stage > len(p.Stages) {
		return fmt.Errorf("update_task_plan: stage %d — the plan has %d", stage, len(p.Stages))
	}
	switch status {
	case StagePending, StageInProgress, StageDone, StageSkipped:
	default:
		return fmt.Errorf("update_task_plan: status is pending, in_progress, done or skipped")
	}
	if status == StageSkipped && strings.TrimSpace(note) == "" {
		return fmt.Errorf("update_task_plan: a skipped stage needs a note saying why")
	}
	// A finished stage stays finished: Stage 1, built, turned "skipped"
	// when a re-sent plan replaced it (2026-11-12).
	if p.Stages[stage-1].Status == StageDone && status == StageSkipped {
		return fmt.Errorf("update_task_plan: stage %d is done — what it built stays; a done stage is not skipped", stage)
	}
	p.Stages[stage-1].Status = status
	if note = strings.TrimSpace(note); note != "" {
		p.Stages[stage-1].Note = note
	}
	return nil
}

// compactStagesAbove: a plan of more stages renders compactly (renderStages).
const compactStagesAbove = 12

// currentSheets are the sheets the current stage builds (none when it
// builds none).
func (p *TaskPlan) currentSheets() []string {
	if cur := p.Current(); cur >= 0 {
		return p.Stages[cur].StageSheets()
	}
	return nil
}
