package workflow

// A workflow whose subject is a table of records ("Single saved record in a
// table", "Selected records in a table") drives the status of the records it
// runs on, when their table has a status field and the workflow decides
// something (it has an approval step): a record is submitted while the run
// is open, approved or rejected by the run's last decision, and a draft
// again when the run is cancelled (unless another run on it is still open). The records are the ones the run's
// context names — record_id, or record_ids separated by commas, which is
// what a record's form_submit or form_approval trigger gives it — and only
// those of the subject table. A test run drives nothing.
//
// The status is set by the gateway (WithRecordStatus), which saves it and
// re-posts the record through its table's mappings. A store without one —
// the scheduler's, a dry run's — sets no status.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

// RecordStatusFunc sets a record's status on a workflow run's behalf, as
// byUserID: the person whose action moved the run.
type RecordStatusFunc func(ctx context.Context, recordID, status, byUserID string)

// WithRecordStatus returns the store with f setting the status of the
// records its runs drive.
func (s *Store) WithRecordStatus(f RecordStatusFunc) *Store {
	s2 := *s
	s2.recordStatus = f
	return &s2
}

// Run events that move a driven record's status.
const (
	runStarted   = "started"
	runFinished  = "finished"
	runCancelled = "cancelled"
)

// subjectRecords is each record instanceID drives, with its status ("" for
// none).
func (s *Store) subjectRecords(ctx context.Context, instanceID string) map[string]string {
	var ctxJSON, subjectConfig, steps []byte
	var subjectType string
	var testRun bool
	if err := s.db.QueryRow(ctx, `
		SELECT COALESCE(wi.context, '{}'::jsonb), COALESCE(wd.subject_type, ''), COALESCE(wd.subject_config, '{}'::jsonb),
		       wi.test_run, COALESCE(wi.steps_snapshot, wd.steps)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid`, instanceID).Scan(&ctxJSON, &subjectType, &subjectConfig, &testRun, &steps); err != nil {
		return nil
	}
	if testRun || (subjectType != "form_record" && subjectType != "form_records") {
		return nil
	}
	var cfg struct {
		FormID string `json:"form_id"`
	}
	_ = json.Unmarshal(subjectConfig, &cfg)
	if cfg.FormID == "" || !decides(steps) {
		return nil
	}
	var vars map[string]any
	_ = json.Unmarshal(ctxJSON, &vars)
	var ids []string
	for _, key := range []string{"record_id", "record_ids"} {
		v, _ := vars[key].(string)
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT r.id::text, COALESCE(r.status::text, '')
		FROM runtime.form_record r
		JOIN model.form_def fd ON fd.id = r.form_id
		WHERE r.id::text = ANY($1) AND r.form_id::text = $2
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(fd.fields) = 'array' THEN fd.fields ELSE '[]'::jsonb END) e
		              WHERE e->>'type' = 'status')`, ids, cfg.FormID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if rows.Scan(&id, &status) == nil {
			out[id] = status
		}
	}
	return out
}

// DrivenRecords lists the records whose status instanceID drives: what an
// approval decided in it will approve.
func (s *Store) DrivenRecords(ctx context.Context, instanceID string) []string {
	records := s.subjectRecords(ctx, instanceID)
	out := make([]string, 0, len(records))
	for id := range records {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// decides reports whether a workflow's steps include an approval.
func decides(steps []byte) bool {
	defs, err := parseStepDefsRouted(steps)
	if err != nil {
		return false
	}
	for _, d := range defs {
		if d.Type == workflowv1.StepType_STEP_TYPE_APPROVAL {
			return true
		}
	}
	return false
}

// driveRecords moves the status of the records instanceID drives on a run
// event, as byUserID. On a finished run it is the last approval decision's
// — a task's "done" or a notification's "sent" after it decides nothing:
// approve approves, reject rejects, and a run that decided nothing leaves
// them.
func (s *Store) driveRecords(ctx context.Context, instanceID, event, byUserID string) {
	if s.recordStatus == nil {
		return
	}
	records := s.subjectRecords(ctx, instanceID)
	if len(records) == 0 {
		return
	}
	var target string
	var from map[string]bool
	switch event {
	case runStarted:
		target = "submitted"
		from = map[string]bool{"": true, "draft": true, "rejected": true}
	case runCancelled:
		target = "draft"
		from = map[string]bool{"submitted": true}
	case runFinished:
		var decision, decider string
		if err := s.db.QueryRow(ctx, `
			SELECT decision, COALESCE(assignee_user_id::text, '') FROM workflow.workflow_step
			WHERE instance_id = $1::uuid AND completed_at IS NOT NULL
			  AND decision IN ('approve', 'approved', 'reject', 'rejected')
			ORDER BY completed_at DESC LIMIT 1`, instanceID).Scan(&decision, &decider); err != nil {
			return
		}
		switch decision {
		case "approve", "approved":
			target = "approved"
		case "reject", "rejected":
			target = "rejected"
		default:
			return
		}
		if decider != "" {
			byUserID = decider
		}
		from = map[string]bool{"": true, "draft": true, "submitted": true, "approved": true, "rejected": true}
	default:
		return
	}
	for id, status := range records {
		if status == target || !from[status] {
			continue
		}
		// A cancelled run leaves a record another open run is about under
		// review.
		if event == runCancelled && s.otherRunOpen(ctx, instanceID, id) {
			continue
		}
		s.recordStatus(ctx, id, target, byUserID)
	}
}

// otherRunOpen reports whether a run other than instanceID, not a test
// run, is still open on record recordID.
func (s *Store) otherRunOpen(ctx context.Context, instanceID, recordID string) bool {
	var open bool
	_ = s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workflow.workflow_instance
			WHERE id <> $1::uuid AND status = 'running' AND NOT test_run
			  AND (context->>'record_id' = $2 OR $2 = ANY(string_to_array(replace(COALESCE(context->>'record_ids', ''), ' ', ''), ','))))`,
		instanceID, recordID).Scan(&open)
	return open
}
