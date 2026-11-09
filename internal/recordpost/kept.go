package recordpost

// Tables that keep a dimension's members (model.form_def.keeps_dimension_id):
// each record of such a table is one member of the dimension — its auto-code
// the member's code, its label field the member's label. The member is added
// with its record, renamed with it and deleted with it, in the record's own
// transaction, so the two never disagree. Every other way of writing the
// dimension's members is refused by the kept_member_guard trigger (migration
// 144); the functions here say, for their transaction, that the records path
// is the one writing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// keptWrite runs op with the kept-member guard told the records path is
// writing, for db's transaction only.
func keptWrite(ctx context.Context, db dbx.DB, op func() error) error {
	if _, err := db.Exec(ctx, `SELECT set_config('mvx.kept_member_write', 'on', true)`); err != nil {
		return err
	}
	opErr := op()
	if _, err := db.Exec(ctx, `SELECT set_config('mvx.kept_member_write', '', true)`); err != nil && opErr == nil {
		return err
	}
	return opErr
}

// KeepMember makes the member a record of form holding data is: added when
// the dimension has no member of its code, relabelled when its label
// changed, and its declared properties that a field of the table fills set
// from the record (KeptProperties). db is the record's transaction. It
// reports whether the dimension changed — a member added or a property
// value — so what reads it is recalculated. A table keeping no dimension
// changes nothing.
func KeepMember(ctx context.Context, db dbx.DB, form *crudapp.FormDef, data map[string]any) (changed bool, err error) {
	added, err := keepMember(ctx, db, form, data)
	if err != nil || !form.KeepsDimension() {
		return added, err
	}
	props, err := keepProperties(ctx, db, form, form.MemberCode(data), data)
	return added || props, err
}

func keepMember(ctx context.Context, db dbx.DB, form *crudapp.FormDef, data map[string]any) (added bool, err error) {
	if !form.KeepsDimension() {
		return false, nil
	}
	code := form.MemberCode(data)
	if code == "" {
		return false, fmt.Errorf("a record of %s needs its code to be a member of the dimension the table keeps", form.Label)
	}
	label := form.MemberLabel(data)
	err = keptWrite(ctx, db, func() error {
		tag, err := db.Exec(ctx, `
			UPDATE model.dimension_member SET label = $3
			WHERE dimension_id = $1::uuid AND code = $2 AND label IS DISTINCT FROM $3`,
			form.KeepsDimensionID, code, label)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		tag, err = db.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, sort_order, parent_member_id)
			SELECT $1::uuid, $2, $3,
			       (SELECT COALESCE(MAX(sort_order), 0) + 1 FROM model.dimension_member WHERE dimension_id = $1::uuid),
			       (SELECT id FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $4 AND $5)
			WHERE NOT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
			form.KeepsDimensionID, code, label, crudapp.RootMemberCode, form.MemberRootLabel != "")
		if err != nil {
			return err
		}
		added = tag.RowsAffected() > 0
		return nil
	})
	return added, err
}

// DropKeptMember deletes the member a deleted record of form was, with the
// input values filed against it (modeledit.DeleteMember), inside tx. A
// member already gone, or a table keeping no dimension, changes nothing.
func DropKeptMember(ctx context.Context, tx pgx.Tx, form *crudapp.FormDef, code string) error {
	if !form.KeepsDimension() || code == "" {
		return nil
	}
	var memberID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2`,
		form.KeepsDimensionID, code).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return keptWrite(ctx, tx, func() error { return modeledit.DeleteMember(ctx, tx, form.KeepsDimensionID, memberID) })
}

// A dimension a table keeps takes its declared properties from the table's
// fields: a property whose name is a field's name — compared without case,
// spaces and punctuation read as underscores ("Reporting line" fills
// reporting_line) — holds that field's value for every member. The AI
// Developer's Actual P&L read {GL Account}.reporting_line, declared on the
// dimension, while the reporting line of each account was a field of the GL
// Mapping table keeping it; no member had a value and every closed month
// read 0 (2026-11-08). A developer reaches for the same.

// propertyFields maps each declared property of the dimension form keeps
// that a field of form fills to that field's name.
func propertyFields(ctx context.Context, db dbx.DB, form *crudapp.FormDef) (map[string]string, error) {
	rows, err := db.Query(ctx, `SELECT name FROM model.dimension_property WHERE dimension_id = $1::uuid`, form.KeepsDimensionID)
	if err != nil {
		return nil, err
	}
	props, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range props {
		for _, f := range form.Fields {
			if f.Type != "autocode" && f.Type != "status" && identKey(f.Name) == identKey(p) {
				out[p] = f.Name
				break
			}
		}
	}
	return out, nil
}

// identKey is a name as a property compares it: lower case, every run of
// other characters than letters and digits one underscore.
func identKey(s string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
			under = false
		} else if !under && b.Len() > 0 {
			b.WriteByte('_')
			under = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// propertyValue is a record value as a member property holds it: text, a
// number without trailing zeros; "" for none.
func propertyValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return string(b)
}

// keepProperties sets the properties of member code that fields of form
// fill from data, emptying those the record leaves empty. It reports
// whether a value changed.
func keepProperties(ctx context.Context, db dbx.DB, form *crudapp.FormDef, code string, data map[string]any) (bool, error) {
	fields, err := propertyFields(ctx, db, form)
	if err != nil || len(fields) == 0 || code == "" {
		return false, err
	}
	set := map[string]string{}
	drop := []string{}
	for prop, field := range fields {
		if v := propertyValue(data[field]); v != "" {
			set[prop] = v
		} else {
			drop = append(drop, prop)
		}
	}
	b, _ := json.Marshal(set)
	tag, err := db.Exec(ctx, `
		UPDATE model.dimension_member
		SET properties = (COALESCE(properties, '{}'::jsonb) - $3::text[]) || $4::jsonb
		WHERE dimension_id = $1::uuid AND code = $2
		  AND COALESCE(properties, '{}'::jsonb) IS DISTINCT FROM (COALESCE(properties, '{}'::jsonb) - $3::text[]) || $4::jsonb`,
		form.KeepsDimensionID, code, drop, string(b))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FillKeptProperties sets, for every record of the table keeping
// dimensionID, the member properties its fields fill: run when the table
// starts keeping the dimension and when a property is declared on it. It
// returns how many members changed; a dimension no table keeps changes
// nothing.
func FillKeptProperties(ctx context.Context, db dbx.DB, dimensionID string) (int, error) {
	var formID string
	err := db.QueryRow(ctx, `SELECT id::text FROM model.form_def WHERE keeps_dimension_id = $1::uuid`, dimensionID).Scan(&formID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	form, err := crudapp.NewStoreOn(db).GetForm(ctx, formID)
	if err != nil || !form.KeepsDimension() {
		return 0, err
	}
	rows, err := db.Query(ctx, `SELECT data FROM runtime.form_record WHERE form_id = $1::uuid`, formID)
	if err != nil {
		return 0, err
	}
	var all []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, err
		}
		var data map[string]any
		if json.Unmarshal(raw, &data) == nil {
			all = append(all, data)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, data := range all {
		changed, err := keepProperties(ctx, db, form, form.MemberCode(data), data)
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}

// SyncKeptMembers makes the dimension form keeps hold exactly the members
// its records are, when a table starts keeping it: a member for every
// record (added, or relabelled), in the records' order. It refuses when the
// dimension has members no record is — they would be members nobody could
// change — naming a few. db is the caller's transaction.
func SyncKeptMembers(ctx context.Context, db dbx.DB, form *crudapp.FormDef) (added int, err error) {
	if !form.KeepsDimension() {
		return 0, nil
	}
	codeField := form.CodeField().Name
	var orphans []string
	rows, err := db.Query(ctx, `
		SELECT m.code FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid
		  AND NOT ($4 AND m.code = $5)
		  AND NOT EXISTS (SELECT 1 FROM runtime.form_record r WHERE r.form_id = $2::uuid AND r.data->>$3::text = m.code)
		ORDER BY m.sort_order, m.code LIMIT 6`, form.KeepsDimensionID, form.ID, codeField, form.MemberRootLabel != "", crudapp.RootMemberCode)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, err
		}
		orphans = append(orphans, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(orphans) > 0 {
		return 0, &KeepError{Msg: fmt.Sprintf("the dimension already has members that are no record of %s (%s): only an empty dimension, or one whose members are all its records' codes, can be kept by a table", form.Label, joinCodes(orphans))}
	}
	labelField := form.MemberLabelField
	err = keptWrite(ctx, db, func() error {
		if err := keepRoot(ctx, db, form); err != nil {
			return err
		}
		const recs = `
			SELECT r.data->>$2::text AS code,
			       COALESCE(NULLIF(btrim(r.data->>$3::text), ''), r.data->>$2::text) AS label,
			       r.created_at, r.id
			FROM runtime.form_record r
			WHERE r.form_id = $1::uuid AND COALESCE(r.data->>$2::text, '') <> ''`
		if _, err := db.Exec(ctx, `
			UPDATE model.dimension_member m SET label = x.label
			FROM (`+recs+`) x
			WHERE m.dimension_id = $4::uuid AND m.code = x.code AND m.label IS DISTINCT FROM x.label`,
			form.ID, codeField, labelField, form.KeepsDimensionID); err != nil {
			return err
		}
		tag, err := db.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, sort_order)
			SELECT $4::uuid, x.code, x.label,
			       (SELECT COALESCE(MAX(sort_order), 0) FROM model.dimension_member WHERE dimension_id = $4::uuid)
			         + row_number() OVER (ORDER BY x.created_at, x.id)
			FROM (`+recs+`) x
			WHERE NOT EXISTS (SELECT 1 FROM model.dimension_member m WHERE m.dimension_id = $4::uuid AND m.code = x.code)`,
			form.ID, codeField, labelField, form.KeepsDimensionID)
		if err != nil {
			return err
		}
		added = int(tag.RowsAffected())
		// Every record's member under the total, when the table names one.
		if form.MemberRootLabel != "" {
			if _, err := db.Exec(ctx, `
				UPDATE model.dimension_member m
				SET parent_member_id = (SELECT id FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)
				WHERE m.dimension_id = $1::uuid AND m.code <> $2 AND m.parent_member_id IS NULL`,
				form.KeepsDimensionID, crudapp.RootMemberCode); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return added, err
	}
	// The properties its fields fill, for every record.
	_, err = FillKeptProperties(ctx, db, form.KeepsDimensionID)
	return added, err
}

// keepRoot makes the total member form names (MemberRootLabel) exist with
// that label, or removes it when the table names none — its members are
// then at the top. db is the caller's transaction, told the records path is
// writing.
func keepRoot(ctx context.Context, db dbx.DB, form *crudapp.FormDef) error {
	if form.MemberRootLabel == "" {
		if _, err := db.Exec(ctx, `
			UPDATE model.dimension_member SET parent_member_id = NULL
			WHERE dimension_id = $1::uuid
			  AND parent_member_id = (SELECT id FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
			form.KeepsDimensionID, crudapp.RootMemberCode); err != nil {
			return err
		}
		_, err := db.Exec(ctx, `
			DELETE FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2
			  AND NOT EXISTS (SELECT 1 FROM runtime.form_record r, jsonb_array_elements(
			      (SELECT fields FROM model.form_def WHERE id = $3::uuid)) f
			      WHERE r.form_id = $3::uuid AND f->>'type' = 'autocode' AND r.data->>(f->>'name') = $2)`,
			form.KeepsDimensionID, crudapp.RootMemberCode, form.ID)
		return err
	}
	tag, err := db.Exec(ctx, `UPDATE model.dimension_member SET label = $3 WHERE dimension_id = $1::uuid AND code = $2`,
		form.KeepsDimensionID, crudapp.RootMemberCode, form.MemberRootLabel)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, sort_order)
		VALUES ($1::uuid, $2, $3, 0)`, form.KeepsDimensionID, crudapp.RootMemberCode, form.MemberRootLabel)
	return err
}

// AfterRecordCreated is what runs in a new record's transaction: the
// member it is, in a table keeping a dimension (setting *added when it
// added one), and the first values of its history fields, given by userID.
func (ps *Poster) AfterRecordCreated(userID string, added *bool) crudapp.AfterCreate {
	return func(ctx context.Context, tx dbx.DB, form *crudapp.FormDef, rec *crudapp.FormRecord) error {
		ok, err := KeepMember(ctx, tx, form, rec.Data)
		if err != nil {
			return err
		}
		if ok && added != nil {
			*added = true
		}
		return crudapp.LogFieldChanges(ctx, tx, form, rec.ID, nil, rec.Data, userID)
	}
}

// KeepError is a table that cannot keep the dimension it was given.
type KeepError struct{ Msg string }

func (e *KeepError) Error() string { return e.Msg }

func joinCodes(codes []string) string {
	out := ""
	for i, c := range codes {
		if i == 5 {
			return out + ", …"
		}
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}

// TableSettings are the settings of a table beyond its fields; each nil
// keeps what the table has.
type TableSettings struct {
	// KeepsDimensionID: the dimension whose members the table's records
	// are ("" to stop keeping one); MemberLabelField: the field naming
	// each member.
	KeepsDimensionID *string `json:"keeps_dimension_id"`
	MemberLabelField *string `json:"member_label_field"`
	// MemberRootLabel: the label of a total member the records' members sit
	// under ("" for none).
	MemberRootLabel *string `json:"member_root_label"`
	// EditAccess: "creator" or "everyone".
	EditAccess *string `json:"edit_access"`
	// CreateRoles: the roles whose people may add records; empty, anyone
	// who reaches the application (migration 157).
	CreateRoles *[]string `json:"create_roles"`
}

// SettingError is a table setting refused: Status is the HTTP status it
// answers with.
type SettingError struct {
	Status int
	Msg    string
}

func (e *SettingError) Error() string { return e.Msg }

func settingErr(status int, format string, args ...any) error {
	return &SettingError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// ApplyTableSettings saves s on form, a table whose fields are saved, and
// sets form's fields to match. When the table starts keeping a dimension or
// names another label field, the dimension's members become its records
// (SyncKeptMembers), all in one transaction: it reports how many members it
// added. A setting refused is a *SettingError.
func (ps *Poster) ApplyTableSettings(ctx context.Context, form *crudapp.FormDef, s TableSettings) (int, error) {
	return ApplyTableSettingsOn(ctx, ps.db(ctx), form, s)
}

// ApplyTableSettingsOn is ApplyTableSettings on db (a pool, or a
// transaction the caller keeps).
func ApplyTableSettingsOn(ctx context.Context, db dbx.DB, form *crudapp.FormDef, s TableSettings) (int, error) {
	if s.EditAccess != nil {
		if *s.EditAccess != crudapp.EditCreator && *s.EditAccess != crudapp.EditEveryone {
			return 0, settingErr(400, "edit_access is %q or %q, not %q", crudapp.EditCreator, crudapp.EditEveryone, *s.EditAccess)
		}
		if _, err := db.Exec(ctx, `UPDATE model.form_def SET edit_access=$2 WHERE id=$1::uuid`, form.ID, *s.EditAccess); err != nil {
			return 0, err
		}
		form.EditAccess = *s.EditAccess
	}
	if s.CreateRoles != nil {
		roles := cleanRoleNames(*s.CreateRoles)
		raw, _ := json.Marshal(roles)
		if _, err := db.Exec(ctx, `UPDATE model.form_def SET create_roles=$2::jsonb WHERE id=$1::uuid`, form.ID, string(raw)); err != nil {
			return 0, err
		}
		form.CreateRoles = roles
	}
	if s.KeepsDimensionID == nil && s.MemberLabelField == nil && s.MemberRootLabel == nil {
		return 0, nil
	}
	dimID, labelField, rootLabel := form.KeepsDimensionID, form.MemberLabelField, form.MemberRootLabel
	if s.KeepsDimensionID != nil {
		dimID = strings.TrimSpace(*s.KeepsDimensionID)
	}
	if s.MemberLabelField != nil {
		labelField = strings.TrimSpace(*s.MemberLabelField)
	}
	if s.MemberRootLabel != nil {
		rootLabel = strings.TrimSpace(*s.MemberRootLabel)
	}
	if dimID == form.KeepsDimensionID && labelField == form.MemberLabelField && rootLabel == form.MemberRootLabel {
		return 0, nil
	}
	if dimID == "" {
		if _, err := db.Exec(ctx, `UPDATE model.form_def SET keeps_dimension_id = NULL, member_label_field = NULL, member_root_label = NULL WHERE id = $1::uuid`, form.ID); err != nil {
			return 0, err
		}
		form.KeepsDimensionID, form.MemberLabelField, form.MemberRootLabel = "", "", ""
		return 0, nil
	}
	if form.CodeField() == nil {
		return 0, settingErr(400, "a table keeping a dimension needs a code field — an auto-code field, or a text field marked as the code (\"code\": true) for codes given from outside, such as a ledger's account numbers: its codes are the members' codes")
	}
	if lf := form.Field(labelField); lf == nil || !crudapp.IsTextType(lf.Type) && lf.Type != "select" || lf.Type == crudapp.FieldURL {
		return 0, settingErr(400, "member_label_field names the text field that labels each member (%q is not one)", labelField)
	}
	var sameModel, sameRevision bool
	var dimType, keptBy string
	err := db.QueryRow(ctx, `
		SELECT d.model_id = fd.model_id,
		       d.revision_id IS NOT DISTINCT FROM fd.revision_id,
		       COALESCE(d.dimension_type, 'standard'),
		       COALESCE((SELECT o.label FROM model.form_def o WHERE o.keeps_dimension_id = d.id AND o.id <> fd.id LIMIT 1), '')
		FROM model.dimension_def d, model.form_def fd
		WHERE d.id::text = $1 AND fd.id = $2::uuid`, dimID, form.ID).Scan(&sameModel, &sameRevision, &dimType, &keptBy)
	if err != nil || !sameModel || !sameRevision {
		return 0, settingErr(400, "keeps_dimension_id names no dimension of this table's model and revision")
	}
	switch {
	case dimType == "time":
		return 0, settingErr(400, "a time dimension's members are periods, not records: a table cannot keep one")
	case keptBy != "":
		return 0, settingErr(409, "that dimension's members are already the records of the table %q", keptBy)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE model.form_def SET keeps_dimension_id = $2::uuid, member_label_field = $3, member_root_label = NULLIF($4, '') WHERE id = $1::uuid`,
		form.ID, dimID, labelField, rootLabel); err != nil {
		return 0, err
	}
	prevDim, prevLabel, prevRoot := form.KeepsDimensionID, form.MemberLabelField, form.MemberRootLabel
	form.KeepsDimensionID, form.MemberLabelField, form.MemberRootLabel = dimID, labelField, rootLabel
	added, err := SyncKeptMembers(ctx, tx, form)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		form.KeepsDimensionID, form.MemberLabelField, form.MemberRootLabel = prevDim, prevLabel, prevRoot
		var keepErr *KeepError
		if errors.As(err, &keepErr) {
			return 0, settingErr(409, "%s", keepErr.Msg)
		}
		return 0, err
	}
	return added, nil
}

// cleanRoleNames trims role names and drops empty ones and repeats, as a
// workflow's start roles are cleaned.
func cleanRoleNames(names []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out
}

// CheckColumnMapFields refuses a column map for the table formID that maps a
// column to a name that is none of the table's fields (by name or label,
// without case) nor its status: such a column used to be read past, as an
// export's own id column is, and the loader reported success while a field
// stayed empty in every record (2026-11-08). A column the map does not name
// is still read past. "ignore", "" and the "*" entry are not targets.
func CheckColumnMapFields(ctx context.Context, db dbx.DB, formID string, columnMap map[string]string) error {
	if len(columnMap) == 0 {
		return nil
	}
	form, err := crudapp.NewStoreOn(db).GetForm(ctx, formID)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	var names []string
	for _, f := range form.Fields {
		known[strings.ToLower(strings.TrimSpace(f.Name))] = true
		known[strings.ToLower(strings.TrimSpace(f.Label))] = true
		names = append(names, f.Name)
	}
	if form.HasStatus() {
		known["status"] = true
	}
	var bad []string
	for col, target := range columnMap {
		t := strings.ToLower(strings.TrimSpace(target))
		if col == "*" || t == "" || t == "ignore" || known[t] {
			continue
		}
		bad = append(bad, fmt.Sprintf("%q to %q", col, target))
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	table := form.Label
	if strings.TrimSpace(table) == "" {
		table = form.Name
	}
	return fmt.Errorf("column_map maps %s, which is no field of table %q — its fields are: %s; map each column to a field, or to \"ignore\"",
		strings.Join(bad, ", "), table, strings.Join(names, ", "))
}
