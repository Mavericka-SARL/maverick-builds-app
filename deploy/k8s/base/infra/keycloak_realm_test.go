package infra

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// The realm the platform needs exists twice, like the login theme: inlined in
// the keycloak-realm ConfigMap here for the cluster, and as a file the Compose
// stack mounts (deploy/compose/keycloak/mavericks-realm.json). Both take the
// console's host name from the CONSOLE_HOST placeholder, so nothing in either
// is specific to one deployment and they can be required to be identical: a
// role, a client or a brute-force setting added to one and not the other
// would give the two install paths different sign-in behaviour.
func TestKeycloakRealmMatchesTheComposeRealm(t *testing.T) {
	var cm struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	f, err := os.Open("keycloak.yaml")
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	for {
		if err := dec.Decode(&cm); err != nil {
			t.Fatalf("keycloak-realm ConfigMap not found in keycloak.yaml: %v", err)
		}
		if cm.Metadata.Name == "keycloak-realm" {
			break
		}
	}
	var cluster any
	if err := json.Unmarshal([]byte(cm.Data["realm.json"]), &cluster); err != nil {
		t.Fatalf("realm.json in the ConfigMap is not JSON: %v", err)
	}

	raw, err := os.ReadFile("../../../compose/keycloak/mavericks-realm.json")
	if err != nil {
		t.Fatalf("read the Compose realm: %v", err)
	}
	var compose any
	if err := json.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("the Compose realm is not JSON: %v", err)
	}

	if !reflect.DeepEqual(cluster, compose) {
		a, _ := json.MarshalIndent(cluster, "", "  ")
		b, _ := json.MarshalIndent(compose, "", "  ")
		t.Errorf("the realms have drifted — make them identical.\nkeycloak.yaml:\n%s\n\ndeploy/compose/keycloak/mavericks-realm.json:\n%s", a, b)
	}
}
