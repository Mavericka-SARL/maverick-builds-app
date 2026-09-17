package aiassistant

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dslipak/pdf"
	"github.com/xuri/excelize/v2"
)

// maxDocumentChars caps the extracted text stored per document so a single
// upload can't blow out the LLM context window.
const maxDocumentChars = 60000

// maxSheetRows caps how many rows per sheet are extracted from spreadsheets.
const maxSheetRows = 500

// ExtractDocumentText parses an uploaded file into plain text for LLM context.
// Returns the text, a display mime type, and whether the text was truncated.
func ExtractDocumentText(filename string, data []byte) (text, mimeType string, truncated bool, err error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		text, err = extractPDF(data)
		mimeType = "application/pdf"
	case ".xlsx", ".xlsm":
		text, err = extractXLSX(data)
		mimeType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		text, err = extractDOCX(data)
		mimeType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".csv":
		text, err = extractPlain(data)
		mimeType = "text/csv"
	case ".txt", ".md", ".json", ".log":
		text, err = extractPlain(data)
		mimeType = "text/plain"
	default:
		return "", "", false, fmt.Errorf("unsupported file type %q — supported: pdf, xlsx, docx, csv, txt, md, json", ext)
	}
	if err != nil {
		return "", "", false, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", false, fmt.Errorf("no text could be extracted from %s", filename)
	}
	if len(text) > maxDocumentChars {
		text = text[:maxDocumentChars]
		// don't cut a UTF-8 rune in half
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	return text, mimeType, truncated, nil
}

func extractPDF(data []byte) (out string, err error) {
	// The pdf library panics on some malformed files — convert to an error.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse failed: %v", r)
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pdf parse failed: %w", err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("pdf text extraction failed: %w", err)
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, plain); err != nil {
		return "", fmt.Errorf("pdf text extraction failed: %w", err)
	}
	return sb.String(), nil
}

func extractXLSX(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xlsx parse failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	var sb strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "## Sheet: %s\n", sheet)
		for i, row := range rows {
			if i >= maxSheetRows {
				fmt.Fprintf(&sb, "... (%d more rows omitted)\n", len(rows)-maxSheetRows)
				break
			}
			sb.WriteString(strings.Join(row, "\t"))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// extractDOCX unzips the docx container and walks word/document.xml, keeping
// text runs (<w:t>) and paragraph/tab structure. No external dependency.
func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx parse failed: %w", err)
	}
	var docXML io.ReadCloser
	for _, zf := range zr.File {
		if zf.Name == "word/document.xml" {
			docXML, err = zf.Open()
			if err != nil {
				return "", fmt.Errorf("docx parse failed: %w", err)
			}
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("docx parse failed: word/document.xml not found")
	}
	defer func() { _ = docXML.Close() }()

	var sb strings.Builder
	dec := xml.NewDecoder(docXML)
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx parse failed: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteString("\t")
			case "br":
				sb.WriteString("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}

func extractPlain(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("file is not valid UTF-8 text")
	}
	return string(data), nil
}
