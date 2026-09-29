package aiassistant

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// TestPromptListsEveryFormulaFunction: the assistant is told of exactly the
// functions the engine has.
func TestPromptListsEveryFormulaFunction(t *testing.T) {
	prompt := BuildSystemPrompt(ModelContext{})
	i := strings.Index(prompt, "The functions are exactly these: ")
	if i < 0 {
		t.Fatal("the prompt has no formula function list")
	}
	list := prompt[i+len("The functions are exactly these: "):]
	list = list[:strings.Index(list, ".\n")]
	got := map[string]bool{}
	for _, n := range strings.Split(list, ", ") {
		got[n] = true
	}
	for _, n := range formula.BuiltinNames() {
		if !got[n] {
			t.Errorf("the prompt does not list %s", n)
		}
	}
	if len(got) != len(formula.BuiltinNames()) {
		t.Errorf("the prompt lists %d functions, the engine has %d", len(got), len(formula.BuiltinNames()))
	}
}
