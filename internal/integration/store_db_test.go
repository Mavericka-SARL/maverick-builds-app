package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

func setupStore(t *testing.T) (*integration.Store, string, string, string) {
	t.Helper()
	pool := testdb.New(t, migrationfs.FS, ".")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('IntCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	return integration.NewStore(pool), appID, modelID, revID
}

func baseConfig(gridID string) *integration.Config {
	return &integration.Config{
		Kind: integration.ConfigKind, Direction: integration.DirectionPull,
		TargetType: integration.TargetGrid, TargetID: gridID,
		ImportMode: integration.ModeIncremental,
		Request:    integration.RequestConfig{Method: "GET", URL: "https://api.example.com/v1", BodyMode: integration.BodyNone},
		Auth:       integration.AuthPlacement{Type: "none"},
		Response:   integration.ResponseConfig{Format: integration.FormatJSON},
		Mapping:    integration.MappingConfig{Fields: []integration.FieldMap{{Source: "$.id", Target: "x"}}},
	}
}

func TestConnectionSecretLifecycleAndLeakage(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	s, appID, _, _ := setupStore(t)
	ctx := context.Background()

	conn, err := s.CreateConnection(ctx, appID, "prod-api", "bearer", json.RawMessage(`{"note":"n"}`), []byte(`{"token":"hunter2"}`), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !conn.HasSecret {
		t.Fatal("HasSecret false after storing a secret")
	}
	// LEAKAGE: nothing readable from the store's public shapes may carry the
	// secret, and the DB row must hold only ciphertext.
	if b, _ := json.Marshal(conn); strings.Contains(string(b), "hunter2") {
		t.Fatal("connection JSON leaks the secret")
	}
	var stored string
	_ = s.Pool().QueryRow(ctx, `SELECT secret_enc FROM model.integration_connection WHERE id=$1::uuid`, conn.ID).Scan(&stored)
	if strings.Contains(stored, "hunter2") || !strings.HasPrefix(stored, "iv1:") {
		t.Fatalf("DB row is not sealed ciphertext: %q", stored[:12])
	}

	// Keep (nil): metadata-only edit leaves the credential in place.
	upd, err := s.UpdateConnection(ctx, appID, conn.ID, "prod-api-2", "", nil, nil)
	if err != nil || !upd.HasSecret {
		t.Fatalf("keep: %v hasSecret=%v", err, upd.HasSecret)
	}
	// Worker-side open still succeeds and matches.
	_, _, secret, err := s.OpenCredential(ctx, appID, conn.ID)
	if err != nil || string(secret) != `{"token":"hunter2"}` {
		t.Fatalf("open: %q %v", secret, err)
	}
	// Replace.
	if _, err := s.UpdateConnection(ctx, appID, conn.ID, "", "", nil, []byte(`{"token":"new"}`)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	_, _, secret, _ = s.OpenCredential(ctx, appID, conn.ID)
	if string(secret) != `{"token":"new"}` {
		t.Fatalf("replace not applied: %q", secret)
	}
	// Remove (empty non-nil).
	if _, err := s.UpdateConnection(ctx, appID, conn.ID, "", "", nil, []byte{}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ := s.GetConnection(ctx, appID, conn.ID); got.HasSecret {
		t.Fatal("HasSecret true after removal")
	}

	// Cross-application isolation: another app cannot read it.
	if _, err := s.GetConnection(ctx, "00000000-0000-0000-0000-000000000001", conn.ID); err == nil {
		t.Fatal("foreign application read a connection")
	}
}

func TestDefinitionActivationGateAndAtomicity(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	s, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	gridID := "11111111-1111-1111-1111-111111111111"

	// Draft saves even with a half-finished config; active demands validity.
	cfg := baseConfig(gridID)
	def, err := s.CreateDefinition(ctx, modelID, revID, "Conn A", "", nil, "draft", "", cfg, false)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if def.Status != "draft" || def.Config == nil {
		t.Fatalf("draft round-trip: %+v", def)
	}

	// Activation without a test of the CURRENT config hash is refused.
	active := "active"
	if _, err := s.UpdateDefinition(ctx, modelID, def.ID, nil, nil, nil, &active, nil, nil, false); err == nil {
		t.Fatal("activated without a successful test")
	}
	// Mark tested for the current hash → activation passes.
	if err := s.MarkTested(ctx, def.ID, integration.ConfigHash(cfg)); err != nil {
		t.Fatalf("mark tested: %v", err)
	}
	if _, err := s.UpdateDefinition(ctx, modelID, def.ID, nil, nil, nil, &active, nil, nil, false); err != nil {
		t.Fatalf("activate after test: %v", err)
	}

	// Changing a request-critical field invalidates the test: the update
	// itself passes (still active? no — activation gate re-checks) …
	cfg2 := baseConfig(gridID)
	cfg2.Request.URL = "https://api.example.com/v2"
	if _, err := s.UpdateDefinition(ctx, modelID, def.ID, nil, nil, nil, &active, nil, cfg2, false); err == nil {
		t.Fatal("config change kept active status without a fresh test")
	}
	draft := "draft"
	upd, err := s.UpdateDefinition(ctx, modelID, def.ID, nil, nil, nil, &draft, nil, cfg2, false)
	if err != nil {
		t.Fatalf("save changed config as draft: %v", err)
	}
	if upd.Tested() {
		t.Fatal("Tested() true after request-critical change")
	}
	if upd.ConfigVersion != def.ConfigVersion+1 {
		t.Fatalf("config_version not bumped: %d", upd.ConfigVersion)
	}
}

func TestQueueClaimLeaseAndScheduleDedup(t *testing.T) {
	s, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	cfg := baseConfig("11111111-1111-1111-1111-111111111111")
	def, err := s.CreateDefinition(ctx, modelID, revID, "Q", "", nil, "draft", "", cfg, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enqueue → claim → second claim sees nothing (lease held).
	runID, err := s.Enqueue(ctx, def.ID, "manual", "", false, nil)
	if err != nil || runID == "" {
		t.Fatalf("enqueue: %q %v", runID, err)
	}
	claimed, err := s.Claim(ctx, "w1", 30*time.Second)
	if err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if again, _ := s.Claim(ctx, "w2", 30*time.Second); again != nil {
		t.Fatalf("second worker claimed a leased run: %+v", again)
	}
	// Finish → terminal.
	if err := s.Finish(ctx, runID, integration.RunResult{Status: "success", RecordsWritten: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := s.GetRun(ctx, runID)
	if got.Status != "success" || got.RecordsWritten != 3 || got.FinishedAt == nil {
		t.Fatalf("finished run: %+v", got)
	}

	// Expired lease is reclaimable (crashed worker).
	runID2, _ := s.Enqueue(ctx, def.ID, "manual", "", false, nil)
	if c, _ := s.Claim(ctx, "w1", 1*time.Millisecond); c == nil || c.ID != runID2 {
		t.Fatalf("claim2")
	}
	time.Sleep(50 * time.Millisecond)
	re, err := s.Claim(ctx, "w2", 30*time.Second)
	if err != nil || re == nil || re.ID != runID2 {
		t.Fatalf("expired lease not reclaimed: %+v %v", re, err)
	}
	_ = s.Finish(ctx, runID2, integration.RunResult{Status: "failed", ErrorCode: "timeout", Message: "deadline"})

	// Scheduled tick dedup: same (integration, scheduled_for) enqueues once.
	tick := time.Now().Truncate(time.Minute)
	id1, err := s.Enqueue(ctx, def.ID, "schedule", "", false, &tick)
	if err != nil || id1 == "" {
		t.Fatalf("sched enqueue: %v", err)
	}
	id2, err := s.Enqueue(ctx, def.ID, "schedule", "", false, &tick)
	if err != nil || id2 != "" {
		t.Fatalf("duplicate tick enqueued: %q %v", id2, err)
	}

	// Cancel semantics: queued → cancelled immediately; running → flagged.
	if st, _ := s.Cancel(ctx, id1); st != "cancelled" {
		t.Fatalf("cancel queued: %q", st)
	}
	runID3, _ := s.Enqueue(ctx, def.ID, "manual", "", false, nil)
	_, _ = s.Claim(ctx, "w1", 30*time.Second)
	if st, _ := s.Cancel(ctx, runID3); st != "running" {
		t.Fatalf("cancel running: %q", st)
	}
	if !s.IsCancelRequested(ctx, runID3) {
		t.Fatal("cancel_requested not set")
	}

	// Schedules: upsert + due scan + advance.
	sc := &integration.Schedule{IntegrationID: def.ID, Kind: "interval", IntervalSeconds: 60, Enabled: true, Timezone: "UTC"}
	if err := s.UpsertSchedule(ctx, sc); err != nil {
		t.Fatalf("upsert schedule: %v", err)
	}
	due, err := s.DueSchedules(ctx, time.Now().Add(2*time.Minute), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %d %v", len(due), err)
	}
	if err := s.AdvanceSchedule(ctx, &due[0], time.Now()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got2, _ := s.GetSchedule(ctx, def.ID)
	if got2.LastFireAt == nil || got2.NextFireAt == nil {
		t.Fatalf("advance did not stamp: %+v", got2)
	}
	// Invalid cron/timezone refused.
	if err := s.UpsertSchedule(ctx, &integration.Schedule{IntegrationID: def.ID, Kind: "cron", CronExpr: "not a cron", Enabled: true}); err == nil {
		t.Fatal("bad cron accepted")
	}
	if err := s.UpsertSchedule(ctx, &integration.Schedule{IntegrationID: def.ID, Kind: "cron", CronExpr: "0 * * * *", Timezone: "Mars/Olympus", Enabled: true}); err == nil {
		t.Fatal("bad timezone accepted")
	}
}
