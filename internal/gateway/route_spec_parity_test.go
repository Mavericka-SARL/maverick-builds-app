// Checks the manual router (BuildRoutes, see handler.go's registerRoutes)
// against api/openapi.yaml, per IMPLEMENTATION_PLAN.md's "Complete HTTP
// contract documentation" P1 item. Every registered route must have exactly
// one precise method+pattern entry in the spec, and vice versa — this is the
// item's exit criterion, enforced unconditionally now that the spec has full
// coverage. If this test ever needs an allowlist again (a new resource area
// added faster than it's documented), reintroduce one deliberately rather
// than resurrecting this comment.
package gateway

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type specDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

func loadSpecOperations(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	ops := map[string]bool{}
	for path, methods := range doc.Paths {
		for method := range methods {
			ops[strings.ToUpper(method)+" "+path] = true
		}
	}
	return ops
}

func TestRouteSpecParity(t *testing.T) {
	specOps := loadSpecOperations(t)
	routes := BuildRoutes()

	registeredKeys := map[string]bool{}
	for _, rt := range routes {
		if rt.Method == "" {
			t.Errorf("route %q has no precise method registered — give it explicit method+pattern register() calls", rt.Pattern)
			continue
		}
		key := rt.Method + " " + rt.Pattern
		registeredKeys[key] = true
		if !specOps[key] {
			t.Errorf("route %q has no api/openapi.yaml operation — document it", key)
		}
	}

	for key := range specOps {
		if !registeredKeys[key] {
			t.Errorf("api/openapi.yaml documents %q but no route is registered for it — fix the spec or the router", key)
		}
	}
}
