package gateway

// The chat connector: an MCP server at /mcp that ChatGPT, Claude and other
// MCP hosts call with an OAuth access token issued for it
// (internal/mcpserver, internal/reporting). It works on grid data only:
// charts and reports are made in the chat from grids, never from
// dashboards, and are not saved here; the one change it makes is writing
// grid cells the person may write in the console.
//
// A connector token is not a console token. It must name this connector
// (audience = the connector's resource URL) and carry its read scope, and it
// must not be the console's own: /mcp refuses console tokens, and the REST
// routes refuse connector tokens (restClaims). A host's token therefore
// opens only what the connector serves — reads, and cell writes when the
// token also carries the write scope and the tenant allows them.
//
// Every tool call reads as the token's subject through this gateway's own
// routes, run in-process through the same chain as a console request
// (appIDMiddleware, tenantRouting, planGuard, the route's handler): tenant
// routing, role guards and access rules are the console's, decided afresh
// for every read. The person is resolved like any account — a disabled or
// removed one reads nothing — and nothing about them is cached between
// calls. Three things are narrower than the console: delegatedReadGate lets
// a connector's request reach only the read routes in delegatedReadRoutes,
// and the write routes in delegatedWriteRoutes only when mcpReader.Write
// made it; the request carries nothing of the host's but the verified
// subject and the application and model it names; and a write is refused
// when the model's tenant has turned chat writes off (chatWritesAllowed).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/mavericks-engine/mavericks/internal/mcpserver"
	"github.com/mavericks-engine/mavericks/internal/reporting"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
)

// MCPConfig configures the connector. The zero value leaves it off.
type MCPConfig struct {
	Enabled bool
	// ResourceURL is the connector's canonical URL, e.g.
	// https://app.example.com/mcp: the audience its tokens must carry and
	// the resource its protected-resource metadata names.
	ResourceURL string
	// AuthorizationServer is the issuer hosts send people to sign in at;
	// empty means this deployment's Keycloak realm.
	AuthorizationServer string
	// Scope is the scope a connector token must carry; empty means
	// DefaultMCPScope.
	Scope string
	// WriteScope is the scope a token must also carry for the connector to
	// write cells; empty means DefaultMCPWriteScope.
	WriteScope string
	// Clients are the OAuth clients registered for the chat hosts (a
	// token's azp; cmd/connector-clients); empty means DefaultMCPClients.
	Clients []string
	// ClientSecrets are those clients' secrets (client id → secret), shown
	// to every signed-in person from the account menu so they can add the
	// connector in each host (connectorInfo). Every workspace on a
	// deployment shares them: each person still signs in as themselves.
	ClientSecrets map[string]string
	// Version is reported to hosts as the server's version.
	Version string
	// OpenAIAppsChallenge is the token OpenAI's plugin submission asks the
	// connector's domain to serve at /.well-known/openai-apps-challenge, to
	// prove the domain is ours; empty serves nothing there.
	OpenAIAppsChallenge string
}

// DefaultMCPScope is the connector's read scope: reading models and their
// grids. It permits the connector's use; what it reads is the person's
// access, nothing more.
const DefaultMCPScope = keycloak.ConnectorScope

// DefaultMCPWriteScope is the connector's write scope: writing the grid
// cells the person may write in the console. A connection granted before it
// existed holds the read scope only, and writes nothing until the person
// connects again.
const DefaultMCPWriteScope = keycloak.ConnectorWriteScope

// DefaultMCPClients are the clients cmd/connector-clients registers: one per
// chat host, never one a host registered for itself.
var DefaultMCPClients = []string{"chatgpt-connector", "claude-connector"}

func (c MCPConfig) clients() []string {
	if len(c.Clients) == 0 {
		return DefaultMCPClients
	}
	return c.Clients
}

// mcpReadTimeout bounds one read a tool makes.
const mcpReadTimeout = 30 * time.Second

func (c MCPConfig) scope() string {
	if c.Scope == "" {
		return DefaultMCPScope
	}
	return c.Scope
}

func (c MCPConfig) writeScope() string {
	if c.WriteScope == "" {
		return DefaultMCPWriteScope
	}
	return c.WriteScope
}

// ── delegated reads ─────────────────────────────────────────────────────────

type delegatedKey struct{}

// withDelegatedSubject marks ctx as a delegated read for sub: a chat
// connector's, or a model link's. Only mcpReader sets it, on requests it
// builds itself: nothing from the network can.
func withDelegatedSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, delegatedKey{}, sub)
}

// delegatedSubject is the subject of a connector's read, if ctx is one.
func delegatedSubject(ctx context.Context) (string, bool) {
	sub, ok := ctx.Value(delegatedKey{}).(string)
	return sub, ok && sub != ""
}

type delegatedWriteKey struct{}

// withDelegatedWrite marks a delegated request as a chat connection's write,
// made through via (the host's client). Only mcpReader.Write sets it: a
// model link's reads never carry it.
func withDelegatedWrite(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, delegatedWriteKey{}, via)
}

// delegatedWrite is the client a delegated write was made through, if ctx
// is one.
func delegatedWrite(ctx context.Context) (string, bool) {
	if _, ok := delegatedSubject(ctx); !ok {
		return "", false
	}
	via, ok := ctx.Value(delegatedWriteKey{}).(string)
	return via, ok
}

// delegatedReadRoutes are the routes a connector's read may reach: who the
// person is, which models they open, grid data, and the dashboards and
// workflows that explain the process around it. Each decides its own
// access as for the console. Anything else answers 403 before any handler
// runs, so a tool that asked for another route, by mistake or otherwise,
// could not reach it.
var delegatedReadRoutes = []struct{ method, pattern string }{
	{http.MethodGet, "/api/me"},
	{http.MethodGet, "/api/apps"},
	{http.MethodGet, "/api/demo"},
	{http.MethodGet, "/api/grid"},
	{http.MethodGet, "/api/grids"},
	{http.MethodGet, "/api/grid/series"},
	// The process around the grids: dashboards (their text, links and
	// what each widget shows — never its data, which the grid reads give)
	// and published workflows with their steps.
	{http.MethodGet, "/api/dashboards"},
	{http.MethodGet, "/api/dashboards/{}"},
	{http.MethodGet, "/api/folders"},
	{http.MethodGet, "/api/workflow/definitions"},
	{http.MethodGet, "/api/workflow/definitions/{}"},
}

// delegatedWriteRoutes are the routes a chat connection's write may reach:
// a batch of grid cells, all or nothing, under the console's own checks.
var delegatedWriteRoutes = []struct{ method, pattern string }{
	{http.MethodPost, "/api/cells/batch"},
}

func delegatedReadAllowed(method, path string) bool {
	return routeListed(delegatedReadRoutes, method, path)
}

func routeListed(routes []struct{ method, pattern string }, method, path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, r := range routes {
		if r.method != method {
			continue
		}
		want := strings.Split(strings.Trim(r.pattern, "/"), "/")
		if len(want) != len(segs) {
			continue
		}
		ok := true
		for i := range want {
			if want[i] == "{}" {
				if segs[i] == "" {
					ok = false
					break
				}
				continue
			}
			if want[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// delegatedReadGate refuses a connector's read of any route outside
// delegatedReadRoutes, and its write of any outside delegatedWriteRoutes.
// Console requests pass untouched.
func (h *handler) delegatedReadGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, writes := delegatedWrite(r.Context())
		if _, ok := delegatedSubject(r.Context()); ok && !delegatedReadAllowed(r.Method, r.URL.Path) &&
			(!writes || !routeListed(delegatedWriteRoutes, r.Method, r.URL.Path)) {
			jsonErr(w, fmt.Errorf("forbidden: not a read this connection may make"), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mcpReader runs a delegated read through the gateway's own chain: a chat
// connector's, and a model link's source read (model_links.go).
type mcpReader struct {
	api http.Handler
	// timeout bounds one read; zero is mcpReadTimeout.
	timeout time.Duration
}

func (m mcpReader) Read(ctx context.Context, subject string, req reporting.Request) (reporting.Response, error) {
	return m.serve(ctx, subject, "", false, req)
}

// Write makes a chat connection's write as subject, through via (the host's
// client): the one kind of delegated request that may reach
// delegatedWriteRoutes. A model link never writes through it.
func (m mcpReader) Write(ctx context.Context, subject, via string, req reporting.Request) (reporting.Response, error) {
	return m.serve(ctx, subject, via, true, req)
}

func (m mcpReader) serve(ctx context.Context, subject, via string, write bool, req reporting.Request) (reporting.Response, error) {
	timeout := m.timeout
	if timeout == 0 {
		timeout = mcpReadTimeout
	}
	// A fresh context: the host's request contributes its cancellation and
	// nothing else — no value of it reaches the read but the subject.
	rctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	rctx = withDelegatedSubject(rctx, subject)
	if write {
		rctx = withDelegatedWrite(rctx, via)
	}

	u := url.URL{Path: req.Path}
	if req.Query != nil {
		u.RawQuery = req.Query.Encode()
	}
	var body *strings.Reader
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return reporting.Response{}, err
		}
		body = strings.NewReader(string(b))
	} else {
		body = strings.NewReader("")
	}
	hr, err := http.NewRequestWithContext(rctx, req.Method, u.String(), body) //nolint:contextcheck // deliberately fresh: the host's request lends its cancellation only (AfterFunc above), none of its values
	if err != nil {
		return reporting.Response{}, err
	}
	if req.Body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if req.AppID != "" {
		hr.Header.Set("X-App-Id", req.AppID)
	}
	if req.ModelID != "" {
		hr.Header.Set("X-Model-Id", req.ModelID)
	}
	rec := httptest.NewRecorder()
	m.api.ServeHTTP(rec, hr)
	// A write's answer is the route's, even past the deadline: once its
	// cells are stored the route finishes regardless, and says so.
	if err := rctx.Err(); err != nil && (!write || rec.Code != http.StatusOK) {
		return reporting.Response{}, err
	}
	return reporting.Response{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}, nil
}

// ── /mcp ────────────────────────────────────────────────────────────────────

// mcpLimit throttles each person's connector requests.
const (
	mcpRate  = 2.0 // requests per second
	mcpBurst = 60
)

// mountMCP serves the connector beside api when it is enabled.
func (h *handler) mountMCP(api http.Handler) http.Handler {
	if !h.mcp.Enabled {
		return api
	}
	resource := strings.TrimSuffix(h.mcp.ResourceURL, "/")
	issuer := h.mcp.AuthorizationServer
	if issuer == "" && h.kcPublicURL != "" && h.kcRealm != "" {
		issuer = strings.TrimSuffix(h.kcPublicURL, "/") + "/realms/" + h.kcRealm
	}
	metadataURL := resourceMetadataURL(resource)

	conn := mcpReader{api: api}
	server := mcpserver.New(reporting.New(conn).WithWriter(conn), mcpserver.Options{
		Version: h.mcp.Version, WriteScope: h.mcp.writeScope(), Observe: h.observeMCP,
	})
	streamable := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	limiter := &discoverLimiter{rate: mcpRate, burst: mcpBurst}
	throttled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ti := auth.TokenInfoFromContext(r.Context()); ti == nil || !limiter.allow("mcp:"+ti.UserID) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		streamable.ServeHTTP(w, r)
	})
	protected := auth.RequireBearerToken(h.verifyMCPToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
		Scopes:              []string{h.mcp.scope()},
		ClockSkew:           30 * time.Second,
	})(throttled)
	metadata := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   nonEmpty(issuer),
		ScopesSupported:        []string{h.mcp.scope(), h.mcp.writeScope()},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "maverickbuilds.app grids",
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", protected)
	if token := strings.TrimSpace(h.mcp.OpenAIAppsChallenge); token != "" {
		mux.HandleFunc("GET /.well-known/openai-apps-challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(token))
		})
	}
	mux.Handle("/.well-known/oauth-protected-resource", metadata)
	if path := wellKnownPath(resource); path != "/.well-known/oauth-protected-resource" {
		mux.Handle(path, metadata)
	}
	mux.Handle("/", api)
	return mux
}

// verifyMCPToken accepts a connector token: valid (issuer, signature,
// expiry), issued for this connector (audience) to one of the registered
// host clients — so never a console token.
// Every failure reads the same, "invalid token". The subject is resolved
// to an account on each read, not here.
func (h *handler) verifyMCPToken(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if h.devMode && h.jwks == nil {
		return devMCPToken(token)
	}
	if h.jwks == nil {
		return nil, auth.ErrInvalidToken
	}
	claims, err := h.jwks.Validate(token)
	if err != nil || claims.Subject == "" || claims.ExpiresAt == nil {
		return nil, auth.ErrInvalidToken
	}
	if !claims.HasAudience(strings.TrimSuffix(h.mcp.ResourceURL, "/")) ||
		!claims.IssuedTo(h.mcp.clients()) || claims.IssuedTo(h.tokenClients) {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		Scopes:     strings.Fields(claims.Scope),
		Expiration: claims.ExpiresAt.Time,
		UserID:     claims.Subject,
		Extra:      map[string]any{mcpserver.ClientKey: claims.AuthorizedParty},
	}, nil
}

// devMCPToken is the dev stack's stand-in for a connector token, which has
// no identity provider: "dev:<persona>" reads and writes as that persona
// (an X-Dev-User value), "dev-read:<persona>" only reads — a connection
// granted before the write scope existed. Only reachable with DEV_MODE on
// and no JWKS.
func devMCPToken(token string) (*auth.TokenInfo, error) {
	scopes := []string{DefaultMCPScope, DefaultMCPWriteScope}
	persona, ok := strings.CutPrefix(token, "dev:")
	if !ok {
		if persona, ok = strings.CutPrefix(token, "dev-read:"); ok {
			scopes = scopes[:1]
		}
	}
	if !ok || persona == "" {
		return nil, auth.ErrInvalidToken
	}
	sub := persona
	if s, ok := devPersonas[persona]; ok {
		sub = s
	}
	return &auth.TokenInfo{Scopes: scopes, Expiration: time.Now().Add(time.Hour), UserID: sub,
		Extra: map[string]any{mcpserver.ClientKey: "dev"}}, nil
}

// observeMCP logs each tool call: who, which tool, how long, how it ended —
// never its arguments or results.
func (h *handler) observeMCP(_ context.Context, subject, tool string, took time.Duration, err error) {
	ev := h.log.Info()
	if err != nil {
		ev = h.log.Warn().Str("outcome", strings.SplitN(err.Error(), ":", 2)[0])
	}
	ev.Str("component", "mcp").Str("subject", subject).Str("tool", tool).Dur("took", took).Msg("connector call")
}

// resourceMetadataURL is where RFC 9728 places resource's metadata: the
// well-known path inserted before the resource's own path.
func resourceMetadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	u.Path = wellKnownPath(resource)
	return u.String()
}

func wellKnownPath(resource string) string {
	u, err := url.Parse(resource)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "/.well-known/oauth-protected-resource"
	}
	return "/.well-known/oauth-protected-resource" + u.Path
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// connectorHost is one chat host's connection details.
type connectorHost struct {
	Name         string `json:"name"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// connectorInfo serves GET /api/connector to any signed-in person: whether
// this deployment serves the chat connector, its URL, and each host's client
// id and secret — what they enter when adding the connector in ChatGPT or
// Claude (the account menu). The secret is the host's, not the person's:
// each person who connects signs in with their own account, and reads only
// what their own access allows.
func (h *handler) connectorInfo(w http.ResponseWriter, r *http.Request) {
	if _, err := h.resolveActor(r.Context(), r); err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if !h.mcp.Enabled {
		jsonOK(w, map[string]any{"enabled": false, "hosts": []connectorHost{}})
		return
	}
	names := map[string]string{}
	for _, host := range keycloak.DefaultConnectorHosts {
		names[host.ClientID] = host.Name
	}
	hosts := []connectorHost{}
	for _, id := range h.mcp.clients() {
		name := names[id]
		if name == "" {
			name = id
		}
		hosts = append(hosts, connectorHost{Name: name, ClientID: id, ClientSecret: h.mcp.ClientSecrets[id]})
	}
	jsonOK(w, map[string]any{
		"enabled": true,
		"url":     strings.TrimSuffix(h.mcp.ResourceURL, "/"),
		"scope":   h.mcp.scope() + " " + h.mcp.writeScope(),
		"hosts":   hosts,
	})
}
