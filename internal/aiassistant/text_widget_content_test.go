package aiassistant

import "testing"

// TestTextWidgetContentTakesEscapedLineBreaks: Markdown with no line break
// but "\n" in it was escaped once too often and rendered as one heading;
// it gets its line breaks. Real line breaks, and other widgets, stay as sent.
func TestTextWidgetContentTakesEscapedLineBreaks(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, c := range []struct{ typ, in, want string }{
		{"text", `# Guide\n\nWho does what:\r\n- FP&A`, "# Guide\n\nWho does what:\n- FP&A"},
		{"text", "# Guide\n\nA path C:\\new\\n stays", "# Guide\n\nA path C:\\new\\n stays"},
		{"text", "no breaks at all", "no breaks at all"},
		{"image", `data:image/png;base64,AA\nBB`, `data:image/png;base64,AA\nBB`},
	} {
		if got := *textWidgetContent(c.typ, str(c.in)); got != c.want {
			t.Errorf("%s %q: got %q, want %q", c.typ, c.in, got, c.want)
		}
	}
	if textWidgetContent("text", nil) != nil {
		t.Error("no content became some")
	}
}
