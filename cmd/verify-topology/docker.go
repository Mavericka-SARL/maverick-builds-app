package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// replaceHost rewrites a postgres:// DSN's host:port to newHost:5432,
// keeping user/pass/db/query params intact — used to point a container at
// the docker_default network's "postgres" service name instead of the
// "localhost" this host-side process uses to reach the same database.
func replaceHost(dsn, newHost string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Host = newHost + ":5432"
	return u.String()
}

const dockerNetwork = "docker_default" // deploy/docker/docker-compose.dev.yml's default network

// buildImage runs the same `docker build --build-arg SERVICE=<name>`
// invocation the real CI docker-publish job uses (deploy/docker/Dockerfile),
// so this tool verifies the actual shipped artifact, not a bespoke build.
func buildImage(service string) error {
	tag := "verify-topology/" + service + ":latest"
	cmd := exec.CommandContext(context.Background(), "docker", "build", "--build-arg", "SERVICE="+service,
		"-f", "deploy/docker/Dockerfile", "-t", tag, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w\n%s", service, err, tail(out, 40))
	}
	return nil
}

// runContainer starts a container for service on dockerNetwork, under the
// exact security constraints every k8s manifest declares
// (deploy/k8s/base/services/*.yaml), publishing hostPort -> 9090 so this
// process can dial it directly.
func runContainer(name, service string, hostPort int, env map[string]string) error {
	_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", name).Run() //nolint:errcheck // best-effort cleanup of a stale container from a prior run

	args := []string{
		"run", "-d", "--name", name, "--network", dockerNetwork,
		"--user", "65534:65534", "--read-only", "--cap-drop=ALL",
		"-p", fmt.Sprintf("%d:9090", hostPort),
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, "verify-topology/"+service+":latest")

	out, err := exec.CommandContext(context.Background(), "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s: %w\n%s", name, err, tail(out, 20))
	}
	return nil
}

// containerLogs returns a container's recent logs, for diagnosing a check
// failure without requiring the operator to reproduce it manually.
func containerLogs(name string) string {
	out, _ := exec.CommandContext(context.Background(), "docker", "logs", "--tail", "30", name).CombinedOutput() //nolint:errcheck
	return string(out)
}

// cleanupContainers stops and removes every container this run started,
// regardless of whether the checks passed — never leaves stray containers
// behind, matching cmd/qa-engine-test's own "not idempotent, cleans up
// after itself" convention.
func cleanupContainers(names []string) {
	for _, n := range names {
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", n).Run() //nolint:errcheck
	}
}

func tail(out []byte, lines int) string {
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
