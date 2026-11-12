package providers

import (
	"context"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ModelLister is a provider that can say which models its key reaches. The
// model setting was free text, so a name the key cannot use ("gpt-5.2-mini",
// 2026-11-11) failed only on the first call, with a 404.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// ListModels lists the chat models the key reaches, sorted. On OpenAI the
// embedding, image, audio and moderation models are left out.
func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	list, err := p.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range list.Models {
		if p.label == "openai" && !openAIChatModel(m.ID) {
			continue
		}
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out, nil
}

// openAIChatModel reports an OpenAI model id that answers chat completions.
func openAIChatModel(id string) bool {
	m := strings.ToLower(id)
	if !strings.HasPrefix(m, "gpt-") && !strings.HasPrefix(m, "chatgpt") && !openAIReasoningModel(m) {
		return false
	}
	for _, not := range []string{"embedding", "tts", "whisper", "dall-e", "image", "audio", "realtime", "transcribe", "search", "moderation", "instruct"} {
		if strings.Contains(m, not) {
			return false
		}
	}
	return true
}

// ListModels lists the models the key reaches, sorted.
func (p *AnthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	iter := p.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []string
	for iter.Next() {
		out = append(out, iter.Current().ID)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
