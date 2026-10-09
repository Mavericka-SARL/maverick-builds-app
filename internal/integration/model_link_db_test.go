package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/integration"
)

func linkConfig(sourceModelID, gridID string) *integration.Config {
	return &integration.Config{
		Kind: integration.ConfigKind, Protocol: integration.ProtocolModel, Direction: integration.DirectionPull,
		Model:      &integration.ModelSource{ModelID: sourceModelID, Grid: "Sales"},
		TargetType: integration.TargetGrid, TargetID: gridID,
		ImportMode: integration.ModeReplace,
		Auth:       integration.AuthPlacement{Type: "none"},
		Mapping:    integration.MappingConfig{Fields: []integration.FieldMap{{Source: "$.region", Target: "region"}}},
	}
}

// The worker and the gateway split the run queue: the worker never claims
// a model link (it cannot read one's source), the gateway claims nothing
// else.
func TestModelLinkRunsAreTheGatewaysOnly(t *testing.T) {
	s, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	grid := "11111111-1111-1111-1111-111111111111"
	https, err := s.CreateDefinition(ctx, modelID, revID, "HTTPS", "", nil, "draft", "", baseConfig(grid), false)
	if err != nil {
		t.Fatal(err)
	}
	link, err := s.CreateDefinition(ctx, modelID, revID, "Link", "", nil, "draft", "", linkConfig(modelID, grid), false)
	if err != nil {
		t.Fatal(err)
	}
	if link.SourceModelID != modelID || !link.SourceEnabled || link.LinkID == "" || link.LinkID == https.LinkID {
		t.Fatalf("link columns: %+v", link)
	}
	linkRun, _ := s.Enqueue(ctx, link.ID, "manual", "", false, nil)
	httpsRun, _ := s.Enqueue(ctx, https.ID, "manual", "", false, nil)

	if r, err := s.Claim(ctx, "worker", time.Minute); err != nil || r == nil || r.ID != httpsRun {
		t.Fatalf("worker claimed %+v (%v), want the HTTPS run", r, err)
	}
	if r, _ := s.Claim(ctx, "worker", time.Minute); r != nil {
		t.Fatalf("worker claimed the model link's run: %+v", r)
	}
	if r, err := s.ClaimModelLink(ctx, "gateway", time.Minute); err != nil || r == nil || r.ID != linkRun {
		t.Fatalf("gateway claimed %+v (%v), want the link's run", r, err)
	}
	if r, _ := s.ClaimModelLink(ctx, "gateway", time.Minute); r != nil {
		t.Fatalf("gateway claimed a second run: %+v", r)
	}
}

// A due schedule of a draft, a switched-off connector or a link switched
// off on its source side is not runnable: the scheduler advances it and
// queues nothing.
func TestDueSchedulesOnlyRunnable(t *testing.T) {
	s, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	grid := "11111111-1111-1111-1111-111111111111"
	mk := func(name string, cfg *integration.Config, set string) string {
		d, err := s.CreateDefinition(ctx, modelID, revID, name, "", nil, "draft", "", cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		if set != "" {
			if _, err := s.Pool().Exec(ctx, `UPDATE model.integration_def SET `+set+` WHERE id=$1::uuid`, d.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.UpsertSchedule(ctx, &integration.Schedule{IntegrationID: d.ID, Kind: "interval", IntervalSeconds: 60,
			Timezone: "UTC", Enabled: true, OverlapPolicy: "skip", MisfirePolicy: "skip", EnabledBy: ""}); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	want := map[string]bool{
		mk("draft", baseConfig(grid), ""):                                                         false,
		mk("active", baseConfig(grid), "status='active'"):                                         true,
		mk("off", baseConfig(grid), "status='active', enabled=false"):                             false,
		mk("link on", linkConfig(modelID, grid), "status='active'"):                               true,
		mk("link source off", linkConfig(modelID, grid), "status='active', source_enabled=false"): false,
	}
	due, err := s.DueSchedules(ctx, time.Now().Add(time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != len(want) {
		t.Fatalf("due schedules: %d, want %d", len(due), len(want))
	}
	for _, sc := range due {
		if sc.Runnable != want[sc.IntegrationID] {
			t.Errorf("schedule of %s: runnable %v, want %v", sc.IntegrationID, sc.Runnable, want[sc.IntegrationID])
		}
	}
}

// A real run of a switched-off connector reads nothing, whatever queued
// it; a test still runs.
func TestRunner_SwitchedOffConnectorDoesNotRun(t *testing.T) {
	rn, st, modelID, revID, gridID := setupRunner(t)
	def := mkPullDef(t, st, modelID, revID, gridID, "http://127.0.0.1:1/never", nil)
	if _, err := st.Pool().Exec(context.Background(), `UPDATE model.integration_def SET enabled=false WHERE id=$1::uuid`, def.ID); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"manual", "schedule"} {
		run := claimAndRun(t, rn, st, def.ID, trigger, false)
		if run.Status != "failed" || run.ErrorCode != integration.ErrCodeSwitchedOff || run.Requests != 0 {
			t.Errorf("%s run of a switched-off connector: %s %s requests=%d", trigger, run.Status, run.ErrorCode, run.Requests)
		}
	}
	if run := claimAndRun(t, rn, st, def.ID, "test", false); run.ErrorCode == integration.ErrCodeSwitchedOff {
		t.Errorf("a test of a switched-off connector was refused: %s", run.Message)
	}
}

// The worker's runner has no source reader: were it ever handed a model
// link's run, it fails it rather than reading anything.
func TestRunner_ModelLinkWithoutSourceReader(t *testing.T) {
	rn, st, modelID, revID, gridID := setupRunner(t)
	def, err := st.CreateDefinition(context.Background(), modelID, revID, "Link", "", nil, "draft", "", linkConfig(modelID, gridID), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.Enqueue(ctx, def.ID, "test", runnerUserID, false, nil); err != nil {
		t.Fatal(err)
	}
	run, err := st.ClaimModelLink(ctx, "w", time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim: %v", err)
	}
	rn.Execute(ctx, run)
	got, _ := st.GetRun(ctx, run.ID)
	if got.Status != "failed" || !strings.Contains(got.Message, "run by the gateway") {
		t.Fatalf("model link run without a source reader: %s %q", got.Status, got.Message)
	}
}
