package assignee

import "strings"

// NamedSQL is true when the step — the workflow.workflow_step row whose
// alias ws is, defined by the jsonb def — is not narrowed to one person, or
// when user is that person or an administrator of the application app
// (business_admin or tenant_admin there, as SQL draws them). A step is
// narrowed when its definition names an assignee_field and its assignee is
// set while it is in progress (workflow.AssignNamedPeople, owner decision
// 2026-11-27): the inbox, deciding and reminders then reach that person
// only. A step without assignee_field is as before — a reworked step's
// assignee (the person it went back to) narrows nothing here.
//
// The expressions follow SQL's rules (no alias beginning asg_).
func NamedSQL(ws, def, app, user string) string {
	return strings.NewReplacer("{ws}", ws, "{def}", def, "{user}", "("+user+")").Replace(`(
	{ws}.assignee_user_id IS NULL
	OR {ws}.status <> 'in_progress'
	OR COALESCE({def}->>'assignee_field', '') = ''
	OR {ws}.assignee_user_id = {user}
	OR `) + SQL(app, `'["business_admin","tenant_admin"]'::jsonb`, user) + `)`
}
