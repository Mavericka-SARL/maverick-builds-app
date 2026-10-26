package recordpost

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// tablePosting is what checking or posting the records of one table reads
// once, however many records there are: the table, its live mappings, the
// revision each posts into, the periods dates fall in, each cell a creator
// was already checked for, and what the write guard read for those checks
// (writeguard.Memo). A file of 900 GL rows names 180 cells; each row used
// to read all of it again and check its cells afresh — about forty
// statements per row and mapping, twice (the import's check, then the
// posting): 140,000 statements for the file (2026-10-22).
type tablePosting struct {
	form      *crudapp.FormDef
	mappings  []formMapping
	revisions []string // revisions[i] is the revision mappings[i] posts into
	period    periodOf
	refusals  map[string]cellRefusal // PostingRefusal's answer for a cell
	guards    map[string]cellGuard   // what posting into a cell found
	memo      *writeguard.Memo
}

func (ps *Poster) newTablePosting(ctx context.Context, form *crudapp.FormDef) (*tablePosting, error) {
	mappings, err := ps.liveFormMappings(ctx, form.ID)
	if err != nil {
		return nil, err
	}
	tp := &tablePosting{
		form: form, mappings: mappings, period: ps.periodResolver(ctx),
		refusals: map[string]cellRefusal{}, guards: map[string]cellGuard{}, memo: writeguard.NewMemo(),
	}
	for _, m := range mappings {
		tp.revisions = append(tp.revisions, ps.postingRevision(ctx, m))
	}
	return tp, nil
}

// cellKey names the cell members names through mapping i, as checked for
// userID.
func cellKey(i int, userID string, members map[string]string) string {
	b, _ := json.Marshal(members) // a map's keys are marshalled sorted
	return strconv.Itoa(i) + "|" + userID + "|" + string(b)
}

// cellRefusal is PostingRefusal's answer for one cell: a status code and
// message, 0 and "" when the cell may be written.
type cellRefusal struct {
	code int
	msg  string
}

// cellGuard is what posting into one cell found.
type cellGuard struct {
	memberIDs []string
	// skip: the creator may not write the metric or the cell. The record's
	// posting there stays as it is.
	skip bool
	// noLeaf: the record names a code the dimension does not have, a parent
	// or a calculated member. Its posting is retracted.
	noLeaf bool
}

// postingRecord is a record to post, as it was read: its status, data and
// creator, and when it last changed. Its postings are written only while it
// still is as read.
type postingRecord struct {
	id, status, creator string
	data                map[string]any
	updatedAt           time.Time
}

// asStored is data as a re-read of its record gives it (a JSON round trip,
// numbers as float64), so a record posts the same whether it was read back
// or kept from the import that created it.
func asStored(data map[string]any) map[string]any {
	out := map[string]any{}
	if b, err := json.Marshal(data); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// recordsToPost reads every record of formID to post.
func (ps *Poster) recordsToPost(ctx context.Context, formID string) ([]postingRecord, error) {
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT id::text, COALESCE(status::text, ''), data, COALESCE(created_by::text, ''), updated_at
		FROM runtime.form_record WHERE form_id = $1::uuid ORDER BY created_at, id`, formID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []postingRecord
	for rows.Next() {
		var rec postingRecord
		var raw []byte
		if err := rows.Scan(&rec.id, &rec.status, &raw, &rec.creator, &rec.updatedAt); err != nil {
			return nil, err
		}
		rec.data = map[string]any{}
		_ = json.Unmarshal(raw, &rec.data)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// post posts recs through each of tp's mappings — each record once — and
// answers the mappings whose postings it wrote or retracted: the totals the
// caller rebuilds. A record posts within its creator's access (the owner's
// decision of 2026-10-06): an approver, or an administrator syncing the
// table, needs no write access where it posts. A legacy record with no
// creator posts within the access of userID, whom the posted facts are
// entered by.
func (ps *Poster) post(ctx context.Context, tp *tablePosting, recs []postingRecord, userID string) (touched []int) {
	// A record without a status — every record of a table without a status
	// field — counts whatever its mappings' posting statuses say.
	hasStatus := tp.form.HasStatus()
	for i, m := range tp.mappings {
		var retract []postingRecord
		var rows []postingRow
		for _, rec := range recs {
			// A record with no numeric value in the source field contributes
			// nothing, like one whose status is not eligible: its posting is
			// retracted and the postings that remain re-aggregated. A missing
			// value used to retract without re-aggregating, leaving the old
			// value in the metric until something else recomputed it.
			val, hasValue := m.value(rec.data)
			if !m.postsAt(hasStatus, rec.status) || !hasValue {
				retract = append(retract, rec)
				continue
			}
			members := m.members(rec.data, tp.period)
			g, decided := ps.guard(ctx, tp, i, firstNonEmpty(rec.creator, userID), members, rec.id)
			switch {
			case !decided || g.skip:
			case g.noLeaf:
				retract = append(retract, rec)
			default:
				dims, _ := json.Marshal(members)
				rows = append(rows, postingRow{id: rec.id, readAt: rec.updatedAt, dims: string(dims), value: val})
			}
		}
		retracted := ps.retractPostings(ctx, m, retract)
		if posted := ps.insertPostings(ctx, m, tp.revisions[i], rows); retracted || posted {
			touched = append(touched, i)
		}
	}
	return touched
}

// guard is the generic write guard every interactive grid write passes
// (writeguard.MetricAccess, ResolveWriteMembers, CheckWriteMetrics), for
// guardUser — the record's creator — posting into the cell members names
// through mapping i, decided once per cell and creator. Hidden from or
// read-only in the metric or a member, the creator must not have that
// bypassed by saving a record instead of writing the cell. A save is
// checked before it is made (PostingRefusal), so a refusal here means the
// rules changed in between, or a sync or a workflow moved a record whose
// creator has lost access since: skip-and-log, as this runs after the
// response. A refused re-post leaves the record's posting as it is: the
// checks run before anything is retracted, and the upsert replaces a
// posting in place. The posting used to be deleted first, so a caller the
// guard refuses — a restricted user's own edit, or a restricted
// administrator's sync re-posting other people's records — withdrew the
// contribution without writing anything, and the next recompute dropped it
// from the metric.
//
// A transient error is NOT a denial, but both used to drop the posting for
// good. Each read is retried; decided is false once one keeps failing
// (said at error level), and the next record at the cell tries again.
func (ps *Poster) guard(ctx context.Context, tp *tablePosting, i int, guardUser string, members map[string]string, recordID string) (g cellGuard, decided bool) {
	key := cellKey(i, guardUser, members)
	if g, ok := tp.guards[key]; ok {
		return g, true
	}
	m, revisionID := tp.mappings[i], tp.revisions[i]
	gctx := writeguard.WithMemo(ctx, tp.memo)
	var access string
	if err := RetryTransient(ctx, 3, func() error {
		a, e := writeguard.MetricAccess(gctx, ps.db(ctx), guardUser, m.targetMetricID)
		access = a
		return e
	}); err != nil {
		ps.Log.Error().Err(err).Str("mapping_id", m.id).Str("record_id", recordID).Msg("posting: metric access check kept failing — posting dropped, aggregate now under-counts")
		return cellGuard{}, false
	}
	if access == "hidden" || access == "read" {
		ps.Log.Warn().Str("mapping_id", m.id).Str("metric_id", m.targetMetricID).Msg("posting rejected, the record's creator lacks write access to the metric")
		tp.guards[key] = cellGuard{skip: true}
		return tp.guards[key], true
	}
	// A record naming a code the dimension does not have, a parent or a
	// calculated member posts nowhere: the posting used to be stored at that
	// code, where no grid or total reads it. Its earlier posting is
	// retracted, as for a record with no value — the record no longer names
	// that coordinate.
	var refused error
	if err := RetryTransient(ctx, 3, func() error {
		ids, e := writeguard.ResolveWriteMembers(gctx, ps.db(ctx), m.modelID, revisionID, members)
		g.memberIDs = ids
		if writeguard.IsMemberError(e) {
			refused = e // a refusal, not a failed read: no retry
			return nil
		}
		return e
	}); refused != nil {
		ps.Log.Warn().Err(refused).Str("mapping_id", m.id).Str("record_id", recordID).Msg("posting: record names no leaf member — nothing posted")
		tp.guards[key] = cellGuard{noLeaf: true}
		return tp.guards[key], true
	} else if err != nil {
		ps.Log.Error().Err(err).Str("mapping_id", m.id).Str("record_id", recordID).Msg("posting: member lookup kept failing — posting dropped, aggregate now under-counts")
		return cellGuard{}, false
	}
	var reason string
	if err := RetryTransient(ctx, 3, func() error {
		r, e := writeguard.CheckWriteMetrics(gctx, ps.db(ctx), m.modelID, revisionID, guardUser, g.memberIDs, []string{m.targetMetricID})
		if e == nil && r == "" {
			// A closed cell takes no posting either (PostingRefusal).
			r, e = writeguard.ClosedReason(gctx, ps.db(ctx), revisionID, []writeguard.Cell{{MetricID: m.targetMetricID, DimCodes: members}})
		}
		reason = r
		return e
	}); err != nil {
		ps.Log.Error().Err(err).Str("mapping_id", m.id).Str("record_id", recordID).Msg("posting: write guard check kept failing — posting dropped, aggregate now under-counts")
		return cellGuard{}, false
	} else if reason != "" {
		ps.Log.Warn().Str("mapping_id", m.id).Str("reason", reason).Msg("posting rejected by write guard")
		g.skip = true
	}
	tp.guards[key] = g
	return g, true
}

// postingRow is one record's posting through a mapping.
type postingRow struct {
	id     string
	readAt time.Time // the record's updated_at when it was read
	dims   string    // the {dimension id: member code} coordinate, as JSON
	value  float64
}

// postingChunk is the most postings one statement writes.
const postingChunk = 5000

// insertPostings writes rows' postings through m into revisionID, each
// replacing its record's earlier one, one statement per chunk; wrote says
// some were. A posting is written only while its record is as it was read:
// a record changed since posts through its own change, which a slower
// batch must not overwrite with what it read before — what each record's
// re-read ensured when records posted one at a time (see Apply). A record
// deleted since posts nothing.
//
// This is the failure that most directly under-counts a total: the posting
// row IS the record's contribution, so losing it loses that record's value
// until something else happens to re-post it. ON CONFLICT DO UPDATE makes
// the retry idempotent. A foreign key refusal is not a failure: the mapping
// was deleted after it was read, or a record in the middle of the
// statement. The chunk is then written one posting at a time, so the
// records still there still post.
func (ps *Poster) insertPostings(ctx context.Context, m formMapping, revisionID string, rows []postingRow) (wrote bool) {
	for len(rows) > 0 {
		chunk := rows[:min(len(rows), postingChunk)]
		rows = rows[len(chunk):]
		ids := make([]string, len(chunk))
		readAt := make([]time.Time, len(chunk))
		dims := make([]string, len(chunk))
		values := make([]float64, len(chunk))
		for j, r := range chunk {
			ids[j], readAt[j], dims[j], values[j] = r.id, r.readAt, r.dims, r.value
		}
		gone := false
		err := RetryTransient(ctx, 3, func() error {
			_, e := ps.db(ctx).Exec(ctx, `
				INSERT INTO runtime.form_record_posting
				  (mapping_id, form_record_id, target_metric_id, revision_id, dim_members, posted_value)
				SELECT $1::uuid, u.id, $2::uuid, NULLIF($3,'')::uuid, u.dims::jsonb, u.value
				FROM unnest($4::uuid[], $5::timestamptz[], $6::text[], $7::numeric[]) AS u(id, read_at, dims, value)
				JOIN runtime.form_record r ON r.id = u.id AND r.updated_at = u.read_at
				ON CONFLICT (mapping_id, form_record_id) DO UPDATE
				  SET target_metric_id = EXCLUDED.target_metric_id, revision_id = EXCLUDED.revision_id,
				      dim_members = EXCLUDED.dim_members, posted_value = EXCLUDED.posted_value, posted_at = now()`,
				m.id, m.targetMetricID, revisionID, ids, readAt, dims, values)
			if isForeignKeyViolation(e) {
				gone = true
				return nil
			}
			return e
		})
		switch {
		case gone && len(chunk) > 1:
			for _, r := range chunk {
				if ps.insertPostings(ctx, m, revisionID, []postingRow{r}) {
					wrote = true
				}
			}
		case gone:
		case err != nil:
			ps.Log.Error().Err(err).Str("mapping_id", m.id).Int("records", len(chunk)).Msg("form posting insert kept failing — these records' values are missing from the metric total")
		default:
			wrote = true
		}
	}
	return wrote
}

// retractPostings deletes recs' postings through m, each while its record
// is as it was read; retracted says there were records to retract, whose
// mapping's total is rebuilt whether or not they had a posting.
func (ps *Poster) retractPostings(ctx context.Context, m formMapping, recs []postingRecord) (retracted bool) {
	for len(recs) > 0 {
		chunk := recs[:min(len(recs), postingChunk)]
		recs = recs[len(chunk):]
		ids := make([]string, len(chunk))
		readAt := make([]time.Time, len(chunk))
		for j, r := range chunk {
			ids[j], readAt[j] = r.id, r.updatedAt
		}
		if _, err := ps.db(ctx).Exec(ctx, `
			DELETE FROM runtime.form_record_posting p
			USING unnest($2::uuid[], $3::timestamptz[]) AS u(id, read_at)
			JOIN runtime.form_record r ON r.id = u.id AND r.updated_at = u.read_at
			WHERE p.mapping_id = $1::uuid AND p.form_record_id = u.id`,
			m.id, ids, readAt); err != nil {
			ps.Log.Warn().Err(err).Str("mapping_id", m.id).Int("records", len(chunk)).Msg("retracting form postings failed")
		}
		retracted = true
	}
	return retracted
}

// totals rebuilds the totals of tp's mappings at touched and recalculates
// what they feed, once, against each mapping's OWN revision: a
// revision-scoped mapping can target another revision than the model's
// active one. A single re-derived active_revision_id used to skip the
// recalculation whenever the model had no active revision yet, or
// recalculate the wrong revision.
func (ps *Poster) totals(ctx context.Context, tp *tablePosting, touched []int, userID string) {
	var affected []struct{ RevisionID, MetricID string }
	modelID := ""
	for _, i := range touched {
		m := tp.mappings[i]
		ps.RecomputeFactInput(ctx, m.id, tp.revisions[i], userID)
		affected = append(affected, struct{ RevisionID, MetricID string }{tp.revisions[i], m.targetMetricID})
		modelID = m.modelID
	}
	ps.recalc(ctx, modelID, affected)
}
