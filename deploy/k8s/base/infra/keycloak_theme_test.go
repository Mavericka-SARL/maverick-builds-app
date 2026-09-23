package infra

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Keycloak login theme exists twice on purpose: as files under
// deploy/docker/config/keycloak/themes/maverickbuilds/login/, which both
// compose stacks mount so the theme can be worked on and seen, and inlined in
// the keycloak-theme ConfigMap here, which is the only way the cluster can
// mount it (a ConfigMap cannot reference a directory).
//
// Nothing copies one to the other, so the danger is editing the stylesheet a
// developer sees and shipping the one nobody looked at. This test fails when
// the two drift.
func TestKeycloakThemeConfigMapMatchesTheFiles(t *testing.T) {
	const themeDir = "../../../docker/config/keycloak/themes/maverickbuilds/login"

	var cm struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data       map[string]string `yaml:"data"`
		BinaryData map[string]string `yaml:"binaryData"`
	}
	f, err := os.Open("keycloak.yaml")
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	for {
		if err := dec.Decode(&cm); err != nil {
			t.Fatalf("keycloak-theme ConfigMap not found in keycloak.yaml: %v", err)
		}
		if cm.Metadata.Name == "keycloak-theme" {
			break
		}
	}

	for key, path := range map[string]string{
		"theme.properties": "theme.properties",
		"brand.css":        "resources/css/brand.css",
		"wordmark.svg":     "resources/img/wordmark.svg",
		"brand.js":         "resources/js/brand.js",
	} {
		want, err := os.ReadFile(filepath.Join(themeDir, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got, ok := cm.Data[key]
		if !ok {
			t.Errorf("ConfigMap has no %q: the cluster would serve the theme without it", key)
			continue
		}
		if got != string(want) {
			t.Errorf("ConfigMap key %q has drifted from %s — copy the file into keycloak.yaml", key, path)
		}
	}

	want, err := os.ReadFile(filepath.Join(themeDir, "resources/img/favicon.ico"))
	if err != nil {
		t.Fatalf("read favicon: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(cm.BinaryData["favicon.ico"])
	if err != nil {
		t.Fatalf("ConfigMap favicon.ico is not base64: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ConfigMap favicon.ico has drifted from resources/img/favicon.ico")
	}
}
