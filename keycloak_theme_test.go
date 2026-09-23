package mavericks_test

import (
	"os"
	"strings"
	"testing"
)

// The sign-in theme exists twice: as files under deploy/docker/config (where
// the dev stack and any Docker deployment mount them) and embedded in the
// keycloak-theme ConfigMap in the Kubernetes manifest, because a ConfigMap
// cannot reference a directory. Two copies drift silently — a fix applied to
// one leaves the other serving the old page — so this test holds them equal.
//
// If it fails, copy the file content into the ConfigMap block, indented by
// four spaces under its key.
func TestKeycloakThemeMatchesTheManifest(t *testing.T) {
	const manifestPath = "deploy/k8s/base/infra/keycloak.yaml"
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}

	for _, f := range []struct{ key, path string }{
		{"theme.properties", "deploy/docker/config/keycloak/themes/maverickbuilds/login/theme.properties"},
		{"brand.css", "deploy/docker/config/keycloak/themes/maverickbuilds/login/resources/css/brand.css"},
	} {
		source, err := os.ReadFile(f.path)
		if err != nil {
			t.Errorf("read %s: %v", f.path, err)
			continue
		}
		embedded, ok := blockScalar(string(manifest), "  "+f.key+": |")
		if !ok {
			t.Errorf("%s has no %q key; the theme would not reach the cluster", manifestPath, f.key)
			continue
		}
		if embedded != strings.TrimRight(string(source), "\n") {
			t.Errorf("%s and the %q key of %s have drifted apart", f.path, f.key, manifestPath)
		}
	}
}

// blockScalar returns the content of a YAML literal block introduced by
// header, with its four-space indentation removed.
func blockScalar(manifest, header string) (string, bool) {
	i := strings.Index(manifest, header+"\n")
	if i < 0 {
		return "", false
	}
	var out []string
	for _, line := range strings.Split(manifest[i+len(header)+1:], "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(line, "    "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n"), true
}
