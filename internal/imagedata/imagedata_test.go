package imagedata

import (
	"encoding/base64"
	"strings"
	"testing"
)

func dataURL(mime string, payload []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

// What a stored picture may be: an allowed type, really base64, and small
// enough that a dashboard stays a row rather than a payload.
func TestValidate(t *testing.T) {
	small := []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")
	for _, ok := range []string{
		"",
		dataURL("image/svg+xml", small),
		dataURL("image/png", small),
		dataURL("image/jpeg", small),
		dataURL("image/webp", small),
		dataURL("image/gif", small),
	} {
		if err := Validate("the image", ok, MaxWidgetBytes); err != nil {
			t.Fatalf("%.30s: %v", ok, err)
		}
	}
	for _, bad := range []struct{ what, url, says string }{
		{"a plain URL", "https://example.com/logo.png", "data URL"},
		{"not base64-flagged", "data:image/png,rawbytes", "base64"},
		{"a script", dataURL("text/html", []byte("<script>alert(1)</script>")), "PNG"},
		{"a PDF", dataURL("application/pdf", small), "PNG"},
		{"not really base64", "data:image/png;base64,!!!!", "valid base64"},
		{"too large", dataURL("image/png", make([]byte, MaxWidgetBytes+1)), "at most"},
	} {
		err := Validate("the image", bad.url, MaxWidgetBytes)
		if err == nil {
			t.Fatalf("%s was accepted", bad.what)
		}
		if !strings.Contains(err.Error(), bad.says) {
			t.Fatalf("%s: %q does not mention %q", bad.what, err, bad.says)
		}
	}
	// A favicon's limit is tighter than a dashboard's, and each is enforced.
	big := dataURL("image/png", make([]byte, MaxFaviconBytes+1))
	if err := Validate("favicon", big, MaxFaviconBytes); err == nil {
		t.Fatal("an oversized favicon was accepted")
	}
	if err := Validate("the image", big, MaxWidgetBytes); err != nil {
		t.Fatalf("the same file is fine as a dashboard image: %v", err)
	}
}
