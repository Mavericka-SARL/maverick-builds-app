package keycloak

// The chat connector's OAuth clients (docs/CHAT_CONNECTOR.md): one
// registered, confidential client per chat host — never dynamic client
// registration — and two client scopes: models:read, whose tokens name the
// connector as their audience, and grids:write, which lets the connector
// write the grid cells the person may write. The gateway's /mcp accepts
// exactly these tokens; its REST API refuses them.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"time"
)

// ConnectorScope is the connector's read scope: models and their grids.
const ConnectorScope = "models:read"

// ConnectorWriteScope is the connector's write scope: the grid cells the
// person may write in the console. A default scope of every connector
// client, so each new connection asks for it on the consent screen; one
// granted before it existed keeps the read scope only.
const ConnectorWriteScope = "grids:write"

// ConnectorHost is a chat host and the client registered for it.
type ConnectorHost struct {
	ClientID     string
	Name         string
	RedirectURIs []string
}

// DefaultConnectorHosts are ChatGPT's and Claude's hosted surfaces with the
// callback URLs their documentation gives (checked 2026-10-02): ChatGPT's
// stable redirect, which needs the issuer in the authorization response
// (Keycloak sends it), and its per-connection redirects; Claude's one
// callback for claude.ai, Desktop, mobile and Cowork.
var DefaultConnectorHosts = []ConnectorHost{
	{ClientID: "chatgpt-connector", Name: "ChatGPT", RedirectURIs: []string{
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://chatgpt.com/connector/oauth/*",
	}},
	{ClientID: "claude-connector", Name: "Claude", RedirectURIs: []string{
		"https://claude.ai/api/mcp/auth_callback",
	}},
}

// ConnectorSetup is what EnsureConnectorClients configures.
type ConnectorSetup struct {
	// ResourceURL is the connector's URL (MCP_RESOURCE_URL): the audience
	// every connector token carries.
	ResourceURL string
	// Hosts defaults to DefaultConnectorHosts.
	Hosts []ConnectorHost
	// AccessTokenLifespan defaults to five minutes: a revoked consent or a
	// disabled account stops refreshes, and a short token ends the rest.
	AccessTokenLifespan time.Duration
}

// ConnectorClient is one host's client as configured.
type ConnectorClient struct {
	ClientID     string
	Name         string
	Secret       string
	Created      bool
	RedirectURIs []string
}

type clientScopeRep struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Protocol        string            `json:"protocol"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	ProtocolMappers []mapperRep       `json:"protocolMappers,omitempty"`
}

type mapperRep struct {
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

type clientRep struct {
	ID                        string            `json:"id,omitempty"`
	ClientID                  string            `json:"clientId"`
	Name                      string            `json:"name,omitempty"`
	Description               string            `json:"description,omitempty"`
	Enabled                   bool              `json:"enabled"`
	Protocol                  string            `json:"protocol"`
	PublicClient              bool              `json:"publicClient"`
	ClientAuthenticatorType   string            `json:"clientAuthenticatorType"`
	StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
	ImplicitFlowEnabled       bool              `json:"implicitFlowEnabled"`
	DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
	ServiceAccountsEnabled    bool              `json:"serviceAccountsEnabled"`
	ConsentRequired           bool              `json:"consentRequired"`
	FullScopeAllowed          bool              `json:"fullScopeAllowed"`
	FrontchannelLogout        bool              `json:"frontchannelLogout"`
	RedirectURIs              []string          `json:"redirectUris"`
	WebOrigins                []string          `json:"webOrigins"`
	Attributes                map[string]string `json:"attributes"`
}

// EnsureConnectorClients creates or updates the connector's client scope
// and one client per host. It is idempotent: an existing client keeps its
// id and secret and is brought back to these settings. It needs a realm
// administrator (NewAdmin), not the gateway's service account.
func (c *Client) EnsureConnectorClients(ctx context.Context, setup ConnectorSetup) ([]ConnectorClient, error) {
	if setup.ResourceURL == "" {
		return nil, fmt.Errorf("the connector's resource URL is required (MCP_RESOURCE_URL)")
	}
	if u, err := url.Parse(setup.ResourceURL); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("resource URL %q is not an absolute URL", setup.ResourceURL)
	}
	hosts := setup.Hosts
	if len(hosts) == 0 {
		hosts = DefaultConnectorHosts
	}
	lifespan := setup.AccessTokenLifespan
	if lifespan == 0 {
		lifespan = 5 * time.Minute
	}

	scopeID, err := c.ensureConnectorScope(ctx, setup.ResourceURL)
	if err != nil {
		return nil, err
	}
	writeScopeID, err := c.ensureClientScope(ctx, clientScopeRep{
		Name:        ConnectorWriteScope,
		Description: "Write grid cells through the chat connector, with the person's own access",
		Protocol:    "openid-connect",
		Attributes: map[string]string{
			"include.in.token.scope":    "true",
			"display.on.consent.screen": "true",
			"consent.screen.text":       "Change the grid values you can change in maverickbuilds.app, with your access.",
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	offlineID, err := c.clientScopeID(ctx, "offline_access")
	if err != nil {
		return nil, err
	}

	var out []ConnectorClient
	for _, host := range hosts {
		rep := clientRep{
			ClientID: host.ClientID, Name: "maverickbuilds.app in " + host.Name,
			Description: "Chat connector (/mcp) for " + host.Name + ": reads grids, and writes cells with grids:write. Managed by cmd/connector-clients.",
			Enabled:     true, Protocol: "openid-connect",
			PublicClient: false, ClientAuthenticatorType: "client-secret",
			StandardFlowEnabled: true,
			// Each person consents, in their own name, to what the host reads.
			ConsentRequired: true,
			// No realm or client roles in the token: the gateway decides
			// access from its own grants, never from token roles.
			FullScopeAllowed: false,
			RedirectURIs:     host.RedirectURIs,
			WebOrigins:       []string{},
			Attributes: map[string]string{
				"pkce.code.challenge.method":        "S256",
				"access.token.lifespan":             strconv.Itoa(int(lifespan.Seconds())),
				"exclude.issuer.from.auth.response": "false",
				"use.refresh.tokens":                "true",
			},
		}
		var existing []clientRep
		if _, err := c.do(ctx, http.MethodGet, "/clients?clientId="+url.QueryEscape(host.ClientID), nil, &existing); err != nil {
			return nil, err
		}
		cc := ConnectorClient{ClientID: host.ClientID, Name: host.Name, RedirectURIs: host.RedirectURIs}
		var id string
		if len(existing) == 0 {
			res, err := c.do(ctx, http.MethodPost, "/clients", rep, nil)
			if err != nil {
				return nil, fmt.Errorf("create client %s: %w", host.ClientID, err)
			}
			id, cc.Created = path.Base(res.Location), true
		} else {
			id = existing[0].ID
			rep.ID = id
			if _, err := c.do(ctx, http.MethodPut, "/clients/"+id, rep, nil); err != nil {
				return nil, fmt.Errorf("update client %s: %w", host.ClientID, err)
			}
		}
		// models:read and grids:write always; offline_access on request
		// (Claude asks for it to refresh). The realm's own default scopes
		// stay: newer Keycloak carries the subject itself in one of them.
		for _, sid := range []string{scopeID, writeScopeID} {
			if _, err := c.do(ctx, http.MethodPut, "/clients/"+id+"/default-client-scopes/"+sid, nil, nil); err != nil {
				return nil, err
			}
		}
		if _, err := c.do(ctx, http.MethodPut, "/clients/"+id+"/optional-client-scopes/"+offlineID, nil, nil); err != nil {
			return nil, err
		}
		// With no realm roles in its tokens, the client is let issue offline
		// tokens by mapping the one role they need: a host stays connected
		// until the person revokes it, or loses the account.
		var offlineRole map[string]any
		if _, err := c.do(ctx, http.MethodGet, "/roles/offline_access", nil, &offlineRole); err != nil {
			return nil, err
		}
		if _, err := c.do(ctx, http.MethodPost, "/clients/"+id+"/scope-mappings/realm", []map[string]any{offlineRole}, nil); err != nil {
			return nil, err
		}
		var secret struct {
			Value string `json:"value"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/clients/"+id+"/client-secret", nil, &secret); err != nil {
			return nil, err
		}
		cc.Secret = secret.Value
		out = append(out, cc)
	}
	return out, nil
}

func (c *Client) clientScopeID(ctx context.Context, name string) (string, error) {
	var scopes []clientScopeRep
	if _, err := c.do(ctx, http.MethodGet, "/client-scopes", nil, &scopes); err != nil {
		return "", err
	}
	for _, s := range scopes {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", nil
}

// ensureConnectorScope creates or updates models:read with its audience
// mapper, and returns its id.
func (c *Client) ensureConnectorScope(ctx context.Context, resource string) (string, error) {
	scope := clientScopeRep{
		Name:        ConnectorScope,
		Description: "Read models and their grid data through the chat connector",
		Protocol:    "openid-connect",
		Attributes: map[string]string{
			"include.in.token.scope":    "true",
			"display.on.consent.screen": "true",
			"consent.screen.text":       "Read the models and grid data you can open in maverickbuilds.app, with your access.",
		},
	}
	mapper := mapperRep{
		Name: "connector audience", Protocol: "openid-connect", ProtocolMapper: "oidc-audience-mapper",
		Config: map[string]string{
			"included.custom.audience":  resource,
			"access.token.claim":        "true",
			"id.token.claim":            "false",
			"introspection.token.claim": "true",
		},
	}
	return c.ensureClientScope(ctx, scope, &mapper)
}

// ensureClientScope creates or updates scope, with mapper when it has one,
// and returns its id.
func (c *Client) ensureClientScope(ctx context.Context, scope clientScopeRep, mapper *mapperRep) (string, error) {
	id, err := c.clientScopeID(ctx, scope.Name)
	if err != nil {
		return "", err
	}
	if id == "" {
		if mapper != nil {
			scope.ProtocolMappers = []mapperRep{*mapper}
		}
		res, err := c.do(ctx, http.MethodPost, "/client-scopes", scope, nil)
		if err != nil {
			return "", fmt.Errorf("create client scope %s: %w", scope.Name, err)
		}
		return path.Base(res.Location), nil
	}
	scope.ID = id
	if _, err := c.do(ctx, http.MethodPut, "/client-scopes/"+id, scope, nil); err != nil {
		return "", fmt.Errorf("update client scope %s: %w", scope.Name, err)
	}
	if mapper == nil {
		return id, nil
	}
	var mappers []mapperRep
	if _, err := c.do(ctx, http.MethodGet, "/client-scopes/"+id+"/protocol-mappers/models", nil, &mappers); err != nil {
		return "", err
	}
	i := slices.IndexFunc(mappers, func(m mapperRep) bool { return m.Name == mapper.Name })
	if i < 0 {
		_, err = c.do(ctx, http.MethodPost, "/client-scopes/"+id+"/protocol-mappers/models", mapper, nil)
	} else {
		mapper.ID = mappers[i].ID
		_, err = c.do(ctx, http.MethodPut, "/client-scopes/"+id+"/protocol-mappers/models/"+mapper.ID, mapper, nil)
	}
	if err != nil {
		return "", fmt.Errorf("audience mapper: %w", err)
	}
	return id, nil
}
