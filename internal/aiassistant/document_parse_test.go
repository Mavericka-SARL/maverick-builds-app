package aiassistant

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestExtractDocumentText_PlainTextTypes(t *testing.T) {
	cases := []struct {
		filename string
		mime     string
	}{
		{"notes.txt", "text/plain"},
		{"readme.md", "text/plain"},
		{"config.json", "text/plain"},
		{"app.log", "text/plain"},
		{"data.csv", "text/csv"},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			text, mime, truncated, err := ExtractDocumentText(tc.filename, []byte("hello world\nline two"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if text != "hello world\nline two" {
				t.Fatalf("unexpected text: %q", text)
			}
			if mime != tc.mime {
				t.Fatalf("expected mime %q, got %q", tc.mime, mime)
			}
			if truncated {
				t.Fatal("did not expect truncation")
			}
		})
	}
}

func TestExtractDocumentText_UnsupportedExtension(t *testing.T) {
	_, _, _, err := ExtractDocumentText("payload.exe", []byte("binary junk"))
	if err == nil {
		t.Fatal("expected an error for unsupported extension")
	}
	if !strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExtractDocumentText_EmptyResultErrors(t *testing.T) {
	_, _, _, err := ExtractDocumentText("blank.txt", []byte("   \n\t  "))
	if err == nil {
		t.Fatal("expected an error when no text is extracted")
	}
}

func TestExtractDocumentText_InvalidUTF8Errors(t *testing.T) {
	_, _, _, err := ExtractDocumentText("bad.txt", []byte{0xff, 0xfe, 0x00, 0x01})
	if err == nil {
		t.Fatal("expected an error for invalid UTF-8 plain text")
	}
}

func TestExtractDocumentText_Truncation(t *testing.T) {
	huge := strings.Repeat("a", maxDocumentChars+5000)
	text, _, truncated, err := ExtractDocumentText("big.txt", []byte(huge))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true for oversized document")
	}
	if len(text) > maxDocumentChars {
		t.Fatalf("expected text capped at %d chars, got %d", maxDocumentChars, len(text))
	}
}

func TestExtractDocumentText_XLSX(t *testing.T) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	sheet := "Budget"
	idx, err := f.NewSheet(sheet)
	if err != nil {
		t.Fatalf("new sheet: %v", err)
	}
	f.SetActiveSheet(idx)
	_ = f.SetCellValue(sheet, "A1", "department")
	_ = f.SetCellValue(sheet, "B1", "cost")
	_ = f.SetCellValue(sheet, "A2", "Engineering")
	_ = f.SetCellValue(sheet, "B2", 600000)
	f.DeleteSheet("Sheet1")

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}

	text, mime, truncated, err := ExtractDocumentText("budget.xlsx", buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Fatal("did not expect truncation")
	}
	if mime != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("unexpected mime: %q", mime)
	}
	if !strings.Contains(text, "## Sheet: Budget") {
		t.Fatalf("expected sheet header in extracted text, got: %q", text)
	}
	if !strings.Contains(text, "department\tcost") || !strings.Contains(text, "Engineering\t600000") {
		t.Fatalf("expected row data in extracted text, got: %q", text)
	}
}

func TestExtractDocumentText_XLSX_RowCap(t *testing.T) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	for i := 1; i <= maxSheetRows+10; i++ {
		_ = f.SetCellValue("Sheet1", fmt.Sprintf("A%d", i), "row")
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}
	text, _, _, err := ExtractDocumentText("many_rows.xlsx", buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "more rows omitted") {
		t.Fatalf("expected row-cap notice in extracted text, got: %q", text)
	}
}

func TestExtractDocumentText_XLSX_Malformed(t *testing.T) {
	_, _, _, err := ExtractDocumentText("broken.xlsx", []byte("not a real xlsx file"))
	if err == nil {
		t.Fatal("expected an error for a malformed xlsx file")
	}
}

// buildMinimalDocx constructs a valid docx zip container with the given
// paragraphs, mirroring the shape Word actually produces.
func buildMinimalDocx(t *testing.T, paragraphs []string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		sb.WriteString(`<w:p><w:r><w:t>`)
		sb.WriteString(p)
		sb.WriteString(`</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDocumentText_DOCX(t *testing.T) {
	data := buildMinimalDocx(t, []string{"Budget Policy 2026", "All spend requires CFO approval."})
	text, mime, truncated, err := ExtractDocumentText("policy.docx", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Fatal("did not expect truncation")
	}
	if mime != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("unexpected mime: %q", mime)
	}
	if !strings.Contains(text, "Budget Policy 2026") || !strings.Contains(text, "CFO approval") {
		t.Fatalf("expected both paragraphs in extracted text, got: %q", text)
	}
}

func TestExtractDocumentText_DOCX_MissingDocumentXML(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/other.xml")
	_, _ = w.Write([]byte("<x/>"))
	_ = zw.Close()

	_, _, _, err := ExtractDocumentText("empty.docx", buf.Bytes())
	if err == nil {
		t.Fatal("expected an error when word/document.xml is missing")
	}
}

func TestExtractDocumentText_DOCX_NotAZip(t *testing.T) {
	_, _, _, err := ExtractDocumentText("fake.docx", []byte("not a zip at all"))
	if err == nil {
		t.Fatal("expected an error for a non-zip docx")
	}
}

func TestExtractDocumentText_PDF_Malformed(t *testing.T) {
	// The pdf library can panic on garbage input — this exercises the
	// recover() in extractPDF and confirms it surfaces as a plain error.
	_, _, _, err := ExtractDocumentText("broken.pdf", []byte("%PDF-1.4 not a real pdf"))
	if err == nil {
		t.Fatal("expected an error for a malformed pdf file")
	}
}
