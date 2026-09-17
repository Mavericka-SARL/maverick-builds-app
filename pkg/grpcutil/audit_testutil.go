package grpcutil

import (
	"testing"

	"google.golang.org/grpc"
)

// AssertAuditPolicyComplete fails t if any method in a service's generated
// ServiceDesc.Methods is missing from both policy and deliberatelyUnaudited
// (unclassified — the exact failure mode AuditPolicy exists to catch), or
// present in both (ambiguous). Called from each service's own
// main_test.go, e.g.:
//
//	grpcutil.AssertAuditPolicyComplete(t, tenantv1.TenantService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
func AssertAuditPolicyComplete(t *testing.T, methods []grpc.MethodDesc, policy AuditPolicy, deliberatelyUnaudited []string) {
	t.Helper()
	unaudited := make(map[string]bool, len(deliberatelyUnaudited))
	for _, m := range deliberatelyUnaudited {
		unaudited[m] = true
	}
	for _, m := range methods {
		_, audited := policy[m.MethodName]
		switch {
		case audited && unaudited[m.MethodName]:
			t.Errorf("RPC %q is in both the audit policy and deliberatelyUnaudited", m.MethodName)
		case !audited && !unaudited[m.MethodName]:
			t.Errorf("RPC %q is not classified — add it to auditPolicy or deliberatelyUnaudited", m.MethodName)
		}
	}
}
