// Command connector-clients registers the chat connector's OAuth clients in
// a deployment's Keycloak realm: the models:read scope, whose tokens name the
// connector as their audience, and one confidential client per chat host
// (ChatGPT, Claude) with exactly that host's callback URLs. It is idempotent;
// run it again after changing the connector's URL.
//
// It signs in as a master-realm administrator, because it changes the
// realm's configuration, which the gateway's own service account cannot:
//
//	KEYCLOAK_ADMIN=… KEYCLOAK_ADMIN_PASSWORD=… go run ./cmd/connector-clients \
//	  -keycloak https://auth.example.com -resource https://app.example.com/mcp
//
// It prints each client's id and secret for the host's connector settings.
// A secret is printed when its client is created, or with -show-secrets.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/keycloak"
)

func main() {
	var (
		kcURL       = flag.String("keycloak", os.Getenv("KEYCLOAK_URL"), "Keycloak origin")
		realm       = flag.String("realm", envOr("KEYCLOAK_REALM", "mavericks"), "realm")
		adminUser   = flag.String("admin-user", os.Getenv("KEYCLOAK_ADMIN"), "master-realm administrator")
		resource    = flag.String("resource", os.Getenv("MCP_RESOURCE_URL"), "the connector's URL, e.g. https://app.example.com/mcp (the gateway's MCP_RESOURCE_URL)")
		showSecrets = flag.Bool("show-secrets", false, "print the secrets of clients that already existed")
	)
	flag.Parse()
	password := os.Getenv("KEYCLOAK_ADMIN_PASSWORD")
	if *kcURL == "" || *adminUser == "" || password == "" || *resource == "" {
		fmt.Fprintln(os.Stderr, "need -keycloak, -admin-user (KEYCLOAK_ADMIN), KEYCLOAK_ADMIN_PASSWORD and -resource (MCP_RESOURCE_URL)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clients, err := keycloak.NewAdmin(*kcURL, *realm, *adminUser, password).
		EnsureConnectorClients(ctx, keycloak.ConnectorSetup{ResourceURL: strings.TrimSuffix(*resource, "/")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "connector clients:", err)
		os.Exit(1)
	}
	fmt.Printf("Scope %s issues tokens for %s.\n\n", keycloak.ConnectorScope, *resource)
	for _, c := range clients {
		state := "updated"
		if c.Created {
			state = "created"
		}
		fmt.Printf("%s — client %q (%s)\n  callbacks: %s\n", c.Name, c.ClientID, state, strings.Join(c.RedirectURIs, ", "))
		if c.Created || *showSecrets {
			fmt.Printf("  secret:    %s\n", c.Secret)
		}
		fmt.Println()
	}
	fmt.Printf("In each host, add the connector URL %s and enter its client id and secret.\n", *resource)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
