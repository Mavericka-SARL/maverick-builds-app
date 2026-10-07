// cmd/third-party-notices writes the licence notices a shipped Go binary
// must carry: Go's own licence (the standard library is compiled in) and the
// licence and NOTICE texts of every module the given packages link, read
// from the module cache. It also refuses a module whose licence is missing
// or not on the reviewed list, so a dependency with new obligations fails the
// build instead of shipping unnoticed.
//
//	third-party-notices [-C dir] [-out FILE] [-append] [-heading TEXT] [-report] packages...
//
// deploy/docker/Dockerfile runs it per service (and once more, with -C, for
// grpc-health-probe) and copies the result to /licenses/ in the image. The
// test in this package runs it over ./cmd/... so CI fails on the same
// conditions before an image is built. See docs/LICENSING.md, "What ships
// with a release".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// allowed are the licences reviewed as compatible with shipping a binary
// under the repository's own licences, provided their notices travel with
// it (which is what this tool does). MPL-2.0 is file-level copyleft: fine
// for unmodified dependencies, whose source stays publicly available.
var allowed = map[string]bool{
	"MIT": true, "Apache-2.0": true, "BSD-2-Clause": true, "BSD-3-Clause": true,
	"ISC": true, "MPL-2.0": true, "Zlib": true, "0BSD": true, "Unlicense": true, "CC0-1.0": true,
}

// exceptions are modules accepted after review although their licence text
// is not classified automatically; each names why. Empty today.
var exceptions = map[string]string{}

type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

type pkg struct {
	ImportPath string
	Standard   bool
	Module     *module
}

// Notice is one module's entry.
type Notice struct {
	Path, Version, Licence string
	Files                  map[string]string // file name → text
}

func main() {
	dir := flag.String("C", "", "run `go list` in this directory (another module, e.g. a tool built with go install)")
	out := flag.String("out", "", "write the notices here instead of stdout")
	appendOut := flag.Bool("append", false, "append to -out instead of replacing it, without repeating Go's own licence")
	heading := flag.String("heading", "", "a line naming what the notices cover")
	report := flag.Bool("report", false, "print module, version and licence per line instead of the notices")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: third-party-notices [-C dir] [-out FILE] [-append] [-heading TEXT] [-report] packages...")
		os.Exit(2)
	}
	ctx := context.Background()
	notices, err := Collect(ctx, *dir, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if bad := Unreviewed(notices); len(bad) > 0 {
		fmt.Fprintln(os.Stderr, "error: these modules need a licence review before they can ship (see cmd/third-party-notices):")
		for _, b := range bad {
			fmt.Fprintln(os.Stderr, "  "+b)
		}
		os.Exit(1)
	}
	var text string
	switch {
	case *report:
		text = Report(notices)
	case *appendOut:
		text = Render(notices, *heading, "")
	default:
		goroot, err := goRoot(ctx, *dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		text = Render(notices, *heading, goroot)
	}
	if err := emit(*out, *appendOut, text); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// emit writes text to stdout, or to path (appending when asked).
func emit(path string, appendOut bool, text string) error {
	if path == "" {
		_, err := os.Stdout.WriteString(text)
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendOut {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// goRoot is the GOROOT of the toolchain that builds the binaries — the
// standard library compiled into them is that one's.
func goRoot(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOROOT: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Collect lists the non-standard modules the packages link (main module
// excluded unless it is not this repository, as for a tool built from its
// own module) and reads their licence files.
func Collect(ctx context.Context, dir string, patterns []string) ([]Notice, error) {
	args := append([]string{"list", "-deps", "-json=ImportPath,Standard,Module"}, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	seen := map[string]bool{}
	var notices []Notice
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if p.Standard || p.Module == nil {
			continue
		}
		m := *p.Module
		if m.Main && m.Path == "github.com/mavericks-engine/mavericks" {
			continue // ours: LICENSE and ee/LICENSE ship beside these notices
		}
		if m.Replace != nil {
			m.Version, m.Dir = m.Replace.Version, m.Replace.Dir
		}
		key := m.Path + "@" + m.Version
		if seen[key] {
			continue
		}
		seen[key] = true
		files, err := licenceFiles(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		notices = append(notices, Notice{Path: m.Path, Version: m.Version, Licence: classify(files), Files: files})
	}
	sort.Slice(notices, func(i, j int) bool { return notices[i].Path < notices[j].Path })
	return notices, nil
}

// licenceFiles reads LICENSE/LICENCE/COPYING/NOTICE files at a module root.
// (A module zip carries its repository's root LICENSE when the module has
// none of its own, so the root is the place to look.)
func licenceFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		up := strings.ToUpper(e.Name())
		for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "UNLICENSE"} {
			if strings.HasPrefix(up, prefix) {
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return nil, err
				}
				files[e.Name()] = string(b)
				break
			}
		}
	}
	return files, nil
}

// classify names the licence of a module from its texts. Anything it cannot
// name is "UNKNOWN" and needs a human (exceptions).
func classify(files map[string]string) string {
	var found []string
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasPrefix(strings.ToUpper(n), "NOTICE") {
			continue
		}
		if id := identify(files[n]); id != "" && !contains(found, id) {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		if len(files) == 0 {
			return "MISSING"
		}
		return "UNKNOWN"
	case 1:
		return found[0]
	}
	return strings.Join(found, " AND ")
}

func identify(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	has := func(s string) bool { return strings.Contains(t, s) }
	switch {
	case has("GNU AFFERO GENERAL PUBLIC LICENSE"):
		return "AGPL-3.0"
	case has("GNU LESSER GENERAL PUBLIC LICENSE"):
		return "LGPL"
	case has("GNU GENERAL PUBLIC LICENSE"):
		return "GPL"
	case has("Mozilla Public License Version 2.0") || has("Mozilla Public License, version 2.0"):
		return "MPL-2.0"
	case has("Apache License") && (has("Version 2.0") || has("version 2.0")):
		return "Apache-2.0"
	case has("Permission is hereby granted, free of charge"):
		return "MIT"
	case has("Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted") && has("THE SOFTWARE IS PROVIDED \"AS IS\""):
		if has("copyright notice and this permission notice appear in all copies") {
			return "ISC"
		}
		return "0BSD"
	case has("Permission to use, copy, modify, and distribute this software for any purpose with or without fee is hereby granted"):
		return "ISC"
	case has("Redistribution and use in source and binary forms"):
		if has("Neither the name") || has("names of its contributors may") || has("name of the copyright holder nor") {
			return "BSD-3-Clause"
		}
		return "BSD-2-Clause"
	case has("This is free and unencumbered software released into the public domain"):
		return "Unlicense"
	case has("CC0 1.0 Universal"):
		return "CC0-1.0"
	case has("This software is provided 'as-is', without any express or implied warranty"):
		return "Zlib"
	}
	return ""
}

// Unreviewed lists the modules that may not ship as they are: a licence
// that is missing, unknown or not on the allowed list, unless excepted.
func Unreviewed(notices []Notice) []string {
	var bad []string
	for _, n := range notices {
		if _, ok := exceptions[n.Path]; ok {
			continue
		}
		ok := true
		for _, id := range strings.Split(n.Licence, " AND ") {
			ok = ok && allowed[id]
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s %s: %s", n.Path, n.Version, n.Licence))
		}
	}
	return bad
}

// Report is one "module, version, licence" line per module.
func Report(notices []Notice) string {
	var b strings.Builder
	for _, n := range notices {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", n.Path, n.Version, n.Licence)
	}
	return b.String()
}

// Render is the notices file. With a goroot it starts with the file's
// header and the licence of the Go standard library under it; without one
// (an -append section) it is the modules alone.
func Render(notices []Notice, heading, goroot string) string {
	var b strings.Builder
	rule := strings.Repeat("=", 78)
	if goroot != "" {
		b.WriteString("THIRD-PARTY NOTICES\n\n")
		b.WriteString("This software includes the third-party components listed below, each under its\n")
		b.WriteString("own licence, reproduced here. Mavericka's own licences (LICENSE, ee/LICENSE)\n")
		b.WriteString("ship beside this file and do not replace or restrict any of these.\n\n")
		if lic, err := os.ReadFile(filepath.Join(goroot, "LICENSE")); err == nil {
			version := "Go"
			if v, err := os.ReadFile(filepath.Join(goroot, "VERSION")); err == nil {
				version, _, _ = strings.Cut(strings.TrimSpace(string(v)), "\n")
			}
			fmt.Fprintf(&b, "%s\nThe Go standard library (%s) — BSD-3-Clause\n%s\n\n%s\n\n", rule, version, rule, strings.TrimSpace(string(lic)))
		}
	}
	if heading != "" {
		fmt.Fprintf(&b, "%s\n%s\n%s\n\n", rule, heading, rule)
	}
	for _, n := range notices {
		fmt.Fprintf(&b, "%s\n%s %s — %s\n%s\n", rule, n.Path, n.Version, n.Licence, rule)
		names := make([]string, 0, len(n.Files))
		for name := range n.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "\n--- %s ---\n\n%s\n", name, strings.TrimSpace(n.Files[name]))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
