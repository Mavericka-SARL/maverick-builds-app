package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A dashboard's widgets ask for their grid reads again every two seconds,
// and a read pinning two dimensions is worked out again each time, though
// nothing changed (an OPEX budget's Management summary: eight 1–1.6 s reads
// every 2 s kept its tenant's turns full, 2026-11-22). A read carries an
// ETag naming everything its answer depends on; a poll sending it back
// (If-None-Match) while none of that changed is answered 304 Not Modified
// before it waits for a turn or reads anything else.
//
// The tag is taken BEFORE the answer is worked out: a change committing
// meanwhile is in the answer but not in the tag, so the next poll misses
// and reads again — never the other way round (migration 198).

// readTagBucket bounds how long anything not in the tag can go unnoticed:
// the clock (a recalculation's pending mark goes stale after 15 minutes,
// TODAY()), the licence. Every read is worked out again at least this often.
const readTagBucket = 10 * time.Minute

// executableTag names the running gateway's code: the same on every replica
// of a release, different after a deploy, whose answers may differ.
var executableTag = sync.OnceValue(func() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
})

// dataGeneration is modelID's data generation in the routed database
// (migration 198): its own changes and those every model's reads depend on.
// false where it cannot be read (a database not migrated yet).
func (h *handler) dataGeneration(ctx context.Context, modelID string) (int64, bool) {
	if modelID == "" {
		return 0, false
	}
	var gen int64
	if err := h.db.For(ctx).QueryRow(ctx, `SELECT runtime.data_generation($1::uuid)`, modelID).Scan(&gen); err != nil {
		return 0, false
	}
	return gen, true
}

// readTagWith is the ETag of a read by act of modelID: the model's data
// generation (a save in another model leaves it as it is), the code, the
// clock's bucket, the database the request is routed to, who reads
// (their account and roles; the access rules they read under are data), the
// model and the request itself. false: no tag (the generation is unknown).
//
// readTagWith takes in what else the answer depends on that the URL does not
// carry (a chart's request body and widget settings); "" for a grid read.
func (h *handler) readTagWith(ctx context.Context, act *actor, modelID string, r *http.Request, extra string) (string, bool) {
	gen, ok := h.dataGeneration(ctx, modelID)
	if !ok || act == nil {
		return "", false
	}
	roles := slices.Clone(act.Roles)
	slices.Sort(roles)
	parts := []string{
		"read/1",
		executableTag(),
		strconv.FormatInt(gen, 10),
		strconv.FormatInt(time.Now().UTC().Truncate(readTagBucket).Unix(), 10),
		tenantdb.TenantFrom(ctx),
		act.UserID, act.CustomerID, strings.Join(roles, ","),
		strconv.FormatBool(act.onControl), strconv.FormatBool(act.platformWide),
		modelID,
		r.URL.Path, r.URL.Query().Encode(),
		r.Header.Get("X-App-Id"), r.Header.Get("X-Model-Id"),
		extra,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`, true
}

// answeredUnchanged tags the response with the read's ETag and, when the
// request already holds it (If-None-Match), answers 304 Not Modified: true,
// and the caller sends nothing more. It must run before the answer is
// worked out (see the file comment).
func (h *handler) answeredUnchanged(w http.ResponseWriter, r *http.Request, act *actor, modelID string) bool {
	return h.answeredUnchangedWith(w, r, act, modelID, "")
}

// answeredUnchangedWith is answeredUnchanged over readTagWith's extra.
func (h *handler) answeredUnchangedWith(w http.ResponseWriter, r *http.Request, act *actor, modelID, extra string) bool {
	tag, ok := h.readTagWith(r.Context(), act, modelID, r, extra)
	if !ok {
		return false
	}
	w.Header().Set("ETag", tag)
	// The answer is the reader's own, and kept by the console in memory
	// (api.getGrid), not in the browser's cache on disk.
	w.Header().Set("Cache-Control", "private, no-store")
	if !etagListed(r.Header.Get("If-None-Match"), tag) {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}

// etagListed reports whether an If-None-Match header names tag (weak
// comparison: a W/ prefix on either side is ignored).
func etagListed(header, tag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(tag, "W/")
	for _, t := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(t), "W/") == want {
			return true
		}
	}
	return false
}

// withReadGeneration lets ctx's whole-revision aggregate reads of modelID be
// answered from the memo at the model's data generation, taken now — before
// the read is worked out, as its ETag is (calculation.WithReadGeneration).
// Unchanged where the generation cannot be read.
func (h *handler) withReadGeneration(ctx context.Context, modelID string) context.Context {
	gen, ok := h.dataGeneration(ctx, modelID)
	if !ok {
		return ctx
	}
	return calculation.WithReadGeneration(ctx, tenantdb.TenantFrom(ctx)+"|"+modelID+"|"+strconv.FormatInt(gen, 10))
}
