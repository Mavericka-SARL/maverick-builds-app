package main

import (
	"testing"

	modelv1 "github.com/mavericks-engine/mavericks/gen/go/model/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, modelv1.ModelService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
