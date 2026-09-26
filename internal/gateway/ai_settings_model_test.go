package gateway

import (
	"net/http"
	"testing"
)

// The model is free text and may be blank: blank is stored as blank and the
// provider's default applies when the key is used. It used to be replaced by
// an OpenAI model, which every other provider rejects.
func TestAISettings_BlankModelStaysBlank_UnknownProviderRefused(t *testing.T) {
	f := setupAIAuditFixture(t)

	if status, body := f.do(t, "PUT", "/api/ai/settings", map[string]string{"provider": "google", "model": ""}); status != http.StatusOK {
		t.Fatalf("save google/blank: status=%d body=%v", status, body)
	}
	status, body := f.do(t, "GET", "/api/ai/settings", nil)
	if status != http.StatusOK || body["provider"] != "google" || body["model"] != "" {
		t.Fatalf("stored settings = %d %v, want provider google and a blank model", status, body)
	}

	if status, _ := f.do(t, "PUT", "/api/ai/settings", map[string]string{"provider": "google", "model": "gemini-99-ultra-tomorrow"}); status != http.StatusOK {
		t.Fatalf("a model this code has never heard of must be accepted: status=%d", status)
	}
	if _, body := f.do(t, "GET", "/api/ai/settings", nil); body["model"] != "gemini-99-ultra-tomorrow" {
		t.Fatalf("free-text model not stored: %v", body)
	}

	if status, _ := f.do(t, "PUT", "/api/ai/settings", map[string]string{"provider": "gemini"}); status != http.StatusBadRequest {
		t.Fatalf("unknown provider: status=%d, want 400", status)
	}
}
