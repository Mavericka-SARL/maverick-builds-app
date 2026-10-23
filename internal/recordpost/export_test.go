package recordpost

import (
	"context"
	"time"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// PostAsRead posts one record of form as a batch that read it at readAt,
// holding data in status, posts it: what a batch slower than a change to
// the record would write.
func (ps *Poster) PostAsRead(ctx context.Context, form *crudapp.FormDef, recordID, status string, data map[string]any, readAt time.Time, userID string) error {
	tp, err := ps.newTablePosting(ctx, form)
	if err != nil {
		return err
	}
	rec := postingRecord{id: recordID, status: status, data: data, updatedAt: readAt}
	ps.totals(ctx, tp, ps.post(ctx, tp, []postingRecord{rec}, userID), userID)
	return nil
}
