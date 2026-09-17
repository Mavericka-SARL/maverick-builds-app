package importpkg

// URL-shape and fetch-behavior coverage for the Google Sheets source. The
// fetch tests run against an httptest server standing in for
// docs.google.com — including the trap where a private sheet answers the
// export URL with HTTP 200 and an HTML sign-in page instead of a 403.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSheetID = "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms"

func TestParseSheetURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantID  string
		wantGid string
		wantErr bool
	}{
		{"edit url with query gid", "https://docs.google.com/spreadsheets/d/" + testSheetID + "/edit?gid=1234#gid=1234", testSheetID, "1234", false},
		{"edit url with fragment gid only", "https://docs.google.com/spreadsheets/d/" + testSheetID + "/edit#gid=77", testSheetID, "77", false},
		{"bare url no gid", "https://docs.google.com/spreadsheets/d/" + testSheetID, testSheetID, "", false},
		{"user-scoped path", "https://docs.google.com/spreadsheets/u/0/d/" + testSheetID + "/edit", testSheetID, "", false},
		{"bare id pasted", testSheetID, testSheetID, "", false},
		{"whitespace trimmed", "  https://docs.google.com/spreadsheets/d/" + testSheetID + "/edit  ", testSheetID, "", false},
		{"wrong host", "https://example.com/spreadsheets/d/" + testSheetID, "", "", true},
		{"lookalike host", "https://docs.google.com.evil.example/spreadsheets/d/" + testSheetID, "", "", true},
		{"not a sheets path", "https://docs.google.com/document/d/" + testSheetID + "/edit", "", "", true},
		{"short id", "https://docs.google.com/spreadsheets/d/abc/edit", "", "", true},
		{"malformed gid", "https://docs.google.com/spreadsheets/d/" + testSheetID + "/edit?gid=abc", "", "", true},
		{"empty", "   ", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, gid, err := ParseSheetURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSheetURL(%q) = (%q, %q), want error", tc.in, id, gid)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSheetURL(%q): %v", tc.in, err)
			}
			if id != tc.wantID || gid != tc.wantGid {
				t.Errorf("ParseSheetURL(%q) = (%q, %q), want (%q, %q)", tc.in, id, gid, tc.wantID, tc.wantGid)
			}
		})
	}
}

func TestSheetFetcherFetchCSV(t *testing.T) {
	const csvBody = "FixtureRevenue\n100\n"
	var gotGid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGid = r.URL.Query().Get("gid")
		switch {
		case strings.Contains(r.URL.Path, "/d/"+testSheetID+"/"):
			w.Header().Set("Content-Type", "text/csv")
			// Google's export always leads with a UTF-8 BOM.
			_, _ = w.Write(append([]byte("\xef\xbb\xbf"), []byte(csvBody)...))
		case strings.Contains(r.URL.Path, "/d/private_sheet_id_000000/"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>Sign in - Google Accounts</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &SheetFetcher{BaseURL: srv.URL, Client: srv.Client()}
	ctx := context.Background()

	t.Run("shared sheet, BOM stripped, gid forwarded", func(t *testing.T) {
		data, err := f.FetchCSV(ctx, testSheetID, "42")
		if err != nil {
			t.Fatalf("FetchCSV: %v", err)
		}
		if string(data) != csvBody {
			t.Errorf("body = %q, want %q (BOM must be stripped)", data, csvBody)
		}
		if gotGid != "42" {
			t.Errorf("gid forwarded = %q, want %q", gotGid, "42")
		}
	})

	t.Run("private sheet's sign-in page is not CSV", func(t *testing.T) {
		_, err := f.FetchCSV(ctx, "private_sheet_id_000000", "")
		if !errors.Is(err, ErrSheetNotAccessible) {
			t.Fatalf("err = %v, want ErrSheetNotAccessible", err)
		}
	})

	t.Run("unknown sheet is not accessible", func(t *testing.T) {
		_, err := f.FetchCSV(ctx, "no_such_spreadsheet_id_x", "")
		if !errors.Is(err, ErrSheetNotAccessible) {
			t.Fatalf("err = %v, want ErrSheetNotAccessible", err)
		}
	})

	t.Run("invalid id rejected before any request", func(t *testing.T) {
		if _, err := f.FetchCSV(ctx, "../../etc/passwd", ""); err == nil {
			t.Fatal("expected error for invalid spreadsheet ID")
		}
	})
}
