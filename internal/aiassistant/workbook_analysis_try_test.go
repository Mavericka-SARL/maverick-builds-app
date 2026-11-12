package aiassistant

import (
	"os"
	"testing"
)

func TestAnalyseWorkbookTry(t *testing.T) {
	p := os.Getenv("WB")
	if p == "" {
		t.Skip("WB not set")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := AnalyseWorkbook(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d chars\n%s", len(out), out)
}
