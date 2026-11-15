package gateway

import (
	"sync"
	"testing"
	"time"
)

// The status a turn sends: each change at once, the provider's stages until
// it writes, the call's silence measured as the idle watchdog measures it,
// and the steps before.
func TestTurnStatusSteps(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	ts := newTurnStatus(func(event string, payload any) {
		if event != "status" {
			t.Errorf("event %q", event)
		}
		mu.Lock()
		sent = append(sent, payload.(map[string]any))
		mu.Unlock()
	}, 5*time.Minute, 6*time.Minute)
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return clock }
	last := func() map[string]any { mu.Lock(); defer mu.Unlock(); return sent[len(sent)-1] }

	ts.server("list_metrics")
	clock = clock.Add(300 * time.Millisecond)
	ts.aiCall(9, 1, "", 54000)
	s := last()
	if s["phase"] != "ai" || s["call"] != 9 || s["stage"] != "sending" || s["context_tokens"] != 54000 || s["idle_limit_s"] != 300 {
		t.Fatalf("call start %v", s)
	}
	if r := s["recent"].([]turnStepDone); len(r) != 1 || r[0].Label != "list_metrics" || r[0].MS != 300 {
		t.Fatalf("recent %v", r)
	}

	ts.providerStage("connected")
	ts.providerStage("reasoning")
	ts.providerStage("reasoning") // no change, nothing sent
	n := len(sent)
	if last()["stage"] != "reasoning" {
		t.Fatalf("stage %v", last())
	}
	clock = clock.Add(70 * time.Second)
	ts.output()
	ts.output() // already writing: carried by the beat
	if len(sent) != n+1 || last()["stage"] != "writing" {
		t.Fatalf("writing sent %d, %v", len(sent)-n, last())
	}
	ts.providerStage("reasoning") // a stage after output does not go back
	if last()["stage"] != "writing" {
		t.Fatalf("stage went back: %v", last())
	}

	clock = clock.Add(12 * time.Second)
	ts.mu.Lock()
	s = ts.snapshotLocked()
	ts.mu.Unlock()
	if s["step_s"] != 82 || s["quiet_s"] != 12 {
		t.Fatalf("step %v quiet %v", s["step_s"], s["quiet_s"])
	}

	ts.aiCall(9, 2, "the AI provider sent nothing for 5m0s after 5m0s, so it was asked again", 54000)
	if s := last(); s["attempt"] != 2 || s["note"] == nil || s["quiet_s"] != 0 {
		t.Fatalf("second attempt %v", s)
	}
	for i := 0; i < 5; i++ {
		ts.server("step")
	}
	ts.done()
	ts.mu.Lock()
	recent := append([]turnStepDone{}, ts.recent...)
	ts.mu.Unlock()
	if len(recent) != statusRecent || recent[0].Label != "step" {
		t.Fatalf("recent kept %v", recent)
	}
}

// The beat repeats the status while a step runs, and stops for good on
// close.
func TestTurnStatusBeat(t *testing.T) {
	var mu sync.Mutex
	beats := 0
	ts := newTurnStatus(func(string, any) { mu.Lock(); beats++; mu.Unlock() }, time.Minute, time.Minute)
	ts.beat(5 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if beats != 0 {
		t.Errorf("beat with no step running: %d", beats)
	}
	mu.Unlock()
	ts.server("preview_file_import")
	time.Sleep(40 * time.Millisecond)
	ts.close()
	mu.Lock()
	got := beats
	mu.Unlock()
	if got < 3 {
		t.Errorf("beats %d", got)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if beats != got {
		t.Errorf("beat after close: %d → %d", got, beats)
	}
}
