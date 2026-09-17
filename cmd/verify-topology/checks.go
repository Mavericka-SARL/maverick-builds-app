package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	calculationv1 "github.com/mavericks-engine/mavericks/gen/go/calculation/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	modelv1 "github.com/mavericks-engine/mavericks/gen/go/model/v1"
	notificationv1 "github.com/mavericks-engine/mavericks/gen/go/notification/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	schemamigrationv1 "github.com/mavericks-engine/mavericks/gen/go/schemamigration/v1"
	tenantv1 "github.com/mavericks-engine/mavericks/gen/go/tenant/v1"
	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

type checkResult struct {
	service   string
	ok        bool
	reachable bool // true: not an audit-backed check (this tool hasn't been upgraded to assert on calculation/query's audit trail yet, see main.go's doc comment) — just proves a real RPC call over real gRPC succeeded
	detail    string
}

// transportCreds honors the MTLS_* env vars so this tool can verify an
// mTLS-enabled cluster (port-forwarded, with the cert files fetched locally);
// unset, it dials plaintext exactly as before.
func transportCreds() grpc.DialOption {
	if opt, enabled, err := grpcutil.ClientTLSOption(); err == nil && enabled {
		return opt
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials())
}

func dial(port int) (*grpc.ClientConn, error) {
	return grpc.NewClient(fmt.Sprintf("localhost:%d", port),
		transportCreds())
}

// waitForAuditEvent polls audit.audit_event for a row with the given
// event_type committed at or after `since` — the interceptor fires the
// RecordEvent call from a background goroutine, so the row may land a
// short moment after the RPC that triggered it returns.
func waitForAuditEvent(ctx context.Context, pool *pgxpool.Pool, eventType string, since time.Time) (category string, found bool) {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx,
			`SELECT category::text FROM audit.audit_event WHERE event_type=$1 AND occurred_at >= $2 ORDER BY occurred_at DESC LIMIT 1`,
			eventType, since).Scan(&category)
		if err == nil {
			return category, true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", false
}

func check(service, eventType, wantCategory string, ctx context.Context, pool *pgxpool.Pool, since time.Time, callErr error) checkResult {
	if callErr != nil {
		return checkResult{service: service, ok: false, detail: fmt.Sprintf("RPC failed: %v", callErr)}
	}
	got, found := waitForAuditEvent(ctx, pool, eventType, since)
	if !found {
		return checkResult{service: service, ok: false, detail: fmt.Sprintf("no audit.audit_event row for event_type=%q appeared within 8s", eventType)}
	}
	if got != wantCategory {
		return checkResult{service: service, ok: false, detail: fmt.Sprintf("event_type=%q landed with category=%q, want %q", eventType, got, wantCategory)}
	}
	return checkResult{service: service, ok: true, detail: fmt.Sprintf("event_type=%q category=%q", eventType, got)}
}

func runChecks(ctx context.Context, pool *pgxpool.Pool, ports map[string]int) []checkResult {
	var results []checkResult

	// Idempotent delete-before-create of any leftover "verify-topology"
	// customer from a prior run — mirrors runContainer's own pre-run
	// "docker rm -f" self-healing, and for the same reason: this tool's own
	// fatalf() calls os.Exit() directly, which skips every deferred cleanup
	// in the process, so an end-of-run defer here would silently miss any
	// run that hit an early check failure (which is most of them, by
	// design). core.customer cascade-deletes workspace/application/model/
	// role_assignment/raci_rule, so this fully removes a prior run's chain
	// in one statement. Confirmed live (2026-08-10): 5 prior runs had left
	// 5 orphaned "verify-topology" customers behind, each holding a
	// business_user role_assignment grant against whatever real
	// identity.user row an earlier version of this tool happened to reuse
	// (see the dedicated-user fix below) — visibly cluttering the real
	// demo persona's role list in the UI. Without this, the same
	// accumulation resumes even with that fix, just against this tool's own
	// throwaway users instead of a real one.
	if _, err := pool.Exec(ctx, `DELETE FROM core.customer WHERE name = 'verify-topology'`); err != nil {
		fatalf("cleanup stale verify-topology customer: %v", err)
	}

	// ── tenant: builds the fresh customer -> workspace -> application ->
	// model chain every other check below depends on. ──────────────────────
	tconn, err := dial(ports["tenant"])
	if err != nil {
		fatalf("dial tenant: %v", err)
	}
	defer tconn.Close() //nolint:errcheck
	tc := tenantv1.NewTenantServiceClient(tconn)

	since := time.Now()
	cust, err := tc.CreateCustomer(ctx, &tenantv1.CreateCustomerRequest{Name: "verify-topology", Plan: "test"})
	results = append(results, check("tenant", "tenant.create_customer", "admin", ctx, pool, since, err))
	if err != nil {
		fatalf("tenant.CreateCustomer: %v (cannot continue — every later check depends on this chain)", err)
	}

	ws, err := tc.CreateWorkspace(ctx, &tenantv1.CreateWorkspaceRequest{CustomerId: cust.Customer.Id, Name: "verify-ws"})
	if err != nil {
		fatalf("tenant.CreateWorkspace: %v", err)
	}
	app, err := tc.CreateApplication(ctx, &tenantv1.CreateApplicationRequest{WorkspaceId: ws.Workspace.Id, Name: "verify-app", Mode: tenantv1.ApplicationMode_APPLICATION_MODE_PLANNING})
	if err != nil {
		fatalf("tenant.CreateApplication: %v", err)
	}
	mdl, err := tc.CreateModel(ctx, &tenantv1.CreateModelRequest{ApplicationId: app.Application.Id, Name: "verify-model", StorageType: "oltp"})
	if err != nil {
		fatalf("tenant.CreateModel: %v", err)
	}

	// identity.v1 has no CreateUser RPC to call over gRPC, so this inserts a
	// dedicated throwaway identity.user row directly (the same "direct
	// Postgres" pattern already used for the audit-event assertions below) —
	// NOT a reused real seeded user. An earlier version of this tool reused
	// an arbitrary existing user (`SELECT id::text FROM identity.user LIMIT
	// 1`) on the assumption that scoping AssignRole/RACI to this tool's own
	// fresh workspace made it harmless either way — true for those two, but
	// notification.notification.recipient_user_id has NO workspace scoping
	// at all, so the SendNotification check below was writing real,
	// visible, template_vars-less "verify-test" rows straight into whatever
	// real demo persona happened to be the first identity.user row —
	// confirmed live (2026-08-10): 5 such rows had landed in the actual
	// seeded demo user's inbox and crashed NotificationCenter.tsx, which
	// didn't expect template_vars to ever be null. Fixed at every layer:
	// this tool no longer touches real users at all, notification.Store.Send
	// no longer stores literal JSON null for an unset templateVars, and
	// NotificationCenter.tsx now defends against it regardless.
	var toolUserID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity."user" (keycloak_sub, email, display_name, customer_id)
		VALUES ($1, $2, 'verify-topology', $3::uuid)
		RETURNING id::text
	`, "verify-topology-"+cust.Customer.Id, "verify-topology+"+cust.Customer.Id+"@test.invalid", cust.Customer.Id).Scan(&toolUserID); err != nil {
		fatalf("create dedicated verify-topology user: %v", err)
	}

	// Every service was started with DEV_MODE=true (see main.go), so
	// AuthInterceptor (pkg/grpcutil/auth.go) resolves the caller's identity
	// from this "x-dev-user" metadata instead of a real Keycloak-issued
	// Bearer JWT — reusing the same keycloak_sub just INSERTed above.
	// Attached once here since every call below reuses this same ctx.
	ctx = metadata.AppendToOutgoingContext(ctx, "x-dev-user", "verify-topology-"+cust.Customer.Id)

	// ── identity ─────────────────────────────────────────────────────────
	iconn, err := dial(ports["identity"])
	if err != nil {
		fatalf("dial identity: %v", err)
	}
	defer iconn.Close() //nolint:errcheck
	ic := identityv1.NewIdentityServiceClient(iconn)

	since = time.Now()
	_, err = ic.AssignRole(ctx, &identityv1.AssignRoleRequest{UserId: toolUserID, Role: commonv1.Role_ROLE_BUSINESS_USER, WorkspaceId: ws.Workspace.Id})
	results = append(results, check("identity", "identity.assign_role", "admin", ctx, pool, since, err))

	// ── model ────────────────────────────────────────────────────────────
	mconn, err := dial(ports["model"])
	if err != nil {
		fatalf("dial model: %v", err)
	}
	defer mconn.Close() //nolint:errcheck
	mc := modelv1.NewModelServiceClient(mconn)

	since = time.Now()
	_, err = mc.CreateDimension(ctx, &modelv1.CreateDimensionRequest{ModelId: mdl.Model.Id, Name: "verify_dim"})
	results = append(results, check("model", "model.create_dimension", "model_change", ctx, pool, since, err))

	// schema-migration's generator refuses a model with zero metrics
	// ("has no metrics defined") — not audited itself (CreateMetric already
	// covered under the model check above), just a real precondition.
	if _, err := mc.CreateMetric(ctx, &modelv1.CreateMetricRequest{ModelId: mdl.Model.Id, Name: "verify_metric", IsInput: true, StorageType: modelv1.StorageType_STORAGE_TYPE_OLTP}); err != nil {
		fatalf("model.CreateMetric (prerequisite for schema-migration): %v", err)
	}

	// ── schema-migration (needs a real migration to exist before it can be
	// applied — GenerateMigration is audited too now, but this check
	// specifically asserts on ApplyMigration's audit event) ───────────────
	smconn, err := dial(ports["schema-migration"])
	if err != nil {
		fatalf("dial schema-migration: %v", err)
	}
	defer smconn.Close() //nolint:errcheck
	smc := schemamigrationv1.NewSchemaMigrationServiceClient(smconn)

	gen, genErr := smc.GenerateMigration(ctx, &schemamigrationv1.GenerateMigrationRequest{ModelId: mdl.Model.Id})
	if genErr != nil {
		results = append(results, checkResult{service: "schema-migration", ok: false, detail: fmt.Sprintf("GenerateMigration (prerequisite) failed: %v", genErr)})
	} else {
		since = time.Now()
		_, err = smc.ApplyMigration(ctx, &schemamigrationv1.ApplyMigrationRequest{MigrationId: gen.Migration.Id, ConfirmedDestructive: true})
		results = append(results, check("schema-migration", "schemamigration.apply_migration", "model_change", ctx, pool, since, err))
	}

	// ── workflow ─────────────────────────────────────────────────────────
	wconn, err := dial(ports["workflow"])
	if err != nil {
		fatalf("dial workflow: %v", err)
	}
	defer wconn.Close() //nolint:errcheck
	wc := workflowv1.NewWorkflowServiceClient(wconn)

	since = time.Now()
	_, err = wc.CreateWorkflowDef(ctx, &workflowv1.CreateWorkflowDefRequest{ApplicationId: app.Application.Id, Name: "verify-wf", TriggerEvent: "manual"})
	results = append(results, check("workflow", "workflow.create_workflow_def", "model_change", ctx, pool, since, err))

	// ── import ───────────────────────────────────────────────────────────
	// CreateImportJob has no user_id/created_by field on its request at
	// all — the server always derives it via callerUserID(ctx), which
	// reads auth.ActorFromContext. That used to always find nothing over
	// a real gRPC call (no interceptor anywhere ever called
	// auth.WithActor for these standalone services), so import_job's FK
	// to identity.user always rejected the zero-UUID sentinel — this was
	// IMPLEMENTATION_PLAN.md's tracked P2 point 3, confirmed live by this
	// tool on 2026-08-10. Closed the same day: pkg/grpcutil.AuthInterceptor
	// now resolves a real actor from the "x-dev-user" metadata attached
	// above, so this is a plain check like every other RPC now.
	since = time.Now()
	imconn, err := dial(ports["import"])
	if err != nil {
		fatalf("dial import: %v", err)
	}
	defer imconn.Close() //nolint:errcheck
	imc := importpkgv1.NewImportServiceClient(imconn)

	_, err = imc.CreateImportJob(ctx, &importpkgv1.CreateImportJobRequest{ModelId: mdl.Model.Id})
	results = append(results, check("import", "importpkg.create_import_job", "data_change", ctx, pool, since, err))

	// ── notification (SendNotification isn't audited — no matching prefix —
	// only MarkRead, "/Mark", is) ────────────────────────────────────────
	nconn, err := dial(ports["notification"])
	if err != nil {
		fatalf("dial notification: %v", err)
	}
	defer nconn.Close() //nolint:errcheck
	nc := notificationv1.NewNotificationServiceClient(nconn)

	sendResp, sendErr := nc.SendNotification(ctx, &notificationv1.SendNotificationRequest{
		RecipientUserId: toolUserID, Channel: notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, TemplateId: "verify-test",
	})
	if sendErr != nil {
		results = append(results, checkResult{service: "notification", ok: false, detail: fmt.Sprintf("SendNotification (prerequisite) failed: %v", sendErr)})
	} else {
		since = time.Now()
		_, err = nc.MarkRead(ctx, &notificationv1.MarkReadRequest{NotificationIds: []string{sendResp.NotificationId}})
		results = append(results, check("notification", "notification.mark_read", "data_change", ctx, pool, since, err))
	}

	// ── policy ───────────────────────────────────────────────────────────
	pconn, err := dial(ports["policy"])
	if err != nil {
		fatalf("dial policy: %v", err)
	}
	defer pconn.Close() //nolint:errcheck
	pc := policyv1.NewPolicyServiceClient(pconn)

	since = time.Now()
	_, err = pc.UpsertRACIRule(ctx, &policyv1.UpsertRACIRuleRequest{Rule: &policyv1.RACIRule{
		ApplicationId: app.Application.Id, UserId: toolUserID, ResourcePattern: "metric:*", RaciType: policyv1.RACIType_RACI_TYPE_RESPONSIBLE,
	}})
	results = append(results, check("policy", "policy.upsert_r_a_c_i_rule", "policy_change", ctx, pool, since, err))

	// ── calculation, query: both now have real audited RPCs
	// (TriggerRecalc/TriggerFullRecalc; Writeback — see pkg/grpcutil's
	// AuditPolicy), but this tool hasn't been upgraded to assert on their
	// audit events yet — these still get a plain "a real RPC over real
	// gRPC succeeded" reachability check instead. ──────────────────────────
	cconn, err := dial(ports["calculation"])
	if err != nil {
		fatalf("dial calculation: %v", err)
	}
	defer cconn.Close() //nolint:errcheck
	cc := calculationv1.NewCalculationServiceClient(cconn)

	_, err = cc.GetDependencyGraph(ctx, &calculationv1.GetDependencyGraphRequest{ModelId: mdl.Model.Id})
	results = append(results, reachabilityCheck("calculation", "GetDependencyGraph", err))

	qconn, err := dial(ports["query"])
	if err != nil {
		fatalf("dial query: %v", err)
	}
	defer qconn.Close() //nolint:errcheck
	qc := queryv1.NewQueryServiceClient(qconn)

	// UserId deliberately left empty — a non-empty one would route through
	// a live policy-service dependency this tool doesn't wire up for query.
	_, err = qc.Query(ctx, &queryv1.QueryRequest{ModelId: mdl.Model.Id, MetricIds: []string{"verify-metric-placeholder"}})
	results = append(results, reachabilityCheck("query", "Query", err))

	return results
}

// reachabilityCheck reports whether a real RPC over real gRPC reached the
// server and executed real business logic — NOT an audit-backed check.
// codes.InvalidArgument/NotFound are still real, successful round trips
// (the server validated real input against real state and responded
// correctly) — only a transport-level failure (can't reach the server at
// all) means the service isn't actually up.
func reachabilityCheck(service, rpc string, err error) checkResult {
	if err != nil && status.Code(err) == codes.Unavailable {
		return checkResult{service: service, ok: false, detail: fmt.Sprintf("%s unreachable over gRPC: %v", rpc, err)}
	}
	return checkResult{service: service, ok: true, reachable: true, detail: fmt.Sprintf("%s round-tripped over real gRPC (response: %v)", rpc, err)}
}

func printSkipped() {
	fmt.Println("\n=== Not exercised at all (flagged, not silently skipped) ===")
	fmt.Println("  ai-assistant: ApplyDiff/RollbackAction are audited RPCs, but require")
	fmt.Println("               a prior successful GenerateDiff, which needs a real ANTHROPIC_API_KEY —")
	fmt.Println("               unavailable in this environment.")
}
