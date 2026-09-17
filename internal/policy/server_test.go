package policy

import (
	"context"
	"testing"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
)

func TestEvaluate_PlatformAdminWildcard(t *testing.T) {
	s := &Server{}
	actor := &commonv1.Actor{Role: commonv1.Role_ROLE_PLATFORM_ADMIN, UserId: "u1"}
	for _, resourceType := range []string{"application", "model", "policy", "anything"} {
		for _, action := range []policyv1.Action{
			policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE,
			policyv1.Action_ACTION_ADMIN,
		} {
			allowed, _ := s.evaluate(context.TODO(), actor, "", resourceType, "id", action)
			if !allowed {
				t.Errorf("platform_admin should have wildcard access: resource=%s action=%s", resourceType, action)
			}
		}
	}
}

func TestEvaluate_DeveloperAccess(t *testing.T) {
	s := &Server{}
	actor := &commonv1.Actor{Role: commonv1.Role_ROLE_DEVELOPER, UserId: "u2"}

	// Developer can read+write model
	allowed, _ := s.evaluate(context.TODO(), actor, "", "model", "id", policyv1.Action_ACTION_WRITE)
	if !allowed {
		t.Error("developer should be able to write model")
	}

	// Developer cannot admin
	allowed, _ = s.evaluate(context.TODO(), actor, "", "model", "id", policyv1.Action_ACTION_ADMIN)
	if allowed {
		t.Error("developer should not have admin on model")
	}
}

func TestEvaluate_BusinessUserReadOnly(t *testing.T) {
	s := &Server{}
	actor := &commonv1.Actor{Role: commonv1.Role_ROLE_BUSINESS_USER, UserId: "u3"}

	// Business user can read application
	allowed, _ := s.evaluate(context.TODO(), actor, "", "application", "id", policyv1.Action_ACTION_READ)
	if !allowed {
		t.Error("business_user should be able to read application")
	}

	// Business user cannot write application
	allowed, _ = s.evaluate(context.TODO(), actor, "", "application", "id", policyv1.Action_ACTION_WRITE)
	if allowed {
		t.Error("business_user should not be able to write application")
	}
}

func TestRACIAllows(t *testing.T) {
	tests := []struct {
		raci    policyv1.RACIType
		action  policyv1.Action
		allowed bool
	}{
		{policyv1.RACIType_RACI_TYPE_RESPONSIBLE, policyv1.Action_ACTION_WRITE, true},
		{policyv1.RACIType_RACI_TYPE_ACCOUNTABLE, policyv1.Action_ACTION_APPROVE, true},
		{policyv1.RACIType_RACI_TYPE_CONSULTED, policyv1.Action_ACTION_READ, true},
		{policyv1.RACIType_RACI_TYPE_CONSULTED, policyv1.Action_ACTION_WRITE, false},
		{policyv1.RACIType_RACI_TYPE_INFORMED, policyv1.Action_ACTION_READ, true},
		{policyv1.RACIType_RACI_TYPE_INFORMED, policyv1.Action_ACTION_APPROVE, false},
		{policyv1.RACIType_RACI_TYPE_UNSPECIFIED, policyv1.Action_ACTION_READ, false},
	}
	for _, tt := range tests {
		got, _ := raciAllows(tt.raci, tt.action)
		if got != tt.allowed {
			t.Errorf("raciAllows(%v, %v) = %v, want %v", tt.raci, tt.action, got, tt.allowed)
		}
	}
}
