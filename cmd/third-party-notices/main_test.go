package main

import (
	"strings"
	"testing"
)

// TestShippedModulesHaveReviewedLicences is the CI gate: every module any
// cmd/ binary links must have a licence on the reviewed list, so a new
// dependency with other obligations fails here, not in a release.
func TestShippedModulesHaveReviewedLicences(t *testing.T) {
	notices, err := Collect(t.Context(), "../..", []string{"./cmd/..."})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) < 50 {
		t.Fatalf("only %d modules found; go list did not see the binaries' dependencies", len(notices))
	}
	if bad := Unreviewed(notices); len(bad) > 0 {
		t.Fatalf("modules needing a licence review:\n  %s", strings.Join(bad, "\n  "))
	}
	goroot, err := goRoot(t.Context(), "../..")
	if err != nil {
		t.Fatal(err)
	}
	text := Render(notices, "", goroot)
	for _, want := range []string{"The Go standard library (go1.", "github.com/jackc/pgx/v5", "Permission is hereby granted"} {
		if !strings.Contains(text, want) {
			t.Errorf("notices lack %q", want)
		}
	}
}

func TestIdentify(t *testing.T) {
	for want, text := range map[string]string{
		"AGPL-3.0":     "GNU AFFERO GENERAL PUBLIC LICENSE Version 3",
		"GPL":          "GNU GENERAL PUBLIC LICENSE Version 2",
		"Apache-2.0":   "Apache License\n   Version 2.0, January 2004",
		"MIT":          "Permission is hereby granted, free of charge, to any person",
		"BSD-3-Clause": "Redistribution and use in source and binary forms ... Neither the name of Google",
		"BSD-2-Clause": "Redistribution and use in source and binary forms, with or without modification",
		"MPL-2.0":      "Mozilla Public License Version 2.0",
	} {
		if got := identify(text); got != want {
			t.Errorf("identify(%q) = %q, want %q", text, got, want)
		}
	}
	if bad := Unreviewed([]Notice{{Path: "x", Licence: "AGPL-3.0"}, {Path: "y", Licence: "MISSING"}, {Path: "z", Licence: "MIT AND Apache-2.0"}}); len(bad) != 2 {
		t.Errorf("Unreviewed = %v, want x and y", bad)
	}
}
