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
	case req.SystemPrompt == attachmentAnalysisPrompt:
		return answer("ANALYSIS: a test attachment")
	case req.SystemPrompt == aiassistant.CompactMemoryPrompt:
		return answer("SUMMARY: the earlier conversation")
	}
	return providers.ChatResponse{}, false
}

// modelGiven is what a main call gave the model to work from: the
// instructions and the turn's message as sent, which carries the turn's
// context and brief (2026-11-25: they left the instructions so the provider
// keeps those cached).
func modelGiven(req providers.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.SystemPrompt + "\n" + req.Messages[i].Content
		}
	}
	return req.SystemPrompt
}
