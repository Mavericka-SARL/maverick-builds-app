package gateway

import "testing"

// Every provider the settings screens offer must build, have an env-var
// fallback and a default model; anything else is refused by name.
func TestBuildProvider_KnownProviders(t *testing.T) {
	for _, name := range []string{"openai", "anthropic", "google", "mistral", "deepseek"} {
		if _, err := buildProvider(name, "k"); err != nil {
			t.Errorf("buildProvider(%q): %v", name, err)
		}
		if providerEnvKeys[name] == "" || providerDefaultModels[name] == "" {
			t.Errorf("%q has no env key or default model", name)
		}
	}
	if _, err := buildProvider("gemini", "k"); err == nil {
		t.Error(`buildProvider("gemini") should be refused — the provider is "google"`)
	}
}
