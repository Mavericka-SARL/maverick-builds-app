package gateway

import "testing"

// TestNoChangeNote: a turn that ends without a proposal gets the server's
// "no change was made" under a reply that follows a refused plan or claims
// a change — gpt-5-mini's "the pages are created and already applied" over
// refused plans — and none under an answer to a question.
func TestNoChangeNote(t *testing.T) {
	for _, tc := range []struct {
		reply   string
		refused bool
		note    bool
	}{
		{"I've created the four pages and granted them to the CFO role.", false, true},
		{"Done — the pages were reordered and are already applied.", false, true},
		{"The dashboards have been updated to follow the selector bar.", false, true},
		{"I could not fix step 3: FR is not a member of country.", true, true},
		{"The model has 12 dimensions and 140 metrics; revenue is an input.", false, false},
		{"Which scenario should the tile show — Base or Upside?", false, false},
	} {
		if got := noChangeNote(tc.reply, tc.refused) != ""; got != tc.note {
			t.Errorf("%q (refused %v): note %v, want %v", tc.reply, tc.refused, got, tc.note)
		}
	}
}
