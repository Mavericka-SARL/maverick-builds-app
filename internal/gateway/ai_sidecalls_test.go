package gateway

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// sideCall answers the turn's own calls — the request's classification and a
// plan's coverage check — for a fake provider, so a test's script holds only
// the turn's replies. ok is false for any other call.
func sideCall(req providers.ChatRequest) (providers.ChatResponse, bool) {
	answer := func(s string) (providers.ChatResponse, bool) {
		return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: s}}, true
	}
	switch {
	case req.SystemPrompt == aiassistant.ClassifierPrompt:
		return answer(`{"mode": "direct", "tasks": []}`)
	case strings.HasPrefix(req.SystemPrompt, coverageSystemPrompt):
		return answer(`{"missing": [], "extra": []}`)
	}
	return providers.ChatResponse{}, false
}
