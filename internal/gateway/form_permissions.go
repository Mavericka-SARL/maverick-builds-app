package gateway

// What one caller may do with a form's records as a whole, served on every
// form GET /api/forms lists. It is computed from the same formRecordScope the
// record routes decide by — POST /api/forms/{id}/sync is the scope's admin's,
// and a create's status is createAccess().CanCreate's — so the interface
// offers Sync and a create status exactly when the server accepts them.

import (
	"context"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// formPermissions is what the caller may do with one form's records.
type formPermissions struct {
	// Sync: the caller may re-post the form's records into their metrics
	// (POST /api/forms/{id}/sync).
	Sync bool `json:"sync"`
	// Create: the caller may create records (POST /api/forms/{id}/records,
	// an import).
	Create bool `json:"create"`
	// CreateStatuses: in a table with a status field, the statuses the
	// caller may create a record in, in lifecycle order; empty otherwise.
	CreateStatuses []string `json:"create_statuses"`
}

// formPermissions is what the scope's caller may do with its form's records.
func (s formRecordScope) formPermissions() formPermissions {
	p := formPermissions{Sync: s.reach && s.admin, CreateStatuses: []string{}}
	create := s.createAccess()
	if s.hasStatus() {
		for _, st := range crudapp.RecordStatuses {
			if create.CanCreate(st) {
				p.CreateStatuses = append(p.CreateStatuses, st)
			}
		}
	}
	p.Create = len(p.CreateStatuses) > 0 || create.CanCreate("")
	return p
}

// servedForm is a form as GET /api/forms serves it: its definition, what
// the caller may do with its records, and where its records post.
type servedForm struct {
	*crudapp.FormDef
	Permissions formPermissions `json:"permissions"`
	// PostingFields: each field a live mapping takes a member from, with
	// the statuses a record posts in ("" for a record without a status) —
	// where a member the person may only read cannot be chosen, since the
	// save would post into it.
	PostingFields map[string][]string `json:"posting_fields"`
	// Posts: the table has a live mapping, so its records post into a grid
	// (and a sync has something to do).
	Posts bool `json:"posts"`
}

// withFormPermissions pairs each form with act's permissions on it. A caller
// who cannot be resolved may do nothing. Reach and admin are the model's and
// its application's, so they are resolved once per model (GET /api/forms
// lists one model's forms) through the same resolveFormRecordScope the sync
// and the create decide by; what depends on the table itself — whether it
// has a status field — is the table's.
func (h *handler) withFormPermissions(ctx context.Context, act *actor, forms []*crudapp.FormDef) ([]servedForm, error) {
	out := make([]servedForm, 0, len(forms))
	byModel := map[string]formRecordScope{}
	for _, f := range forms {
		served := servedForm{FormDef: f, Permissions: formRecordScope{}.formPermissions()}
		if act != nil {
			scope, ok := byModel[f.ModelID]
			if !ok || f.ModelID == "" {
				var err error
				if scope, err = h.resolveFormRecordScope(ctx, act, f.ID); err != nil {
					return nil, err
				}
				if f.ModelID != "" {
					byModel[f.ModelID] = scope
				}
			}
			scope.formID, scope.form = f.ID, f
			served.Permissions = scope.formPermissions()
		}
		posting, posts, err := h.records().PostingOf(ctx, f)
		if err != nil {
			return nil, err
		}
		served.PostingFields, served.Posts = posting, posts
		out = append(out, served)
	}
	return out, nil
}
