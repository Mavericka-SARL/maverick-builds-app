package grpcutil

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
)

func TestMethodToEventType(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{"/tenant.v1.TenantService/CreateCustomer", "tenant.create_customer"},
		{"/workflow.v1.WorkflowService/AssignTask", "workflow.assign_task"},
		{"malformed", "malformed"},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			if got := methodToEventType(c.method); got != c.want {
				t.Errorf("methodToEventType(%q) = %q, want %q", c.method, got, c.want)
			}
		})
	}
}

// fakeAuditClient records every RecordEvent call it receives, signaling on
// recorded so tests can wait for the interceptor's background goroutine
// without a sleep.
type fakeAuditClient struct {
	auditv1.AuditServiceClient
	recorded chan *auditv1.AuditEvent
}

func (f *fakeAuditClient) RecordEvent(_ context.Context, in *auditv1.RecordEventRequest, _ ...grpc.CallOption) (*auditv1.RecordEventResponse, error) {
	f.recorded <- in.Event
	return &auditv1.RecordEventResponse{}, nil
}

// TestAuditInterceptor_RecordsMutationsOnly proves AuditInterceptor fires
// RecordEvent for a mutation-shaped method and does not for a read-shaped
// one — the exact behavior that was previously dead code, never wired into
// any of the 11 gRPC services that now install it via WithAudit.
func TestAuditInterceptor_RecordsMutationsOnly(t *testing.T) {
	fake := &fakeAuditClient{recorded: make(chan *auditv1.AuditEvent, 1)}
	policy := AuditPolicy{"CreateCustomer": auditv1.EventCategory_EVENT_CATEGORY_ADMIN}
	interceptor := AuditInterceptor(fake, policy, zerolog.Nop())

	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	t.Run("mutation fires RecordEvent", func(t *testing.T) {
		info := &grpc.UnaryServerInfo{FullMethod: "/tenant.v1.TenantService/CreateCustomer"}
		if _, err := interceptor(context.Background(), "req", info, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		select {
		case event := <-fake.recorded:
			if event.EventType != "tenant.create_customer" {
				t.Errorf("event_type = %q, want %q", event.EventType, "tenant.create_customer")
			}
			// The audit.event_category Postgres column is a NOT NULL enum
			// with no "unspecified" label — a zero-value (unset) Category
			// fails that insert outright (SQLSTATE 22P02), silently
			// dropping every gRPC-originated audit event. Every caller
			// passes an explicit category, so it must land on the event.
			if event.Category != auditv1.EventCategory_EVENT_CATEGORY_ADMIN {
				t.Errorf("category = %v, want %v (unset/UNSPECIFIED would fail the DB insert)", event.Category, auditv1.EventCategory_EVENT_CATEGORY_ADMIN)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for RecordEvent to be called")
		}
	})

	t.Run("read does not fire RecordEvent", func(t *testing.T) {
		info := &grpc.UnaryServerInfo{FullMethod: "/tenant.v1.TenantService/GetCustomer"}
		if _, err := interceptor(context.Background(), "req", info, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		select {
		case event := <-fake.recorded:
			t.Fatalf("unexpected RecordEvent call for a read method: %+v", event)
		case <-time.After(200 * time.Millisecond):
			// expected: no call within the wait window
		}
	})
}

// TestWithAudit_NilClientIsNoOp proves the composed helper every cmd/*
// service uses never installs the interceptor when dialing the audit
// service failed (client == nil) — guarding against a nil-client panic
// inside AuditInterceptor's background goroutine, which recoveryInterceptor
// cannot catch since it only wraps the synchronous handler call.
func TestWithAudit_NilClientIsNoOp(t *testing.T) {
	policy := AuditPolicy{"CreateCustomer": auditv1.EventCategory_EVENT_CATEGORY_ADMIN}
	if opts := WithAudit(nil, policy, zerolog.Nop()); opts != nil {
		t.Errorf("WithAudit(nil, ...) = %v, want nil", opts)
	}

	fake := &fakeAuditClient{recorded: make(chan *auditv1.AuditEvent, 1)}
	if opts := WithAudit(fake, policy, zerolog.Nop()); len(opts) != 1 {
		t.Errorf("WithAudit(non-nil, ...) returned %d options, want 1", len(opts))
	}
}
