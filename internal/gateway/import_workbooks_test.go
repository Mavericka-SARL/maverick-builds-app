package gateway

// The Import Wizard's workbook preview and the Import widget's template are
// read and written by the gateway: the browser no longer parses a workbook.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestImportWorkbookEndpoints(t *testing.T) {
	f := setupRollupFixture(t)
	post := func(path, persona string, body any) (int, http.Header, []byte) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.srv.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if persona != "" {
			req.Header.Set("X-Dev-User", persona)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, out
	}
	dev := "rollup-test-approver"

	wb := excelize.NewFile()
	_ = wb.SetSheetRow("Sheet1", "A1", &[]any{"ignored"})
	_, _ = wb.NewSheet("Data")
	_ = wb.SetSheetRow("Data", "A1", &[]any{" staff ", "amount"})
	_ = wb.SetSheetRow("Data", "A2", &[]any{"STAFF_A1", 1234.5})
	style, _ := wb.NewStyle(&excelize.Style{NumFmt: 4})
	_ = wb.SetCellStyle("Data", "B2", "B2", style)
	var buf bytes.Buffer
	_ = wb.Write(&buf)
	_ = wb.Close()
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	if status, _, body := post("/api/import/parse-workbook", "", map[string]any{"xlsx_base64": b64}); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated parse: %d %s, want 401", status, body)
	}
	status, _, body := post("/api/import/parse-workbook", dev, map[string]any{"xlsx_base64": b64, "sheet": "data"})
	if status != http.StatusOK {
		t.Fatalf("parse: %d %s", status, body)
	}
	var parsed struct {
		Sheets  []string
		Sheet   string
		Headers []string
		Rows    [][]string
	}
	_ = json.Unmarshal(body, &parsed)
	if strings.Join(parsed.Sheets, ",") != "Sheet1,Data" || parsed.Sheet != "Data" ||
		strings.Join(parsed.Headers, ",") != "staff,amount" || len(parsed.Rows) != 1 || strings.Join(parsed.Rows[0], ",") != "STAFF_A1,1234.5" {
		t.Errorf("parsed %+v; want both sheets, Data's trimmed header and its stored number", parsed)
	}
	if status, _, body := post("/api/import/parse-workbook", dev, map[string]any{"xlsx_base64": b64, "sheet": "Budget"}); status != http.StatusBadRequest || !strings.Contains(string(body), "Sheet1, Data") {
		t.Errorf("unknown sheet: %d %s, want 400 listing the sheets", status, body)
	}
	if status, _, body := post("/api/import/parse-workbook", dev, map[string]any{"xlsx_base64": base64.StdEncoding.EncodeToString([]byte("not a workbook"))}); status != http.StatusBadRequest {
		t.Errorf("not a workbook: %d %s, want 400", status, body)
	}

	status, hdr, body := post("/api/import/template-workbook", dev, map[string]any{
		"filename": "Staff grid template", "rows": []any{[]any{"staff", "amount"}, []any{"STAFF_A1", 0}}})
	if status != http.StatusOK || !strings.Contains(hdr.Get("Content-Disposition"), "Staff grid template.xlsx") {
		t.Fatalf("template: %d %v %s", status, hdr, body)
	}
	tpl, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("template is not a workbook: %v", err)
	}
	defer tpl.Close() //nolint:errcheck
	rows, _ := tpl.GetRows("Import")
	if len(rows) != 2 || strings.Join(rows[0], ",") != "staff,amount" || strings.Join(rows[1], ",") != "STAFF_A1,0" {
		t.Errorf("template rows %v", rows)
	}
	if status, _, _ := post("/api/import/template-workbook", dev, map[string]any{"rows": []any{}}); status != http.StatusBadRequest {
		t.Errorf("empty template: %d, want 400", status)
	}
}
