package gateway

// Who may read, create, change and delete a form record. The rule itself
// ("submitter + admins") is crudapp.RecordAccess.Permissions; this file
// decides, for one caller and one form, the facts it takes: whether the
// caller reaches the form's application and whether they administer it.
// Every record route — list, read, create, update, delete, export, import —
// resolves a formRecordScope, and every record the API returns carries the
// permissions the same scope computes, so what the interface offers is what
// the server accepts.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// errRecordNotReached answers a record or form the caller does not reach
// the same way as one that does not exist: 404, confirming nothing.
var errRecordNotReached = errors.New("not found")

// formRecordScope is what one caller is to one form's records.
type formRecordScope struct {
	formID, modelID, appID, revisionID string
	userID                             string
	// reach: the caller opens the form's model the way the business console
	// does (actorCanAccessModel on a business route).
	reach bool
	// admin: the caller administers the application's records — a
	// business_admin of its workspace, or its builder and administrator
	// scope (a developer or tenant admin within their scope, a platform
	// admin: actorCanAccessModel on a builder route).
	admin bool
}

// resolveFormRecordScope decides reach and admin for act on formID. A form
// that does not exist, or a malformed id, resolves to a scope that reaches
// nothing.
func (h *handler) resolveFormRecordScope(ctx context.Context, act *actor, formID string) (formRecordScope, error) {
	s := formRecordScope{formID: formID, userID: act.UserID}
	if _, err := uuid.Parse(formID); err != nil {
		return s, nil
	}
	err := h.db.QueryRow(ctx, `
		SELECT fd.model_id::text, m.application_id::text, COALESCE(fd.revision_id::text, '')
		FROM model.form_def fd
		JOIN core.model m ON m.id = fd.model_id
		WHERE fd.id = $1::uuid`, formID).Scan(&s.modelID, &s.appID, &s.revisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("resolve form: %w", err)
	}

	// Records are the business surface: whatever route the request came
	// through, reaching them is the business console's reach.
	reach, err := h.actorCanAccessModel(context.WithValue(ctx, builderRouteKey{}, false), act, s.modelID)
	if err != nil || !reach {
		return s, err
	}
	s.reach = true

	admin, err := h.actorCanAccessModel(withBuilderRoute(ctx), act, s.modelID)
	if err != nil {
		return s, err
	}
	if !admin {
		// A business_admin of the application's workspace — of any
		// workspace of its tenant for a tenant-level application, which
		// belongs to all of them (roleReachesAppSQL's business arms, and
		// workflowAdminScopeSQL's business_admin arm). The per-user app and
		// model restrictions are reach's, checked above.
		err = h.db.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1
			    FROM core.application app
			    JOIN identity.role_assignment ra ON ra.user_id = $2::uuid AND ra.role = 'business_admin'
			    JOIN core.workspace rws ON rws.id = ra.workspace_id
			    WHERE app.id = $1::uuid
			      AND (rws.id = app.workspace_id
			           OR (app.workspace_id IS NULL AND rws.customer_id = app.customer_id))
			)`, s.appID, act.UserID).Scan(&admin)
		if err != nil {
			return s, fmt.Errorf("resolve record admin: %w", err)
		}
	}
	s.admin = admin
	return s, nil
}

// access is who the scope's caller is to rec.
func (s formRecordScope) access(rec *crudapp.FormRecord) crudapp.RecordAccess {
	return crudapp.RecordAccess{
		Reach:   s.reach,
		Admin:   s.admin,
		Creator: rec != nil && rec.CreatedBy != nil && *rec.CreatedBy == s.userID,
	}
}

// createAccess is who the scope's caller is to a record they are creating.
func (s formRecordScope) createAccess() crudapp.RecordAccess {
	return crudapp.RecordAccess{Reach: s.reach, Admin: s.admin, Creator: true}
}

// permit sets rec.Permissions for the scope's caller — the record permissions
// every record the API returns carries — and returns the access they were
// computed from, for the mutation check.
func (s formRecordScope) permit(rec *crudapp.FormRecord) crudapp.RecordAccess {
	a := s.access(rec)
	p := a.Permissions(rec.Status)
	rec.Permissions = &p
	return a
}

// reachRecord loads recordID for act, with its form's scope and the access
// act has to it. errRecordNotReached when the record does not exist, its
// form is outside act's reach, or act's access rules withhold it
// (filterFormRecords, as the record list does).
func (h *handler) reachRecord(ctx context.Context, act *actor, store *crudapp.Store, recordID string) (*crudapp.FormRecord, formRecordScope, crudapp.RecordAccess, error) {
	if _, err := uuid.Parse(recordID); err != nil {
		return nil, formRecordScope{}, crudapp.RecordAccess{}, errRecordNotReached
	}
	rec, err := store.GetRecord(ctx, recordID)
	if errors.Is(err, crudapp.ErrRecordNotFound) {
		return nil, formRecordScope{}, crudapp.RecordAccess{}, errRecordNotReached
	}
	if err != nil {
		return nil, formRecordScope{}, crudapp.RecordAccess{}, err
	}
	scope, err := h.resolveFormRecordScope(ctx, act, rec.FormID)
	if err != nil {
		return nil, scope, crudapp.RecordAccess{}, err
	}
	if !scope.reach {
		return nil, scope, crudapp.RecordAccess{}, errRecordNotReached
	}
	form, err := store.GetForm(ctx, rec.FormID)
	if err != nil {
		return nil, scope, crudapp.RecordAccess{}, err
	}
	kept, err := h.filterFormRecords(ctx, act, form, []*crudapp.FormRecord{rec})
	if err != nil {
		return nil, scope, crudapp.RecordAccess{}, err
	}
	if len(kept) == 0 {
		return nil, scope, crudapp.RecordAccess{}, errRecordNotReached
	}
	return rec, scope, scope.permit(rec), nil
}

// recordPosting is one metric contribution a form record holds through a
// form-to-metric mapping.
type recordPosting struct {
	mappingID, targetMetricID, aggregation, modelID, revisionID string
}

// recordPostings lists the postings recordID holds, read before the record
// is deleted so retractPostings can take them back out of the metrics.
func (h *handler) recordPostings(ctx context.Context, recordID string) ([]recordPosting, error) {
	rows, err := h.db.Query(ctx, `
		SELECT p.mapping_id::text, m.target_metric_id::text, m.aggregation,
		       m.model_id::text, COALESCE(p.revision_id::text, '')
		FROM runtime.form_record_posting p
		JOIN model.form_metric_mapping m ON m.id = p.mapping_id
		WHERE p.form_record_id = $1::uuid`, recordID)
	if err != nil {
		return nil, fmt.Errorf("read record postings: %w", err)
	}
	defer rows.Close()
	var out []recordPosting
	for rows.Next() {
		var p recordPosting
		if err := rows.Scan(&p.mappingID, &p.targetMetricID, &p.aggregation, &p.modelID, &p.revisionID); err != nil {
			return nil, fmt.Errorf("read record postings: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// retractPostings re-aggregates each mapping a deleted record posted into,
// and recalculates the metrics that read them. The posting rows cascade
// away with the record; the aggregate in runtime.fact_input does not, so
// without this a deleted record's value stayed in the metric until some
// other posting happened to recompute it.
func (h *handler) retractPostings(ctx context.Context, ps []recordPosting, userID string) {
	affected := map[string][]struct{ RevisionID, MetricID string }{}
	for _, p := range ps {
		h.recomputeFactInput(ctx, p.mappingID, p.targetMetricID, p.aggregation, p.modelID, p.revisionID, userID)
		affected[p.modelID] = append(affected[p.modelID], struct{ RevisionID, MetricID string }{p.revisionID, p.targetMetricID})
	}
	for modelID, a := range affected {
		h.recalcAfterDimChange(ctx, modelID, a)
	}
}

// jsonRecordErr answers a record route's error: 404 for a record or form
// the caller does not reach, 500 otherwise.
func jsonRecordErr(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, errRecordNotReached) {
		jsonErr(w, fmt.Errorf("%s not found", what), http.StatusNotFound)
		return
	}
	jsonErr(w, err, http.StatusInternalServerError)
}
