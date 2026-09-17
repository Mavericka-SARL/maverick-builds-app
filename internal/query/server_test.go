// Tests for the P0-2 fail-closed fixes at the gRPC Server layer, plus the
// later access-rule-synchronization pass: Writeback enforces the same
// business rules HTTP /api/cells already does (system-managed revision,
// is_input) rather than relying solely on the Policy Service's per-metric
// permission check. Query and GetCell no longer authorize via the Policy
// Service at all (security.metric_policy/dimension_member_policy are a
// separate, unpopulated-in-normal-operation subsystem — nothing in the
// real product writes to those tables) — both now check
// identity.user_access_rule directly via internal/writeguard, the same
// mechanism every other read path uses.
package query_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/query"
)

// allowPolicyClient embeds the (nil) interface so it satisfies
// policyv1.PolicyServiceClient by promotion, overriding only the methods a
// test needs — any other method panics on a nil-pointer call, which is fine
// for a test-only fake.
type allowPolicyClient struct {
	policyv1.PolicyServiceClient
}

func (allowPolicyClient) CheckPermission(context.Context, *policyv1.CheckPermissionRequest, ...grpc.CallOption) (*policyv1.CheckPermissionResponse, error) {
	return &policyv1.CheckPermissionResponse{Allowed: true}, nil
}

type erroringPolicyClient struct {
	policyv1.PolicyServiceClient
}

func (erroringPolicyClient) GetEffectivePermissions(context.Context, *policyv1.GetEffectivePermissionsRequest, ...grpc.CallOption) (*policyv1.GetEffectivePermissionsResponse, error) {
	return nil, fmt.Errorf("policy service down")
}

// TestServerQueryFiltersHiddenMetric is a regression test: Query used to
// authorize purely via the (in practice unpopulated) Policy Service — a
// user hidden from a metric per identity.user_access_rule had no effect
// on what Query returned. erroringPolicyClient here proves the point
// structurally: if Query still depended on the Policy Service, this call
// would error; it must not, since Query no longer consults it at all.
func TestServerQueryFiltersHiddenMetric(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	srv := query.NewServer(zerolog.Nop(), store, nil, erroringPolicyClient{})
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000001"
	revisionID := "00000000-0000-0000-0001-000000000001"
	userID := "00000000-0000-0000-0000-000000000099"
	metricID := insertMetric(t, store, modelID, "revenue", true)
	insertRevision(t, store, revisionID, modelID)

	if _, err := store.Writeback(ctx, modelID, revisionID, userID, []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{}, MetricId: metricID, Value: 500},
	}); err != nil {
		t.Fatalf("seed writeback: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'hidden')
	`, userID, metricID); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	resp, err := srv.Query(ctx, &queryv1.QueryRequest{
		ModelId: modelID, RevisionId: revisionID, MetricIds: []string{metricID}, UserId: userID,
	})
	if err != nil {
		t.Fatalf("Query: %v (must not depend on the erroring Policy Service)", err)
	}
	if len(resp.Cells) != 0 {
		t.Errorf("Query returned %d cell(s) for a hidden metric, want 0", len(resp.Cells))
	}
}

// TestServerGetCellRejectsHiddenMetricAndMember is a regression test:
// GetCell used to run NO access check of any kind — not even the Policy
// Service — and GetCellRequest didn't carry a user_id at all. Both a
// hidden metric and a hidden dimension member must now be rejected.
func TestServerGetCellRejectsHiddenMetricAndMember(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	srv := query.NewServer(zerolog.Nop(), store, nil, allowPolicyClient{})
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000020"
	revisionID := "00000000-0000-0000-0020-000000000020"
	userID := "00000000-0000-0000-0000-000000000098"
	metricID := insertMetric(t, store, modelID, "salary", true)
	insertRevision(t, store, revisionID, modelID)

	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.dimension_def (id, model_id, name) VALUES ('00000000-0000-0000-0000-000000000030'::uuid, $1::uuid, 'dept')
	`, modelID); err != nil {
		t.Fatalf("insert dimension: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.dimension_member (id, dimension_id, code, label) VALUES ('00000000-0000-0000-0000-000000000031'::uuid, '00000000-0000-0000-0000-000000000030'::uuid, 'ENG', 'Engineering')
	`); err != nil {
		t.Fatalf("insert dimension member: %v", err)
	}
	deptMemberID := "00000000-0000-0000-0000-000000000031"
	deptDimID := "00000000-0000-0000-0000-000000000030"

	// No user_id at all — must be rejected outright.
	if _, err := srv.GetCell(ctx, &queryv1.GetCellRequest{ModelId: modelID, MetricId: metricID}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetCell with no user_id: code = %v, want InvalidArgument", status.Code(err))
	}

	// Hidden metric.
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'hidden')
	`, userID, metricID); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}
	if _, err := srv.GetCell(ctx, &queryv1.GetCellRequest{
		ModelId: modelID, RevisionId: revisionID, MetricId: metricID, UserId: userID,
	}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("GetCell for hidden metric: code = %v, want PermissionDenied", status.Code(err))
	}

	// Visible metric, hidden dimension member.
	visibleMetricID := insertMetric(t, store, modelID, "headcount", true)
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2::uuid, 'hidden')
	`, userID, deptMemberID); err != nil {
		t.Fatalf("seed hidden member rule: %v", err)
	}
	if _, err := srv.GetCell(ctx, &queryv1.GetCellRequest{
		ModelId: modelID, RevisionId: revisionID, MetricId: visibleMetricID, UserId: userID,
		DimMembers: map[string]string{deptDimID: "ENG"},
	}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("GetCell for hidden dimension member: code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestServerWritebackEnforcesSystemManagedRevision(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	srv := query.NewServer(zerolog.Nop(), store, nil, allowPolicyClient{})

	modelID := "00000000-0000-0000-0000-000000000010"
	metricID := insertMetric(t, store, modelID, "amount", true)
	revisionID := "00000000-0000-0000-0010-000000000010"
	if _, err := store.Pool().Exec(context.Background(), `
		INSERT INTO model.revision (id, model_id, system_managed) VALUES ($1::uuid, $2::uuid, true)
	`, revisionID, modelID); err != nil {
		t.Fatalf("insert system-managed revision: %v", err)
	}

	_, err := srv.Writeback(context.Background(), &queryv1.WritebackRequest{
		ModelId: modelID, RevisionId: revisionID, UserId: "00000000-0000-0000-0000-000000000099",
		Updates: []*queryv1.WritebackUpdate{{MetricId: metricID, Value: 1}},
	})
	if err == nil {
		t.Fatal("expected Writeback to reject a write into a system-managed revision — Policy alone would have allowed it")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", got)
	}
}

func TestServerWritebackEnforcesIsInput(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	srv := query.NewServer(zerolog.Nop(), store, nil, allowPolicyClient{})

	modelID := "00000000-0000-0000-0000-000000000011"
	metricID := insertMetric(t, store, modelID, "total", false) // calculated, not writable
	revisionID := "00000000-0000-0000-0011-000000000011"
	insertRevision(t, store, revisionID, modelID)

	_, err := srv.Writeback(context.Background(), &queryv1.WritebackRequest{
		ModelId: modelID, RevisionId: revisionID, UserId: "00000000-0000-0000-0000-000000000099",
		Updates: []*queryv1.WritebackUpdate{{MetricId: metricID, Value: 1}},
	})
	if err == nil {
		t.Fatal("expected Writeback to reject a write to a non-input metric — Policy alone would have allowed it")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", got)
	}
}
