// cmd/verify-topology — real, live distributed-topology verification.
//
// Every other test in this repo (testcontainers-based, ~35 files) exercises
// internal/* code in-process within one test binary — never the actual
// compiled cmd/* binaries communicating over real gRPC the way a Kubernetes
// deployment would run them. This tool closes that gap for one concrete,
// high-value surface: the AuditInterceptor path wired into 11 of the 12
// gRPC services (pkg/grpcutil/audit.go) — for each reachable service, it
// builds the real Dockerfile image, runs it under the exact k8s
// SecurityContext every manifest declares, drives one real mutating RPC via
// a real gRPC client, and asserts a correctly-categorized row lands in
// audit.audit_event, queried directly from Postgres.
//
// Found and fixed a real bug the first time this was run manually
// (2026-08-09): AuditInterceptor never set AuditEvent.Category, so every
// gRPC-originated audit insert had been silently failing since the feature
// was wired up on 2026-08-04 — see IMPLEMENTATION_PLAN.md's "Recently
// closed" section. This tool exists so that verification is repeatable,
// not a one-off manual grpcurl session.
//
// calculation and query are additionally started and checked for basic
// live-gRPC reachability (a real read RPC each) rather than the full
// audit-backed check the other 9 services get, reported as its own
// REACHABLE status distinct from PASS/FAIL. This predates
// pkg/grpcutil/audit.go's AuditPolicy mechanism (2026-08-10) — at the
// time, neither service had any RPC matching the old isMutation prefix
// heuristic, so their AuditInterceptor structurally never fired. Both now
// DO have real audited RPCs (calculation: TriggerRecalc/TriggerFullRecalc;
// query: Writeback), so upgrading these two to the same audit-backed
// PASS/FAIL check as the other 9 is a natural, currently-unbuilt
// follow-up — the REACHABLE-only checks below were simply never revisited
// after that mechanism changed, not because it's still structurally
// impossible.
//
// Every service is started with DEV_MODE=true and every gRPC call below
// carries an "x-dev-user" metadata identity (see checks.go), so
// pkg/grpcutil.AuthInterceptor (added 2026-08-10, closing this tool's own
// previously-confirmed P2 point 3 gap — no gRPC-level caller identity)
// resolves a real actor for every check, including import.CreateImportJob,
// which used to be reported as a confirmed, separately-tracked BLOCKED gap
// and is now a plain PASS/FAIL check like everything else.
//
// ai-assistant is NOT exercised at all — its two most natural RPCs to
// drive here (ApplyDiff, RollbackAction) require a prior successful
// GenerateDiff, which needs a real ANTHROPIC_API_KEY unavailable in this
// environment —
// see printSkipped.
//
// Requires: a real Docker daemon, and the dev stack already running
// (`make dev-up`, or `docker compose -f deploy/docker/docker-compose.dev.yml
// up -d`) — this tool does not start Postgres/NATS/Redis itself, matching
// cmd/qa-engine-test's own "requires bash dev.sh already running" contract.
//
// Usage:
//
//	go run ./cmd/verify-topology
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/db"
)

func main() {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"
	}
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		fatalf("connect to database (is `make dev-up` running?): %v", err)
	}
	defer pool.Close()

	services := []string{"tenant", "identity", "model", "schema-migration", "workflow", "import", "notification", "policy", "calculation", "query", "audit"}

	fmt.Println("=== Building images (deploy/docker/Dockerfile, the exact image CI publishes) ===")
	for _, s := range services {
		fmt.Printf("  building %s...\n", s)
		if err := buildImage(s); err != nil {
			fatalf("%v", err)
		}
	}

	var containerNames []string
	defer func() {
		fmt.Println("\n=== Cleaning up containers ===")
		cleanupContainers(containerNames)
	}()

	fmt.Println("\n=== Starting containers (real k8s SecurityContext: non-root, read-only root fs, all caps dropped) ===")
	audit := "verify-audit"
	containerNames = append(containerNames, audit)
	if err := runContainer(audit, "audit", 20009, map[string]string{"DATABASE_URL": dbURLForContainer(dbURL), "DEV_MODE": "true"}); err != nil {
		fatalf("%v", err)
	}
	time.Sleep(2 * time.Second)

	ports := map[string]int{
		"tenant": 20001, "identity": 20002, "model": 20003, "schema-migration": 20004,
		"workflow": 20005, "import": 20006, "notification": 20007, "policy": 20008,
		"calculation": 20010, "query": 20011,
	}
	for svc, port := range ports {
		name := "verify-" + svc
		containerNames = append(containerNames, name)
		env := map[string]string{
			"DATABASE_URL": dbURLForContainer(dbURL),
			"AUDIT_ADDR":   audit + ":9090",
			// calculation/query call log.Fatal() on a NATS connect failure
			// (unlike import, which only warns) — matches the real
			// mavericks-config ConfigMap's NATS_URL, which every service
			// gets via envFrom in the real k8s manifests.
			"NATS_URL": "nats://nats:4222",
			// Exercises AuthInterceptor's dev-mode path (x-dev-user gRPC
			// metadata, set below in checks.go) instead of requiring a real
			// Keycloak-issued JWT — matches the real k8s manifests' only
			// other DEV_MODE usage (cmd/gateway).
			"DEV_MODE": "true",
		}
		if err := runContainer(name, svc, port, env); err != nil {
			fatalf("%v", err)
		}
	}
	fmt.Println("  waiting for services to become ready...")
	time.Sleep(5 * time.Second)

	fmt.Println("\n=== Running live cross-service verification ===")
	results := runChecks(ctx, pool, ports)

	printSkipped()

	failed := 0
	fmt.Println("\n=== Results ===")
	for _, r := range results {
		status := "PASS"
		switch {
		case r.reachable && r.ok:
			status = "REACHABLE"
		case !r.ok:
			status = "FAIL"
			failed++
		}
		fmt.Printf("  [%s] %-18s %s\n", status, r.service, r.detail)
		if !r.ok {
			fmt.Printf("         container logs (tail):\n%s\n", indent(containerLogs("verify-"+r.service), "           "))
		}
	}

	fmt.Printf("\n%d/%d services verified\n", len(results)-failed, len(results))
	if failed > 0 {
		os.Exit(1)
	}
}

// dbURLForContainer rewrites a host-facing DATABASE_URL (localhost) to the
// docker_default network's service name, so a container can reach the same
// Postgres this process connects to from the host.
func dbURLForContainer(hostURL string) string {
	return replaceHost(hostURL, "postgres")
}

func indent(s, prefix string) string {
	out := prefix
	for _, r := range s {
		out += string(r)
		if r == '\n' {
			out += prefix
		}
	}
	return out
}
