package scim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Provider is the identity-provider side of provisioning — the same calls
// the console's Users screen makes. *keycloak.Client satisfies it; a nil
// Provider (the dev stack) creates accounts with synthetic subjects that
// X-Dev-User accepts, exactly as the console does there.
type Provider interface {
	FindUserByEmail(ctx context.Context, email string) (string, error)
	CreateUser(ctx context.Context, email, firstName, lastName string) (string, error)
	AssignRealmRole(ctx context.Context, sub, role string) error
	SendInvite(ctx context.Context, sub string, lifetime time.Duration) error
	DeleteUser(ctx context.Context, sub string) error
	SetUserEnabled(ctx context.Context, sub string, enabled bool) error
	UpdateUserProfile(ctx context.Context, sub, email, firstName, lastName string) error
}

// Service serves /scim/v2 for ONE tenant: the gateway authenticates the
// bearer token, routes the request to the tenant's database and builds a
// Service for it. Nothing here re-checks tenancy; everything it touches is
// already scoped by Pool and CustomerID.
type Service struct {
	Pool       *pgxpool.Pool
	IdP        Provider
	CustomerID string
	// DefaultRole is the platform role a provisioned user gets, and
	// WorkspaceID the workspace that role and every group live in (the
	// tenant's first workspace when empty).
	DefaultRole string
	WorkspaceID string
	// InviteNewUsers sends the set-your-password mail to a created user.
	// Off when the tenant signs in through its own identity provider: such
	// a user never has a password here.
	InviteNewUsers bool
	InviteLifetime time.Duration
	// Directory hooks the gateway supplies (tenant routing needs to know
	// which database a subject belongs to).
	OnUserCreated func(ctx context.Context, sub, email string)
	OnUserDeleted func(ctx context.Context, sub string)
	// CanCreateUser, when set, is asked before a user is created; an error
	// refuses the creation with its message (the tenant's plan limit).
	CanCreateUser func(ctx context.Context) error
	Log           zerolog.Logger
}

const (
	schemaUser      = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup     = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaList      = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaPatch     = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaError     = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaSPConfig  = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaResType   = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema    = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	contentType     = "application/scim+json; charset=utf-8"
	basePath        = "/api/scim/v2"
	maxPage         = 500
	defaultPage     = 100
	syntheticPrefix = "scim-created-"
)

// ── wire shapes ───────────────────────────────────────────────────────────────

type scimName struct {
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
	Formatted  string `json:"formatted,omitempty"`
}

type scimEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary"`
}

type scimRef struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Ref     string `json:"$ref,omitempty"`
}

type meta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location"`
}

type userResource struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id"`
	ExternalID  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	Name        *scimName   `json:"name,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
	Emails      []scimEmail `json:"emails,omitempty"`
	Active      bool        `json:"active"`
	Groups      []scimRef   `json:"groups"`
	Meta        meta        `json:"meta"`
}

type groupResource struct {
	Schemas     []string  `json:"schemas"`
	ID          string    `json:"id"`
	ExternalID  string    `json:"externalId,omitempty"`
	DisplayName string    `json:"displayName"`
	Members     []scimRef `json:"members"`
	Meta        meta      `json:"meta"`
}

type listResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

// ── errors ────────────────────────────────────────────────────────────────────

type scimErr struct {
	status int
	typ    string
	detail string
}

func (e *scimErr) Error() string { return e.detail }

func bad(typ, detail string) error { return &scimErr{http.StatusBadRequest, typ, detail} }
func notFound(what string) error   { return &scimErr{http.StatusNotFound, "", what + " not found"} }
func conflict(detail string) error { return &scimErr{http.StatusConflict, "uniqueness", detail} }
func upstream(err error) error {
	return &scimErr{http.StatusBadGateway, "", "identity provider: " + err.Error()}
}
func internal(err error) error { return &scimErr{http.StatusInternalServerError, "", err.Error()} }
func writeErr(w http.ResponseWriter, err error) {
	var se *scimErr
	if !errors.As(err, &se) {
		se = &scimErr{http.StatusInternalServerError, "", err.Error()}
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(se.status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemas": []string{schemaError}, "status": strconv.Itoa(se.status), "scimType": se.typ, "detail": se.detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── routing ───────────────────────────────────────────────────────────────────

// ServeHTTP dispatches everything under /scim/v2.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.WorkspaceID == "" {
		if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM core.workspace WHERE customer_id = $1::uuid ORDER BY created_at LIMIT 1`, s.CustomerID).Scan(&s.WorkspaceID); err != nil {
			writeErr(w, internal(fmt.Errorf("this tenant has no workspace to provision into")))
			return
		}
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, basePath), "/")
	parts := strings.SplitN(rest, "/", 2)
	resource, id := parts[0], ""
	if len(parts) == 2 {
		id = parts[1]
	}
	var err error
	switch {
	case resource == "ServiceProviderConfig" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, serviceProviderConfig())
	case resource == "ResourceTypes" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, list(resourceTypes(), 1, len(resourceTypes())))
	case resource == "Schemas" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, list(schemas(), 1, len(schemas())))
	case resource == "Users":
		err = s.users(w, r, id)
	case resource == "Groups":
		err = s.groups(w, r, id)
	default:
		err = notFound("resource")
	}
	if err != nil {
		writeErr(w, err)
	}
}

func list(items []any, start, total int) listResponse {
	return listResponse{Schemas: []string{schemaList}, TotalResults: total, StartIndex: start, ItemsPerPage: len(items), Resources: items}
}

func page(r *http.Request, n int) (start, count int) {
	start, _ = strconv.Atoi(r.URL.Query().Get("startIndex"))
	if start < 1 {
		start = 1
	}
	count, _ = strconv.Atoi(r.URL.Query().Get("count"))
	if count <= 0 {
		count = defaultPage
	}
	if count > maxPage {
		count = maxPage
	}
	_ = n
	return start, count
}

func slicePage[T any](all []T, start, count int) []any {
	out := []any{}
	for i := start - 1; i < len(all) && i < start-1+count; i++ {
		out = append(out, all[i])
	}
	return out
}

// ── users ─────────────────────────────────────────────────────────────────────

type userRow struct {
	ID, Sub, Email, DisplayName, ExternalID string
	DisabledAt                              *time.Time
	CreatedAt, UpdatedAt                    time.Time
}

// scopeSQL is the membership definition the console uses: the tenant's own
// users, plus anyone holding a role in one of its workspaces (invited users
// created before customer_id was set carry roles but no customer).
const scopeSQL = `(u.customer_id = $1::uuid OR EXISTS (
	SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
	WHERE ra.user_id = u.id AND w.customer_id = $1::uuid))`

func (s *Service) loadUsers(ctx context.Context, where string, args ...any) ([]userRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT u.id::text, u.keycloak_sub, u.email, u.display_name, COALESCE(u.external_id,''), u.disabled_at, u.created_at, u.updated_at
		FROM identity.user u WHERE `+scopeSQL+where+` ORDER BY u.created_at`, append([]any{s.CustomerID}, args...)...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.ID, &u.Sub, &u.Email, &u.DisplayName, &u.ExternalID, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, internal(err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Service) loadUser(ctx context.Context, id string) (userRow, error) {
	if !uuidShaped(id) {
		return userRow{}, notFound("user")
	}
	users, err := s.loadUsers(ctx, ` AND u.id = $2::uuid`, id)
	if err != nil {
		return userRow{}, err
	}
	if len(users) == 0 {
		return userRow{}, notFound("user")
	}
	return users[0], nil
}

func (s *Service) userGroups(ctx context.Context, userID string) []scimRef {
	rows, err := s.Pool.Query(ctx, `
		SELECT br.id::text, br.name FROM identity.business_role_member brm
		JOIN identity.business_role br ON br.id = brm.role_id
		WHERE brm.user_id = $1::uuid AND br.workspace_id = $2::uuid ORDER BY br.name`, userID, s.WorkspaceID)
	if err != nil {
		return []scimRef{}
	}
	defer rows.Close()
	out := []scimRef{}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out = append(out, scimRef{Value: id, Display: name, Ref: basePath + "/Groups/" + id})
		}
	}
	return out
}

func (s *Service) userResource(ctx context.Context, u userRow) userResource {
	first, last := splitName(u.DisplayName)
	return userResource{
		Schemas:     []string{schemaUser},
		ID:          u.ID,
		ExternalID:  u.ExternalID,
		UserName:    u.Email,
		Name:        &scimName{GivenName: first, FamilyName: last, Formatted: u.DisplayName},
		DisplayName: u.DisplayName,
		Emails:      []scimEmail{{Value: u.Email, Type: "work", Primary: true}},
		Active:      u.DisabledAt == nil,
		Groups:      s.userGroups(ctx, u.ID),
		Meta:        meta{ResourceType: "User", Created: u.CreatedAt, LastModified: u.UpdatedAt, Location: basePath + "/Users/" + u.ID},
	}
}

// userInput is what a create/replace/patch resolves to before persisting.
type userInput struct {
	Email, DisplayName, ExternalID string
	Active                         bool
	activeSet                      bool
}

func decodeUser(body []byte) (userInput, error) {
	var in struct {
		UserName    string      `json:"userName"`
		ExternalID  string      `json:"externalId"`
		DisplayName string      `json:"displayName"`
		Name        *scimName   `json:"name"`
		Emails      []scimEmail `json:"emails"`
		Active      *bool       `json:"active"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return userInput{}, bad("invalidSyntax", "body is not a SCIM user: "+err.Error())
	}
	u := userInput{Email: strings.ToLower(strings.TrimSpace(in.UserName)), ExternalID: in.ExternalID, DisplayName: in.DisplayName, Active: true}
	if u.Email == "" || !strings.Contains(u.Email, "@") {
		for _, e := range in.Emails {
			if e.Primary || u.Email == "" {
				u.Email = strings.ToLower(strings.TrimSpace(e.Value))
			}
		}
	}
	if u.DisplayName == "" && in.Name != nil {
		u.DisplayName = strings.TrimSpace(strings.TrimSpace(in.Name.GivenName) + " " + strings.TrimSpace(in.Name.FamilyName))
		if u.DisplayName == "" {
			u.DisplayName = in.Name.Formatted
		}
	}
	if in.Active != nil {
		u.Active, u.activeSet = *in.Active, true
	}
	return u, nil
}

func (s *Service) users(w http.ResponseWriter, r *http.Request, id string) error {
	ctx := r.Context()
	switch {
	case id == "" && r.Method == http.MethodGet:
		clauses, err := parseFilter(r.URL.Query().Get("filter"))
		if err != nil {
			return bad("invalidFilter", err.Error())
		}
		where, args, err := userFilterSQL(clauses)
		if err != nil {
			return err
		}
		users, err := s.loadUsers(ctx, where, args...)
		if err != nil {
			return err
		}
		resources := make([]userResource, 0, len(users))
		for _, u := range users {
			resources = append(resources, s.userResource(ctx, u))
		}
		start, count := page(r, len(resources))
		writeJSON(w, http.StatusOK, list(slicePage(resources, start, count), start, len(resources)))
		return nil
	case id == "" && r.Method == http.MethodPost:
		body, _ := readBody(r)
		in, err := decodeUser(body)
		if err != nil {
			return err
		}
		u, err := s.createUser(ctx, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, s.userResource(ctx, u))
		return nil
	case id != "" && r.Method == http.MethodGet:
		u, err := s.loadUser(ctx, id)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.userResource(ctx, u))
		return nil
	case id != "" && r.Method == http.MethodPut:
		cur, err := s.loadUser(ctx, id)
		if err != nil {
			return err
		}
		body, _ := readBody(r)
		in, err := decodeUser(body)
		if err != nil {
			return err
		}
		if !in.activeSet {
			in.Active = cur.DisabledAt == nil
		}
		u, err := s.updateUser(ctx, cur, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.userResource(ctx, u))
		return nil
	case id != "" && r.Method == http.MethodPatch:
		cur, err := s.loadUser(ctx, id)
		if err != nil {
			return err
		}
		body, _ := readBody(r)
		in := userInput{Email: cur.Email, DisplayName: cur.DisplayName, ExternalID: cur.ExternalID, Active: cur.DisabledAt == nil}
		if err := applyUserPatch(body, &in); err != nil {
			return err
		}
		u, err := s.updateUser(ctx, cur, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.userResource(ctx, u))
		return nil
	case id != "" && r.Method == http.MethodDelete:
		cur, err := s.loadUser(ctx, id)
		if err != nil {
			return err
		}
		if err := s.deleteUser(ctx, cur); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return &scimErr{http.StatusMethodNotAllowed, "", "method not allowed"}
}

func userFilterSQL(clauses []clause) (string, []any, error) {
	var where strings.Builder
	var args []any
	for _, c := range clauses {
		n := len(args) + 2 // $1 is the customer id
		switch c.attr {
		case "username", "emails.value":
			fmt.Fprintf(&where, " AND lower(u.email) = lower($%d)", n)
		case "externalid":
			fmt.Fprintf(&where, " AND u.external_id = $%d", n)
		case "id":
			if !uuidShaped(c.value) {
				return " AND FALSE", nil, nil
			}
			fmt.Fprintf(&where, " AND u.id = $%d::uuid", n)
		case "displayname":
			fmt.Fprintf(&where, " AND lower(u.display_name) = lower($%d)", n)
		case "active":
			if strings.EqualFold(c.value, "true") {
				where.WriteString(" AND u.disabled_at IS NULL")
			} else {
				where.WriteString(" AND u.disabled_at IS NOT NULL")
			}
			continue
		default:
			return "", nil, bad("invalidFilter", "unsupported user attribute in filter: "+c.attr)
		}
		args = append(args, c.value)
	}
	return where.String(), args, nil
}

// createUser is the console's own provisioning sequence: identity-provider
// account first (adopting one that already exists), then the application
// row, the default role, the directory entry, and — only when the tenant
// has no SSO — the invitation. A failure after the account was created
// removes it again, so a retry from the identity provider never finds a
// half-made user.
func (s *Service) createUser(ctx context.Context, in userInput) (userRow, error) {
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		return userRow{}, bad("invalidValue", "userName must be the person's e-mail address")
	}
	var existingID string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE lower(email) = $1`, in.Email).Scan(&existingID); err == nil {
		return userRow{}, conflict("a user with userName " + in.Email + " already exists")
	}
	if s.CanCreateUser != nil {
		if err := s.CanCreateUser(ctx); err != nil {
			return userRow{}, &scimErr{http.StatusForbidden, "", err.Error()}
		}
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Email
	}
	first, last := splitName(in.DisplayName)
	sub := syntheticPrefix + in.Email
	createdInIdP := false
	if s.IdP != nil {
		existing, err := s.IdP.FindUserByEmail(ctx, in.Email)
		if err != nil {
			return userRow{}, upstream(err)
		}
		if existing == "" {
			if existing, err = s.IdP.CreateUser(ctx, in.Email, first, last); err != nil {
				return userRow{}, upstream(err)
			}
			createdInIdP = true
		}
		sub = existing
		if err := s.IdP.AssignRealmRole(ctx, sub, s.DefaultRole); err != nil {
			s.abortIdP(ctx, createdInIdP, sub)
			return userRow{}, upstream(err)
		}
	}
	var disabledAt *time.Time
	if !in.Active {
		now := time.Now()
		disabledAt = &now
	}
	var userID string
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id, external_id, scim_managed, disabled_at)
		VALUES ($1, $2, $3, $4::uuid, NULLIF($5,''), TRUE, $6)
		ON CONFLICT (keycloak_sub) DO UPDATE
		SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, external_id = EXCLUDED.external_id,
		    scim_managed = TRUE, disabled_at = EXCLUDED.disabled_at, customer_id = COALESCE(identity.user.customer_id, EXCLUDED.customer_id)
		RETURNING id::text
	`, sub, in.Email, in.DisplayName, s.CustomerID, in.ExternalID, disabledAt).Scan(&userID); err != nil {
		s.abortIdP(ctx, createdInIdP, sub)
		return userRow{}, internal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, $2::identity.user_role, $3::uuid) ON CONFLICT DO NOTHING`, userID, s.DefaultRole, s.WorkspaceID); err != nil {
		return userRow{}, internal(err)
	}
	if s.OnUserCreated != nil {
		s.OnUserCreated(ctx, sub, in.Email)
	}
	if s.IdP != nil && !in.Active {
		if err := s.IdP.SetUserEnabled(ctx, sub, false); err != nil {
			s.Log.Warn().Err(err).Str("email", in.Email).Msg("scim: user created inactive but the identity provider account is still enabled")
		}
	}
	if s.IdP != nil && s.InviteNewUsers && in.Active {
		if err := s.IdP.SendInvite(ctx, sub, s.InviteLifetime); err != nil {
			s.Log.Warn().Err(err).Str("email", in.Email).Msg("scim: user created but the invitation could not be sent")
		}
	}
	return s.loadUser(ctx, userID)
}

func (s *Service) abortIdP(ctx context.Context, created bool, sub string) {
	if created && s.IdP != nil {
		if err := s.IdP.DeleteUser(ctx, sub); err != nil {
			s.Log.Error().Err(err).Str("sub", sub).Msg("scim: could not remove the identity provider account after a failed creation")
		}
	}
}

func (s *Service) updateUser(ctx context.Context, cur userRow, in userInput) (userRow, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		return userRow{}, bad("invalidValue", "userName must be the person's e-mail address")
	}
	if in.DisplayName == "" {
		in.DisplayName = cur.DisplayName
	}
	if in.Email != cur.Email {
		var other string
		if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE lower(email) = $1 AND id <> $2::uuid`, in.Email, cur.ID).Scan(&other); err == nil {
			return userRow{}, conflict("a user with userName " + in.Email + " already exists")
		}
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE identity.user SET email = $2, display_name = $3, external_id = NULLIF($4,''),
		    disabled_at = CASE WHEN $5 THEN NULL ELSE COALESCE(disabled_at, now()) END,
		    scim_managed = TRUE, updated_at = now()
		WHERE id = $1::uuid`, cur.ID, in.Email, in.DisplayName, in.ExternalID, in.Active); err != nil {
		return userRow{}, internal(err)
	}
	if s.IdP != nil && !strings.HasPrefix(cur.Sub, syntheticPrefix) && !strings.HasPrefix(cur.Sub, "admin-created-") {
		wasActive := cur.DisabledAt == nil
		if wasActive != in.Active {
			if err := s.IdP.SetUserEnabled(ctx, cur.Sub, in.Active); err != nil {
				return userRow{}, upstream(err)
			}
		}
		if in.Email != cur.Email || in.DisplayName != cur.DisplayName {
			first, last := splitName(in.DisplayName)
			if err := s.IdP.UpdateUserProfile(ctx, cur.Sub, in.Email, first, last); err != nil {
				return userRow{}, upstream(err)
			}
		}
	}
	return s.loadUser(ctx, cur.ID)
}

func (s *Service) deleteUser(ctx context.Context, cur userRow) error {
	if s.OnUserDeleted != nil {
		s.OnUserDeleted(ctx, cur.Sub)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM identity.user WHERE id = $1::uuid`, cur.ID); err != nil {
		return internal(err)
	}
	if s.IdP != nil && !strings.HasPrefix(cur.Sub, syntheticPrefix) && !strings.HasPrefix(cur.Sub, "admin-created-") {
		if err := s.IdP.DeleteUser(ctx, cur.Sub); err != nil {
			s.Log.Error().Err(err).Str("user_id", cur.ID).Msg("scim: application user deleted but the identity provider account remains")
		}
	}
	return nil
}

// ── groups ────────────────────────────────────────────────────────────────────

type groupRow struct {
	ID, Name, ExternalID string
	CreatedAt            time.Time
}

func (s *Service) loadGroups(ctx context.Context, where string, args ...any) ([]groupRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT br.id::text, br.name, COALESCE(br.external_id,''), br.created_at
		FROM identity.business_role br WHERE br.workspace_id = $1::uuid`+where+` ORDER BY br.name`, append([]any{s.WorkspaceID}, args...)...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.ID, &g.Name, &g.ExternalID, &g.CreatedAt); err != nil {
			return nil, internal(err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) loadGroup(ctx context.Context, id string) (groupRow, error) {
	if !uuidShaped(id) {
		return groupRow{}, notFound("group")
	}
	groups, err := s.loadGroups(ctx, ` AND br.id = $2::uuid`, id)
	if err != nil {
		return groupRow{}, err
	}
	if len(groups) == 0 {
		return groupRow{}, notFound("group")
	}
	return groups[0], nil
}

func (s *Service) groupMembers(ctx context.Context, groupID string) []scimRef {
	rows, err := s.Pool.Query(ctx, `
		SELECT u.id::text, u.email FROM identity.business_role_member brm
		JOIN identity.user u ON u.id = brm.user_id WHERE brm.role_id = $1::uuid ORDER BY u.email`, groupID)
	if err != nil {
		return []scimRef{}
	}
	defer rows.Close()
	out := []scimRef{}
	for rows.Next() {
		var id, email string
		if rows.Scan(&id, &email) == nil {
			out = append(out, scimRef{Value: id, Display: email, Ref: basePath + "/Users/" + id})
		}
	}
	return out
}

func (s *Service) groupResource(ctx context.Context, g groupRow) groupResource {
	return groupResource{
		Schemas: []string{schemaGroup}, ID: g.ID, ExternalID: g.ExternalID, DisplayName: g.Name,
		Members: s.groupMembers(ctx, g.ID),
		Meta:    meta{ResourceType: "Group", Created: g.CreatedAt, LastModified: g.CreatedAt, Location: basePath + "/Groups/" + g.ID},
	}
}

type groupInput struct {
	DisplayName, ExternalID string
	Members                 []string // user ids
	membersSet              bool
}

func decodeGroup(body []byte) (groupInput, error) {
	var in struct {
		DisplayName string    `json:"displayName"`
		ExternalID  string    `json:"externalId"`
		Members     []scimRef `json:"members"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return groupInput{}, bad("invalidSyntax", "body is not a SCIM group: "+err.Error())
	}
	g := groupInput{DisplayName: strings.TrimSpace(in.DisplayName), ExternalID: in.ExternalID}
	if in.Members != nil {
		g.membersSet = true
		for _, m := range in.Members {
			g.Members = append(g.Members, m.Value)
		}
	}
	return g, nil
}

func (s *Service) groups(w http.ResponseWriter, r *http.Request, id string) error {
	ctx := r.Context()
	switch {
	case id == "" && r.Method == http.MethodGet:
		clauses, err := parseFilter(r.URL.Query().Get("filter"))
		if err != nil {
			return bad("invalidFilter", err.Error())
		}
		var where strings.Builder
		var args []any
		for _, c := range clauses {
			n := len(args) + 2
			switch c.attr {
			case "displayname":
				fmt.Fprintf(&where, " AND lower(br.name) = lower($%d)", n)
			case "externalid":
				fmt.Fprintf(&where, " AND br.external_id = $%d", n)
			case "id":
				if !uuidShaped(c.value) {
					where.WriteString(" AND FALSE")
					continue
				}
				fmt.Fprintf(&where, " AND br.id = $%d::uuid", n)
			default:
				return bad("invalidFilter", "unsupported group attribute in filter: "+c.attr)
			}
			args = append(args, c.value)
		}
		groups, err := s.loadGroups(ctx, where.String(), args...)
		if err != nil {
			return err
		}
		resources := make([]groupResource, 0, len(groups))
		for _, g := range groups {
			resources = append(resources, s.groupResource(ctx, g))
		}
		start, count := page(r, len(resources))
		writeJSON(w, http.StatusOK, list(slicePage(resources, start, count), start, len(resources)))
		return nil
	case id == "" && r.Method == http.MethodPost:
		body, _ := readBody(r)
		in, err := decodeGroup(body)
		if err != nil {
			return err
		}
		if in.DisplayName == "" {
			return bad("invalidValue", "displayName is required")
		}
		var gid string
		err = s.Pool.QueryRow(ctx, `
			INSERT INTO identity.business_role (workspace_id, name, external_id) VALUES ($1::uuid, $2, NULLIF($3,''))
			RETURNING id::text`, s.WorkspaceID, in.DisplayName, in.ExternalID).Scan(&gid)
		if err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				return conflict("a group named " + in.DisplayName + " already exists")
			}
			return internal(err)
		}
		if err := s.setMembers(ctx, gid, in.Members); err != nil {
			return err
		}
		g, err := s.loadGroup(ctx, gid)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, s.groupResource(ctx, g))
		return nil
	case id != "" && r.Method == http.MethodGet:
		g, err := s.loadGroup(ctx, id)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.groupResource(ctx, g))
		return nil
	case id != "" && r.Method == http.MethodPut:
		g, err := s.loadGroup(ctx, id)
		if err != nil {
			return err
		}
		body, _ := readBody(r)
		in, err := decodeGroup(body)
		if err != nil {
			return err
		}
		if in.DisplayName == "" {
			in.DisplayName = g.Name
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE identity.business_role SET name = $2, external_id = NULLIF($3,'') WHERE id = $1::uuid`, g.ID, in.DisplayName, in.ExternalID); err != nil {
			return internal(err)
		}
		if in.membersSet {
			if err := s.setMembers(ctx, g.ID, in.Members); err != nil {
				return err
			}
		}
		g, _ = s.loadGroup(ctx, id)
		writeJSON(w, http.StatusOK, s.groupResource(ctx, g))
		return nil
	case id != "" && r.Method == http.MethodPatch:
		g, err := s.loadGroup(ctx, id)
		if err != nil {
			return err
		}
		body, _ := readBody(r)
		if err := s.applyGroupPatch(ctx, g, body); err != nil {
			return err
		}
		g, _ = s.loadGroup(ctx, id)
		writeJSON(w, http.StatusOK, s.groupResource(ctx, g))
		return nil
	case id != "" && r.Method == http.MethodDelete:
		g, err := s.loadGroup(ctx, id)
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, `DELETE FROM identity.business_role WHERE id = $1::uuid`, g.ID); err != nil {
			return internal(err)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return &scimErr{http.StatusMethodNotAllowed, "", "method not allowed"}
}

// setMembers replaces a group's membership. Only users of this tenant may
// be members; an unknown id is a 400 so the identity provider sees which
// reference it got wrong.
func (s *Service) setMembers(ctx context.Context, groupID string, userIDs []string) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM identity.business_role_member WHERE role_id = $1::uuid`, groupID); err != nil {
		return internal(err)
	}
	for _, uid := range userIDs {
		if err := s.addMember(ctx, groupID, uid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) addMember(ctx context.Context, groupID, userID string) error {
	if _, err := s.loadUser(ctx, userID); err != nil {
		return bad("invalidValue", "member "+userID+" is not a user of this tenant")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, groupID, userID)
	if err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) removeMember(ctx context.Context, groupID, userID string) error {
	if !uuidShaped(userID) {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM identity.business_role_member WHERE role_id = $1::uuid AND user_id = $2::uuid`, groupID, userID)
	if err != nil {
		return internal(err)
	}
	return nil
}

// ── discovery documents ───────────────────────────────────────────────────────

func serviceProviderConfig() map[string]any {
	return map[string]any{
		"schemas":          []string{schemaSPConfig},
		"documentationUri": "https://github.com/mavericks-engine/mavericks/blob/main/docs/SSO_SCIM.md",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": maxPage},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "OAuth Bearer Token",
			"description": "A SCIM token issued by the tenant admin under Admin › Provisioning, sent as Authorization: Bearer.",
			"primary":     true,
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": basePath + "/ServiceProviderConfig"},
	}
}

func resourceTypes() []any {
	return []any{
		map[string]any{"schemas": []string{schemaResType}, "id": "User", "name": "User", "endpoint": "/Users", "schema": schemaUser,
			"meta": map[string]any{"resourceType": "ResourceType", "location": basePath + "/ResourceTypes/User"}},
		map[string]any{"schemas": []string{schemaResType}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": schemaGroup,
			"meta": map[string]any{"resourceType": "ResourceType", "location": basePath + "/ResourceTypes/Group"}},
	}
}

func attr(name, typ string, multi bool, mutability string) map[string]any {
	return map[string]any{"name": name, "type": typ, "multiValued": multi, "required": false, "caseExact": false,
		"mutability": mutability, "returned": "default", "uniqueness": "none"}
}

func schemas() []any {
	user := map[string]any{"schemas": []string{schemaSchema}, "id": schemaUser, "name": "User", "description": "A person who can sign in to this tenant",
		"attributes": []map[string]any{
			attr("userName", "string", false, "readWrite"), attr("externalId", "string", false, "readWrite"),
			attr("displayName", "string", false, "readWrite"), attr("active", "boolean", false, "readWrite"),
			{"name": "name", "type": "complex", "multiValued": false, "required": false, "mutability": "readWrite", "returned": "default",
				"subAttributes": []map[string]any{attr("givenName", "string", false, "readWrite"), attr("familyName", "string", false, "readWrite"), attr("formatted", "string", false, "readWrite")}},
			{"name": "emails", "type": "complex", "multiValued": true, "required": false, "mutability": "readWrite", "returned": "default",
				"subAttributes": []map[string]any{attr("value", "string", false, "readWrite"), attr("type", "string", false, "readWrite"), attr("primary", "boolean", false, "readWrite")}},
			{"name": "groups", "type": "complex", "multiValued": true, "required": false, "mutability": "readOnly", "returned": "default",
				"subAttributes": []map[string]any{attr("value", "string", false, "readOnly"), attr("display", "string", false, "readOnly")}},
		},
		"meta": map[string]any{"resourceType": "Schema", "location": basePath + "/Schemas/" + schemaUser}}
	group := map[string]any{"schemas": []string{schemaSchema}, "id": schemaGroup, "name": "Group", "description": "A business role of this tenant",
		"attributes": []map[string]any{
			attr("displayName", "string", false, "readWrite"), attr("externalId", "string", false, "readWrite"),
			{"name": "members", "type": "complex", "multiValued": true, "required": false, "mutability": "readWrite", "returned": "default",
				"subAttributes": []map[string]any{attr("value", "string", false, "readWrite"), attr("display", "string", false, "readOnly")}},
		},
		"meta": map[string]any{"resourceType": "Schema", "location": basePath + "/Schemas/" + schemaGroup}}
	return []any{user, group}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close() //nolint:errcheck
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			return nil, bad("invalidSyntax", "body too large")
		}
	}
	return buf, nil
}

func splitName(display string) (first, last string) {
	d := strings.TrimSpace(display)
	if i := strings.Index(d, " "); i > 0 {
		return d[:i], strings.TrimSpace(d[i+1:])
	}
	return d, ""
}

func uuidShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			hexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !hexDigit {
				return false
			}
		}
	}
	return true
}

var _ = pgx.ErrNoRows
