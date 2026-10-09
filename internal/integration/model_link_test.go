package integration_test

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
)

func TestValidateModelLink(t *testing.T) {
	grid := "11111111-1111-1111-1111-111111111111"
	model := "22222222-2222-2222-2222-222222222222"
	if err := linkConfig(model, grid).Validate(false); err != nil {
		t.Fatalf("a complete link: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(*integration.Config)
		want   string
	}{
		{"no source", func(c *integration.Config) { c.Model = nil }, "model settings are required"},
		{"no model", func(c *integration.Config) { c.Model.ModelID = "" }, "model_id"},
		{"no grid", func(c *integration.Config) { c.Model.Grid = " " }, "model.grid"},
		{"push", func(c *integration.Config) { c.Direction = integration.DirectionPush }, "direction pull"},
		{"sign-in", func(c *integration.Config) { c.Auth.Type = "bearer" }, "auth must be none"},
		{"sftp settings", func(c *integration.Config) { c.SFTP = &integration.SFTPSource{} }, "sftp protocol only"},
		{"empty filter", func(c *integration.Config) { c.Model.Filters = map[string][]string{"region": nil} }, "filters"},
		{"member display", func(c *integration.Config) { c.Model.MemberDisplay = "both" }, "member_display"},
		{"incremental", func(c *integration.Config) { c.ImportMode = integration.ModeIncremental }, "replace or full_reload"},
	} {
		cfg := linkConfig(model, grid)
		c.mutate(cfg)
		if err := cfg.Validate(false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.name, err, c.want)
		}
	}
	// Model settings belong to the model protocol only.
	https := baseConfig(grid)
	https.Model = &integration.ModelSource{ModelID: model, Grid: "Sales"}
	if err := https.Validate(false); err == nil || !strings.Contains(err.Error(), "model protocol only") {
		t.Errorf("HTTPS with model settings: %v", err)
	}
	// The source is part of what a test vouches for; an HTTPS config's hash
	// is what it was before links existed.
	a, b := linkConfig(model, grid), linkConfig(model, grid)
	b.Model.Grid = "Other"
	if integration.ConfigHash(a) == integration.ConfigHash(b) {
		t.Error("changing the source grid kept the tested hash")
	}
}
