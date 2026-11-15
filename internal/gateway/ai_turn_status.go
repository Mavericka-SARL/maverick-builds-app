package gateway

import (
	"strconv"
	"sync"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// turnStatus is what the developer is told while an AI developer turn
// works: the step running now — a call to the AI provider, or a step on the
// server (a tool, the plan check, the memory) — how long it has run, how far
// the provider has got with a call and how long it has been silent, and the
// steps before it. It goes out as a "status" event at every change and every
// statusBeat while a step runs, so a console that hears nothing for longer
// knows the stream is gone. The console used to show the last tool's name
// with a timer of its own — "Calling list_metrics… 11:53" stood for minutes
// of a model reasoning, and a working turn could not be told from a frozen
// one (2026-11-15).
type turnStatus struct {
	mu   sync.Mutex
	send func(event string, payload any)
	now  func() time.Time

	phase         string // "ai", "server", or "" between steps
	label         string // the server step's label
	call, attempt int
	note          string // why the call is asked again
	contextTokens int
	started       time.Time
	stage         string    // the provider's: sending, connected, reasoning, writing
	lastOutput    time.Time // the provider's last output; the idle watchdog's measure
	idleLimit     time.Duration
	maxDur        time.Duration
	recent        []turnStepDone

	stop chan struct{}
	wg   sync.WaitGroup
}

// turnStepDone is a finished step, as the status lists it.
type turnStepDone struct {
	Label string `json:"label"`
	MS    int64  `json:"ms"`
}

// statusBeat is how often a running step's status is sent again (a
// variable for tests).
var statusBeat = 5 * time.Second

const statusRecent = 4

func newTurnStatus(send func(event string, payload any), idleLimit, maxDur time.Duration) *turnStatus {
	return &turnStatus{send: send, now: time.Now, idleLimit: idleLimit, maxDur: maxDur}
}

// beat sends the status every interval while a step runs, until close.
func (t *turnStatus) beat(interval time.Duration) {
	t.stop = make(chan struct{})
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				t.mu.Lock()
				if t.phase != "" {
					t.emitLocked()
				}
				t.mu.Unlock()
			}
		}
	}()
}

// close stops the beat and waits for it: nothing is written to the response
// after the handler returns.
func (t *turnStatus) close() {
	if t.stop != nil {
		close(t.stop)
		t.wg.Wait()
		t.stop = nil
	}
}

// aiCall starts call number call (the session's, as its usage counts calls)
// of the AI provider; attempt 2 is the same call asked again, for note.
func (t *turnStatus) aiCall(call, attempt int, note string, contextTokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishLocked()
	t.phase, t.label = "ai", ""
	t.call, t.attempt, t.note, t.contextTokens = call, attempt, note, contextTokens
	t.started = t.now()
	t.stage = "sending"
	t.lastOutput = time.Time{}
	t.emitLocked()
}

// server starts a step on the server named label.
func (t *turnStatus) server(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishLocked()
	t.phase, t.label = "server", label
	t.started = t.now()
	t.emitLocked()
}

// providerStage records a stage the provider's stream reported.
func (t *turnStatus) providerStage(stage string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != "ai" || t.stage == stage || t.stage == "writing" {
		return
	}
	t.stage = stage
	t.emitLocked()
}

// output records output from the provider: the model writing text or a tool
// call. Only the first one of a call is sent at once; the beat carries the
// rest.
func (t *turnStatus) output() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase != "ai" {
		return
	}
	t.lastOutput = t.now()
	if t.stage != "writing" {
		t.stage = "writing"
		t.emitLocked()
	}
}

// done ends the step running now: the turn is answering.
func (t *turnStatus) done() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishLocked()
}

func (t *turnStatus) finishLocked() {
	if t.phase == "" {
		return
	}
	label := t.label
	if t.phase == "ai" {
		label = "AI call " + strconv.Itoa(t.call)
	}
	t.recent = append(t.recent, turnStepDone{Label: label, MS: t.now().Sub(t.started).Milliseconds()})
	if len(t.recent) > statusRecent {
		t.recent = t.recent[len(t.recent)-statusRecent:]
	}
	t.phase = ""
}

func (t *turnStatus) emitLocked() {
	t.send("status", t.snapshotLocked())
}

func (t *turnStatus) snapshotLocked() map[string]any {
	now := t.now()
	recent := append([]turnStepDone{}, t.recent...)
	s := map[string]any{
		"phase":  t.phase,
		"step_s": int(now.Sub(t.started).Seconds()),
		"recent": recent,
	}
	switch t.phase {
	case "server":
		s["label"] = t.label
	case "ai":
		quietSince := t.started
		if !t.lastOutput.IsZero() {
			quietSince = t.lastOutput
		}
		s["call"] = t.call
		s["attempt"] = t.attempt
		s["stage"] = t.stage
		s["quiet_s"] = int(now.Sub(quietSince).Seconds())
		s["idle_limit_s"] = int(t.idleLimit.Seconds())
		s["max_s"] = int(t.maxDur.Seconds())
		if t.contextTokens > 0 {
			s["context_tokens"] = t.contextTokens
		}
		if t.note != "" {
			s["note"] = t.note
		}
	}
	return s
}

// estimateTokens is about how many tokens req sends: four characters a
// token, over the system prompt, the messages and the tools.
func estimateTokens(req providers.ChatRequest) int {
	n := len(req.SystemPrompt)
	for _, m := range req.Messages {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	for _, td := range req.Tools {
		n += len(td.Name) + len(td.Description) + len(td.Parameters)
	}
	return n / 4
}
