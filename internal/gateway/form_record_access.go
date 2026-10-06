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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// A form of a revision being built is its builders' (revision_access.go).
	if !h.revisionOpen(ctx, act, s.modelID, s.revisionID) {
		return s, nil
	}
	s.reach = true

	admin, err := h.administersApp(ctx, act, s.modelID, s.appID)
	if err != nil {
		return s, err
	}
	s.admin = admin
	return s, nil
}

// administersApp reports whether act administers the business surface of
// appID, whose model modelID is: builder and administrator reach over the
// model (a developer or tenant admin within their scope, a platform admin:
// actorCanAccessModel on a builder route), or business_admin of the
// application's workspace — of any workspace of its tenant for a
// tenant-level application, which belongs to all of them
// (roleReachesAppSQL's business arms, and workflowAdminScopeSQL's
// business_admin arm). A role held for another workspace or tenant counts
// for nothing here. Per-user app and model restrictions are the caller's
// reach check, made before this one.
func (h *handler) administersApp(ctx context.Context, act *actor, modelID, appID string) (bool, error) {
	admin, err := h.actorCanAccessModel(withBuilderRoute(ctx), act, modelID)
	if err != nil || admin {
		return admin, err
	}
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM core.application app
		    JOIN identity.role_assignment ra ON ra.user_id = $2::uuid AND ra.role = 'business_admin'
		    JOIN core.workspace rws ON rws.id = ra.workspace_id
		    WHERE app.id = $1::uuid
		      AND (rws.id = app.workspace_id
		           OR (app.workspace_id IS NULL AND rws.customer_id = app.customer_id))
		)`, appID, act.UserID).Scan(&admin)
	if err != nil {
		return false, fmt.Errorf("resolve application admin: %w", err)
	}
	return admin, nil
}

// administersModel is administersApp for the application modelID belongs to.
func (h *handler) administersModel(ctx context.Context, act *actor, modelID string) (bool, error) {
	var appID string
	if err := h.db.QueryRow(ctx,
		`SELECT application_id::text FROM core.model WHERE id = $1::uuid`, modelID,
	).Scan(&appID); err != nil {
		return false, fmt.Errorf("resolve model application: %w", err)
	}
	return h.administersApp(ctx, act, modelID, appID)
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
	mappingID, targetMetricID, modelID, revisionID string
}

// recordPostings lists the postings recordID holds, read before the record
// is deleted so retractPostings can take them back out of the metrics.
func (h *handler) recordPostings(ctx context.Context, recordID string) ([]recordPosting, error) {
	rows, err := h.db.Query(ctx, `
		SELECT p.mapping_id::text, m.target_metric_id::text,
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
		if err := rows.Scan(&p.mappingID, &p.targetMetricID, &p.modelID, &p.revisionID); err != nil {
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
		h.recomputeFactInput(ctx, p.mappingID, p.revisionID, userID)
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

// nextCursorHeader carries the continuation of a paged record list: present
// when more records the caller may see can follow, absent on the last page.
const nextCursorHeader = "X-Next-Cursor"

// Record pages: limit records per page by default, at most maxRecordPage.
const (
	defaultRecordPage = 100
	maxRecordPage     = 1000
)

// recordPageParams reads ?limit and ?cursor of a record list.
func recordPageParams(r *http.Request) (limit int, after *crudapp.RecordPosition, err error) {
	limit = defaultRecordPage
	if v := r.URL.Query().Get("limit"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > maxRecordPage {
			return 0, nil, fmt.Errorf("limit must be between 1 and %d", maxRecordPage)
		}
		limit = n
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		if after, err = decodeRecordCursor(c); err != nil {
			return 0, nil, err
		}
	}
	return limit, after, nil
}

// visibleRecordPage is up to limit records of form the caller may see,
// newest first, after `after`: pages of the form's records are read and
// filtered (filterFormRecords) until limit are kept or none are left, so
// records the caller's access rules withhold neither fill the page nor end
// the list early — a list of the newest 100 rows, filtered, did both. next
// continues after the last record served, never after a withheld one, whose
// place would otherwise be disclosed; "" when no more can follow.
func (h *handler) visibleRecordPage(ctx context.Context, act *actor, store *crudapp.Store, form *crudapp.FormDef, after *crudapp.RecordPosition, limit int) (records []*crudapp.FormRecord, next string, err error) {
	pos := after
	for {
		batch, err := store.ListRecordsPage(ctx, form.ID, pos, limit)
		if err != nil {
			return nil, "", err
		}
		if len(batch) == 0 {
			return records, "", nil
		}
		last := batch[len(batch)-1]
		pos = &crudapp.RecordPosition{CreatedAt: last.CreatedAt, ID: last.ID}
		kept, err := h.filterFormRecords(ctx, act, form, batch)
		if err != nil {
			return nil, "", err
		}
		for _, rec := range kept {
			records = append(records, rec)
			if len(records) == limit {
				return records, encodeRecordCursor(rec), nil
			}
		}
		if len(batch) < limit {
			return records, "", nil
		}
	}
}

// A record cursor is the position of the last record served, opaque to the
// caller. It grants nothing: every page is read and filtered for whoever
// presents it.
func encodeRecordCursor(rec *crudapp.FormRecord) string {
	return base64.RawURLEncoding.EncodeToString([]byte(rec.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + rec.ID))
}

func decodeRecordCursor(c string) (*crudapp.RecordPosition, error) {
	bad := fmt.Errorf("invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, bad
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, bad
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, bad
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, bad
	}
	return &crudapp.RecordPosition{CreatedAt: t, ID: id}, nil
}
