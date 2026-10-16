package mavericks_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The per-model reporting views were removed by migration 143 (nothing read
// them, and they summed every correction of a cell). The product builds no
// views from code, and no later migration may create a per-model one again —
// by a runtime.m_ name or by a name formatted per model.
func TestNoPerModelViews(t *testing.T) {
	createView := regexp.MustCompile(`(?is)\bcreate\s+(or\s+replace\s+)?(materialized\s+)?view\b`)
	for _, root := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if loc := createView.FindIndex(src); loc != nil {
				t.Errorf("%s creates a view (%q); the product builds no views from code", path, src[loc[0]:loc[1]])
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	perModel := regexp.MustCompile(`(?is)\bcreate\s+(or\s+replace\s+)?(materialized\s+)?view\s+(if\s+not\s+exists\s+)?("?runtime"?\s*\.\s*)?"?m_|\bcreate\b[^;]*\bview\b[^;]*%[sI]`)
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		num, err := strconv.Atoi(strings.SplitN(filepath.Base(path), "_", 2)[0])
		if err != nil || num <= 143 {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if perModel.Match(src) {
			t.Errorf("%s creates a per-model view; migration 143 removed them for good", path)
		}
	}
}
