package main

import (
	"testing"

	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, identityv1.IdentityService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
