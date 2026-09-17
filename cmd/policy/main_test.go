package main

import (
	"testing"

	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, policyv1.PolicyService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
