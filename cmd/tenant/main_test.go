package main

import (
	"testing"

	tenantv1 "github.com/mavericks-engine/mavericks/gen/go/tenant/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, tenantv1.TenantService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
