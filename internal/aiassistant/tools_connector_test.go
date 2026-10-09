package aiassistant

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
)

// A secret typed into a request is hidden from the assistant, and comes
// back unchanged when the assistant sends the whole config back.
func TestHiddenRequestValuesRoundTrip(t *testing.T) {
	stored := integration.Config{Request: integration.RequestConfig{
		Headers: []integration.KV{{Key: "Authorization", Value: "Bearer s3cret", Enabled: true}, {Key: "Accept", Value: "application/json", Enabled: true}},
		Query:   []integration.KV{{Key: "api_key", Value: "k-1"}, {Key: "page", Value: "1"}},
	}}
	shown := stored
	hideSensitive(&shown)
	if shown.Request.Headers[0].Value != hiddenValue || shown.Request.Headers[1].Value != "application/json" ||
		shown.Request.Query[0].Value != hiddenValue || shown.Request.Query[1].Value != "1" {
		t.Fatalf("hidden: %+v", shown.Request)
	}
	if stored.Request.Headers[0].Value != "Bearer s3cret" {
		t.Fatal("hiding changed the stored config")
	}
	back := shown
	back.Request.Headers = append([]integration.KV(nil), shown.Request.Headers...)
	back.Request.Query = append([]integration.KV(nil), shown.Request.Query...)
	restoreHidden(&back, &stored)
	if back.Request.Headers[0].Value != "Bearer s3cret" || back.Request.Query[0].Value != "k-1" {
		t.Fatalf("restored: %+v", back.Request)
	}
}
