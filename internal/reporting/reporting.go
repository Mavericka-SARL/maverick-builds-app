// Package reporting reads a model's grid data for the chat connector
// (internal/mcpserver), as one signed-in person, through the engine's own
// read routes (Reader), and lays it out as tables, charts, comparisons and
// reports (presentation.go). The connector analyses grid data only: charts
// and reports are made in the chat from grids, never from dashboards, and
// nothing is saved in maverickbuilds.app.
//
// It decides nothing about access. Every read is a route's — /api/grid,
// /api/grids, /api/grid/series — made as the person the connector was
// granted by, so their tenant routing, role guards and access rules are the
// console's own; a second, connector-specific opinion of what someone may
// read would drift from them. What this package adds is shape (typed tables
// with their provenance) and two checks that keep a read on the context it
// was asked for:
//
//   - the application, model and revision a request names are confirmed
//     against what the person may open before anything is read (Pin) — the
//     model header the routes take falls back to another model when it
//     names one the person may not open;
//   - a filter naming a member the person cannot see is refused, never
//     reported as a different slice.
package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is one read made through the engine's HTTP routes.
type Request struct {
	// Method is GET, or POST for the few reads served by POST (a dashboard
	// chart's data).
	Method string
	Path   string
	Query  url.Values
	Body   any
	// AppID and ModelID select the application and model, as the console's
	// X-App-Id and X-Model-Id headers do.
	AppID, ModelID string
}

// Response is a route's answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Reader makes a read as subject, the identity a verified connector token
// names. The gateway implements it by running the request through its own
// middleware and handlers (internal/gateway/mcp.go).
type Reader interface {
	Read(ctx context.Context, subject string, req Request) (Response, error)
}

// Error codes. NotFound answers both an object that does not exist and one
// the person may not read: the routes do the same, and a different answer
// would confirm what they may not see.
const (
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid_request"
	CodeUnsupported  = "unsupported"
	CodeUnauthorized = "unauthorized"
	CodeUnavailable  = "unavailable"
)

// Error is a refusal shown to the person. Its message never says more than
// the routes would have said to them.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func notFound(what string) *Error {
	return &Error{Code: CodeNotFound, Message: what + " not found, or not accessible to you"}
}

func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

var errUnavailable = &Error{Code: CodeUnavailable, Message: "the read could not be completed; try again"}

// Service reads for any subject; As binds it to one.
type Service struct {
	reader  Reader
	cursors *cursorCodec
	now     func() time.Time
}

// New returns a Service reading through r.
func New(r Reader) *Service {
	return &Service{reader: r, cursors: newCursorCodec(), now: time.Now}
}

// Session is a Service bound to one verified subject. It holds nothing
// between calls: every method reads afresh, so a grant or rule changed since
// the last call applies to the next one.
type Session struct {
	s       *Service
	subject string
}

// As binds the service to subject.
func (s *Service) As(subject string) *Session { return &Session{s: s, subject: subject} }

// get makes req and decodes a 200 answer into out (when non-nil).
func (s *Session) get(ctx context.Context, what string, req Request, out any) (http.Header, error) {
	if s.subject == "" {
		return nil, &Error{Code: CodeUnauthorized, Message: "this connection is not signed in"}
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	resp, err := s.s.reader.Read(ctx, s.subject, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Code: CodeUnavailable, Message: "the read took too long and was stopped"}
		}
		return nil, errUnavailable
	}
	switch resp.Status {
	case http.StatusOK:
		if out != nil {
			if err := json.Unmarshal(resp.Body, out); err != nil {
				return nil, errUnavailable
			}
		}
		return resp.Header, nil
	case http.StatusUnauthorized:
		return nil, &Error{Code: CodeUnauthorized, Message: "your account is not active in maverickbuilds.app or no longer signs in; reconnect"}
	case http.StatusPaymentRequired:
		return nil, &Error{Code: CodeUnavailable, Message: "the workspace's plan does not allow this right now"}
	case http.StatusForbidden, http.StatusNotFound:
		return nil, notFound(what)
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return nil, invalid("%s", routeMessage(resp.Body))
	default:
		return nil, errUnavailable
	}
}

// routeMessage is the error message of a route's JSON error body.
func routeMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return "the request was not valid"
}

// ── models and context ──────────────────────────────────────────────────────

// ModelRef is a model the person may open.
type ModelRef struct {
	TenantID        string `json:"tenant_id,omitempty"`
	TenantName      string `json:"tenant_name,omitempty"`
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	Workspace       string `json:"workspace,omitempty"`
	ModelID         string `json:"model_id"`
	ModelName       string `json:"model_name"`
	IsDefault       bool   `json:"is_default"`
	// ActiveRevision is the active revision's name; its id comes with a
	// pinned context (Pin).
	ActiveRevision string `json:"active_revision,omitempty"`
}

// ListModels lists every model the person may open, across their tenants,
// optionally narrowed to names containing search.
func (s *Session) ListModels(ctx context.Context, search string) ([]ModelRef, error) {
	var apps []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		WorkspaceName string `json:"workspace_name"`
		TenantID      string `json:"tenant_id"`
		TenantName    string `json:"tenant_name"`
		Models        []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			IsDefault      bool   `json:"is_default"`
			ActiveRevision string `json:"active_revision"`
		} `json:"models"`
	}
	if _, err := s.get(ctx, "applications", Request{Path: "/api/apps"}, &apps); err != nil {
		return nil, err
	}
	out := []ModelRef{}
	for _, a := range apps {
		for _, m := range a.Models {
			ref := ModelRef{
				TenantID: a.TenantID, TenantName: a.TenantName,
				ApplicationID: a.ID, ApplicationName: a.Name, Workspace: a.WorkspaceName,
				ModelID: m.ID, ModelName: m.Name, IsDefault: m.IsDefault, ActiveRevision: m.ActiveRevision,
			}
			if matches(search, a.Name, m.Name, a.WorkspaceName, a.TenantName) {
				out = append(out, ref)
			}
		}
	}
	return out, nil
}

// Pinned is a confirmed read context: an application and model the person
// may open, and the revision every read in it is made against.
type Pinned struct {
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ModelID         string `json:"model_id"`
	ModelName       string `json:"model_name"`
	RevisionID      string `json:"revision_id"`
	RevisionName    string `json:"revision_name"`
}

func (p Pinned) request(path string, q url.Values) Request {
	return Request{Path: path, Query: q, AppID: p.ApplicationID, ModelID: p.ModelID}
}

// Pin confirms appID/modelID against the models the person may open and
// resolves the revision the reads are made against: the model's active
// revision, the one the business console reads. A revisionID naming any
// other revision is refused — a revision being built is the builders',
// and this connection reads what the model's users read.
func (s *Session) Pin(ctx context.Context, appID, modelID, revisionID string) (Pinned, error) {
	if appID == "" || modelID == "" {
		return Pinned{}, invalid("application_id and model_id are required; list_models returns them")
	}
	models, err := s.ListModels(ctx, "")
	if err != nil {
		return Pinned{}, err
	}
	var ref *ModelRef
	for i := range models {
		if models[i].ApplicationID == appID && models[i].ModelID == modelID {
			ref = &models[i]
			break
		}
	}
	if ref == nil {
		return Pinned{}, notFound("model")
	}
	// /api/demo answers the context the headers resolve to: the model
	// itself, unless access to it ended in between — then another model,
	// which is refused here rather than read under this one's name.
	var demo struct {
		AppID      string `json:"app_id"`
		ModelID    string `json:"model_id"`
		RevisionID string `json:"revision_id"`
		Revision   string `json:"revision"`
	}
	if _, err := s.get(ctx, "model", Request{Path: "/api/demo", AppID: appID, ModelID: modelID}, &demo); err != nil {
		return Pinned{}, err
	}
	if demo.ModelID != modelID || demo.AppID != appID || demo.RevisionID == "" {
		return Pinned{}, notFound("model")
	}
	if revisionID != "" && revisionID != demo.RevisionID {
		return Pinned{}, invalid("only the model's active revision (%s) can be read through this connection", demo.RevisionID)
	}
	return Pinned{
		ApplicationID: appID, ApplicationName: ref.ApplicationName,
		ModelID: modelID, ModelName: ref.ModelName,
		RevisionID: demo.RevisionID, RevisionName: demo.Revision,
	}, nil
}

// ── access summary ──────────────────────────────────────────────────────────

// Access describes what this connection can read for the person: their
// roles, and each resource family with how its reads are decided. It names
// no one — not even the person, whom the host already knows — and lists
// nothing about anyone else or any object they may not read.
type Access struct {
	Roles    []string       `json:"roles"`
	ReadOnly bool           `json:"read_only"`
	Families []AccessFamily `json:"families"`
	Note     string         `json:"note"`
}

// AccessFamily is one kind of resource this connection reads.
type AccessFamily struct {
	Family    string   `json:"family"`
	Tools     []string `json:"tools"`
	Available bool     `json:"available"`
	DecidedBy string   `json:"decided_by"`
}

// Access summarizes the person's reach through this connection.
func (s *Session) Access(ctx context.Context) (Access, error) {
	var a Access
	var me struct {
		Roles []string `json:"roles"`
	}
	if _, err := s.get(ctx, "account", Request{Path: "/api/me"}, &me); err != nil {
		return a, err
	}
	a.Roles = me.Roles
	if a.Roles == nil {
		a.Roles = []string{}
	}
	a.ReadOnly = true
	a.Note = "Every read is made as you, under the access your administrators and developers configured in maverickbuilds.app, and is decided again on every call. This connection reads grid data of each model's active revision; it never changes data, and charts and reports made from it are not saved in maverickbuilds.app."
	a.Families = []AccessFamily{
		{Family: "models", Tools: []string{"list_models"}, Available: true,
			DecidedBy: "your application and model grants"},
		{Family: "grids", Tools: []string{"list_sources", "describe_source", "list_members", "query_grid", "compare_grid", "render_chart", "render_report"}, Available: true,
			DecidedBy: "model access; hidden members and metrics are left out, and values that depend on them are withheld"},
	}
	return a, nil
}

// ── shared helpers ──────────────────────────────────────────────────────────

// matches reports whether search (case-insensitive) occurs in any field;
// an empty search matches everything.
func matches(search string, fields ...string) bool {
	search = strings.ToLower(strings.TrimSpace(search))
	if search == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), search) {
			return true
		}
	}
	return false
}
