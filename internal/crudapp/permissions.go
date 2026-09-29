package crudapp

import "slices"

// Record statuses: the values of runtime.record_status.
const (
	StatusDraft     = "draft"
	StatusSubmitted = "submitted"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
)

// RecordStatuses lists every record status, in the order a record moves
// through them.
var RecordStatuses = []string{StatusDraft, StatusSubmitted, StatusApproved, StatusRejected}

// creatorStatuses are the statuses a record's creator works in: before a
// decision. Approved and rejected are decisions, which only an administrator
// of the record's application makes.
var creatorStatuses = []string{StatusDraft, StatusSubmitted}

// ValidRecordStatus reports whether s is a record status.
func ValidRecordStatus(s string) bool { return slices.Contains(RecordStatuses, s) }

// RecordAccess is who a caller is to one form record. The caller decides
// the three facts (the gateway, from the caller's roles and the record's
// form); Permissions turns them into what the caller may do.
type RecordAccess struct {
	// Reach: the caller reaches the application the record's form belongs
	// to, the way the business console opens it. Without it the record does
	// not exist for the caller.
	Reach bool
	// Admin: the caller administers that application's records — a
	// business_admin of its workspace, a developer or tenant admin within
	// their scope, a platform admin.
	Admin bool
	// Creator: the caller created the record. A legacy record with no
	// creator has none.
	Creator bool
}

// RecordPermissions is what one caller may do to one record. It is served
// on every record the API returns, computed by the same RecordAccess the
// mutation checks use, so the interface offers exactly what the server
// accepts.
type RecordPermissions struct {
	// Edit: the caller may change the record's fields.
	Edit bool `json:"edit"`
	// Delete: the caller may delete the record.
	Delete bool `json:"delete"`
	// SetStatus: the statuses the caller may move the record to. It never
	// holds the record's current status, and is empty when there is none.
	SetStatus []string `json:"set_status"`
}

// Permissions is the rule for form records ("submitter + admins"):
//
//   - An administrator of the record's application may edit any record's
//     fields, set any status and delete it.
//   - The record's creator may edit its fields and delete it while it is a
//     draft or submitted, and may move it only between draft and submitted:
//     never to a decided status, and not once it is decided.
//   - Anyone else who reaches the form may read the record, and do nothing
//     else to it.
//
// A caller who does not reach the record may do nothing at all.
func (a RecordAccess) Permissions(status string) RecordPermissions {
	p := RecordPermissions{SetStatus: []string{}}
	switch {
	case !a.Reach:
	case a.Admin:
		p.Edit, p.Delete = true, true
		p.SetStatus = otherStatuses(RecordStatuses, status)
	case a.Creator && slices.Contains(creatorStatuses, status):
		p.Edit, p.Delete = true, true
		p.SetStatus = otherStatuses(creatorStatuses, status)
	}
	return p
}

// CanUpdate reports whether the caller may save a record that is in status
// from, leaving it in status to (the same status for a change of fields
// alone).
func (a RecordAccess) CanUpdate(from, to string) bool {
	p := a.Permissions(from)
	return p.Edit && (to == from || slices.Contains(p.SetStatus, to))
}

// CanDelete reports whether the caller may delete a record in status.
func (a RecordAccess) CanDelete(status string) bool { return a.Permissions(status).Delete }

// CanCreate reports whether the caller may create a record in status: an
// administrator in any status, anyone else who reaches the form as a draft
// or submitted. (The caller of a create is its creator.)
func (a RecordAccess) CanCreate(status string) bool {
	if !a.Reach || !ValidRecordStatus(status) {
		return false
	}
	return a.Admin || slices.Contains(creatorStatuses, status)
}

func otherStatuses(all []string, current string) []string {
	out := make([]string, 0, len(all))
	for _, s := range all {
		if s != current {
			out = append(out, s)
		}
	}
	return out
}
