package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"google.golang.org/protobuf/types/known/timestamppb"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

type Store struct {
	pool *pgxpool.Pool
	log  zerolog.Logger
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, log: zerolog.Nop()} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithLogger returns a copy of s that logs best-effort failures (e.g. a
// post-approval recalculation trigger that isn't allowed to fail the
// caller's response) through log instead of discarding them. Callers that
// don't need that visibility can keep using NewStore's default no-op logger.
func (s *Store) WithLogger(log zerolog.Logger) *Store {
	s2 := *s
	s2.log = log
	return &s2
}

// stepTypeJoin is the integer value for the join step type (proto STEP_TYPE_JOIN = 5).
// Defined here to avoid regenerating pb.go for a single new enum value.
const stepTypeJoin = workflowv1.StepType(5)

// ── Workflow definitions ───────────────────────────────────────────────────────

func (s *Store) CreateWorkflowDef(ctx context.Context, applicationID, name, triggerEvent string, steps []*workflowv1.WorkflowStepDef) (*workflowv1.WorkflowDef, error) {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (application_id, revision_id, name) DO UPDATE SET trigger_event = EXCLUDED.trigger_event, steps = EXCLUDED.steps
		RETURNING id::text, created_at
	`, applicationID, name, triggerEvent, stepsJSON).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}

	return &workflowv1.WorkflowDef{
		Id:            id,
		ApplicationId: applicationID,
		Name:          name,
		TriggerEvent:  triggerEvent,
		Steps:         steps,
		CreatedAt:     timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetWorkflowDef(ctx context.Context, defID string) (*workflowv1.WorkflowDef, error) {
	var id, appID, name, triggerEvent string
	var stepsJSON []byte
	var createdAt time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, name, trigger_event, steps, created_at
		FROM workflow.workflow_def WHERE id = $1::uuid
	`, defID).Scan(&id, &appID, &name, &triggerEvent, &stepsJSON, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workflow def %s not found", defID)
	}
	if err != nil {
		return nil, err
	}

	steps, err := parseWorkflowStepDefs(stepsJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}

	return &workflowv1.WorkflowDef{
		Id:            id,
		ApplicationId: appID,
		Name:          name,
		TriggerEvent:  triggerEvent,
		Steps:         steps,
		CreatedAt:     timestamppb.New(createdAt),
	}, nil
}

// ── Workflow instances ─────────────────────────────────────────────────────────

// isTestRun reports whether the instance was started as a test run. Side
// effects consult this rather than taking a mode parameter, because they run
// from several places (CompleteStep's transaction, processAutoStep's
// background activation, the scheduler) that don't all share a call path.
// Unknown instances report false: failing "not a test run" keeps a lookup
// error from silently disabling real business effects.
func (s *Store) isTestRun(ctx context.Context, instanceID string) bool {
	var testRun bool
	if err := s.pool.QueryRow(ctx,
		`SELECT test_run FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID,
	).Scan(&testRun); err != nil {
		return false
	}
	return testRun
}

// isTestRunTx is isTestRun for callers already inside a transaction that may
// have written to the instance.
func isTestRunTx(ctx context.Context, tx pgx.Tx, instanceID string) bool {
	var testRun bool
	if err := tx.QueryRow(ctx,
		`SELECT test_run FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID,
	).Scan(&testRun); err != nil {
		return false
	}
	return testRun
}

func (s *Store) StartWorkflow(ctx context.Context, defID, startedByUserID string, contextVars map[string]string) (*workflowv1.WorkflowInstance, error) {
	return s.startWorkflow(ctx, defID, startedByUserID, contextVars, false)
}

// StartTestRun starts an instance in test mode: it advances through the graph
// exactly as a real run does, so a designer can see routing and conditions
// behave, but every DURABLE side effect is suppressed — no notifications are
// sent, no on_approve fact copies are written, no partitions are marked
// dirty. See migration 065 for why this is a column on the instance rather
// than a context flag.
func (s *Store) StartTestRun(ctx context.Context, defID, startedByUserID string, contextVars map[string]string) (*workflowv1.WorkflowInstance, error) {
	return s.startWorkflow(ctx, defID, startedByUserID, contextVars, true)
}

func (s *Store) startWorkflow(ctx context.Context, defID, startedByUserID string, contextVars map[string]string, testRun bool) (*workflowv1.WorkflowInstance, error) {
	def, err := s.GetWorkflowDef(ctx, defID)
	if err != nil {
		return nil, err
	}

	ctxJSON, _ := json.Marshal(contextVars)

	// The instance keeps the definition it starts with: steps and context
	// schema are snapshotted here and every runtime reader prefers the
	// snapshot, so editing a published workflow affects new starts only.
	var stepsJSON, schemaJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT steps, COALESCE(context_schema, '[]'::jsonb) FROM workflow.workflow_def WHERE id = $1::uuid`, defID).Scan(&stepsJSON, &schemaJSON); err != nil {
		return nil, fmt.Errorf("load definition for snapshot: %w", err)
	}

	var instanceID string
	var startedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, context, test_run, steps_snapshot, context_schema_snapshot)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5::jsonb, $6::jsonb)
		RETURNING id::text, started_at
	`, defID, startedByUserID, ctxJSON, testRun, string(stepsJSON), string(schemaJSON)).Scan(&instanceID, &startedAt)
	if err != nil {
		return nil, err
	}

	// Create all steps; mark first one as in_progress. SLA clocks start when a
	// step activates, so only the first (immediately active) step gets due_at
	// here — later steps receive theirs in activateNextSteps.
	for i, step := range def.Steps {
		stepStatus := "pending"
		var dueAt *time.Time
		if i == 0 {
			stepStatus = "in_progress"
			if step.SlaHours > 0 {
				t := startedAt.Add(time.Duration(step.SlaHours) * time.Hour)
				dueAt = &t
			}
		}

		_, err = s.pool.Exec(ctx, `
			INSERT INTO workflow.workflow_step
			    (instance_id, step_def_id, status, due_at)
			VALUES ($1::uuid, $2, $3::workflow.step_status, $4)
		`, instanceID, step.Id, stepStatus, dueAt)
		if err != nil {
			return nil, fmt.Errorf("create step %s: %w", step.Id, err)
		}
	}

	// A workflow may open with a notification or condition step: neither needs
	// human action, so process it (and anything it cascades into) immediately.
	if stepDefs, parseErr := parseStepDefsRouted(stepsJSON); parseErr == nil && len(stepDefs) > 0 {
		s.processAutoStep(ctx, instanceID, stepDefs[0], stepDefs)
		s.reconcileInstance(ctx, instanceID, stepDefs)
		s.closeInstanceIfIdle(ctx, instanceID, "completed")
	}

	return &workflowv1.WorkflowInstance{
		Id:            instanceID,
		WorkflowDefId: defID,
		Status:        workflowv1.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		StartedBy:     nil, // populated by server from actor
		Context:       contextVars,
		StartedAt:     timestamppb.New(startedAt),
	}, nil
}

func (s *Store) GetWorkflowInstance(ctx context.Context, instanceID string) (*workflowv1.WorkflowInstance, []*workflowv1.WorkflowStep, error) {
	var id, defID, statusStr, startedBy string
	var ctxJSON []byte
	var startedAt time.Time
	var completedAt *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, workflow_def_id::text, status::text, started_by::text,
		       context, started_at, completed_at
		FROM workflow.workflow_instance WHERE id = $1::uuid
	`, instanceID).Scan(&id, &defID, &statusStr, &startedBy, &ctxJSON, &startedAt, &completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("instance %s not found", instanceID)
	}
	if err != nil {
		return nil, nil, err
	}

	var contextVars map[string]string
	_ = json.Unmarshal(ctxJSON, &contextVars)

	inst := &workflowv1.WorkflowInstance{
		Id:            id,
		WorkflowDefId: defID,
		Status:        workflowStatusFromString(statusStr),
		Context:       contextVars,
		StartedAt:     timestamppb.New(startedAt),
	}
	if completedAt != nil {
		inst.CompletedAt = timestamppb.New(*completedAt)
	}

	steps, err := s.listSteps(ctx, instanceID)
	if err != nil {
		return nil, nil, err
	}
	return inst, steps, nil
}

func (s *Store) ListWorkflowInstances(ctx context.Context, applicationID string, filterStatus workflowv1.WorkflowStatus, limit, offset int32) ([]*workflowv1.WorkflowInstance, error) {
	query := `
		SELECT wi.id::text, wi.workflow_def_id::text, wi.status::text,
		       wi.started_by::text, wi.context, wi.started_at, wi.completed_at
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wd.application_id = $1::uuid
	`
	args := []any{applicationID}

	if filterStatus != workflowv1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED {
		args = append(args, workflowStatusToString(filterStatus))
		query += fmt.Sprintf(" AND wi.status = $%d::workflow.workflow_status", len(args))
	}
	args = append(args, limit, offset)
	query += fmt.Sprintf(" ORDER BY wi.started_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*workflowv1.WorkflowInstance
	for rows.Next() {
		var id, defID, statusStr, startedBy string
		var ctxJSON []byte
		var startedAt time.Time
		var completedAt *time.Time

		if err := rows.Scan(&id, &defID, &statusStr, &startedBy, &ctxJSON, &startedAt, &completedAt); err != nil {
			return nil, err
		}
		var contextVars map[string]string
		_ = json.Unmarshal(ctxJSON, &contextVars)

		inst := &workflowv1.WorkflowInstance{
			Id:            id,
			WorkflowDefId: defID,
			Status:        workflowStatusFromString(statusStr),
			Context:       contextVars,
			StartedAt:     timestamppb.New(startedAt),
		}
		if completedAt != nil {
			inst.CompletedAt = timestamppb.New(*completedAt)
		}
		instances = append(instances, inst)
	}
	return instances, rows.Err()
}

// IsAssigneeEligible reports whether userID is an eligible assignee for
// stepID's step_def.assignee_roles — a step with no assignee_roles (or an
// empty list) is completable by anyone. Checks both a direct platform-role
// assignment (identity.role_assignment) and business-role membership
// (identity.business_role_member) scoped to the step's own application's
// workspace/customer. This is the single shared implementation of the real
// business rule for step-completion eligibility — both the HTTP task-inbox
// path (internal/gateway/handler.go's taskAction) and this package's own
// gRPC Server.CompleteStep call it, after a synchronization audit found the
// two surfaces had drifted onto entirely different (and, on the gRPC side,
// structurally broken) checks.
func (s *Store) IsAssigneeEligible(ctx context.Context, stepID, userID string) (bool, error) {
	var eligible bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM workflow.workflow_def wd
		    JOIN workflow.workflow_instance wi ON wi.workflow_def_id = wd.id
		    JOIN workflow.workflow_step ws ON ws.id = $1::uuid AND ws.instance_id = wi.id
		    CROSS JOIN LATERAL (
		        SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		        WHERE elem->>'id' = ws.step_def_id LIMIT 1
		    ) step_def
		    WHERE ws.status = 'in_progress'
		      AND (
		          (step_def.elem->'assignee_roles') IS NULL
		          OR (step_def.elem->'assignee_roles') = '[]'::jsonb
		          OR EXISTS (
		              SELECT 1 FROM identity.role_assignment ra
		              WHERE ra.user_id = $2::uuid
		                AND ra.role::text IN (
		                    SELECT jsonb_array_elements_text(step_def.elem->'assignee_roles')
		                )
		          )
		          OR EXISTS (
		              SELECT 1
		              FROM identity.business_role_member brm
		              JOIN identity.business_role br ON br.id = brm.role_id
		              JOIN core.workspace bws ON bws.id = br.workspace_id
		              JOIN core.application app ON app.id = wd.application_id
		                   AND (app.workspace_id = bws.id OR app.customer_id = bws.customer_id)
		              WHERE brm.user_id = $2::uuid
		                AND br.name IN (
		                    SELECT jsonb_array_elements_text(step_def.elem->'assignee_roles')
		                )
		          )
		      )
		)
	`, stepID, userID).Scan(&eligible)
	return eligible, err
}

// CompleteStep marks a step done and advances to the next pending step.
func (s *Store) CompleteStep(ctx context.Context, stepID, userID, decision, comment string) (*workflowv1.WorkflowStep, error) {
	var instanceID, stepDefID string
	var currentStatus string

	err := s.pool.QueryRow(ctx, `
		SELECT instance_id::text, step_def_id, status::text
		FROM workflow.workflow_step WHERE id = $1::uuid
	`, stepID).Scan(&instanceID, &stepDefID, &currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("step %s not found", stepID)
	}
	if err != nil {
		return nil, err
	}
	if currentStatus != "in_progress" && currentStatus != "pending" {
		return nil, fmt.Errorf("step is %s, cannot complete", currentStatus)
	}

	newStatus := "completed"
	if decision == "reject" || decision == "rejected" {
		newStatus = "rejected"
	}

	// The decision commit, and — for an approval whose step declares an
	// on_approve action — the resulting fact copy and dirty-partition
	// marking, all happen in one transaction: either the whole thing lands
	// or none of it does. Two concurrent completions of the same step can
	// no longer both succeed either — the status-guarded UPDATE below only
	// matches (and only one transaction's WHERE can match) a step still
	// in_progress/pending, closing a check-then-act race the earlier
	// SELECT-then-UPDATE pair left open.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	var completedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE workflow.workflow_step
		SET status = $2::workflow.step_status,
		    assignee_user_id = $3::uuid,
		    decision = $4,
		    comment = $5,
		    completed_at = now()
		WHERE id = $1::uuid AND status IN ('in_progress', 'pending')
		RETURNING completed_at
	`, stepID, newStatus, userID, decision, comment).Scan(&completedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("step is %s, cannot complete", currentStatus)
	}
	if err != nil {
		return nil, err
	}

	// If this step declares an on_approve action and the decision approves,
	// run its fact copy + dirty-marking inside the same tx as the decision
	// above. This is deliberately transport-uniform: CompleteStep is the one
	// method the HTTP task-inbox path, the gRPC WorkflowService, and every
	// seed script all call, so this now applies to all three, closing what
	// was previously an HTTP-only behavior.
	var copyResult *onApproveCopyResult
	// A test run never writes facts: the on_approve copy is the engine's one
	// path from a workflow decision into real planning data.
	if decision == "approve" && !isTestRunTx(ctx, tx, instanceID) {
		var approveStepsJSON []byte
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(wi.steps_snapshot, wd.steps)
			FROM workflow.workflow_instance wi
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
			WHERE wi.id = $1::uuid
		`, instanceID).Scan(&approveStepsJSON)
		if approveDefs, perr := parseStepDefsRouted(approveStepsJSON); perr == nil {
			for _, d := range approveDefs {
				if d.ID == stepDefID && d.OnApprove != nil {
					copyResult, err = s.runOnApproveCopy(ctx, tx, instanceID, userID, d.OnApprove)
					if err != nil {
						return nil, fmt.Errorf("on-approve copy: %w", err)
					}
					break
				}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Everything below is unchanged from before this fix: the routing/
	// fan-out cascade and instance-close check are already fully
	// best-effort today (every step silently swallows its own errors), and
	// nothing has identified that as broken — folding an 8-method recursive
	// call graph into the transaction above would hold locks far longer for
	// a problem nobody has reported. Recomputing stepDefs post-commit (a
	// second parse of the same steps JSON already read above) is a minor,
	// acceptable redundancy in exchange for keeping the decision+copy
	// transaction narrowly scoped.
	var stepsJSON []byte
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(wi.steps_snapshot, wd.steps)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid
	`, instanceID).Scan(&stepsJSON)
	stepDefs, parseErr := parseStepDefsRouted(stepsJSON)
	if parseErr == nil && len(stepDefs) > 0 {
		s.activateNextSteps(ctx, instanceID, stepDefID, decision, stepDefs)
		// After routing, skip steps on branches that can no longer be reached
		// (e.g. the untaken side of a condition) and fire any join whose
		// remaining predecessors are now all resolved.
		s.reconcileInstance(ctx, instanceID, stepDefs)
	} else {
		// Fallback to sequential activation for legacy / unparseable defs.
		_, _ = s.pool.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'in_progress'
			WHERE id = (
				SELECT id FROM workflow.workflow_step
				WHERE instance_id = $1::uuid AND status = 'pending'
				ORDER BY created_at ASC LIMIT 1
			)
		`, instanceID)
	}

	// A rejection is a decision reached by a workflow that ran to its end —
	// the instance COMPLETED with outcome "reject" (recorded on the step).
	// "cancelled" is reserved for runs aborted before finishing (withdrawn,
	// admin-cancelled). This used to map reject → cancelled, which made a
	// properly-decided rejection indistinguishable from an abandoned run in
	// every history view (reported live). Lock semantics are unaffected:
	// WorkflowLockReason ignores the terminal status and reads the last
	// decision.
	s.closeInstanceIfIdle(ctx, instanceID, "completed")

	// The actual claim-and-execute recalculation pass stays post-commit and
	// pool-based, exactly as before this fix — only the cheap "mark dirty"
	// half moved into the transaction above (see runOnApproveCopy), not the
	// potentially-expensive evaluation. ClaimForCalculation's own
	// WHERE status='dirty' guard is what makes it safe to trigger here even
	// though the partition may already have been marked dirty by the tx.
	if copyResult != nil && len(copyResult.AffectedMetrics) > 0 {
		scheduler := calculation.NewScheduler(s.log, calculation.NewStore(s.pool), nil)
		if err := scheduler.RecalcAffected(ctx, copyResult.ModelID, copyResult.TargetRevisionID, copyResult.AffectedMetrics); err != nil {
			s.log.Warn().Err(err).Str("instance_id", instanceID).Msg("on_approve recalc failed")
		}
	}

	return &workflowv1.WorkflowStep{
		Id:          stepID,
		InstanceId:  instanceID,
		StepDefId:   stepDefID,
		Status:      stepStatusFromString(newStatus),
		Decision:    decision,
		Comment:     comment,
		CompletedAt: timestamppb.New(completedAt),
	}, nil
}

// ── Post-approval fact copy ───────────────────────────────────────────────────

type ancestorRef struct {
	id          string
	dimensionID string
	code        string
}

// descendantRefs returns every dimension_member descending (via
// parent_member_id, any number of levels/dimensions) from the member
// identified by (dimensionID, code), read through tx so it sees a
// consistent snapshot alongside the rest of CompleteStep's transaction.
func (s *Store) descendantRefs(ctx context.Context, tx pgx.Tx, dimensionID, code string) []ancestorRef {
	var rootID string
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
		dimensionID, code,
	).Scan(&rootID); err != nil {
		return nil
	}
	var out []ancestorRef
	frontier := []string{rootID}
	for i := 0; i < 20 && len(frontier) > 0; i++ {
		rows, err := tx.Query(ctx,
			`SELECT id::text, dimension_id::text, code FROM model.dimension_member WHERE parent_member_id = ANY($1::uuid[])`,
			frontier,
		)
		if err != nil {
			break
		}
		var next []string
		for rows.Next() {
			var id, dimID, c string
			if rows.Scan(&id, &dimID, &c) == nil {
				next = append(next, id)
				out = append(out, ancestorRef{id: id, dimensionID: dimID, code: c})
			}
		}
		rows.Close()
		frontier = next
	}
	return out
}

// onApproveCopyResult carries what CompleteStep needs, post-commit, to
// trigger recalculation for a fact copy runOnApproveCopy performed.
type onApproveCopyResult struct {
	ModelID          string
	TargetRevisionID string
	AffectedMetrics  []string
}

// runOnApproveCopy executes a completed step's declared on_approve action:
// copies the latest runtime.fact_input rows scoped to a "Dimension member"
// context variable from the instance's own revision_id to another revision
// named by a second context variable, and marks the affected calculated
// metrics' partitions dirty in the same tx (the actual claim-and-execute
// recalculation pass stays post-commit and pool-based — see CompleteStep).
// Entirely generic — the workflow_def's steps/context_schema decide what (if
// anything) happens; no dimension or workflow is named here. Returns
// (nil, nil) for the legitimate no-op cases (a context variable the
// instance never resolved, or a scope with no descendants) — those aren't
// misconfiguration, just a step whose declared action doesn't apply this
// time. A resolved-but-invalid revision reference (belonging to a different
// model than the instance's own) is not a no-op, though: it's returned as a
// hard error so the whole CompleteStep aborts instead of silently approving
// without copying.
//
// userID (the approver) is checked against identity.user_access_rule via
// internal/writeguard before anything is written — this used to write
// straight into runtime.fact_input with no access check at all, so an
// approval could silently perform the exact write the interactive grid
// (writeguard.CheckWrite) would reject as hidden/read-only. A rejection
// here returns an error, which aborts CompleteStep's whole transaction
// (including the step-decision UPDATE, via the existing defer
// tx.Rollback) — the approval itself fails, not just the copy.
func (s *Store) runOnApproveCopy(ctx context.Context, tx pgx.Tx, instanceID, userID string, onApprove *stepOnApprove) (*onApproveCopyResult, error) {
	if onApprove.CopyFactsToContextKey == "" || onApprove.ScopeContextKey == "" {
		return nil, nil
	}

	var instCtxJSON, schemaJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT wi.context, COALESCE(wi.context_schema_snapshot, wd.context_schema)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id=$1::uuid
	`, instanceID).Scan(&instCtxJSON, &schemaJSON); err != nil {
		return nil, nil
	}

	var instCtx map[string]string
	if json.Unmarshal(instCtxJSON, &instCtx) != nil {
		return nil, nil
	}
	var ctxVars []startContextVarDef
	_ = json.Unmarshal(schemaJSON, &ctxVars)
	var scopeDimID string
	for _, v := range ctxVars {
		if v.Key == onApprove.ScopeContextKey {
			scopeDimID = v.DimensionID
			break
		}
	}

	modelID := instCtx["model_id"]
	sourceRevisionID := instCtx["revision_id"]
	targetRevisionID := instCtx[onApprove.CopyFactsToContextKey]
	scopeValue := instCtx[onApprove.ScopeContextKey]
	if scopeDimID == "" || modelID == "" || sourceRevisionID == "" || targetRevisionID == "" || scopeValue == "" {
		return nil, nil
	}

	// A copy this step's own config points at must actually belong to this
	// instance's model — a stale or tampered context value here would
	// otherwise let an approval silently no-op (old behavior) or, worse,
	// copy into an unrelated model's revision.
	for _, revID := range []string{sourceRevisionID, targetRevisionID} {
		var belongs bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
			revID, modelID,
		).Scan(&belongs); err != nil {
			return nil, fmt.Errorf("validate revision %s: %w", revID, err)
		}
		if !belongs {
			return nil, fmt.Errorf("revision %s does not belong to model %s", revID, modelID)
		}
	}

	descendants := s.descendantRefs(ctx, tx, scopeDimID, scopeValue)
	if len(descendants) == 0 {
		return nil, nil
	}
	dimIDs := make([]string, len(descendants))
	codes := make([]string, len(descendants))
	memberIDs := make([]string, len(descendants))
	for i, d := range descendants {
		dimIDs[i], codes[i], memberIDs[i] = d.dimensionID, d.code, d.id
	}

	// The approver must have real read/write access to every member this
	// copy touches — checked against the pool (not tx; writeguard is
	// deliberately transport/transaction-agnostic, see its package doc),
	// before any row is written. A denial aborts the whole CompleteStep
	// transaction (see this function's own doc comment above).
	//
	// Deliberately NOT writeguard.CheckWrite (which also runs
	// SystemManaged and WorkflowLockReason) — both of those checks exist
	// to block OTHER, unrelated writes to already-locked/approved data,
	// but the on-approve copy IS the legitimate mechanism that produces
	// that data in the first place:
	//   - the target revision is typically itself system_managed (that's
	//     what makes it read-only to everyone ELSE) — SystemManaged would
	//     reject this copy from ever being able to populate it at all;
	//   - the instance being completed right now is itself still
	//     "running"/"approved" at this exact moment (its own status
	//     transitions to "completed" only later in CompleteStep) —
	//     WorkflowLockReason would see THIS SAME instance as locking
	//     scopeValue and reject the very write its own approval is
	//     supposed to authorize.
	// Only the access-rule half of CheckWrite applies here, replicated
	// (including its ancestor cascade) rather than reused, since it can't
	// be requested independently of the other two checks.
	for _, memberID := range memberIDs {
		access, aErr := writeguard.HiddenAccess(ctx, s.pool, userID, memberID)
		if aErr != nil {
			return nil, fmt.Errorf("check access rule: %w", aErr)
		}
		if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("on-approve copy rejected: access denied: the copy references a dimension member outside your access scope")
		}
		chain, cErr := writeguard.AncestorChain(ctx, s.pool, memberID)
		if cErr != nil {
			return nil, fmt.Errorf("check ancestor chain: %w", cErr)
		}
		for _, anc := range chain {
			if anc.ID == memberID {
				continue
			}
			ancAccess, aaErr := writeguard.HiddenAccess(ctx, s.pool, userID, anc.ID)
			if aaErr != nil {
				return nil, fmt.Errorf("check access rule: %w", aaErr)
			}
			if ancAccess == "hidden" {
				return nil, fmt.Errorf("on-approve copy rejected: access denied: the copy references a dimension member outside your access scope")
			}
		}
	}

	var targetRevisionName string
	_ = tx.QueryRow(ctx, `SELECT name FROM model.revision WHERE id=$1::uuid`, targetRevisionID).Scan(&targetRevisionName)

	rows, err := tx.Query(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
		SELECT model_id, $1::uuid, $2, metric_id, dim_members, value, entered_by
		FROM (
			SELECT DISTINCT ON (metric_id, dim_members) model_id, metric_id, dim_members, value, entered_by
			FROM runtime.fact_input
			WHERE model_id=$3::uuid AND revision_id=$4::uuid
			  AND EXISTS (
			      SELECT 1 FROM jsonb_each_text(dim_members) kv
			      JOIN unnest($5::text[], $6::text[]) AS d(dim_id, code) ON kv.key = d.dim_id AND kv.value = d.code
			  )
			ORDER BY metric_id, dim_members, entered_at DESC, id DESC
		) latest
		RETURNING metric_id::text
	`, targetRevisionID, targetRevisionName, modelID, sourceRevisionID, dimIDs, codes)
	if err != nil {
		return nil, fmt.Errorf("copy facts: %w", err)
	}
	seen := map[string]bool{}
	var copiedMetrics []string
	for rows.Next() {
		var mID string
		if rows.Scan(&mID) == nil && !seen[mID] {
			seen[mID] = true
			copiedMetrics = append(copiedMetrics, mID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copy facts: %w", err)
	}
	if len(copiedMetrics) == 0 {
		return nil, nil
	}

	// Metric-level access, checked after the copy discovers which metrics
	// actually had data to copy (the set isn't known up front). A denial
	// here still aborts the whole CompleteStep transaction — the rows
	// just inserted above are rolled back along with everything else,
	// since this all runs before tx.Commit() in CompleteStep.
	for _, metricID := range copiedMetrics {
		access, maErr := writeguard.MetricAccess(ctx, s.pool, userID, metricID)
		if maErr != nil {
			return nil, fmt.Errorf("check metric access: %w", maErr)
		}
		if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("on-approve copy rejected: access denied: the copy references a metric outside your access scope")
		}
	}

	// Mark every calculated metric depending on the copied inputs dirty,
	// atomically with the copy above — see MarkDirtyTx's doc comment for why.
	// defs must be loaded via sourceRevisionID, not targetRevisionID: the
	// copy above (SELECT ... metric_id ...) carries each fact's metric_id
	// verbatim from the source revision's own fact_input rows with no
	// remapping, so copiedMetrics' IDs are the SOURCE revision's metric_def
	// UUIDs — AffectedMetricIDs needs a dependency graph keyed by those same
	// IDs to find what depends on them. Only the resulting partition KEY
	// (below) is scoped to targetRevisionID, matching where the copied
	// facts — and the eventual recalculated result — actually live.
	calcStore := calculation.NewStore(s.pool)
	defs, err := calcStore.LoadModelMetrics(ctx, modelID, sourceRevisionID)
	if err != nil {
		return nil, fmt.Errorf("load metrics for dirty-marking: %w", err)
	}
	timePartition := calculation.PartitionMonth(time.Now())
	for _, metricID := range calculation.AffectedMetricIDs(defs, copiedMetrics) {
		pk := calculation.BuildPartitionKey(modelID, targetRevisionID, metricID, timePartition)
		if err := calcStore.MarkDirtyTx(ctx, tx, []string{pk}, modelID, metricID, targetRevisionID, timePartition); err != nil {
			return nil, fmt.Errorf("mark dirty: %w", err)
		}
	}

	return &onApproveCopyResult{ModelID: modelID, TargetRevisionID: targetRevisionID, AffectedMetrics: copiedMetrics}, nil
}

// ── Parallel / route-based step activation ───────────────────────────────────

// stepDefRouted is a richer internal representation that preserves the routes
// map from JSONB (the proto WorkflowStepDef only carries next_step_ids).
type stepDefRouted struct {
	ID           string
	Name         string
	Type         workflowv1.StepType
	Routes       map[string]string
	NextStepIDs  []string
	Notification *stepNotificationConfig
	Condition    json.RawMessage
	SlaHours     int32
	OnApprove    *stepOnApprove
}

// stepNotificationConfig is a notification step's Designer-configured
// delivery target, as written by the "Recipient" field in WorkflowsTab.tsx.
// RecipientType is "requester" (the user who started the instance) or
// "role" (RecipientRole, matched the same way step assignee_roles are: a
// platform identity.user_role value or an identity.business_role name).
type stepNotificationConfig struct {
	RecipientType string `json:"recipient_type"`
	RecipientRole string `json:"recipient_role"`
	Subject       string `json:"subject"`
	Message       string `json:"message"`
}

// stepOnApprove is a step's optional Designer-configured post-approval
// action: copy the latest runtime.fact_input rows scoped to a "Dimension
// member" context variable (ScopeContextKey) from the instance's own
// revision_id to another revision named by a second context variable
// (CopyFactsToContextKey), then recalculate the affected metrics there.
type stepOnApprove struct {
	CopyFactsToContextKey string `json:"copy_facts_to_context_key"`
	ScopeContextKey       string `json:"scope_context_key"`
}

// parseStepDefsRouted parses the steps JSONB into stepDefRouted structs,
// retaining the full routes map needed for fan-out routing.
func parseStepDefsRouted(stepsJSON []byte) ([]*stepDefRouted, error) {
	var raw []struct {
		ID           string                  `json:"id"`
		Name         string                  `json:"name"`
		Type         json.RawMessage         `json:"type"`
		Routes       map[string]string       `json:"routes"`
		NextStepIDs  []string                `json:"next_step_ids"`
		Notification *stepNotificationConfig `json:"notification"`
		Condition    json.RawMessage         `json:"condition"`
		SlaHours     int32                   `json:"sla_hours"`
		OnApprove    *stepOnApprove          `json:"on_approve"`
	}
	if err := json.Unmarshal(stepsJSON, &raw); err != nil {
		return nil, err
	}
	out := make([]*stepDefRouted, 0, len(raw))
	for _, r := range raw {
		out = append(out, &stepDefRouted{
			ID:           r.ID,
			Name:         r.Name,
			Type:         workflowStepTypeFromJSON(r.Type),
			Routes:       r.Routes,
			NextStepIDs:  r.NextStepIDs,
			Notification: r.Notification,
			Condition:    r.Condition,
			SlaHours:     r.SlaHours,
			OnApprove:    r.OnApprove,
		})
	}
	return out, nil
}

// activateNextSteps follows the completed step's routes and activates all
// appropriate next steps, handling parallel fan-out and join synchronisation.
func (s *Store) activateNextSteps(ctx context.Context, instanceID, completedDefID, decision string, stepDefs []*stepDefRouted) {
	var completedDef *stepDefRouted
	for _, sd := range stepDefs {
		if sd.ID == completedDefID {
			completedDef = sd
			break
		}
	}
	if completedDef == nil || (len(completedDef.Routes) == 0 && len(completedDef.NextStepIDs) == 0) {
		// No routes configured — fall back to sequential.
		_, _ = s.pool.Exec(ctx, `
			UPDATE workflow.workflow_step SET status = 'in_progress'
			WHERE id = (
				SELECT id FROM workflow.workflow_step
				WHERE instance_id = $1::uuid AND status = 'pending'
				ORDER BY created_at ASC LIMIT 1
			)
		`, instanceID)
		return
	}

	for _, nextDefID := range resolveNextStepDefIDs(completedDef, decision) {
		if strings.HasPrefix(nextDefID, "end-") {
			continue
		}
		var nextDef *stepDefRouted
		for _, sd := range stepDefs {
			if sd.ID == nextDefID {
				nextDef = sd
				break
			}
		}
		if nextDef != nil && nextDef.Type == stepTypeJoin {
			if s.allPredecessorsComplete(ctx, instanceID, nextDefID, stepDefs) {
				// Auto-complete the join and continue along its routes.
				_, _ = s.pool.Exec(ctx, `
					UPDATE workflow.workflow_step
					SET status = 'completed', completed_at = now()
					WHERE instance_id = $1::uuid AND step_def_id = $2 AND status = 'pending'
				`, instanceID, nextDefID)
				s.activateNextSteps(ctx, instanceID, nextDefID, "complete", stepDefs)
			}
			// else: still waiting for other parallel branches — do nothing.
		} else {
			// The SLA clock starts at activation, not instance start.
			var sla int32
			if nextDef != nil {
				sla = nextDef.SlaHours
			}
			ct, _ := s.pool.Exec(ctx, `
				UPDATE workflow.workflow_step
				SET status = 'in_progress',
				    due_at = CASE WHEN $3::int > 0 THEN now() + make_interval(hours => $3::int) ELSE due_at END
				WHERE instance_id = $1::uuid AND step_def_id = $2 AND status = 'pending'
			`, instanceID, nextDefID, sla)
			// Notification and condition steps need no human action: process
			// immediately and continue along their routes.
			if nextDef != nil && ct.RowsAffected() > 0 {
				s.processAutoStep(ctx, instanceID, nextDef, stepDefs)
				continue
			}
			// Zero rows: the route points back at a step this instance already
			// passed — a rework loop. Re-activate it (validation guarantees a
			// task or approval sits on every loop, and reworkStep caps the
			// number of rounds, so this cannot recurse forever).
			if nextDef != nil && s.reworkStep(ctx, instanceID, nextDef, completedDef, stepDefs) {
				s.processAutoStep(ctx, instanceID, nextDef, stepDefs)
			}
		}
	}
}

// maxReworks caps how often one step can be sent back within an instance:
// validation refuses loops with no human step, and this guards the human
// variant ("reject forever").
const maxReworks = 20

// reworkStep re-activates target (already completed/rejected/skipped) as the
// route from `from` asks, resetting every step downstream of target that is
// not currently in progress, so the loop runs again against the instance's
// snapshot. The sending step's comment travels to the re-activated step as
// its rework note, and the requester is told. Returns false when the loop
// cap is hit — the instance is then marked failed.
func (s *Store) reworkStep(ctx context.Context, instanceID string, target, from *stepDefRouted, stepDefs []*stepDefRouted) bool {
	var status string
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT status::text, rework_count FROM workflow.workflow_step
		WHERE instance_id = $1::uuid AND step_def_id = $2
	`, instanceID, target.ID).Scan(&status, &count); err != nil || status == "in_progress" || status == "pending" {
		return false
	}
	if count >= maxReworks {
		_, _ = s.pool.Exec(ctx, `UPDATE workflow.workflow_instance SET status = 'failed', completed_at = now() WHERE id = $1::uuid AND status = 'running'`, instanceID)
		_, _ = s.pool.Exec(ctx, `UPDATE workflow.execution SET status = 'failed', error = 'rework loop cap reached', completed_at = now() WHERE instance_id = $1::uuid AND status = 'running'`, instanceID)
		s.log.Warn().Str("instance", instanceID).Str("step", target.ID).Int("reworks", count).Msg("rework loop cap reached; instance failed")
		return false
	}
	var fromComment string
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(comment,'') FROM workflow.workflow_step WHERE instance_id = $1::uuid AND step_def_id = $2`, instanceID, from.ID).Scan(&fromComment)

	// Everything downstream of the target (following every route, not just
	// the decided one) starts over; steps mid-flight on parallel branches
	// are left alone.
	defsByID := make(map[string]*stepDefRouted, len(stepDefs))
	for _, d := range stepDefs {
		defsByID[d.ID] = d
	}
	downstream := map[string]bool{}
	frontier := allRouteTargets(target)
	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]
		if strings.HasPrefix(id, "end-") || id == target.ID || downstream[id] {
			continue
		}
		downstream[id] = true
		if d := defsByID[id]; d != nil {
			frontier = append(frontier, allRouteTargets(d)...)
		}
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE workflow.workflow_step
		SET status = 'pending', decision = NULL, comment = NULL, completed_at = NULL, due_at = NULL
		WHERE instance_id = $1::uuid AND step_def_id = ANY($2) AND status <> 'in_progress'
	`, instanceID, keysOf(downstream))
	fromName := from.Name
	if fromName == "" {
		fromName = from.ID
	}
	targetName := target.Name
	if targetName == "" {
		targetName = target.ID
	}
	note := fmt.Sprintf("Sent back for rework (%d) from %q", count+1, fromName)
	if fromComment != "" {
		note += ": " + fromComment
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE workflow.workflow_step
		SET status = 'in_progress', decision = NULL, comment = $3, completed_at = NULL,
		    rework_count = rework_count + 1,
		    due_at = CASE WHEN $4::int > 0 THEN now() + make_interval(hours => $4::int) ELSE NULL END
		WHERE instance_id = $1::uuid AND step_def_id = $2
	`, instanceID, target.ID, note, target.SlaHours); err != nil {
		return false
	}
	// The requester learns the request went back, with the reason.
	var startedBy string
	if qErr := s.pool.QueryRow(ctx, `SELECT started_by::text FROM workflow.workflow_instance WHERE id = $1::uuid`, instanceID).Scan(&startedBy); qErr == nil && startedBy != "" {
		vars := map[string]string{"subject": "Sent back for rework", "message": fmt.Sprintf("%q needs another pass. %s", targetName, note)}
		_, _ = notification.NewStore(s.pool).Notify(ctx, startedBy, "workflow_step_notification", vars, "workflow_instance", instanceID)
	}
	return true
}

// allRouteTargets lists every step a definition can route to, whatever the
// decision — the shape of the graph, not one path through it.
func allRouteTargets(d *stepDefRouted) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range d.Routes {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range d.NextStepIDs {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// processAutoStep auto-completes an in_progress step that requires no human
// action: notification steps dispatch their message; condition steps evaluate
// their designer-configured expression against instance context. A condition
// whose context key is absent (or whose expression is malformed) is left
// in_progress for a human decision — the pre-evaluator behavior.
func (s *Store) processAutoStep(ctx context.Context, instanceID string, def *stepDefRouted, stepDefs []*stepDefRouted) {
	switch def.Type {
	case workflowv1.StepType_STEP_TYPE_NOTIFICATION:
		sent := s.dispatchStepNotification(ctx, instanceID, def.Notification)
		decision := "sent"
		comment := "Auto-dispatched"
		if !sent {
			// No configured/resolvable recipient — still unblock the
			// route (a workflow must never hang on a misconfigured
			// notification step) but record that truthfully instead
			// of claiming delivery that didn't happen.
			decision = "skipped"
			comment = "No recipient configured or resolved"
		}
		_, _ = s.pool.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'completed', decision = $3, comment = $4, completed_at = now()
			WHERE instance_id = $1::uuid AND step_def_id = $2 AND status = 'in_progress'
		`, instanceID, def.ID, decision, comment)
		s.activateNextSteps(ctx, instanceID, def.ID, "sent", stepDefs)

	case workflowv1.StepType_STEP_TYPE_CONDITION:
		result, evaluated := evalStepCondition(def.Condition, s.instanceContextVars(ctx, instanceID))
		if !evaluated {
			return
		}
		decision := "false"
		if result {
			decision = "true"
		}
		_, _ = s.pool.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'completed', decision = $3, comment = 'Auto-evaluated', completed_at = now()
			WHERE instance_id = $1::uuid AND step_def_id = $2 AND status = 'in_progress'
		`, instanceID, def.ID, decision)
		s.activateNextSteps(ctx, instanceID, def.ID, decision, stepDefs)

	default:
		// TASK/APPROVAL/JOIN/UNSPECIFIED require human action or are routed
		// elsewhere — nothing for processAutoStep to do.
	}
}

// instanceContextVars loads an instance's context as a string map.
func (s *Store) instanceContextVars(ctx context.Context, instanceID string) map[string]string {
	var ctxJSON []byte
	_ = s.pool.QueryRow(ctx, `
		SELECT context FROM workflow.workflow_instance WHERE id = $1::uuid
	`, instanceID).Scan(&ctxJSON)
	vars := map[string]string{}
	_ = json.Unmarshal(ctxJSON, &vars)
	return vars
}

// evalStepCondition evaluates a designer condition {left, operator, right}
// against context vars. Returns (result, true) when it could be evaluated;
// (false, false) when the expression is malformed or the context key is
// missing for an operator that needs a value.
func evalStepCondition(raw json.RawMessage, ctxVars map[string]string) (bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, false
	}
	var cond struct {
		Left     string `json:"left"`
		Operator string `json:"operator"`
		Right    any    `json:"right"`
	}
	if err := json.Unmarshal(raw, &cond); err != nil || cond.Left == "" || cond.Operator == "" {
		return false, false
	}

	left, present := ctxVars[cond.Left]
	right := fmt.Sprintf("%v", cond.Right)

	switch cond.Operator {
	case "is_empty":
		return !present || strings.TrimSpace(left) == "", true
	case "is_not_empty":
		return present && strings.TrimSpace(left) != "", true
	}
	if !present {
		return false, false
	}

	lNum, lErr := strconv.ParseFloat(strings.TrimSpace(left), 64)
	rNum, rErr := strconv.ParseFloat(strings.TrimSpace(right), 64)
	numeric := lErr == nil && rErr == nil

	switch cond.Operator {
	case "equals":
		if numeric {
			return lNum == rNum, true
		}
		return left == right, true
	case "not_equals":
		if numeric {
			return lNum != rNum, true
		}
		return left != right, true
	case "greater_than":
		if !numeric {
			return false, false
		}
		return lNum > rNum, true
	case "greater_than_or_equal":
		if !numeric {
			return false, false
		}
		return lNum >= rNum, true
	case "less_than":
		if !numeric {
			return false, false
		}
		return lNum < rNum, true
	case "less_than_or_equal":
		if !numeric {
			return false, false
		}
		return lNum <= rNum, true
	case "contains":
		return strings.Contains(left, right), true
	}
	return false, false
}

// reconcileInstance brings an instance's step statuses in line with what
// routing can still reach:
//  1. any 'pending' step that is no longer reachable — its branch was ruled
//     out by a condition or approval decision — is marked 'skipped';
//  2. any 'pending' join whose remaining predecessors are all resolved
//     (completed/rejected/skipped, with at least one completed) fires and
//     continues along its routes.
//
// Without this, a join fed by a condition's two branches could never fire:
// the untaken branch stayed 'pending' forever and blocked it.
func (s *Store) reconcileInstance(ctx context.Context, instanceID string, stepDefs []*stepDefRouted) {
	// Legacy sequential defs carry no routing information at all — every
	// pending step is implicitly "next", so reachability (and joins) do not
	// apply and sweeping would skip live work.
	hasRouting := false
	for _, d := range stepDefs {
		if len(d.Routes) > 0 || len(d.NextStepIDs) > 0 {
			hasRouting = true
			break
		}
	}
	if !hasRouting {
		return
	}

	for range stepDefs { // fixpoint: each pass may unlock another join
		reachable := s.reachableStepDefIDs(ctx, instanceID, stepDefs)
		ct, _ := s.pool.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'skipped', comment = COALESCE(comment, 'Branch not taken')
			WHERE instance_id = $1::uuid AND status = 'pending'
			  AND NOT (step_def_id = ANY($2))
		`, instanceID, keysOf(reachable))
		changed := ct.RowsAffected() > 0

		for _, def := range stepDefs {
			if def.Type != stepTypeJoin || !reachable[def.ID] {
				continue
			}
			var status string
			if err := s.pool.QueryRow(ctx, `
				SELECT status::text FROM workflow.workflow_step
				WHERE instance_id = $1::uuid AND step_def_id = $2
			`, instanceID, def.ID).Scan(&status); err != nil || status != "pending" {
				continue
			}
			if !s.allPredecessorsComplete(ctx, instanceID, def.ID, stepDefs) {
				continue
			}
			if !s.anyPredecessorCompleted(ctx, instanceID, def.ID, stepDefs) {
				continue
			}
			_, _ = s.pool.Exec(ctx, `
				UPDATE workflow.workflow_step
				SET status = 'completed', completed_at = now()
				WHERE instance_id = $1::uuid AND step_def_id = $2 AND status = 'pending'
			`, instanceID, def.ID)
			s.activateNextSteps(ctx, instanceID, def.ID, "complete", stepDefs)
			changed = true
		}
		if !changed {
			return
		}
	}
}

// reachableStepDefIDs computes which step defs can still be visited: the
// forward closure over all routes from every in_progress step (future
// decisions are open, so every branch counts) plus, from each finished step,
// only the branch its recorded decision actually took.
func (s *Store) reachableStepDefIDs(ctx context.Context, instanceID string, stepDefs []*stepDefRouted) map[string]bool {
	defsByID := make(map[string]*stepDefRouted, len(stepDefs))
	for _, d := range stepDefs {
		defsByID[d.ID] = d
	}

	type stepRow struct{ status, decision string }
	rows, err := s.pool.Query(ctx, `
		SELECT step_def_id, status::text, COALESCE(decision,'')
		FROM workflow.workflow_step WHERE instance_id = $1::uuid
	`, instanceID)
	if err != nil {
		return map[string]bool{}
	}
	states := map[string]stepRow{}
	for rows.Next() {
		var id string
		var r stepRow
		if err := rows.Scan(&id, &r.status, &r.decision); err == nil {
			states[id] = r
		}
	}
	rows.Close()

	reachable := map[string]bool{}
	expanded := map[string]bool{}
	var frontier []string
	for id, st := range states {
		switch st.status {
		case "in_progress":
			frontier = append(frontier, id)
		case "completed", "rejected":
			// Finished steps expand only along their recorded decision.
			reachable[id] = true
			expanded[id] = true
			if def := defsByID[id]; def != nil {
				frontier = append(frontier, resolveNextStepDefIDs(def, st.decision)...)
			}
		}
	}
	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]
		if strings.HasPrefix(id, "end-") {
			continue
		}
		reachable[id] = true
		if expanded[id] {
			continue
		}
		expanded[id] = true
		def := defsByID[id]
		if def == nil {
			continue
		}
		// Not-yet-finished steps fan out over every branch: future decisions
		// are open, so all their targets stay potentially reachable.
		for _, t := range def.Routes {
			frontier = append(frontier, t)
		}
		frontier = append(frontier, def.NextStepIDs...)
	}
	return reachable
}

// anyPredecessorCompleted reports whether at least one step routing into
// targetDefID actually completed (vs. every predecessor being skipped, in
// which case the join itself lies on a dead branch and must not fire).
func (s *Store) anyPredecessorCompleted(ctx context.Context, instanceID, targetDefID string, stepDefs []*stepDefRouted) bool {
	for _, sd := range stepDefs {
		if !routesTo(sd, targetDefID) {
			continue
		}
		var status string
		if err := s.pool.QueryRow(ctx, `
			SELECT status::text FROM workflow.workflow_step
			WHERE instance_id = $1::uuid AND step_def_id = $2
		`, instanceID, sd.ID).Scan(&status); err == nil && status == "completed" {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// closeInstanceIfIdle finishes an instance that has no in_progress steps
// left: remaining 'pending' steps sit on branches routing never reached, so
// they are marked skipped, the instance closes with finalStatus, and any
// automation executions tied to it are updated to reflect the real outcome.
func (s *Store) closeInstanceIfIdle(ctx context.Context, instanceID, finalStatus string) {
	var active int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workflow.workflow_step
		WHERE instance_id = $1::uuid AND status = 'in_progress'
	`, instanceID).Scan(&active)
	if active > 0 {
		return
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE workflow.workflow_step SET status = 'skipped'
		WHERE instance_id = $1::uuid AND status = 'pending'
	`, instanceID)
	_, _ = s.pool.Exec(ctx, `
		UPDATE workflow.workflow_instance
		SET status = $2::workflow.workflow_status, completed_at = now()
		WHERE id = $1::uuid AND status = 'running'
	`, instanceID, finalStatus)
	execStatus := "completed"
	if finalStatus != "completed" {
		execStatus = "cancelled"
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE workflow.execution
		SET status = $2::workflow.execution_status, completed_at = now()
		WHERE instance_id = $1::uuid AND status = 'running'
	`, instanceID, execStatus)
}

// dispatchStepNotification resolves cfg's recipient(s) and writes a real
// notification.notification row for each, via the same Send() path
// /api/notifications reads from — closing the previous gap where a
// notification step's Designer-configured Recipient/Subject/Message was
// saved but never acted on: the step just auto-completed with a hardcoded
// "Auto-dispatched" comment and nobody was ever actually notified. Returns
// whether at least one notification was actually sent.
func (s *Store) dispatchStepNotification(ctx context.Context, instanceID string, cfg *stepNotificationConfig) bool {
	if cfg == nil || cfg.RecipientType == "" {
		return false
	}
	// A test run must not page real people. The step still auto-completes
	// and routing continues, so the designer sees the shape of the run.
	if s.isTestRun(ctx, instanceID) {
		return false
	}

	var appID, startedBy string
	if err := s.pool.QueryRow(ctx, `
		SELECT wd.application_id::text, wi.started_by::text
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid
	`, instanceID).Scan(&appID, &startedBy); err != nil {
		return false
	}

	recipients := s.resolveNotificationRecipients(ctx, appID, startedBy, cfg)
	if len(recipients) == 0 {
		return false
	}

	notifStore := notification.NewStore(s.pool)
	sent := false
	for _, userID := range recipients {
		vars := map[string]string{"subject": cfg.Subject, "message": cfg.Message}
		// resource_type is always "workflow_instance", never "workflow_step":
		// this step auto-completes itself the instant it activates (see
		// processAutoStep), so by the time a recipient reads the
		// notification there's no longer an actionable step to point at —
		// the instance is the only link this producer can ever supply.
		if _, err := notifStore.Notify(ctx, userID, "workflow_step_notification", vars, "workflow_instance", instanceID); err == nil {
			sent = true
		}
	}
	return sent
}

// resolveNotificationRecipients turns a notification step's configured
// target into concrete user IDs. "requester" is the instance's starter;
// "role" matches assignee_roles' own dual scheme (see /api/tasks in
// internal/gateway/handler.go) — a platform identity.user_role value via
// role_assignment, or an identity.business_role name via
// business_role_member, scoped to the workflow's application.
func (s *Store) resolveNotificationRecipients(ctx context.Context, appID, startedBy string, cfg *stepNotificationConfig) []string {
	if cfg.RecipientType == "requester" {
		if startedBy == "" {
			return nil
		}
		return []string{startedBy}
	}
	if cfg.RecipientType != "role" || cfg.RecipientRole == "" {
		return nil
	}

	// Platform-role matches are scoped to the workflow's application: a
	// role_assignment counts only when its workspace belongs to the same
	// workspace/customer (NULL workspace = platform-wide assignment). Without
	// this, any user holding the same role at ANOTHER customer received this
	// tenant's notifications.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.id::text
		FROM identity.user u
		WHERE EXISTS (
		    SELECT 1
		    FROM identity.role_assignment ra
		    LEFT JOIN core.workspace rws ON rws.id = ra.workspace_id
		    JOIN core.application app ON app.id = $1::uuid
		    WHERE ra.user_id = u.id AND ra.role::text = $2
		      AND (ra.workspace_id IS NULL
		           OR app.workspace_id = rws.id
		           OR app.customer_id = rws.customer_id)
		)
		OR EXISTS (
		    SELECT 1
		    FROM identity.business_role_member brm
		    JOIN identity.business_role br ON br.id = brm.role_id
		    JOIN core.workspace bws ON bws.id = br.workspace_id
		    JOIN core.application app ON app.id = $1::uuid
		         AND (app.workspace_id = bws.id OR app.customer_id = bws.customer_id)
		    WHERE brm.user_id = u.id AND br.name = $2
		)
	`, appID, cfg.RecipientRole)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// resolveNextStepDefIDs returns the step-def IDs to activate after sd completes.
func resolveNextStepDefIDs(sd *stepDefRouted, decision string) []string {
	// Approval: follow only the branch matching the decision.
	if sd.Type == workflowv1.StepType_STEP_TYPE_APPROVAL {
		key := "approve"
		if decision == "reject" || decision == "rejected" {
			key = "reject"
		}
		if target := sd.Routes[key]; target != "" {
			return []string{target}
		}
		// Legacy defs route approvals via next_step_ids with no
		// approve/reject branches: advance on approve, end on reject.
		if key == "approve" && len(sd.Routes) == 0 {
			return sd.NextStepIDs
		}
		return nil
	}
	// Condition: decision carries "true" or "false".
	if sd.Type == workflowv1.StepType_STEP_TYPE_CONDITION {
		if target := sd.Routes[decision]; target != "" {
			return []string{target}
		}
		return nil
	}
	// Task / notification / join: fan out to ALL route targets (parallel).
	if len(sd.Routes) > 0 {
		seen := map[string]bool{}
		var out []string
		for _, target := range sd.Routes {
			if target != "" && !seen[target] {
				seen[target] = true
				out = append(out, target)
			}
		}
		return out
	}
	return sd.NextStepIDs
}

// allPredecessorsComplete returns true when every step that routes to targetDefID
// is already completed/rejected/skipped in this instance.
func (s *Store) allPredecessorsComplete(ctx context.Context, instanceID, targetDefID string, stepDefs []*stepDefRouted) bool {
	for _, sd := range stepDefs {
		if !routesTo(sd, targetDefID) {
			continue
		}
		var status string
		err := s.pool.QueryRow(ctx, `
			SELECT status::text FROM workflow.workflow_step
			WHERE instance_id = $1::uuid AND step_def_id = $2
		`, instanceID, sd.ID).Scan(&status)
		if err != nil || (status != "completed" && status != "rejected" && status != "skipped") {
			return false
		}
	}
	return true
}

// routesTo reports whether sd has targetDefID in any route value or next_step_ids.
func routesTo(sd *stepDefRouted, targetDefID string) bool {
	for _, v := range sd.Routes {
		if v == targetDefID {
			return true
		}
	}
	for _, id := range sd.NextStepIDs {
		if id == targetDefID {
			return true
		}
	}
	return false
}

func (s *Store) GetPendingTasks(ctx context.Context, userID, applicationID string) ([]*workflowv1.WorkflowStep, error) {
	query := `
		SELECT ws.id::text, ws.instance_id::text, ws.step_def_id,
		       ws.status::text, COALESCE(ws.assignee_user_id::text,''),
		       COALESCE(ws.decision,''), COALESCE(ws.comment,''),
		       ws.due_at, ws.completed_at
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE ws.status IN ('pending', 'in_progress')
	`
	args := []any{}

	if userID != "" {
		args = append(args, userID)
		query += fmt.Sprintf(" AND (ws.assignee_user_id = $%d::uuid OR ws.assignee_user_id IS NULL)", len(args))
	}
	if applicationID != "" {
		args = append(args, applicationID)
		query += fmt.Sprintf(" AND wd.application_id = $%d::uuid", len(args))
	}
	query += " ORDER BY ws.due_at ASC NULLS LAST"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSteps(rows)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *Store) listSteps(ctx context.Context, instanceID string) ([]*workflowv1.WorkflowStep, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, instance_id::text, step_def_id,
		       status::text, COALESCE(assignee_user_id::text,''),
		       COALESCE(decision,''), COALESCE(comment,''),
		       due_at, completed_at
		FROM workflow.workflow_step WHERE instance_id = $1::uuid
		ORDER BY created_at ASC
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSteps(rows)
}

// GetStepApplicationID returns the application_id for the workflow a step belongs to.
func (s *Store) GetStepApplicationID(ctx context.Context, stepID string) (string, error) {
	var applicationID string
	err := s.pool.QueryRow(ctx, `
		SELECT wd.application_id::text
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE ws.id = $1::uuid
	`, stepID).Scan(&applicationID)
	return applicationID, err
}

func scanSteps(rows pgx.Rows) ([]*workflowv1.WorkflowStep, error) {
	var steps []*workflowv1.WorkflowStep
	for rows.Next() {
		var id, instanceID, stepDefID, statusStr, assigneeID, decision, comment string
		var dueAt, completedAt *time.Time

		if err := rows.Scan(&id, &instanceID, &stepDefID, &statusStr,
			&assigneeID, &decision, &comment, &dueAt, &completedAt); err != nil {
			return nil, err
		}
		step := &workflowv1.WorkflowStep{
			Id:             id,
			InstanceId:     instanceID,
			StepDefId:      stepDefID,
			Status:         stepStatusFromString(statusStr),
			AssigneeUserId: assigneeID,
			Decision:       decision,
			Comment:        comment,
		}
		if dueAt != nil {
			step.DueAt = timestamppb.New(*dueAt)
		}
		if completedAt != nil {
			step.CompletedAt = timestamppb.New(*completedAt)
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

func parseWorkflowStepDefs(stepsJSON []byte) ([]*workflowv1.WorkflowStepDef, error) {
	var protoSteps []*workflowv1.WorkflowStepDef
	if err := json.Unmarshal(stepsJSON, &protoSteps); err == nil {
		return protoSteps, nil
	}

	var rawSteps []struct {
		ID            string            `json:"id"`
		Name          string            `json:"name"`
		Type          json.RawMessage   `json:"type"`
		AssigneeRoles []string          `json:"assignee_roles"`
		ConditionExpr string            `json:"condition_expr"`
		Condition     json.RawMessage   `json:"condition"`
		NextStepIDs   []string          `json:"next_step_ids"`
		Routes        map[string]string `json:"routes"`
		SlaHours      int32             `json:"sla_hours"`
	}
	if err := json.Unmarshal(stepsJSON, &rawSteps); err != nil {
		return nil, err
	}

	steps := make([]*workflowv1.WorkflowStepDef, 0, len(rawSteps))
	for _, raw := range rawSteps {
		conditionExpr := raw.ConditionExpr
		if conditionExpr == "" && len(raw.Condition) > 0 && string(raw.Condition) != "null" {
			conditionExpr = string(raw.Condition)
		}

		nextStepIDs := raw.NextStepIDs
		if len(nextStepIDs) == 0 && len(raw.Routes) > 0 {
			nextStepIDs = routeTargets(raw.Routes)
		}

		steps = append(steps, &workflowv1.WorkflowStepDef{
			Id:            raw.ID,
			Name:          raw.Name,
			Type:          workflowStepTypeFromJSON(raw.Type),
			AssigneeRoles: raw.AssigneeRoles,
			ConditionExpr: conditionExpr,
			NextStepIds:   nextStepIDs,
			SlaHours:      raw.SlaHours,
		})
	}
	return steps, nil
}

func workflowStepTypeFromJSON(raw json.RawMessage) workflowv1.StepType {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "task", "step_type_task":
			return workflowv1.StepType_STEP_TYPE_TASK
		case "approval", "step_type_approval":
			return workflowv1.StepType_STEP_TYPE_APPROVAL
		case "notification", "step_type_notification":
			return workflowv1.StepType_STEP_TYPE_NOTIFICATION
		case "condition", "step_type_condition":
			return workflowv1.StepType_STEP_TYPE_CONDITION
		case "join", "step_type_join":
			return stepTypeJoin
		default:
			if n, err := strconv.Atoi(s); err == nil {
				return workflowv1.StepType(n)
			}
			return workflowv1.StepType_STEP_TYPE_UNSPECIFIED
		}
	}

	var n int32
	if err := json.Unmarshal(raw, &n); err == nil {
		return workflowv1.StepType(n)
	}
	return workflowv1.StepType_STEP_TYPE_UNSPECIFIED
}

func routeTargets(routes map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range []string{"next", "approve", "true", "false", "reject"} {
		target := routes[key]
		if target == "" || strings.HasPrefix(target, "end-") || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

func workflowStatusFromString(s string) workflowv1.WorkflowStatus {
	switch s {
	case "running":
		return workflowv1.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	case "completed":
		return workflowv1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
	case "cancelled":
		return workflowv1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
	case "failed":
		return workflowv1.WorkflowStatus_WORKFLOW_STATUS_FAILED
	default:
		return workflowv1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
}

func workflowStatusToString(s workflowv1.WorkflowStatus) string {
	switch s {
	case workflowv1.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return "running"
	case workflowv1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED:
		return "completed"
	case workflowv1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED:
		return "cancelled"
	case workflowv1.WorkflowStatus_WORKFLOW_STATUS_FAILED:
		return "failed"
	default:
		return ""
	}
}

func stepStatusFromString(s string) workflowv1.StepStatus {
	switch s {
	case "pending":
		return workflowv1.StepStatus_STEP_STATUS_PENDING
	case "in_progress":
		return workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS
	case "completed":
		return workflowv1.StepStatus_STEP_STATUS_COMPLETED
	case "rejected":
		return workflowv1.StepStatus_STEP_STATUS_REJECTED
	case "skipped":
		return workflowv1.StepStatus_STEP_STATUS_SKIPPED
	default:
		return workflowv1.StepStatus_STEP_STATUS_UNSPECIFIED
	}
}

// ── Developer workflow-def management ────────────────────────────────────────

// WorkflowDefSummary is returned by list operations (no full steps payload).
type WorkflowDefSummary struct {
	ID            string     `json:"id"`
	ApplicationID string     `json:"application_id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	TriggerEvent  string     `json:"trigger_event"`
	Status        string     `json:"status"`
	StepCount     int        `json:"step_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"`
}

// WorkflowDefFull carries all fields including steps and context_schema.
type WorkflowDefFull struct {
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	TriggerEvent  string          `json:"trigger_event"`
	SubjectType   string          `json:"subject_type"`
	SubjectConfig json.RawMessage `json:"subject_config"`
	Status        string          `json:"status"`
	Steps         json.RawMessage `json:"steps"`
	ContextSchema json.RawMessage `json:"context_schema"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	PublishedAt   *time.Time      `json:"published_at,omitempty"`
	ArchivedAt    *time.Time      `json:"archived_at,omitempty"`
	// SingleActiveInstance: only one running instance per dimension-member
	// scope (a second start with the same members is refused as a
	// duplicate). Right for "submit this department's budget", wrong for
	// per-request forms — a per-definition choice, default on.
	SingleActiveInstance bool `json:"single_active_instance"`
}

// ListWorkflowDefs lists a revision's workflow defs plus revision-global
// (NULL revision) ones. An empty revisionID lists every def for the app.
func (s *Store) ListWorkflowDefs(ctx context.Context, appID, revisionID string) ([]*WorkflowDefSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, application_id::text, name, COALESCE(description,''),
		       trigger_event, status, jsonb_array_length(steps),
		       created_at, updated_at, published_at, archived_at
		FROM workflow.workflow_def
		WHERE application_id = $1::uuid
		  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		ORDER BY created_at DESC
	`, appID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*WorkflowDefSummary
	for rows.Next() {
		var w WorkflowDefSummary
		if err := rows.Scan(&w.ID, &w.ApplicationID, &w.Name, &w.Description,
			&w.TriggerEvent, &w.Status, &w.StepCount,
			&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt); err != nil {
			return nil, err
		}
		list = append(list, &w)
	}
	return list, rows.Err()
}

func (s *Store) GetWorkflowDefFull(ctx context.Context, defID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, name, COALESCE(description,''),
		       trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		       created_at, updated_at, published_at, archived_at, single_active_instance
		FROM workflow.workflow_def WHERE id = $1::uuid
	`, defID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workflow def %s not found", defID)
	}
	return &w, err
}

// CreateWorkflowDefFull creates a workflow def scoped to revisionID
// (empty = revision-global, visible in every revision).
// nameTaken turns the unique-index violation on (application_id,
// revision_id, name) into ErrWorkflowDefNameTaken; other errors pass through.
func nameTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "rev_name") {
		return ErrWorkflowDefNameTaken
	}
	return err
}

func (s *Store) CreateWorkflowDefFull(ctx context.Context, appID, revisionID, name, description, triggerEvent, userID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_def
		    (application_id, revision_id, name, description, trigger_event, created_by, updated_by)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, $6::uuid, $6::uuid)
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, appID, revisionID, name, description, triggerEvent, userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if err != nil {
		return nil, nameTaken(err)
	}
	return &w, nil
}

func (s *Store) UpdateWorkflowDefFull(ctx context.Context, defID, name, description, triggerEvent, subjectType, userID string, steps, contextSchema, subjectConfig json.RawMessage) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow.workflow_def
		SET name           = $2,
		    description    = $3,
		    trigger_event  = $4,
		    subject_type   = $5,
		    subject_config = COALESCE($6::jsonb, subject_config),
		    steps          = COALESCE($7::jsonb, steps),
		    context_schema = COALESCE($8::jsonb, context_schema),
		    updated_by     = $9::uuid,
		    updated_at     = now()
		WHERE id = $1::uuid AND status != 'archived'
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, defID, name, description, triggerEvent, subjectType, nullableJSON(subjectConfig), nullableJSON(steps), nullableJSON(contextSchema), userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowDefNotEditable, defID)
	}
	if err != nil {
		return nil, nameTaken(err)
	}
	return &w, nil
}

// SetWorkflowDefSingleActiveInstance flips the per-definition dedup choice
// (see WorkflowDefFull.SingleActiveInstance); archived definitions refuse.
func (s *Store) SetWorkflowDefSingleActiveInstance(ctx context.Context, defID string, v bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workflow.workflow_def SET single_active_instance = $2, updated_at = now()
		WHERE id = $1::uuid AND status != 'archived'
	`, defID, v)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrWorkflowDefNotEditable, defID)
	}
	return nil
}

func (s *Store) PublishWorkflowDef(ctx context.Context, defID, userID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow.workflow_def
		SET status = 'published', published_at = now(), updated_by = $2::uuid, updated_at = now()
		WHERE id = $1::uuid AND status IN ('draft', 'invalid')
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, defID, userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workflow def %s not found or not in publishable state", defID)
	}
	if err != nil {
		return nil, err
	}

	// Publish-to-active. Publishing means "live for the business", and the
	// business-facing surfaces (GET /api/workflow/definitions, the start
	// dialog) only show the ACTIVE revision's defs — but an AI-authored def
	// is born inside the session's lazily-created draft, which may never be
	// promoted, leaving a published workflow stranded where no business
	// user can ever see it (found live: "Country data approval" published
	// into an orphan draft, invisible until a manual one-row re-home).
	// Publishing now moves a non-active-revision def onto the active
	// revision, remapping context_schema dimension references by NAME so a
	// "Dimension member" variable keeps pointing at the geography the
	// author meant rather than a dead revision's copy of it. A dimension
	// with no same-named counterpart keeps its old reference — the start
	// dialog surfaces that as a fix-it warning rather than failing here.
	// The to_regclass guard only skips when core.model does not exist at
	// all — minimal test fixtures; every real deployment migrates it.
	var coreModelExists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('core.model') IS NOT NULL`).Scan(&coreModelExists); err != nil {
		return nil, fmt.Errorf("publish re-home precheck: %w", err)
	}
	if !coreModelExists {
		return &w, nil
	}
	if _, rhErr := s.pool.Exec(ctx, `
		WITH target AS (
			SELECT m.active_revision_id AS active_rev
			FROM workflow.workflow_def wd
			JOIN core.model m ON m.application_id = wd.application_id
			WHERE wd.id = $1::uuid AND m.active_revision_id IS NOT NULL
			LIMIT 1
		)
		UPDATE workflow.workflow_def wd SET
			revision_id = t.active_rev,
			context_schema = COALESCE((
				SELECT jsonb_agg(
					CASE WHEN (e.elem ? 'dimension_id') AND nd.id IS NOT NULL
					     THEN jsonb_set(e.elem, '{dimension_id}', to_jsonb(nd.id::text))
					     ELSE e.elem END ORDER BY e.ord)
				FROM jsonb_array_elements(wd.context_schema) WITH ORDINALITY AS e(elem, ord)
				LEFT JOIN model.dimension_def od ON od.id::text = e.elem->>'dimension_id'
				LEFT JOIN model.dimension_def nd ON nd.model_id = od.model_id
				     AND lower(nd.name) = lower(od.name)
				     AND nd.revision_id = t.active_rev
			), wd.context_schema)
		FROM target t
		WHERE wd.id = $1::uuid
		  AND wd.revision_id IS NOT NULL
		  AND wd.revision_id IS DISTINCT FROM t.active_rev
	`, defID); rhErr != nil {
		return nil, fmt.Errorf("publish re-home to active revision: %w", rhErr)
	}
	return &w, nil
}

func (s *Store) ArchiveWorkflowDef(ctx context.Context, defID, userID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow.workflow_def
		SET status = 'archived', archived_at = now(), updated_by = $2::uuid, updated_at = now()
		WHERE id = $1::uuid AND status != 'archived'
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, defID, userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workflow def %s not found or already archived", defID)
	}
	return &w, err
}

// RestoreWorkflowDef is ArchiveWorkflowDef's inverse: an archived definition
// goes back to draft (never straight to published — it must pass validation
// and an explicit publish again).
func (s *Store) RestoreWorkflowDef(ctx context.Context, defID, userID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow.workflow_def
		SET status = 'draft', archived_at = NULL, updated_by = $2::uuid, updated_at = now()
		WHERE id = $1::uuid AND status = 'archived'
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, defID, userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("workflow def %s not found or not archived", defID)
	}
	return &w, err
}

func (s *Store) DeleteWorkflowDef(ctx context.Context, defID string) error {
	// Only draft workflows with no instances may be deleted
	var instanceCount int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workflow.workflow_instance WHERE workflow_def_id = $1::uuid
	`, defID).Scan(&instanceCount)
	if instanceCount > 0 {
		return fmt.Errorf("cannot delete workflow with existing instances; archive it instead")
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM workflow.workflow_def WHERE id = $1::uuid AND status = 'draft'
	`, defID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("workflow def %s not found or not a draft", defID)
	}
	return nil
}

func (s *Store) DuplicateWorkflowDef(ctx context.Context, defID, newName, userID string) (*WorkflowDefFull, error) {
	var w WorkflowDefFull
	err := s.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_def
		    (application_id, revision_id, name, description, trigger_event, subject_type, subject_config, steps, context_schema, created_by, updated_by)
		SELECT application_id, revision_id, $2, description, trigger_event, subject_type, subject_config, steps, context_schema, $3::uuid, $3::uuid
		FROM workflow.workflow_def WHERE id = $1::uuid
		RETURNING id::text, application_id::text, name, COALESCE(description,''),
		          trigger_event, COALESCE(subject_type,''), COALESCE(subject_config,'{}'), status, steps, context_schema,
		          created_at, updated_at, published_at, archived_at, single_active_instance
	`, defID, newName, userID).Scan(
		&w.ID, &w.ApplicationID, &w.Name, &w.Description,
		&w.TriggerEvent, &w.SubjectType, &w.SubjectConfig, &w.Status, &w.Steps, &w.ContextSchema,
		&w.CreatedAt, &w.UpdatedAt, &w.PublishedAt, &w.ArchivedAt, &w.SingleActiveInstance,
	)
	if err != nil {
		return nil, nameTaken(err)
	}
	return &w, nil
}

// WorkflowDefUsage describes an automation rule that references a workflow.
type WorkflowDefUsage struct {
	RuleID      string `json:"rule_id"`
	RuleName    string `json:"rule_name"`
	TriggerType string `json:"trigger_type"`
	Enabled     bool   `json:"enabled"`
}

func (s *Store) GetWorkflowDefUsage(ctx context.Context, defID string) ([]*WorkflowDefUsage, error) {
	// Match by workflow_def_id (preferred) or legacy workflow_name
	defFull, err := s.GetWorkflowDefFull(ctx, defID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, trigger_type::text, enabled
		FROM workflow.automation_rule
		WHERE workflow_def_id = $1::uuid
		   OR (workflow_def_id IS NULL AND workflow_name = $2)
		ORDER BY created_at
	`, defID, defFull.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var usage []*WorkflowDefUsage
	for rows.Next() {
		var u WorkflowDefUsage
		if err := rows.Scan(&u.RuleID, &u.RuleName, &u.TriggerType, &u.Enabled); err != nil {
			return nil, err
		}
		usage = append(usage, &u)
	}
	return usage, rows.Err()
}

func (s *Store) ListWorkflowInstancesByDef(ctx context.Context, defID string, limit int) ([]*Execution, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, status::text, started_at, completed_at, COALESCE(context, '{}'::jsonb), test_run
		FROM workflow.workflow_instance
		WHERE workflow_def_id = $1::uuid
		ORDER BY started_at DESC LIMIT $2
	`, defID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// The real status, context and test-run flag: this used to hardcode
	// 'running' for every row, so a developer's Instances view never showed
	// a completed or cancelled run (found by the 2026-09-13 scenario run).
	var list []*Execution
	for rows.Next() {
		var e Execution
		var ctxJSON []byte
		if err := rows.Scan(&e.ID, &e.Status, &e.StartedAt, &e.CompletedAt, &ctxJSON, &e.TestRun); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(ctxJSON, &e.Context)
		list = append(list, &e)
	}
	return list, rows.Err()
}

// nullableJSON returns nil if j is empty or "null", enabling COALESCE($n::jsonb, col) in SQL.
func nullableJSON(j json.RawMessage) *string {
	if len(j) == 0 || string(j) == "null" {
		return nil
	}
	s := string(j)
	return &s
}

// ── Automation Rules ──────────────────────────────────────────────────────────

type AutomationRule struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	RevisionID    string `json:"revision_id,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	TriggerType   string `json:"trigger_type"`
	WorkflowName  string `json:"workflow_name"`
	WorkflowDefID string `json:"workflow_def_id,omitempty"`
	SourceFormID  string `json:"source_form_id,omitempty"`
	SourceGridID  string `json:"source_grid_id,omitempty"`
	// SourceIntegrationID scopes an integration_completed / integration_failed
	// rule to one integration; empty = any integration of the application.
	SourceIntegrationID string    `json:"source_integration_id,omitempty"`
	Enabled             bool      `json:"enabled"`
	CreatedAt           time.Time `json:"created_at"`

	// Schedule config — only meaningful when TriggerType == "schedule".
	CronExpr            string     `json:"cron_expr,omitempty"`
	Timezone            string     `json:"timezone,omitempty"`
	MisfirePolicy       string     `json:"misfire_policy,omitempty"`
	MaxRetries          int        `json:"max_retries"`
	RetryBackoffSeconds int        `json:"retry_backoff_seconds"`
	NextFireAt          *time.Time `json:"next_fire_at,omitempty"`
	LastFireAt          *time.Time `json:"last_fire_at,omitempty"`
}

// ScheduleConfig is an automation rule's cron trigger configuration. Pass
// nil to CreateAutomationRule/UpdateAutomationRule for any non-"schedule"
// trigger type.
type ScheduleConfig struct {
	CronExpr            string
	Timezone            string // IANA name; defaults to "UTC" when empty
	MisfirePolicy       string // "skip" (default) or "fire_now"
	MaxRetries          int
	RetryBackoffSeconds int
}

// nextFireTime parses cronExpr (standard 5-field cron) in the given IANA
// timezone and returns its next occurrence after now.
func nextFireTime(cronExpr, timezone string, now time.Time) (time.Time, error) {
	loc := time.UTC
	if timezone != "" {
		l, err := time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone %q: %w", timezone, err)
		}
		loc = l
	}
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return sched.Next(now.In(loc)), nil
}

type Execution struct {
	ID             string            `json:"id"`
	RuleID         string            `json:"rule_id"`
	ApplicationID  string            `json:"application_id"`
	Status         string            `json:"status"`
	TriggerPayload map[string]string `json:"trigger_payload"`
	InstanceID     string            `json:"instance_id,omitempty"`
	Error          string            `json:"error,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
	ScheduledFor   *time.Time        `json:"scheduled_for,omitempty"`
	ClaimedBy      string            `json:"claimed_by,omitempty"`
	// Instance-level fields, filled by ListWorkflowInstancesByDef.
	Context map[string]string `json:"context,omitempty"`
	TestRun bool              `json:"test_run,omitempty"`
}

// CreateAutomationRule creates (or upserts by name) a rule scoped to
// revisionID (empty = revision-global, visible in every revision). sched
// must be non-nil when triggerType is "schedule" (and is ignored
// otherwise) — it computes and stores the rule's first next_fire_at.
// ErrRuleWorkflowInvalid marks a rule whose bound workflow def isn't a legal
// target, so HTTP callers can map it to 400/403 rather than a generic 500.
var ErrRuleWorkflowInvalid = errors.New("automation rule's workflow is not a valid target")

// ValidateRuleWorkflowDef enforces the invariant an automation rule's
// workflow_def_id has to satisfy: it must exist, be published, and belong to
// the SAME application and revision as the rule.
//
// Only the published half of this existed, and only at creation. Update
// validated nothing at all, and fireRule trusted whatever was stored — so a
// rule could be repointed at another APPLICATION's published workflow and
// then fired, starting an instance of another tenant's definition. (The
// gateway's resource guard covers the rule being edited; it says nothing
// about the def that rule names.) A rule could also drift onto another
// revision's definition of the same name, which is a correctness problem
// rather than a security one but produces equally confusing behaviour.
//
// Revisions are compared only when both sides have one: legacy rows carry a
// NULL revision and are visible in every revision, the same tolerance the
// dashboard-widget and form-field checks use.
func (s *Store) ValidateRuleWorkflowDef(ctx context.Context, ruleAppID, ruleRevisionID, defID string) error {
	var defAppID, defRevisionID, status string
	if err := s.pool.QueryRow(ctx, `
		SELECT application_id::text, COALESCE(revision_id::text,''), status
		FROM workflow.workflow_def WHERE id = $1::uuid
	`, defID).Scan(&defAppID, &defRevisionID, &status); err != nil {
		return fmt.Errorf("%w: workflow def %s not found", ErrRuleWorkflowInvalid, defID)
	}
	if defAppID != ruleAppID {
		return fmt.Errorf("%w: it belongs to a different application", ErrRuleWorkflowInvalid)
	}
	if status != "published" {
		// Wraps ErrWorkflowNotPublished, not just ErrRuleWorkflowInvalid: the
		// HTTP layer already maps that sentinel (handler.go's start and
		// trigger paths), and "the workflow isn't published" means exactly
		// the same thing whether ResolveStartContext or this check notices
		// it first. errors.Is matches both.
		return fmt.Errorf("%w: workflow is %s; publish it before wiring an automation rule to it (%w)", ErrWorkflowNotPublished, status, ErrRuleWorkflowInvalid)
	}
	if ruleRevisionID != "" && defRevisionID != "" && ruleRevisionID != defRevisionID {
		return fmt.Errorf("%w: it belongs to a different revision", ErrRuleWorkflowInvalid)
	}
	return nil
}

func (s *Store) CreateAutomationRule(ctx context.Context, appID, revisionID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID string, sched *ScheduleConfig) (*AutomationRule, error) {
	return s.CreateAutomationRuleScoped(ctx, appID, revisionID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID, "", sched)
}

// CreateAutomationRuleScoped is CreateAutomationRule plus the integration
// scope (source_integration_id) for integration_completed/_failed rules.
func (s *Store) CreateAutomationRuleScoped(ctx context.Context, appID, revisionID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID, sourceIntegrationID string, sched *ScheduleConfig) (*AutomationRule, error) {
	if workflowDefID != "" {
		if err := s.ValidateRuleWorkflowDef(ctx, appID, revisionID, workflowDefID); err != nil {
			return nil, err
		}
	}

	var cronExpr, timezone, misfirePolicy string
	var maxRetries, retryBackoff int
	var nextFireAt *time.Time
	if triggerType == "schedule" {
		if sched == nil || sched.CronExpr == "" {
			return nil, fmt.Errorf("cron_expr is required for a schedule-triggered rule")
		}
		timezone = sched.Timezone
		if timezone == "" {
			timezone = "UTC"
		}
		misfirePolicy = sched.MisfirePolicy
		if misfirePolicy == "" {
			misfirePolicy = "skip"
		}
		next, err := nextFireTime(sched.CronExpr, timezone, time.Now())
		if err != nil {
			return nil, err
		}
		cronExpr = sched.CronExpr
		maxRetries = sched.MaxRetries
		retryBackoff = sched.RetryBackoffSeconds
		nextFireAt = &next
	}

	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO workflow.automation_rule
		    (application_id, revision_id, name, description, trigger_type, workflow_name,
		     workflow_def_id, source_form_id, source_grid_id, source_integration_id,
		     cron_expr, timezone, misfire_policy, max_retries, retry_backoff_seconds, next_fire_at)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5::workflow.trigger_type, $6,
		        NULLIF($7,'')::uuid, NULLIF($8,'')::uuid, NULLIF($9,'')::uuid, NULLIF($16,'')::uuid,
		        NULLIF($10,''), COALESCE(NULLIF($11,''),'UTC'), COALESCE(NULLIF($12,''),'skip'), $13, $14, $15)
		ON CONFLICT (application_id, revision_id, name) DO UPDATE
		    SET description    = EXCLUDED.description,
		        workflow_name  = EXCLUDED.workflow_name,
		        workflow_def_id= EXCLUDED.workflow_def_id,
		        source_form_id = EXCLUDED.source_form_id,
		        source_grid_id = EXCLUDED.source_grid_id,
		        source_integration_id = EXCLUDED.source_integration_id,
		        cron_expr      = EXCLUDED.cron_expr,
		        timezone       = EXCLUDED.timezone,
		        misfire_policy = EXCLUDED.misfire_policy,
		        max_retries    = EXCLUDED.max_retries,
		        retry_backoff_seconds = EXCLUDED.retry_backoff_seconds,
		        next_fire_at   = EXCLUDED.next_fire_at
		RETURNING id::text, created_at
	`, appID, revisionID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID,
		cronExpr, timezone, misfirePolicy, maxRetries, retryBackoff, nextFireAt, sourceIntegrationID).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create automation rule: %w", err)
	}
	return &AutomationRule{
		ID:                  id,
		ApplicationID:       appID,
		RevisionID:          revisionID,
		Name:                name,
		Description:         description,
		TriggerType:         triggerType,
		WorkflowName:        workflowName,
		WorkflowDefID:       workflowDefID,
		SourceFormID:        sourceFormID,
		SourceGridID:        sourceGridID,
		SourceIntegrationID: sourceIntegrationID,
		Enabled:             true,
		CreatedAt:           createdAt,
		CronExpr:            cronExpr,
		Timezone:            timezone,
		MisfirePolicy:       misfirePolicy,
		MaxRetries:          maxRetries,
		RetryBackoffSeconds: retryBackoff,
		NextFireAt:          nextFireAt,
	}, nil
}

// ListAutomationRules lists a revision's rules plus revision-global (NULL
// revision) ones. An empty revisionID lists every rule for the app.
func (s *Store) ListAutomationRules(ctx context.Context, appID, revisionID string) ([]*AutomationRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, application_id::text, COALESCE(revision_id::text,''), name, COALESCE(description,''),
		       trigger_type::text, workflow_name,
		       COALESCE(workflow_def_id::text,''), COALESCE(source_form_id::text,''), COALESCE(source_grid_id::text,''),
		       COALESCE(source_integration_id::text,''),
		       enabled, created_at,
		       COALESCE(cron_expr,''), timezone, misfire_policy, max_retries, retry_backoff_seconds,
		       next_fire_at, last_fire_at
		FROM workflow.automation_rule
		WHERE application_id = $1::uuid
		  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		ORDER BY created_at
	`, appID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*AutomationRule
	for rows.Next() {
		var r AutomationRule
		if err := rows.Scan(&r.ID, &r.ApplicationID, &r.RevisionID, &r.Name, &r.Description,
			&r.TriggerType, &r.WorkflowName, &r.WorkflowDefID, &r.SourceFormID, &r.SourceGridID,
			&r.SourceIntegrationID,
			&r.Enabled, &r.CreatedAt,
			&r.CronExpr, &r.Timezone, &r.MisfirePolicy, &r.MaxRetries, &r.RetryBackoffSeconds,
			&r.NextFireAt, &r.LastFireAt); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	return rules, rows.Err()
}

// Sentinel errors ResolveStartContext returns, so callers (the HTTP
// workflowStartInstance handler and TriggerRule's automation-trigger HTTP
// wrapper) can each map them to their own status codes without duplicating
// the underlying checks.
var (
	ErrWorkflowNotPublished = errors.New("workflow is not published")
	ErrNoRACIScope          = errors.New("you have no RACI-responsible scope to start this workflow with")
	ErrDuplicateInstance    = errors.New("a matching workflow instance is already running")
	ErrHiddenScope          = errors.New("access denied: you don't have access to the dimension member this workflow would be scoped to")
	ErrUnknownMetric        = errors.New("the metric this workflow would be scoped to does not exist in the model")
	ErrMissingContext       = errors.New("a required context value is missing")
	ErrUnknownMember        = errors.New("the dimension member this workflow would be scoped to does not exist")
	// ErrWorkflowDefNotEditable: the definition is archived (or gone) — edits
	// are refused rather than silently dropped; restore it to a draft first.
	ErrWorkflowDefNotEditable = errors.New("workflow definition is archived or does not exist; restore it before editing")
	// ErrWorkflowDefNameTaken: (application, revision, name) is unique and
	// archived definitions keep their name — restore or rename the old one.
	ErrWorkflowDefNameTaken = errors.New("a workflow definition with this name already exists in this revision (archived definitions keep their name — restore or rename it)")
)

// startContextVarDef mirrors the frontend's ContextVariable
// (web/src/api/client.ts) — workflow_def.context_schema is stored as opaque
// JSONB, so this is parsed locally rather than shared via a generated type
// (same convention as internal/gateway's contextVarDef).
type startContextVarDef struct {
	Key         string `json:"key"`
	DataType    string `json:"data_type"`
	Required    bool   `json:"required"`
	SourceHint  string `json:"source_hint"`
	DimensionID string `json:"dimension_id"`
}

// resolveRACIResponsible returns the single dimension-member code a user is
// "responsible" for per security.raci_rule (pattern "CC_SALES.*" -> "CC_SALES"),
// scoped to one application. ok=false means the caller has no such grant.
func (s *Store) resolveRACIResponsible(ctx context.Context, appID, userID string) (code string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT resource_pattern FROM security.raci_rule
		WHERE application_id=$1::uuid AND user_id=$2::uuid AND raci_type='responsible'
		ORDER BY created_at LIMIT 1
	`, appID, userID).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(code, ".*"), true, nil
}

// checkDimMemberAccess resolves (dimensionID, code) to a dimension_member
// ID and checks it against userID's identity.user_access_rule — "hidden"
// (direct or via an AncestorChain cascade, mirroring writeguard.CheckWrite)
// returns ErrHiddenScope. An unresolvable code (dimensionID/code doesn't
// match any real member) is not an error here — nothing to restrict,
// matching writeguard's own "unknown member: nothing to restrict" precedent
// (see e.g. query.Store.Writeback).
func (s *Store) checkDimMemberAccess(ctx context.Context, userID, dimensionID, code string) error {
	if dimensionID == "" || code == "" {
		return nil
	}
	var memberID string
	if err := s.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
		dimensionID, code,
	).Scan(&memberID); err != nil {
		return nil //nolint:nilerr // unknown member: nothing to restrict
	}
	access, aErr := writeguard.HiddenAccess(ctx, s.pool, userID, memberID)
	if aErr != nil {
		return fmt.Errorf("check access rule: %w", aErr)
	}
	if access == "hidden" {
		return ErrHiddenScope
	}
	chain, cErr := writeguard.AncestorChain(ctx, s.pool, memberID)
	if cErr != nil {
		return fmt.Errorf("check ancestor chain: %w", cErr)
	}
	for _, anc := range chain {
		if anc.ID == memberID {
			continue
		}
		ancAccess, aaErr := writeguard.HiddenAccess(ctx, s.pool, userID, anc.ID)
		if aaErr != nil {
			return fmt.Errorf("check access rule: %w", aaErr)
		}
		if ancAccess == "hidden" {
			return ErrHiddenScope
		}
	}
	return nil
}

// ResolveStartContext is the one implementation of "is this workflow
// startable right now, and with what final context" shared by every
// UI-driven start path (the direct HTTP /api/workflow/start handler and
// every automation_button click, which goes through TriggerRule below) —
// mirrors the checks internal/writeguard.CheckWrite centralized for writes
// earlier in this codebase's history, applied here to workflow starts:
//   - the workflow must be published (ErrWorkflowNotPublished)
//   - every "Dimension member" context var sourced from RACI
//     (source_hint=raci_responsible) is resolved server-side from the
//     caller's security.raci_rule grant, never trusted from the caller —
//     a Required one with no grant is rejected (ErrNoRACIScope)
//   - every resolved "Dimension member" context value (RACI-sourced or
//     directly supplied by the caller) is checked against the starting
//     user's identity.user_access_rule (hidden, with the same ancestor
//     cascade writeguard.CheckWrite uses) — rejected with ErrHiddenScope.
//     Without this, a non-RACI "Dimension member" var was copied straight
//     from the caller-supplied candidate map with no check at all, so a
//     user hidden from a dimension member could still start (and, via an
//     on_approve action, eventually write facts for) an instance scoped
//     to it, even though the equivalent direct grid write is rejected.
//   - starting a second instance already covering the exact same
//     dimension-member scope is rejected (ErrDuplicateInstance), keyed on
//     every resolved Dimension member context value together so a manually
//     entered scope dedupes the same way as an RACI-resolved one
//
// Returns the final context map to start the instance with (candidate,
// with RACI-resolved values merged in / overridden).
func (s *Store) ResolveStartContext(ctx context.Context, defID, userID string, candidate map[string]string) (map[string]string, error) {
	def, err := s.GetWorkflowDefFull(ctx, defID)
	if err != nil {
		return nil, err
	}
	if def.Status != "published" {
		return nil, ErrWorkflowNotPublished
	}

	var ctxVars []startContextVarDef
	_ = json.Unmarshal(def.ContextSchema, &ctxVars)

	result := make(map[string]string, len(candidate))
	maps.Copy(result, candidate)

	// Every "Metric" context var is validated against the app's active
	// model and normalized to the metric's canonical NAME — names survive
	// revision copies re-minting ids, and the writeguard lock matches by
	// name (see writeguard.WorkflowLockReasonForMetrics). Accepts a name
	// or an id as input (the picker sends names; older payloads sent ids).
	for _, cv := range ctxVars {
		if cv.DataType != "Metric" {
			continue
		}
		raw, ok := result[cv.Key]
		if !ok || strings.TrimSpace(raw) == "" {
			if cv.Required {
				return nil, ErrUnknownMetric
			}
			continue
		}
		var canonical string
		if err := s.pool.QueryRow(ctx, `
			SELECT md.name
			FROM model.metric_def md
			JOIN core.model m ON m.id = md.model_id
			WHERE m.application_id = $1::uuid
			  AND (m.active_revision_id IS NULL OR md.revision_id = m.active_revision_id)
			  AND (md.id::text = $2 OR lower(md.name) = lower($2))
			ORDER BY (md.id::text = $2) DESC
			LIMIT 1
		`, def.ApplicationID, strings.TrimSpace(raw)).Scan(&canonical); err != nil {
			return nil, ErrUnknownMetric
		}
		result[cv.Key] = canonical
	}

	dimMemberKeys := map[string]string{}
	for _, cv := range ctxVars {
		// Metric vars join the duplicate-instance scope key below, so
		// (Canada, revenue) and (Canada, cost) are distinct startable
		// scopes while an exact repeat is still rejected.
		if cv.DataType == "Metric" {
			if v, ok := result[cv.Key]; ok && v != "" {
				dimMemberKeys[cv.Key] = v
			}
			continue
		}
		if cv.DataType != "Dimension member" {
			continue
		}
		if cv.SourceHint == "raci_responsible" {
			code, ok, rErr := s.resolveRACIResponsible(ctx, def.ApplicationID, userID)
			if rErr != nil {
				return nil, rErr
			}
			if !ok {
				if cv.Required {
					return nil, ErrNoRACIScope
				}
				continue
			}
			result[cv.Key] = code
		}
		v, ok := result[cv.Key]
		if !ok || strings.TrimSpace(v) == "" {
			// "required" on a Dimension-member variable was only enforced for
			// RACI-resolved ones; a manual start with the value left out went
			// through and produced an instance scoped to nothing (found by
			// the 2026-09-13 workflow scenario run).
			if cv.Required {
				return nil, fmt.Errorf("%w: %s", ErrMissingContext, cv.Key)
			}
			continue
		}
		// The code must name a real member of the variable's dimension —
		// a typo used to start an instance nobody could act on sensibly.
		if cv.DimensionID != "" {
			var exists bool
			if err := s.pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
				cv.DimensionID, v).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				return nil, fmt.Errorf("%w: %s=%q", ErrUnknownMember, cv.Key, v)
			}
		}
		dimMemberKeys[cv.Key] = v
		if hidErr := s.checkDimMemberAccess(ctx, userID, cv.DimensionID, v); hidErr != nil {
			return nil, hidErr
		}
	}

	if len(dimMemberKeys) > 0 && def.SingleActiveInstance {
		running, dErr := s.pool.Query(ctx,
			// A developer's test run is a dry run: it must never block a real
			// start (found live: every form-triggered start failed "already
			// running" against the developer's test-run instance).
			`SELECT context FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid AND status='running' AND test_run = false`,
			defID,
		)
		if dErr != nil {
			return nil, dErr
		}
		defer running.Close()
		for running.Next() {
			var ctxJSON []byte
			if scanErr := running.Scan(&ctxJSON); scanErr != nil {
				continue
			}
			var existing map[string]string
			if json.Unmarshal(ctxJSON, &existing) != nil {
				continue
			}
			matches := true
			for k, v := range dimMemberKeys {
				if existing[k] != v {
					matches = false
					break
				}
			}
			if matches {
				return nil, ErrDuplicateInstance
			}
		}
		if err := running.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// fireRule resolves rule's workflow def (by workflow_def_id if set,
// otherwise by workflow_name), starts a workflow instance, and returns the
// instance ID plus the execution status/completion that mirrors its
// lifecycle. Shared core between TriggerRule (manual/API/event-dispatched)
// and TriggerScheduledRule (cron) — every fire path ends up here so there
// is exactly one implementation of "how does a rule actually start its
// workflow."
func (s *Store) fireRule(ctx context.Context, rule AutomationRule, triggeredByUserID string, payload map[string]string) (instanceID, execStatus string, instCompletedAt *time.Time, err error) {
	defID := rule.WorkflowDefID
	if defID == "" {
		// Name fallback for rules created before workflow_def_id existed.
		// Scoped to the rule's own revision when it has one, so a rule can't
		// silently bind to another revision's definition of the same name.
		if err = s.pool.QueryRow(ctx, `
			SELECT id::text FROM workflow.workflow_def
			WHERE application_id = $1::uuid AND name = $2
			  AND status != 'archived'
			  AND ($3 = '' OR revision_id IS NULL OR revision_id::text = $3)
			ORDER BY created_at DESC LIMIT 1
		`, rule.ApplicationID, rule.WorkflowName, rule.RevisionID).Scan(&defID); err != nil {
			return "", "", nil, fmt.Errorf("find workflow %q: %w", rule.WorkflowName, err)
		}
	}

	// Revalidate at execution, not just at save: the binding may have been
	// written before these checks existed, or the def may have been archived
	// or unpublished since.
	if err = s.ValidateRuleWorkflowDef(ctx, rule.ApplicationID, rule.RevisionID, defID); err != nil {
		return "", "", nil, err
	}

	contextVars, err := s.ResolveStartContext(ctx, defID, triggeredByUserID, payload)
	if err != nil {
		return "", "", nil, err
	}
	instance, err := s.StartWorkflow(ctx, defID, triggeredByUserID, contextVars)
	if err != nil {
		return "", "", nil, fmt.Errorf("start workflow: %w", err)
	}

	// The execution mirrors the instance lifecycle: 'running' now, updated by
	// closeInstanceIfIdle when the instance finishes. An instance whose steps
	// were all auto-processed may already be closed — report its real status.
	execStatus = "running"
	var instStatus string
	if qErr := s.pool.QueryRow(ctx, `
		SELECT status::text, completed_at FROM workflow.workflow_instance WHERE id = $1::uuid
	`, instance.Id).Scan(&instStatus, &instCompletedAt); qErr == nil && instStatus != "running" {
		execStatus = "completed"
		if instStatus == "cancelled" {
			execStatus = "cancelled"
		}
	} else {
		instCompletedAt = nil
	}
	return instance.Id, execStatus, instCompletedAt, nil
}

// TriggerRule fires an automation rule: resolves the workflow (by
// workflow_def_id if set, otherwise by workflow_name), starts a workflow
// instance, and records the execution.
func (s *Store) TriggerRule(ctx context.Context, ruleID, triggeredByUserID string, payload map[string]string) (*Execution, error) {
	var rule AutomationRule
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, COALESCE(revision_id::text,''), name, workflow_name,
		       COALESCE(workflow_def_id::text,''), enabled
		FROM workflow.automation_rule WHERE id = $1::uuid
	`, ruleID).Scan(&rule.ID, &rule.ApplicationID, &rule.RevisionID, &rule.Name, &rule.WorkflowName, &rule.WorkflowDefID, &rule.Enabled)
	if err != nil {
		return nil, fmt.Errorf("load rule: %w", err)
	}

	payloadJSON, _ := json.Marshal(payload)
	// Every failed fire leaves an execution row — otherwise event-dispatched
	// rules (fire-and-forget) fail with no trace anywhere.
	recordFailure := func(fireErr error) {
		_, _ = s.pool.Exec(ctx, `
			INSERT INTO workflow.execution
			    (rule_id, application_id, status, trigger_payload, error, completed_at)
			VALUES ($1::uuid, $2::uuid, 'failed', $3, $4, now())
		`, ruleID, rule.ApplicationID, payloadJSON, fireErr.Error())
		s.notifyStartFailure(ctx, triggeredByUserID, ruleID, rule.Name, fireErr)
	}

	if !rule.Enabled {
		err := fmt.Errorf("automation rule %s is disabled", ruleID)
		recordFailure(err)
		return nil, err
	}

	instanceID, execStatus, instCompletedAt, err := s.fireRule(ctx, rule, triggeredByUserID, payload)
	if err != nil {
		recordFailure(err)
		return nil, err
	}

	var execID string
	var startedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workflow.execution
		    (rule_id, application_id, status, trigger_payload, instance_id, completed_at)
		VALUES ($1::uuid, $2::uuid, $3::workflow.execution_status, $4, $5::uuid, $6)
		RETURNING id::text, started_at
	`, ruleID, rule.ApplicationID, execStatus, payloadJSON, instanceID, instCompletedAt).Scan(&execID, &startedAt)
	if err != nil {
		return nil, fmt.Errorf("record execution: %w", err)
	}

	return &Execution{
		ID:             execID,
		RuleID:         ruleID,
		ApplicationID:  rule.ApplicationID,
		Status:         execStatus,
		TriggerPayload: payload,
		InstanceID:     instanceID,
		StartedAt:      startedAt,
		CompletedAt:    instCompletedAt,
	}, nil
}

// ErrAlreadyClaimed is returned by TriggerScheduledRule when another
// scheduler instance already claimed (or has already processed) the same
// rule's tick for the given scheduledFor time. Callers should treat this as
// a normal, silent skip — it is the expected outcome of losing a race
// between two concurrently-polling gateway instances, not a failure.
// notifyStartFailure tells the person whose action was meant to start a
// workflow that it did not: event rules fire in the background, so until
// now a refused start (a duplicate, a hidden member, a missing value) was
// visible only in the execution log the submitter cannot see. Nothing is
// sent for system-initiated fires (the scheduler).
func (s *Store) notifyStartFailure(ctx context.Context, userID, ruleID, ruleName string, fireErr error) {
	if userID == "" {
		return
	}
	var sub string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(keycloak_sub,'') FROM identity."user" WHERE id = $1::uuid`, userID).Scan(&sub); err != nil || sub == "system-scheduler" {
		return
	}
	vars := map[string]string{
		"subject": "Workflow could not start",
		"message": fmt.Sprintf("%s: %s", ruleName, fireErr.Error()),
	}
	_, _ = notification.NewStore(s.pool).Notify(ctx, userID, "workflow_step_notification", vars, "automation_rule", ruleID)
}

var ErrAlreadyClaimed = errors.New("scheduled tick already claimed by another worker")

// TriggerScheduledRule fires one cron tick of a schedule-triggered
// automation rule. It first atomically claims the (rule, scheduledFor) slot
// via workflow.execution's partial unique index on (rule_id, scheduled_for)
// — the INSERT succeeding *is* the cross-instance idempotency and execution
// ownership guarantee (see migration 059_scheduled_automation.sql) — then
// fires through the same fireRule core TriggerRule uses, retrying up to
// rule.MaxRetries times (sleeping RetryBackoffSeconds between attempts —
// acceptable to block here since this always runs in a background
// goroutine, never a request handler) before giving up.
func (s *Store) TriggerScheduledRule(ctx context.Context, rule AutomationRule, scheduledFor time.Time, workerID string) (*Execution, error) {
	// Scheduled fires have no human actor. workflow_instance.started_by is a
	// NOT NULL FK into identity.user, so this needs a real row, not a bare
	// sentinel UUID — lazily upserted the first time it's needed. Resolved
	// before the claim below so a failure here never leaves a claimed-but-
	// abandoned execution row behind.
	systemUserID, err := s.ensureSystemUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve system user: %w", err)
	}

	payload := map[string]string{}
	payloadJSON, _ := json.Marshal(payload)

	var execID string
	var startedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workflow.execution
		    (rule_id, application_id, status, trigger_payload, scheduled_for, claimed_by)
		VALUES ($1::uuid, $2::uuid, 'running', $3, $4, $5)
		ON CONFLICT (rule_id, scheduled_for) WHERE scheduled_for IS NOT NULL DO NOTHING
		RETURNING id::text, started_at
	`, rule.ID, rule.ApplicationID, payloadJSON, scheduledFor, workerID).Scan(&execID, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlreadyClaimed
	}
	if err != nil {
		return nil, fmt.Errorf("claim scheduled tick: %w", err)
	}

	var instanceID, execStatus string
	var instCompletedAt *time.Time
	var fireErr error
	attempts := rule.MaxRetries + 1
	backoff := time.Duration(rule.RetryBackoffSeconds) * time.Second
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && backoff > 0 {
			time.Sleep(backoff)
		}
		instanceID, execStatus, instCompletedAt, fireErr = s.fireRule(ctx, rule, systemUserID, payload)
		if fireErr == nil {
			break
		}
	}

	if fireErr != nil {
		_, _ = s.pool.Exec(ctx, `
			UPDATE workflow.execution SET status='failed', error=$2, completed_at=now()
			WHERE id = $1::uuid
		`, execID, fireErr.Error())
		return nil, fireErr
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE workflow.execution
		SET status = $2::workflow.execution_status, instance_id = $3::uuid, completed_at = $4
		WHERE id = $1::uuid
	`, execID, execStatus, instanceID, instCompletedAt); err != nil {
		return nil, fmt.Errorf("record execution: %w", err)
	}

	scheduledForCopy := scheduledFor
	return &Execution{
		ID:             execID,
		RuleID:         rule.ID,
		ApplicationID:  rule.ApplicationID,
		Status:         execStatus,
		TriggerPayload: payload,
		InstanceID:     instanceID,
		StartedAt:      startedAt,
		CompletedAt:    instCompletedAt,
		ScheduledFor:   &scheduledForCopy,
		ClaimedBy:      workerID,
	}, nil
}

// DispatchEventRules finds all enabled automation rules for the given
// application and trigger type, optionally filtered to a specific source
// (form ID for form_submit / form_approval; grid ID for grid_change), and
// fires each matching rule asynchronously. Errors per rule are logged but do
// not block the caller.
// DispatchEventRules fires enabled rules matching the trigger. revisionID
// (when non-empty) restricts firing to rules of that revision, plus
// revision-global (NULL revision) rules — without it, every revision's copy
// of a rule would fire for a single event.
func (s *Store) DispatchEventRules(ctx context.Context, appID, revisionID, triggerType, sourceID, userID string, payload map[string]string) {
	// form_submit means a record was SUBMITTED — drafts being saved must not
	// start approval workflows. Callers that don't know the status (legacy)
	// pass no status and are dispatched as before.
	if triggerType == "form_submit" {
		if st, ok := payload["status"]; ok && st != "" && st != "submitted" {
			return
		}
	}
	// Enrich form-event payloads with the record's field values so condition
	// steps and context_schema variables (e.g. "amount") have data to read.
	if (triggerType == "form_submit" || triggerType == "form_approval") && payload["record_id"] != "" {
		payload = s.enrichFormPayload(ctx, payload)
	}

	// A rule scoped to a specific form/grid fires only when the event names
	// that source; an event with no source matches only unscoped rules.
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM workflow.automation_rule
		WHERE application_id = $1::uuid
		  AND trigger_type = $2::workflow.trigger_type
		  AND enabled = true
		  AND ($4 = '' OR revision_id IS NULL OR revision_id::text = $4)
		  AND (
		      (trigger_type IN ('form_submit','form_approval') AND (source_form_id IS NULL OR ($3 <> '' AND source_form_id::text = $3)))
		      OR (trigger_type = 'grid_change'                 AND (source_grid_id IS NULL OR ($3 <> '' AND source_grid_id::text = $3)))
		      OR (trigger_type IN ('integration_completed','integration_failed')
		          AND (source_integration_id IS NULL OR ($3 <> '' AND source_integration_id::text = $3)))
		      OR trigger_type NOT IN ('form_submit','form_approval','grid_change','integration_completed','integration_failed')
		  )
	`, appID, triggerType, sourceID, revisionID)
	if err != nil {
		return
	}
	defer rows.Close()

	var ruleIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ruleIDs = append(ruleIDs, id)
		}
	}
	_ = rows.Err()

	for _, id := range ruleIDs {
		ruleID := id
		go func() {
			bgCtx := context.WithoutCancel(ctx)
			if _, err := s.TriggerRule(bgCtx, ruleID, userID, payload); err != nil {
				// Fire-and-forget: log at warn level if possible but do not propagate.
				_ = err
			}
		}()
	}
}

// enrichFormPayload merges the triggering form record's scalar field values
// into the event payload (existing keys win), so workflows can read fields
// like "amount" from context. Runs in the gateway process, which already has
// runtime-schema access; failures leave the payload unenriched.
func (s *Store) enrichFormPayload(ctx context.Context, payload map[string]string) map[string]string {
	var dataJSON []byte
	if err := s.pool.QueryRow(ctx, `
		SELECT data FROM runtime.form_record WHERE id = $1::uuid
	`, payload["record_id"]).Scan(&dataJSON); err != nil {
		return payload
	}
	var data map[string]any
	if json.Unmarshal(dataJSON, &data) != nil {
		return payload
	}
	enriched := make(map[string]string, len(payload)+len(data))
	for k, v := range data {
		switch v.(type) {
		case string, float64, bool, json.Number, int:
			enriched[k] = fmt.Sprintf("%v", v)
		}
	}
	for k, v := range payload {
		enriched[k] = v
	}
	return enriched
}

// DeleteAutomationRule removes an automation rule, plus any dashboard
// widgets (automation_button) referencing it — ref_id has no FK, and a
// dangling button fired a 404 on click (SYNC-01 cascade policy).
func (s *Store) DeleteAutomationRule(ctx context.Context, ruleID string) error {
	_, _ = s.pool.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, ruleID)
	_, err := s.pool.Exec(ctx, `DELETE FROM workflow.automation_rule WHERE id = $1::uuid`, ruleID)
	return err
}

// UpdateAutomationRule updates mutable fields of an automation rule. sched,
// when non-nil, replaces the rule's schedule config wholesale and
// recomputes next_fire_at from it; pass nil to leave scheduling untouched.
func (s *Store) UpdateAutomationRule(ctx context.Context, ruleID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID string, enabled *bool, sched *ScheduleConfig) (*AutomationRule, error) {
	return s.UpdateAutomationRuleScoped(ctx, ruleID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID, "", enabled, sched)
}

// UpdateAutomationRuleScoped is UpdateAutomationRule plus the integration
// scope; an empty sourceIntegrationID leaves the stored value untouched.
func (s *Store) UpdateAutomationRuleScoped(ctx context.Context, ruleID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID, sourceIntegrationID string, enabled *bool, sched *ScheduleConfig) (*AutomationRule, error) {
	// Re-check the workflow binding here, not only at creation: this update
	// accepted any workflow_def_id at all, including another application's.
	if workflowDefID != "" {
		var ruleAppID, ruleRevisionID string
		if err := s.pool.QueryRow(ctx, `
			SELECT application_id::text, COALESCE(revision_id::text,'')
			FROM workflow.automation_rule WHERE id = $1::uuid
		`, ruleID).Scan(&ruleAppID, &ruleRevisionID); err != nil {
			return nil, fmt.Errorf("automation rule %s not found", ruleID)
		}
		if err := s.ValidateRuleWorkflowDef(ctx, ruleAppID, ruleRevisionID, workflowDefID); err != nil {
			return nil, err
		}
	}

	var cronExpr, timezone, misfirePolicy *string
	var maxRetries, retryBackoff *int
	var nextFireAt *time.Time
	if sched != nil {
		if sched.CronExpr == "" {
			return nil, fmt.Errorf("cron_expr is required when updating schedule config")
		}
		tz := sched.Timezone
		if tz == "" {
			tz = "UTC"
		}
		mp := sched.MisfirePolicy
		if mp == "" {
			mp = "skip"
		}
		next, err := nextFireTime(sched.CronExpr, tz, time.Now())
		if err != nil {
			return nil, err
		}
		cronExpr = &sched.CronExpr
		timezone = &tz
		misfirePolicy = &mp
		maxRetries = &sched.MaxRetries
		retryBackoff = &sched.RetryBackoffSeconds
		nextFireAt = &next
	}

	var r AutomationRule
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow.automation_rule
		SET name           = COALESCE(NULLIF($2, ''), name),
		    description    = COALESCE(NULLIF($3, ''), description),
		    trigger_type   = COALESCE(NULLIF($4, '')::workflow.trigger_type, trigger_type),
		    workflow_name  = COALESCE(NULLIF($5, ''), workflow_name),
		    workflow_def_id= CASE WHEN $6 = '' THEN workflow_def_id ELSE NULLIF($6,'')::uuid END,
		    source_form_id = CASE WHEN $7 = '' THEN source_form_id ELSE NULLIF($7,'')::uuid END,
		    source_grid_id = CASE WHEN $8 = '' THEN source_grid_id ELSE NULLIF($8,'')::uuid END,
		    source_integration_id = CASE WHEN $16 = '' THEN source_integration_id ELSE NULLIF($16,'')::uuid END,
		    enabled        = COALESCE($9, enabled),
		    cron_expr      = COALESCE($10, cron_expr),
		    timezone       = COALESCE($11, timezone),
		    misfire_policy = COALESCE($12, misfire_policy),
		    max_retries    = COALESCE($13, max_retries),
		    retry_backoff_seconds = COALESCE($14, retry_backoff_seconds),
		    next_fire_at   = COALESCE($15, next_fire_at)
		WHERE id = $1::uuid
		RETURNING id::text, application_id::text, COALESCE(revision_id::text,''), name, COALESCE(description,''),
		          trigger_type::text, workflow_name,
		          COALESCE(workflow_def_id::text,''), COALESCE(source_form_id::text,''), COALESCE(source_grid_id::text,''),
		          COALESCE(source_integration_id::text,''),
		          enabled, created_at,
		          COALESCE(cron_expr,''), timezone, misfire_policy, max_retries, retry_backoff_seconds,
		          next_fire_at, last_fire_at
	`, ruleID, name, description, triggerType, workflowName, workflowDefID, sourceFormID, sourceGridID, enabled,
		cronExpr, timezone, misfirePolicy, maxRetries, retryBackoff, nextFireAt, sourceIntegrationID).Scan(
		&r.ID, &r.ApplicationID, &r.RevisionID, &r.Name, &r.Description,
		&r.TriggerType, &r.WorkflowName, &r.WorkflowDefID, &r.SourceFormID, &r.SourceGridID,
		&r.SourceIntegrationID,
		&r.Enabled, &r.CreatedAt,
		&r.CronExpr, &r.Timezone, &r.MisfirePolicy, &r.MaxRetries, &r.RetryBackoffSeconds,
		&r.NextFireAt, &r.LastFireAt,
	)
	return &r, err
}

func (s *Store) ListExecutions(ctx context.Context, appID string, limit int) ([]*Execution, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, COALESCE(rule_id::text,''), application_id::text,
		       status::text, trigger_payload,
		       COALESCE(instance_id::text,''), COALESCE(error,''),
		       started_at, completed_at, scheduled_for, COALESCE(claimed_by,'')
		FROM workflow.execution
		WHERE application_id = $1::uuid
		ORDER BY started_at DESC LIMIT $2
	`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var execs []*Execution
	for rows.Next() {
		var e Execution
		var payloadJSON []byte
		var completedAt *time.Time
		if err := rows.Scan(&e.ID, &e.RuleID, &e.ApplicationID, &e.Status,
			&payloadJSON, &e.InstanceID, &e.Error, &e.StartedAt, &completedAt,
			&e.ScheduledFor, &e.ClaimedBy); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(payloadJSON, &e.TriggerPayload)
		e.CompletedAt = completedAt
		execs = append(execs, &e)
	}
	return execs, rows.Err()
}
