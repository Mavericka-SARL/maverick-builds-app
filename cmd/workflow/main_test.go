package main

import (
	"testing"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, workflowv1.WorkflowService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
