package recordpost

// The checks of crudapp's field rules that need the database: the roles a
// person holds for a table's transitions, the people a person field may
// name, and the members a table keeping a dimension marks inactive. A table
// with none of the rules reads nothing here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
)

// SetAdmin records that the view's person administers the table's
// application: they make every move a transition names roles for.
func (v *AccessView) SetAdmin(admin bool) {
	if v != nil {
		v.admin = admin
	}
}

// Holds reports whether the view's person holds one of roles (true for no
// roles, and for an administrator of the application).
func (v *AccessView) Holds(roles []string) bool {
	if len(roles) == 0 {
		return true
	}
	if v == nil {
		return false
	}
	if v.admin {
		return true
	}
	return slices.ContainsFunc(roles, func(r string) bool { return v.heldRoles[r] })
}

// resolveTransitionRoles reads which of the roles form's transitions name
// userID holds in the table's application.
func (ps *Poster) resolveTransitionRoles(ctx context.Context, v *AccessView, userID string, form *crudapp.FormDef) error {
	roles := form.TransitionRoles()
	if len(roles) == 0 || userID == "" {
		return nil
	}
	appID, err := ps.appOfForm(ctx, form.ID)
	if err != nil {
		return fmt.Errorf("resolve the table's application: %w", err)
	}
	v.heldRoles = map[string]bool{}
	for _, r := range roles {
		raw, _ := json.Marshal([]string{r})
		var holds bool
		if err := ps.db(ctx).QueryRow(ctx, `SELECT `+assignee.SQL("$1::uuid", "$2::jsonb", "$3::uuid"), appID, string(raw), userID).Scan(&holds); err != nil {
			return fmt.Errorf("resolve who makes a table's moves: %w", err)
		}
		v.heldRoles[r] = holds
	}
	return nil
}

// Person is someone a person field may name: their id and name (never
// their e-mail address: owner decision 2026-11-20).
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// personNameSQL is how a person is named to the people they work with.
const personNameSQL = `COALESCE(NULLIF(btrim(u.display_name), ''), 'Unnamed person')`

// ChoosablePeople lists the people a person field naming roles may name
// in application appID: those who hold one of roles there (no roles:
// everyone who reaches it), as a workflow step naming them is assigned —
// no disabled account and no stand-in.
func (ps *Poster) ChoosablePeople(ctx context.Context, appID string, roles []string) ([]Person, error) {
	if roles == nil {
		roles = []string{}
	}
	raw, _ := json.Marshal(roles)
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT u.id::text, `+personNameSQL+`
		FROM identity."user" u
		WHERE u.disabled_at IS NULL AND NOT u.stand_in
		  AND `+assignee.SQL("$1::uuid", "$2::jsonb", "u.id")+`
		ORDER BY 2, 1`, appID, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PeopleNamed are the names of the people ids, by id (ids naming nobody
// are left out).
func (ps *Poster) PeopleNamed(ctx context.Context, ids []string) (map[string]string, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if uuid.Validate(id) == nil && !slices.Contains(valid, id) {
			valid = append(valid, id)
		}
	}
	out := map[string]string{}
	if len(valid) == 0 {
		return out, nil
	}
	rows, err := ps.db(ctx).Query(ctx, `SELECT u.id::text, `+personNameSQL+` FROM identity."user" u WHERE u.id = ANY($1::uuid[])`, valid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// personValue is the id person field f of form holds for v — a person's
// id, or the name of exactly one person the field may name (a file names
// people as they read them) — or the sentence refusing it.
func (ps *Poster) personValue(ctx context.Context, form *crudapp.FormDef, f crudapp.FormField, v any, cache *MemberCache) (string, string, error) {
	raw := strings.TrimSpace(fmt.Sprint(v))
	label := firstNonEmpty(f.Label, f.Name)
	people, err := cache.people(ctx, ps, form, f)
	if err != nil {
		return "", "", err
	}
	if uuid.Validate(raw) == nil {
		if slices.ContainsFunc(people, func(p Person) bool { return strings.EqualFold(p.ID, raw) }) {
			return strings.ToLower(raw), "", nil
		}
		return "", fmt.Sprintf("%s cannot name this person: %s", label, whoMayBeNamed(f)), nil
	}
	var match []Person
	for _, p := range people {
		if strings.EqualFold(p.Name, raw) {
			match = append(match, p)
		}
	}
	switch len(match) {
	case 1:
		return match[0].ID, "", nil
	case 0:
		return "", fmt.Sprintf("%q is not someone %s can name (%s)", raw, label, whoMayBeNamed(f)), nil
	}
	return "", fmt.Sprintf("%d people are called %q: choose %s in the form", len(match), raw, label), nil
}

func whoMayBeNamed(f crudapp.FormField) string {
	if len(f.Roles) == 0 {
		return "it names people who use this application"
	}
	return "it names holders of " + strings.Join(f.Roles, ", ")
}

// people are the people field f of form may name, read once per cache.
func (c *MemberCache) people(ctx context.Context, ps *Poster, form *crudapp.FormDef, f crudapp.FormField) ([]Person, error) {
	key := form.ID + "/" + f.Name
	if p, ok := c.choosable[key]; ok {
		return p, nil
	}
	appID, err := ps.appOfForm(ctx, form.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve the table's application: %w", err)
	}
	p, err := ps.ChoosablePeople(ctx, appID, f.Roles)
	if err != nil {
		return nil, err
	}
	if c.choosable == nil {
		c.choosable = map[string][]Person{}
	}
	c.choosable[key] = p
	return p, nil
}

// InactiveCodes are the members of dimension dimID the table keeping it,
// in form's revision, marks inactive (its active_flag field unticked); nil
// when no such table has an active_flag field.
func (ps *Poster) InactiveCodes(ctx context.Context, form *crudapp.FormDef, dimID string) (map[string]bool, error) {
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT k.id::text, k.fields FROM model.form_def k
		JOIN model.form_def f ON f.id = $2::uuid
		WHERE k.keeps_dimension_id = $1::uuid AND k.model_id = f.model_id
		  AND k.revision_id IS NOT DISTINCT FROM f.revision_id`, dimID, form.ID)
	if err != nil {
		return nil, err
	}
	type keeper struct {
		id         string
		code, flag string
	}
	var keepers []keeper
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		kf := crudapp.FormDef{}
		_ = json.Unmarshal(raw, &kf.Fields)
		cf := kf.CodeField()
		for _, fd := range kf.Fields {
			if fd.ActiveFlag && cf != nil {
				keepers = append(keepers, keeper{id: id, code: cf.Name, flag: fd.Name})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(keepers) == 0 {
		return nil, nil
	}
	out := map[string]bool{}
	for _, k := range keepers {
		codes, err := ps.db(ctx).Query(ctx, `
			SELECT btrim(data->>$2) FROM runtime.form_record
			WHERE form_id = $1::uuid AND lower(btrim(data->>$3)) = 'false' AND COALESCE(btrim(data->>$2), '') <> ''`,
			k.id, k.code, k.flag)
		if err != nil {
			return nil, err
		}
		for codes.Next() {
			var code string
			if err := codes.Scan(&code); err != nil {
				codes.Close()
				return nil, err
			}
			out[code] = true
		}
		codes.Close()
		if err := codes.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// inactive reports whether code is a member of dimID marked inactive, read
// once per dimension per cache.
func (c *MemberCache) inactive(ctx context.Context, ps *Poster, form *crudapp.FormDef, dimID, code string) (bool, error) {
	key := form.ID + "/" + dimID
	codes, ok := c.inactiveCodes[key]
	if !ok {
		var err error
		if codes, err = ps.InactiveCodes(ctx, form, dimID); err != nil {
			return false, err
		}
		if c.inactiveCodes == nil {
			c.inactiveCodes = map[string]map[string]bool{}
		}
		c.inactiveCodes[key] = codes
	}
	return codes[code], nil
}

// PersonOfMember is the person the record keeping member code of dimension
// dimID names in its first person field, in the table keeping dimID in
// form's revision ("" when no such table, field, record or person): who a
// dimension field with notify tells — a ticket's assignee is an agent
// record, and its person is told.
func (ps *Poster) PersonOfMember(ctx context.Context, form *crudapp.FormDef, dimID, code string) (string, error) {
	var keeperID string
	var raw []byte
	err := ps.db(ctx).QueryRow(ctx, `
		SELECT k.id::text, k.fields FROM model.form_def k
		JOIN model.form_def f ON f.id = $2::uuid
		WHERE k.keeps_dimension_id = $1::uuid AND k.model_id = f.model_id
		  AND k.revision_id IS NOT DISTINCT FROM f.revision_id
		LIMIT 1`, dimID, form.ID).Scan(&keeperID, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	keeper := crudapp.FormDef{}
	_ = json.Unmarshal(raw, &keeper.Fields)
	cf := keeper.CodeField()
	var person string
	for _, f := range keeper.Fields {
		if f.Type == crudapp.FieldPerson {
			person = f.Name
			break
		}
	}
	if cf == nil || person == "" {
		return "", nil
	}
	var id string
	err = ps.db(ctx).QueryRow(ctx, `
		SELECT COALESCE(btrim(data->>$3), '') FROM runtime.form_record
		WHERE form_id = $1::uuid AND btrim(data->>$2) = $4 LIMIT 1`, keeperID, cf.Name, person, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// valueLabel names a field's value as people read it, for a rule's sentence:
// a dimension field's member by its label (the code when it has none, or the
// members cannot be read), anything else as it is.
func (c *MemberCache) valueLabel(ctx context.Context, ps *Poster) func(f crudapp.FormField, v string) string {
	return func(f crudapp.FormField, v string) string {
		if f.Type != "dimension" || f.DimensionID == "" || v == "" {
			return v
		}
		m, err := c.load(ctx, ps, f.DimensionID)
		if err != nil || m.label[v] == "" {
			return v
		}
		return m.label[v]
	}
}
