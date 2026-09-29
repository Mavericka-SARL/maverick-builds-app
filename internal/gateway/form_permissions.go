package gateway

// What one caller may do with a form's records as a whole, served on every
// form GET /api/forms lists. It is computed from the same formRecordScope the
// record routes decide by — POST /api/forms/{id}/sync is the scope's admin's,
// and a create's status is createAccess().CanCreate's — so the interface
// offers Sync and a create status exactly when the server accepts them.

import (
	"context"
	"slices"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// formPermissions is what the caller may do with one form's records.
type formPermissions struct {
	// Sync: the caller may re-post the form's records into their metrics
	// (POST /api/forms/{id}/sync).
	Sync bool `json:"sync"`
	// CreateStatuses: the statuses the caller may create a record in, in
	// lifecycle order; empty when they may create none.
	CreateStatuses []string `json:"create_statuses"`
}

// formPermissions is what the scope's caller may do with its form's records.
func (s formRecordScope) formPermissions() formPermissions {
	p := formPermissions{Sync: s.reach && s.admin, CreateStatuses: []string{}}
	create := s.createAccess()
	for _, st := range crudapp.RecordStatuses {
		if create.CanCreate(st) {
			p.CreateStatuses = append(p.CreateStatuses, st)
		}
	}
	return p
}

// servedForm is a form as GET /api/forms serves it: its definition and what
// the caller may do with its records.
type servedForm struct {
	*crudapp.FormDef
	Permissions formPermissions `json:"permissions"`
}

// withFormPermissions pairs each form with act's permissions on it. A caller
// who cannot be resolved may do nothing. Reach and admin — all the
// permissions depend on — are the model's and its application's, so the
// scope is resolved once per model (GET /api/forms lists one model's forms)
// rather than once per form, through the same resolveFormRecordScope the
// sync and the create decide by.
func (h *handler) withFormPermissions(ctx context.Context, act *actor, forms []*crudapp.FormDef) ([]servedForm, error) {
	out := make([]servedForm, 0, len(forms))
	byModel := map[string]formPermissions{}
	for _, f := range forms {
		p := formRecordScope{}.formPermissions()
		if act != nil {
			cached, ok := byModel[f.ModelID]
			if !ok || f.ModelID == "" {
				scope, err := h.resolveFormRecordScope(ctx, act, f.ID)
				if err != nil {
					return nil, err
				}
				cached = scope.formPermissions()
				if f.ModelID != "" {
					byModel[f.ModelID] = cached
				}
			}
			p = formPermissions{Sync: cached.Sync, CreateStatuses: slices.Clone(cached.CreateStatuses)}
		}
		out = append(out, servedForm{FormDef: f, Permissions: p})
	}
	return out, nil
}
