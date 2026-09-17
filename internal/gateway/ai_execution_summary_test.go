// Pure-function tests for buildExecutionSummary/shortStepLabel — no DB
// needed. Covers the condensation added after a batch proposal (create one
// dimension + add 18 members) produced 19 near-identical chat lines, each
// with a raw UUID, that were unreadable at a glance.
package gateway

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func step(tool, description, result, status string) aiassistant.ProposalStep {
	return aiassistant.ProposalStep{Tool: tool, Description: description, Result: result, Status: status}
}

func TestBuildExecutionSummaryCollapsesSameToolBatch(t *testing.T) {
	steps := []aiassistant.ProposalStep{
		step("create_dimension", "Create dimension 'Secret'", "Dimension 'Secret' created (id: a4fdcfcc-02e4-4ecd-91f3-987087676fb5)", "success"),
	}
	for i := 1; i <= 18; i++ {
		steps = append(steps, step("add_dimension_member",
			"Add Secret member 's"+strconv.Itoa(i)+"'",
			"Dimension member added (id: 7b37b46b-b0be-4a71-9e86-40efbf1bd27d)", "success"))
	}

	summary := buildExecutionSummary(steps)

	if strings.Count(summary, "\n") > 6 {
		t.Errorf("summary has %d lines, want a handful — batch of 18 same-tool steps should collapse to ~1 line:\n%s", strings.Count(summary, "\n"), summary)
	}
	if strings.Contains(summary, "7b37b46b-b0be-4a71-9e86-40efbf1bd27d") {
		t.Error("summary leaks a raw member UUID — should show short labels (s1, s2, ...), not IDs")
	}
	if !strings.Contains(summary, "18") {
		t.Errorf("summary doesn't mention the batch count (18):\n%s", summary)
	}
	if !strings.Contains(summary, "Secret") {
		t.Errorf("summary drops the single create_dimension step entirely:\n%s", summary)
	}
	if !strings.Contains(summary, "Executed 19 steps successfully") {
		t.Errorf("summary missing an overall count line:\n%s", summary)
	}
}

func TestBuildExecutionSummaryNeverCollapsesFailures(t *testing.T) {
	steps := []aiassistant.ProposalStep{
		step("add_dimension_member", "Add Secret member 's1'", "added", "success"),
		step("add_dimension_member", "Add Secret member 's2'", "duplicate code 's2'", "failed"),
		step("add_dimension_member", "Add Secret member 's3'", "added", "success"),
	}

	summary := buildExecutionSummary(steps)

	if !strings.Contains(summary, "✗ Step 2") || !strings.Contains(summary, "duplicate code 's2'") {
		t.Errorf("failure must be listed individually with its full result text:\n%s", summary)
	}
	if !strings.Contains(summary, "1 failed") {
		t.Errorf("summary doesn't report the failure count:\n%s", summary)
	}
}

func TestBuildExecutionSummarySingleStepNoBatchNoise(t *testing.T) {
	steps := []aiassistant.ProposalStep{
		step("create_metric", "Create metric 'total_opex'", "Metric 'total_opex' created (id: xyz)", "success"),
	}
	summary := buildExecutionSummary(steps)
	if !strings.Contains(summary, "total_opex") {
		t.Errorf("single-step summary lost the metric name:\n%s", summary)
	}
	if strings.Contains(summary, "1×") {
		t.Errorf("a single step should read as one line, not a '1×' batch:\n%s", summary)
	}
}
