package mavericks_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Documentation gates, run by the ordinary `go test ./...` job rather than a
// separate CI stage.
//
// Two failure modes this repository actually hit: docs that mixed "what the
// system does" with "what we intend it to do", leaving a reader unable to
// tell which sentences to trust; and links to files that had been moved or
// renamed, which nothing noticed because prose isn't compiled.

var (
	classificationRE = regexp.MustCompile(`(?m)^> \*\*Classification:\*\* (Current|Target|Historical)\b`)
	// [text](target) — skipping images (![...]) is unnecessary here since the
	// same existence check applies to them.
	linkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
)

// markdownFiles returns every tracked doc, excluding vendored trees.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "dist", "bin", "test-results":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// TestDocsDeclareClassification requires every document to say whether it
// describes the system as it is (Current), as it is intended to become
// (Target), or as it once was (Historical). Without it, a specification and a
// reference read identically, and a reader has no way to know which is which.
func TestDocsDeclareClassification(t *testing.T) {
	for _, path := range markdownFiles(t) {
		// The design-system doc lives with the components it documents and is
		// maintained alongside them; it isn't part of the top-level set.
		if strings.HasPrefix(path, "web/") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !classificationRE.Match(body) {
			t.Errorf(`%s has no classification.
	Add a line near the title, e.g.
	> **Classification:** Current — what this describes.
	Use Current (as built), Target (as intended), or Historical (a past record).`, path)
		}
	}
}

// TestDocsLinksResolve checks that relative links point at files that exist.
// External URLs and in-page anchors are out of scope — this catches the
// renames and moves that silently rot a docs set.
func TestDocsLinksResolve(t *testing.T) {
	for _, path := range markdownFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range linkRE.FindAllStringSubmatch(string(body), -1) {
			target := strings.TrimSpace(m[1])
			switch {
			case target == "",
				strings.HasPrefix(target, "#"),
				strings.HasPrefix(target, "http://"),
				strings.HasPrefix(target, "https://"),
				strings.HasPrefix(target, "mailto:"):
				continue
			}
			// Drop any anchor or title suffix: (path#section "title")
			if i := strings.IndexAny(target, "#"); i >= 0 {
				target = target[:i]
			}
			if i := strings.Index(target, " "); i >= 0 {
				target = target[:i]
			}
			if target == "" {
				continue
			}
			resolved := filepath.Join(filepath.Dir(path), target)
			if strings.HasPrefix(target, "/") {
				resolved = strings.TrimPrefix(target, "/")
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s links to %q, which does not exist (resolved to %s)", path, m[1], resolved)
			}
		}
	}
}
