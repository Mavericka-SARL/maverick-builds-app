package gateway

// The AI Developer's memory of a session (aiassistant/memory.go; owner,
// 2026-11-13):
//   - an attachment is gone through once, when it arrives, and its analysis
//     kept; later turns get the analysis, not the file, and a read tool
//     asked for a part already read answers from memory — the file is read
//     again only when the developer asks for it directly;
//   - the developer works in one session as long as they like: when the
//     memory fills up it is compacted (the earlier conversation becomes a
//     summary) and the developer is told how full it is;
//   - a plan from another session can be attached to a session, with the
//     attachments it rests on and their analyses — never carried
//     automatically.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// attachmentAnalysisPrompt asks for the one analysis of a file that is not a
// workbook (a workbook's is the server's, analyse_workbook).
const attachmentAnalysisPrompt = `You are an AI developer that builds planning models (dimensions, metrics, grids,
dashboards, tables of records, workflows, roles) for a developer. They attached the file below to your session. Go
through it once, now: from here on you work from your analysis, not the file — it is read again only if the developer
asks. Write the analysis you will work from:
- What the file is and what it is for.
- Its parts (sections, tables, lists) and what each holds.
- Every figure, rate, rule, formula, list of items, name and period the work may need — exact, with where it is.
- What it means for the model: the dimensions, metrics, inputs, calculations, reports, records, workflows and roles it
  implies.
- What is unclear, contradictory or missing.
Plain text, short bullets, names and numbers exact; at most 2,500 words.`

// maxServerAnalysisRunes bounds a workbook's analysis kept in memory.
const maxServerAnalysisRunes = 60000

// isWorkbookName reports whether a file is a workbook the server reads.
func isWorkbookName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".xlsx" || ext == ".xlsm"
}

// latestDocs keeps, of docs, the latest upload of each file name.
func latestDocs(docs []aiassistant.Document) []aiassistant.Document {
	idx := map[string]int{}
	var out []aiassistant.Document
	for _, d := range docs {
		k := strings.ToLower(d.Filename)
		if i, ok := idx[k]; ok {
			out[i] = d
			continue
		}
		idx[k] = len(out)
		out = append(out, d)
	}
	return out
}

// rereadFiles are the attachments the developer's message asks to read
// again: those it names (by file name, without its extension), else the
// workbooks when it speaks of a workbook or sheet, else every one. nil when
// it asks for no re-reading.
func rereadFiles(msg string, docs []aiassistant.Document) map[string]bool {
	if !aiassistant.AsksToReread(msg) {
		return nil
	}
	norm := func(s string) string {
		return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return r == '-' || r == '_' || r == ' ' || r == '.' || r == '(' || r == ')'
		}), " ")
	}
	text := norm(msg)
	out := map[string]bool{}
	for _, d := range latestDocs(docs) {
		if base := norm(strings.TrimSuffix(d.Filename, filepath.Ext(d.Filename))); base != "" && strings.Contains(text, base) {
			out[strings.ToLower(d.Filename)] = true
		}
	}
	if len(out) == 0 && workbookWordRE.MatchString(msg) {
		for _, d := range latestDocs(docs) {
			if isWorkbookName(d.Filename) {
				out[strings.ToLower(d.Filename)] = true
			}
		}
	}
	if len(out) == 0 {
		for _, d := range latestDocs(docs) {
			out[strings.ToLower(d.Filename)] = true
		}
	}
	return out
}

var workbookWordRE = regexp.MustCompile(`(?i)\b(workbook|spreadsheet|sheet|excel|xlsx)|книг|таблиц|лист|эксел|excel`)

// analyseAttachments goes through every attachment the memory holds no
// analysis of (and those in reread again), once, and keeps the analysis: a
// workbook's is the server's reading of its sheets, a CSV's its overview,
// any other file's a call of the session's model. It returns the files
// analysed now and those of them read again.
func (h *handler) analyseAttachments(ctx context.Context, store *aiassistant.ChatStore, p providers.Provider, model, sessionID string,
	docs []aiassistant.Document, reread map[string]bool, status func(string)) (analysed, again []string) {
	mem, _ := store.Memory(ctx, sessionID)
	for _, d := range latestDocs(docs) {
		redo := reread[strings.ToLower(d.Filename)]
		if a := mem.Attachment(d.Filename); a != nil && a.DocumentID == d.ID && !redo {
			continue
		}
		if redo {
			status(fmt.Sprintf("reading %s again, as you asked", d.Filename))
		} else {
			status(fmt.Sprintf("going through %s — once; the analysis is kept in memory", d.Filename))
		}
		var analysis, by string
		switch {
		case d.Importable && isWorkbookName(d.Filename):
			text, err := h.aiAnalyseWorkbook(ctx, sessionID, d.ID, nil)
			if err != nil {
				log.Printf("AI memory: session=%s analyse %s: %v", sessionID, d.Filename, err)
				continue
			}
			analysis, by = clipAnalysis(text, maxServerAnalysisRunes), "server"
		case d.Importable && strings.EqualFold(filepath.Ext(d.Filename), ".csv"):
			analysis, by = "CSV file kept whole: importable with preview_file_import / import_file_data; its rows readable with read_attached_sheet. Overview:\n"+
				aiassistant.ClipCSVOverview(d.Content), "server"
		default:
			if p == nil {
				continue
			}
			content := d.Content
			if len([]rune(content)) > 150000 {
				content = string([]rune(content)[:150000]) + "\n(… the rest of the file was not read)"
			}
			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			resp, err := p.Chat(cctx, providers.ChatRequest{Model: model, SystemPrompt: attachmentAnalysisPrompt,
				Messages: []providers.Message{{Role: "user", Content: "File: " + d.Filename + "\n\n" + content}}})
			cancel()
			if err != nil || strings.TrimSpace(resp.Message.Content) == "" {
				log.Printf("AI memory: session=%s analyse %s: %v", sessionID, d.Filename, err)
				continue
			}
			if u := resp.Usage; u.InputTokens+u.CacheReadTokens > 0 {
				_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, u)
			}
			analysis, by = aiassistant.ClipAnalysis(resp.Message.Content), "ai"
		}
		if _, err := store.UpdateMemory(ctx, sessionID, func(m *aiassistant.SessionMemory) error {
			m.SetAttachment(d.Filename, d.ID, analysis, by)
			return nil
		}); err == nil {
			if redo {
				again = append(again, d.Filename)
			} else {
				analysed = append(analysed, d.Filename)
			}
		}
	}
	return analysed, again
}

func clipAnalysis(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "\n(… the rest of the analysis was cut; read_attached_sheet reads any sheet)"
}

// documentsSection is the prompt's part on the attached files: the files
// analysed are named (their analysis is in the memory); a file not
// analysed yet, or one the developer asked to read again, gives its text.
func documentsSection(docs []aiassistant.Document, mem *aiassistant.SessionMemory, plan *aiassistant.TaskPlan, workbooks map[string][]byte, reread map[string]bool) string {
	docs = latestDocs(docs)
	if len(docs) == 0 {
		return ""
	}
	if plan != nil && plan.Target != nil && len(reread) == 0 {
		// The target model holds what the workbook says; a sheet is read
		// when it is needed.
		return buildDocumentIndex(docs, workbooks)
	}
	var known, unread []aiassistant.Document
	for _, d := range docs {
		if a := mem.Attachment(d.Filename); a != nil && a.DocumentID == d.ID && !reread[strings.ToLower(d.Filename)] {
			known = append(known, d)
		} else {
			unread = append(unread, d)
		}
	}
	var b strings.Builder
	if len(known) > 0 {
		b.WriteString("\n\n## Attached files (analysed — the analysis is in your memory)\n")
		for _, d := range known {
			fmt.Fprintf(&b, "- %s", d.Filename)
			if d.Importable {
				b.WriteString(" — kept whole: read_attached_sheet reads a sheet's rows and formulas (a part read before answers from memory); preview_file_import / import_file_data import it")
			}
			b.WriteString("\n")
		}
	}
	if len(unread) > 0 {
		if len(reread) > 0 {
			b.WriteString("\n\nThe developer asked for these attachments to be read again: their text is below once more, and read tools go back to them this turn.")
		}
		b.WriteString(buildDocumentContext(unread))
	}
	return b.String()
}

// memoryReads makes the read tools that read an attachment answer from the
// session's memory when they read that part before, and keep what they
// read. A file in reread (the developer asked to read it again) is read
// from the file.
func (h *handler) memoryReads(rh aiassistant.ReadHooks, store *aiassistant.ChatStore, sessionID string, rereadSet map[string]bool) aiassistant.ReadHooks {
	docs := aiassistant.NewDocumentStore(store.Pool())
	if read := rh.ReadAttachedSheet; read != nil {
		rh.ReadAttachedSheet = func(ctx context.Context, file, sheet string, fromRow, toRow int) (string, error) {
			doc, _, err := docs.GetRaw(ctx, sessionID, strings.TrimSpace(file))
			if err != nil {
				return read(ctx, file, sheet, fromRow, toRow)
			}
			key := fmt.Sprintf("read_attached_sheet|%s|%s|%d|%d", doc.ID, strings.ToLower(strings.TrimSpace(sheet)), fromRow, toRow)
			if !rereadSet[strings.ToLower(doc.Filename)] {
				if m, _ := store.Memory(ctx, sessionID); m != nil {
					if r := m.FindRead("read_attached_sheet", key, doc.ID, strings.TrimSpace(sheet), fromRow, toRow); r != nil {
						return fromMemory(*r) + r.Result, nil
					}
				}
			}
			out, err := read(ctx, file, sheet, fromRow, toRow)
			if err == nil {
				_, _ = store.UpdateMemory(ctx, sessionID, func(m *aiassistant.SessionMemory) error {
					m.PutRead(aiassistant.MemoryRead{Tool: "read_attached_sheet", Key: key, File: doc.Filename, Sheet: strings.TrimSpace(sheet),
						FromRow: fromRow, ToRow: toRow, Result: out})
					return nil
				})
			}
			return out, err
		}
	}
	if analyse := rh.AnalyseWorkbook; analyse != nil {
		rh.AnalyseWorkbook = func(ctx context.Context, file string, sheets []string) (string, error) {
			doc, _, err := docs.GetRaw(ctx, sessionID, strings.TrimSpace(file))
			if err != nil {
				return analyse(ctx, file, sheets)
			}
			if !rereadSet[strings.ToLower(doc.Filename)] {
				if m, _ := store.Memory(ctx, sessionID); m != nil {
					if a := m.Attachment(doc.Filename); a != nil && a.DocumentID == doc.ID {
						return fmt.Sprintf("Your memory holds the analysis of %s (made %s), every sheet of it: it is shown in every turn under "+
							"\"Your memory of this session › Attachments\". Work from it — the file is analysed again only when the developer asks.",
							doc.Filename, a.At.Local().Format("2006-01-02 15:04")), nil
					}
				}
			}
			out, err := analyse(ctx, file, sheets)
			if err == nil {
				_, _ = store.UpdateMemory(ctx, sessionID, func(m *aiassistant.SessionMemory) error {
					if len(sheets) == 0 {
						m.SetAttachment(doc.Filename, doc.ID, clipAnalysis(out, maxServerAnalysisRunes), "server")
					} else {
						m.PutRead(aiassistant.MemoryRead{Tool: "analyse_workbook", Key: "analyse_workbook|" + doc.ID + "|" + strings.ToLower(strings.Join(sheets, ",")),
							File: doc.Filename, Sheet: strings.Join(sheets, ", "), Result: out})
					}
					return nil
				})
			}
			return out, err
		}
	}
	return rh
}

// fromMemory heads a read answered from memory.
func fromMemory(r aiassistant.MemoryRead) string {
	return fmt.Sprintf("(From your memory: you read this on %s — the file was not read again.)\n", r.At.Local().Format("2006-01-02 15:04"))
}

// settledAt is when the session's last confirmed plan ran (zero for none):
// tool exchanges before it are settled out of the history.
func (h *handler) settledAt(ctx context.Context, sessionID string) time.Time {
	var settled time.Time
	if props, err := aiassistant.NewProposalStore(h.db.For(ctx)).ListProposals(ctx, sessionID); err == nil {
		for _, p := range props {
			if p.ExecutedAt != nil && p.ExecutedAt.After(settled) {
				settled = *p.ExecutedAt
			}
		}
	}
	return settled
}

// sentHistory is the conversation as a turn sends it: after the memory's
// summary, settled, its long tool results clipped.
func (h *handler) sentHistory(ctx context.Context, sessionID string, history []aiassistant.ChatMessage, mem *aiassistant.SessionMemory) []aiassistant.ChatMessage {
	return aiassistant.CompactHistory(aiassistant.SettleHistory(mem.SinceSummary(history), h.settledAt(ctx, sessionID)))
}

// memoryStatus is how full the session's memory is.
func (h *handler) memoryStatus(ctx context.Context, store *aiassistant.ChatStore, sessionID string) aiassistant.MemoryStatus {
	mem, _ := store.Memory(ctx, sessionID)
	earlier, _ := store.EarlierPlans(ctx, sessionID)
	history, _ := store.ListMessages(ctx, sessionID)
	return mem.Status(aiassistant.ConversationTokens(h.sentHistory(ctx, sessionID, history, mem)), earlier)
}

// openSheets are the sheets the plan's open stages still need.
func openSheets(plan *aiassistant.TaskPlan) map[string]bool {
	out := map[string]bool{}
	if plan == nil {
		return out
	}
	for _, st := range plan.Stages {
		if st.Status == aiassistant.StageDone || st.Status == aiassistant.StageSkipped {
			continue
		}
		for _, s := range st.StageSheets() {
			out[strings.ToLower(s)] = true
		}
	}
	return out
}

// compactMemory compacts the session's memory when it is full (or when
// force, the developer asked): the earlier conversation becomes a summary,
// and reads of sheets no open stage needs are shortened. It returns the
// message that tells the developer ("" when nothing was compacted).
func (h *handler) compactMemory(ctx context.Context, store *aiassistant.ChatStore, p providers.Provider, model, sessionID string, plan *aiassistant.TaskPlan, force bool) (string, error) {
	before := h.memoryStatus(ctx, store, sessionID)
	if !force && before.Percent < aiassistant.CompactAtPercent {
		return "", nil
	}
	mem, _ := store.Memory(ctx, sessionID)
	history, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		return "", err
	}
	since := mem.SinceSummary(history)
	split := aiassistant.CompactSplit(since)
	var summary string
	var upTo time.Time
	if split > 0 {
		if p == nil {
			return "", fmt.Errorf("no AI provider to compact the memory with")
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		resp, err := p.Chat(cctx, providers.ChatRequest{Model: model, SystemPrompt: aiassistant.CompactMemoryPrompt,
			Messages: []providers.Message{{Role: "user", Content: aiassistant.CompactTranscript(mem.Summary, since[:split])}}})
		cancel()
		if err != nil {
			return "", fmt.Errorf("compact the memory: %w", err)
		}
		if u := resp.Usage; u.InputTokens+u.CacheReadTokens > 0 {
			_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, u)
		}
		summary = strings.TrimSpace(resp.Message.Content)
		if summary == "" {
			return "", fmt.Errorf("compact the memory: the model wrote no summary")
		}
		upTo = since[split-1].CreatedAt
	}
	shortened := 0
	if _, err := store.UpdateMemory(ctx, sessionID, func(m *aiassistant.SessionMemory) error {
		if summary != "" {
			m.Summary, m.SummaryUpTo = summary, &upTo
		}
		shortened = m.CompactReads(openSheets(plan))
		if summary != "" || shortened > 0 {
			now := time.Now().UTC()
			m.Compactions++
			m.CompactedAt = &now
		}
		return nil
	}); err != nil {
		return "", err
	}
	if summary == "" && shortened == 0 {
		if force {
			return fmt.Sprintf("🧠 Memory is %d%% full; there is nothing to compact yet — the conversation is short and every part of the attachments I read is still needed.", before.Percent), nil
		}
		return "", nil
	}
	after := h.memoryStatus(ctx, store, sessionID)
	var b strings.Builder
	if force {
		fmt.Fprintf(&b, "🧠 Memory compacted, as you asked: it was %d%% full", before.Percent)
	} else {
		fmt.Fprintf(&b, "🧠 My memory of this session was %d%% full, so I compacted it", before.Percent)
	}
	b.WriteString(".")
	if summary != "" {
		fmt.Fprintf(&b, " The conversation up to %s is now a summary I keep.", upTo.Local().Format("2 Jan 15:04"))
	}
	if shortened > 0 {
		fmt.Fprintf(&b, " %d sheet reads that no open stage needs were shortened.", shortened)
	}
	b.WriteString(" The plan, its decisions, the attachments' analyses and my notes stay whole. ")
	fmt.Fprintf(&b, "Memory is now %d%% full — we go on in this session.", after.Percent)
	if after.Percent >= aiassistant.CompactAtPercent {
		fmt.Fprintf(&b, " It is still nearly full: the attachments' analyses take %d tokens and my notes %d — remove an attachment "+
			"you no longer need, or ask me to shorten my notes.", after.AttachmentTokens, after.NotesTokens)
	}
	return b.String(), nil
}

// ── GET /api/ai/sessions/{id}/memory, POST …/memory/compact ─────────────────

func (h *handler) aiSessionMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	sessionID, rest, _ := strings.Cut(path, "/")
	store := h.aiChatStore(ctx)
	sess, err := store.GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	switch {
	case rest == "memory" && r.Method == http.MethodGet:
		jsonOK(w, h.memoryStatus(ctx, store, sessionID))
	case rest == "memory/compact" && r.Method == http.MethodPost:
		extendAIWriteDeadline(w)
		p, _, model, err := h.buildProviderForRequest(r, a)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		plan, _ := store.TaskPlan(ctx, sessionID)
		note, err := h.compactMemory(ctx, store, p, model, sessionID, plan, true)
		if err != nil {
			jsonErr(w, err, http.StatusBadGateway)
			return
		}
		if note != "" {
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", note, nil, "", "")
		}
		msgs, _ := store.ListMessages(ctx, sessionID)
		jsonOK(w, map[string]any{"memory": h.memoryStatus(ctx, store, sessionID), "messages": msgs, "note": note})
	default:
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
	}
}

// ── GET /api/ai/plans ───────────────────────────────────────────────────────
//
// The plans of the developer's sessions in the model — current and earlier
// ones — that can be attached to another session (?exclude= that session).

func (h *handler) aiPlanRefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	refs, err := h.aiChatStore(ctx).ListPlanRefs(ctx, modelID, a.UserID, r.URL.Query().Get("exclude"))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if refs == nil {
		refs = []aiassistant.PlanRef{}
	}
	jsonOK(w, refs)
}

// ── POST /api/ai/sessions/{id}/attach-plan ──────────────────────────────────
//
// {session_id, index, attachments}: the developer attaches a plan of another
// session of theirs to this one (owner, 2026-11-13: possible, never a must).
// The plan under way here, if any, is set aside in this session. With
// attachments the files of that session come along with their analyses and
// the parts read, so nothing is gone through again. The first turn after it
// checks which open stages the model already holds (checkCarriedPlan).

func (h *handler) aiAttachPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/attach-plan")
	var body struct {
		SessionID   string `json:"session_id"`
		Index       *int   `json:"index"`
		Attachments bool   `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
		jsonErr(w, fmt.Errorf("session_id is required"), http.StatusBadRequest)
		return
	}
	index := -1
	if body.Index != nil {
		index = *body.Index
	}
	store := h.aiChatStore(ctx)
	sess, err := store.GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	src, err := store.GetSession(ctx, body.SessionID)
	if err != nil || src.UserID != a.UserID || src.ModelID != sess.ModelID || src.ID == sess.ID {
		jsonErr(w, fmt.Errorf("the plan's session is not one of yours in this model"), http.StatusNotFound)
		return
	}
	plan, err := store.PlanOf(ctx, src.ID, index)
	if err != nil || plan == nil {
		jsonErr(w, fmt.Errorf("plan not found"), http.StatusNotFound)
		return
	}
	attached := *plan
	attached.CarriedFrom, attached.CarriedChecked = src.ID, false
	// The developer chose it: approved as attached.
	if attached.PendingApproval {
		attached.Approve()
	}
	cur, _ := store.TaskPlan(ctx, sessionID)
	if cur != nil && len(cur.Stages) > 0 {
		if err := store.PutPlanAside(ctx, sessionID, cur); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for _, d := range cur.Decisions {
			attached.AddDecision(d)
		}
	}
	if err := store.SetTaskPlan(ctx, sessionID, &attached); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	var copied, analysedCopies []string
	if body.Attachments {
		docStore := aiassistant.NewDocumentStore(h.db.For(ctx))
		if copied, err = docStore.CopyDocuments(ctx, src.ID, sessionID); err != nil {
			jsonErr(w, fmt.Errorf("copy the attachments: %w", err), http.StatusInternalServerError)
			return
		}
		// Their analyses and the parts read come along: the copies are new
		// documents, so the memory is told their ids.
		if len(copied) > 0 {
			srcMem, _ := store.Memory(ctx, src.ID)
			docs, _ := docStore.ListDocuments(ctx, sessionID)
			ids := map[string]string{}
			for _, d := range latestDocs(docs) {
				ids[strings.ToLower(d.Filename)] = d.ID
			}
			srcDocs, _ := docStore.ListDocuments(ctx, src.ID)
			srcIDs := map[string]string{}
			for _, d := range latestDocs(srcDocs) {
				srcIDs[d.ID] = strings.ToLower(d.Filename)
			}
			isCopied := map[string]bool{}
			for _, n := range copied {
				isCopied[strings.ToLower(n)] = true
			}
			_, _ = store.UpdateMemory(ctx, sessionID, func(m *aiassistant.SessionMemory) error {
				for _, at := range srcMem.Attachments {
					if k := strings.ToLower(at.File); isCopied[k] {
						m.SetAttachment(at.File, ids[k], at.Analysis, at.By)
						analysedCopies = append(analysedCopies, at.File)
					}
				}
				for _, rd := range srcMem.Reads {
					k := strings.ToLower(rd.File)
					if !isCopied[k] {
						continue
					}
					rd.Key = strings.Replace(rd.Key, srcIDOf(srcIDs, k), ids[k], 1)
					m.PutRead(rd)
				}
				return nil
			})
		}
	}
	done := 0
	for _, st := range attached.Stages {
		if st.Status == aiassistant.StageDone || st.Status == aiassistant.StageSkipped {
			done++
		}
	}
	title := src.Title
	if title == "" {
		title = src.CreatedAt.Local().Format("2 Jan 15:04")
	}
	note := fmt.Sprintf("📋 You attached the plan “%s” from the session “%s” (%d of %d stages done).", attached.Goal, title, done, len(attached.Stages))
	if cur != nil && len(cur.Stages) > 0 {
		note += fmt.Sprintf(" The plan that was under way here (“%s”) is set aside in this session.", cur.Goal)
	}
	if len(analysedCopies) > 0 {
		note += " Its attachments came along with their analysis, so I won't go through them again: " + strings.Join(analysedCopies, ", ") + "."
	}
	if rest := without(copied, analysedCopies); len(rest) > 0 {
		note += " Its attachments came along; that session kept no analysis of them, so I go through them once on your next message: " + strings.Join(rest, ", ") + "."
	}
	note += " On your next message I check which of its open stages the model already holds, then go on with it."
	_, _ = store.SaveMessage(ctx, sessionID, "assistant", note, nil, "", "")
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIPlanAttached,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_session", ResourceID: sessionID,
		Metadata: map[string]string{"from_session_id": src.ID, "goal": attached.Goal, "attachments": strings.Join(copied, ", ")},
	})
	msgs, _ := store.ListMessages(ctx, sessionID)
	docs, _ := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocuments(ctx, sessionID)
	if docs == nil {
		docs = []aiassistant.Document{}
	}
	earlier, _ := store.EarlierPlans(ctx, sessionID)
	jsonOK(w, map[string]any{"task_plan": attached, "earlier_plans": earlierOrEmpty(earlier), "messages": msgs, "documents": docs,
		"memory": h.memoryStatus(ctx, store, sessionID)})
}

// without is list less the names in drop (case-insensitive).
func without(list, drop []string) []string {
	var out []string
	for _, s := range list {
		found := false
		for _, d := range drop {
			found = found || strings.EqualFold(s, d)
		}
		if !found {
			out = append(out, s)
		}
	}
	return out
}

// srcIDOf is the id of the source document named k (lower-case) in ids
// (id → name).
func srcIDOf(ids map[string]string, k string) string {
	for id, n := range ids {
		if n == k {
			return id
		}
	}
	return "\x00"
}

func earlierOrEmpty(e []aiassistant.EarlierPlan) []aiassistant.EarlierPlan {
	if e == nil {
		return []aiassistant.EarlierPlan{}
	}
	return e
}

// flatMetricProblems reads a plan's metric names against the revision's
// dimensions and metrics (aiassistant/dimensional_names.go).
func (h *handler) flatMetricProblems(ctx context.Context, revID string, steps []aiassistant.ProposalStep, request string) ([]string, []string, []int) {
	var dims []aiassistant.NamedDimension
	if inv, err := h.loadInventory(ctx, revID); err == nil {
		for _, d := range inv.Dimensions {
			nd := aiassistant.NamedDimension{Name: d.Name, Time: d.Time}
			for _, m := range d.Members {
				nd.Members = append(nd.Members, aiassistant.SnakeName(m.Code))
				if l := aiassistant.SnakeName(m.Label); l != "" && l != aiassistant.SnakeName(m.Code) {
					nd.Members = append(nd.Members, l)
				}
			}
			dims = append(dims, nd)
		}
	}
	var existing []string
	for n := range h.metricNamesOf(ctx, revID) {
		existing = append(existing, n)
	}
	return aiassistant.FlatMetricProblems(steps, request, dims, existing)
}
