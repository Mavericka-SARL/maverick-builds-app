package aiassistant_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// Every tool a proposal step may name has a label in the AI Developer panel.
// Without one the panel shows the raw tool name ("reorder dimension
// members") in the plan the developer confirms — the case when
// reorder_dimension_members was added to WriteToolNames alone.
func TestEveryWriteToolHasAConsoleLabel(t *testing.T) {
	src, err := os.ReadFile("../../web/src/consoles/developer/AIAssistant.tsx")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const TOOL_LABELS: Record<string, string> = \{(.*?)\n\};`).FindSubmatch(src)
	if block == nil {
		t.Fatal("TOOL_LABELS not found in AIAssistant.tsx")
	}
	labelled := map[string]bool{}
	for _, m := range regexp.MustCompile(`([a-z_]+):\s*"[^"]+"`).FindAllSubmatch(block[1], -1) {
		labelled[string(m[1])] = true
	}
	var missing []string
	for _, name := range aiassistant.WriteToolNames {
		if !labelled[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("tools with no TOOL_LABELS entry in AIAssistant.tsx: %s", strings.Join(missing, ", "))
	}
}
