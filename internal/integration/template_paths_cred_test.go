package integration

import (
	"strings"
	"testing"
)

func TestValidateTemplate_ClosedNamespace(t *testing.T) {
	ok := []string{
		"", "plain", "{{context.model_id}}", "{{run.id}}-{{run.started_at}}",
		"page={{page.number}}&c={{page.cursor}}", "{{ context.revision_id }}",
	}
	for _, s := range ok {
		if err := ValidateTemplate(s, false, nil); err != nil {
			t.Errorf("ValidateTemplate(%q): %v — want ok", s, err)
		}
	}
	bad := []string{
		"{{secrets.api_key}}", "{{env.HOME}}", "{{context.password}}",
		"{{row.amount}}",        // row.* outside push
		"{{", "}}", "{{}}", "x{{y",
	}
	for _, s := range bad {
		if err := ValidateTemplate(s, false, nil); err == nil {
			t.Errorf("ValidateTemplate(%q) allowed — want error", s)
		}
	}
	// row.* inside push, constrained to mapped fields.
	if err := ValidateTemplate("{{row.amount}}", true, map[string]bool{"amount": true}); err != nil {
		t.Errorf("mapped row field rejected: %v", err)
	}
	if err := ValidateTemplate("{{row.oops}}", true, map[string]bool{"amount": true}); err == nil {
		t.Error("unmapped row field allowed")
	}
}

func TestRender(t *testing.T) {
	tc := TemplateContext{
		Context: map[string]string{"model_id": "m1"},
		Page:    map[string]string{"number": "3"},
	}
	out, err := Render("m={{context.model_id}}&p={{page.number}}", tc)
	if err != nil || out != "m=m1&p=3" {
		t.Fatalf("Render = %q, %v", out, err)
	}
	if _, err := Render("{{run.id}}", tc); err == nil {
		t.Error("unresolvable variable rendered without error")
	}
}

func TestParseAndLookupPath(t *testing.T) {
	doc := map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"id": "a", "attrs": map[string]any{"amount": 5.0}},
				map[string]any{"id": "b"},
			},
		},
	}
	steps, err := ParsePath("$.data.items[0].attrs.amount")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v, ok := LookupPath(doc, steps)
	if !ok || v != 5.0 {
		t.Fatalf("lookup = %v, %v", v, ok)
	}
	// Missing → (nil, false), never panic.
	steps2, _ := ParsePath("$.data.items[5].id")
	if _, ok := LookupPath(doc, steps2); ok {
		t.Error("out-of-range index resolved")
	}
	// Root path.
	if s, err := ParsePath("$"); err != nil || s != nil {
		t.Errorf("root path: %v %v", s, err)
	}
	// Rejected shapes: wildcards/filters/recursion are not a thing here.
	for _, bad := range []string{"$.items[*]", "$..id", "$.a[?(@.x)]", "$.a[-1]", "$.a[b]", "$.a["} {
		if _, err := ParsePath(bad); err == nil {
			t.Errorf("ParsePath(%q) allowed — want error", bad)
		}
	}
}

func TestCredentialRoundTripRotationAndBinding(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	sealed, err := EncryptCredential([]byte(`{"token":"s3cr3t"}`), "app1", "conn1")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(sealed, "s3cr3t") {
		t.Fatal("ciphertext contains plaintext")
	}
	if !strings.HasPrefix(sealed, "iv1:") {
		t.Fatalf("missing version prefix: %q", sealed[:8])
	}
	plain, err := DecryptCredential(sealed, "app1", "conn1")
	if err != nil || string(plain) != `{"token":"s3cr3t"}` {
		t.Fatalf("decrypt: %q, %v", plain, err)
	}

	// AAD binding: same ciphertext under another app/connection must FAIL.
	if _, err := DecryptCredential(sealed, "app2", "conn1"); err == nil {
		t.Error("ciphertext decrypted under a different application")
	}
	if _, err := DecryptCredential(sealed, "app1", "connX"); err == nil {
		t.Error("ciphertext decrypted under a different connection")
	}

	// Key rotation: new key cannot open old ciphertext (explicit re-encrypt
	// flow required), and unset key is a hard error — no plaintext mode.
	t.Setenv("INTEGRATION_CRED_KEY", "ffffffffffffffffffffffffffffffff")
	if _, err := DecryptCredential(sealed, "app1", "conn1"); err == nil {
		t.Error("old ciphertext opened with rotated key")
	}
	t.Setenv("INTEGRATION_CRED_KEY", "")
	if _, err := EncryptCredential([]byte("x"), "a", "c"); err == nil {
		t.Error("encrypt succeeded without a key — plaintext fallback exists")
	}
	if _, err := DecryptCredential(sealed, "app1", "conn1"); err == nil {
		t.Error("decrypt succeeded without a key")
	}
	// Plaintext passthrough must not exist.
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	if _, err := DecryptCredential(`{"token":"raw"}`, "app1", "conn1"); err == nil {
		t.Error("unencrypted stored value accepted")
	}
}

func TestConfigValidate(t *testing.T) {
	base := func() *Config {
		return &Config{
			Kind: ConfigKind, Direction: DirectionPull, TargetType: TargetGrid,
			TargetID: "11111111-1111-1111-1111-111111111111", ImportMode: ModeIncremental,
			Request:  RequestConfig{Method: "GET", URL: "https://api.example.com/v1", BodyMode: BodyNone},
			Auth:     AuthPlacement{Type: "none"},
			Response: ResponseConfig{Format: FormatJSON, RecordsPath: "$.data"},
			Mapping:  MappingConfig{Fields: []FieldMap{{Source: "$.id", Target: "product", TargetKind: "dimension"}}},
		}
	}
	if err := base().Validate(false); err != nil {
		t.Fatalf("baseline config invalid: %v", err)
	}
	// A grab-bag of must-fail mutations.
	muts := []func(*Config){
		func(c *Config) { c.Request.URL = "https://10.0.0.1/x" },
		func(c *Config) { c.Request.Method = "TRACE" },
		func(c *Config) { c.Request.BodyMode = BodyJSON; c.Request.BodyJSON = "{nope" },
		func(c *Config) { c.Request.Method = "GET"; c.Request.BodyMode = BodyJSON; c.Request.BodyJSON = "{}" },
		func(c *Config) { c.Request.Headers = []KV{{Key: "Host", Value: "evil", Enabled: true}} },
		func(c *Config) { c.Request.Headers = []KV{{Key: "X-A", Value: "a\r\nb", Enabled: true}} },
		func(c *Config) { c.Request.MaxRetries = 3; c.Request.Method = "POST"; c.Request.BodyMode = BodyJSON; c.Request.BodyJSON = "{}" }, // retry w/o idempotency
		func(c *Config) { c.Auth = AuthPlacement{Type: "api_key"} },                                                                       // no placement
		func(c *Config) { c.Pagination = PaginationConfig{Mode: PageCursor} },                                                             // no cursor path
		func(c *Config) { c.Response.RecordsPath = "$..x" },
		func(c *Config) { c.Kind = "other" },
		func(c *Config) { c.Request.Query = []KV{{Key: "k", Value: "{{env.X}}", Enabled: true}} },
	}
	for i, m := range muts {
		c := base()
		m(c)
		if err := c.Validate(false); err == nil {
			t.Errorf("mutation %d validated — want error", i)
		}
	}
	// Push may use row.* in body; retries with idempotency key are legal.
	push := base()
	push.Direction = DirectionPush
	push.ImportMode = ""
	push.Response = ResponseConfig{}
	push.Request.Method = "POST"
	push.Request.BodyMode = BodyJSON
	push.Request.BodyJSON = `{"amount": {{row.amount}}, "run": "{{run.id}}"}`
	push.Request.MaxRetries = 2
	push.Request.IdempotencyKeyHeader = "Idempotency-Key"
	push.Mapping.Fields = []FieldMap{{Source: "amount", Target: "amount"}}
	if err := push.Validate(false); err != nil {
		t.Fatalf("push config invalid: %v", err)
	}
}
