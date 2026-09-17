package main

import (
	"testing"

	aiassistantv1 "github.com/mavericks-engine/mavericks/gen/go/aiassistant/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, aiassistantv1.AIAssistantService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
