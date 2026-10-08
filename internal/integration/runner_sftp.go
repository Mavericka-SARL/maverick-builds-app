package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// executeSFTPPull reads one file from an SFTP server, reads its rows with
// the file-import reader (sheet + reshape), and maps and commits them
// through the same tail as an HTTPS pull. A scheduled run skips a file
// identical to the one the last import read; a manual run always imports.
func (rn *Runner) executeSFTPPull(ctx context.Context, run *Run, def *Definition, authType string, secret map[string]string, res RunResult) RunResult {
	cfg := def.Config
	src := cfg.SFTP

	reqStart := rn.now()
	file, err := fetchSFTPFile(ctx, src, authType, secret, (&Limits{}).withDefaults().MaxResponseSize, rn.AllowInsecure)
	res.Requests = 1
	durMS := int(rn.now().Sub(reqStart).Milliseconds())
	target := "sftp://" + sftpHostName(src.Host) + "/"
	if file != nil {
		target += strings.TrimPrefix(file.Path, "/")
	}
	if err != nil {
		code := classifySFTPErr(err)
		msg := err.Error()
		var hk *HostKeyError
		if errors.As(err, &hk) {
			msg = hk.Error() // without the "ssh: handshake failed:" wrapper
			res.Meta["host_key"] = AuthorizedKeyLine(hk.Presented)
			res.Meta["host_key_fingerprint"] = HostKeyLabel(hk.Presented)
			if hk.Changed {
				res.Meta["host_key_changed"] = "true"
			}
		}
		rn.Store.RecordAttempt(ctx, run.ID, 1, 1, "SFTP", target, 0, durMS, code, sanitizeMsg(msg))
		res.ErrorCode, res.Message = code, msg
		return res
	}
	rn.Store.RecordAttempt(ctx, run.ID, 1, 1, "SFTP", target, 0, durMS, "", "")

	sum := sha256.Sum256(file.Data)
	fileHash := hex.EncodeToString(sum[:])
	res.Meta["file"] = file.Path
	res.Meta["file_size"] = fmt.Sprintf("%d", file.Size)
	res.Meta["file_modified"] = file.Modified.UTC().Format(time.RFC3339)
	res.Meta["file_sha256"] = fileHash
	if !strings.EqualFold(filepath.Ext(file.Path), ".csv") {
		if names, nerr := importpkg.XLSXSheetNames(file.Data); nerr == nil {
			j, _ := json.Marshal(names)
			res.Meta["sheets"] = string(j)
		}
	}

	if run.TriggerType == "schedule" && !run.DryRun && rn.Store.LastImportedFileHash(ctx, def.ID, run.ID) == fileHash {
		res.Status = "success"
		res.Meta["unchanged"] = "true"
		res.Message = "file unchanged since the last import; nothing written"
		return res
	}

	header, raw, perr := importpkg.ReadShaped(file.Path, file.Data, src.Sheet, src.Reshape)
	if perr != nil {
		res.ErrorCode, res.Message = ErrCodeInvalidData, file.Path+": "+perr.Error()
		return res
	}
	records := rawRowsToRecords(header, raw)
	maxRecords := cfg.Limits.MaxRecords
	if maxRecords <= 0 {
		maxRecords = 100_000
	}
	if len(records) > maxRecords {
		records = records[:maxRecords]
		res.Meta["stopped"] = "max_records"
	}
	res.Pages = 1
	res.RecordsRead = len(records)

	if run.TriggerType == "test" {
		attachFilePreview(&res, header, records)
	}

	failThreshold := cfg.Limits.FailureThreshold
	if failThreshold <= 0 {
		failThreshold = 100
	}
	h, rows, recordErrs := MapPullRecords(cfg, records, 0)
	if len(recordErrs) > failThreshold {
		res.ErrorCode, res.Message = ErrCodeInvalidData, fmt.Sprintf("%d record(s) failed mapping (threshold %d)", len(recordErrs), failThreshold)
		res.RecordsSkipped = len(recordErrs)
		return res
	}
	return rn.finishPull(ctx, run, def, h, rows, recordErrs, res)
}

// rawRowsToRecords turns the file reader's rows into connector records keyed
// by column name — the shape a CSV response produces, blanks as "".
func rawRowsToRecords(header []string, raw []importpkg.RawRow) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		rec := make(map[string]any, len(header))
		for _, h := range header {
			rec[strings.TrimSpace(h)] = r.Cells[h]
		}
		out = append(out, rec)
	}
	return out
}

// filePreviewRecords is how many records a test run shows.
const filePreviewRecords = 20

// attachFilePreview stores a test run's first records as a JSON array with
// the file's column order, so the wizard previews and maps them as it does
// an API's records.
func attachFilePreview(res *RunResult, header []string, records []map[string]any) {
	n := min(len(records), filePreviewRecords)
	var b bytes.Buffer
	b.WriteByte('[')
	for i, rec := range records[:n] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for j, h := range header {
			if j > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(strings.TrimSpace(h))
			v, _ := json.Marshal(rec[strings.TrimSpace(h)])
			b.Write(k)
			b.WriteByte(':')
			b.Write(v)
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	res.Meta["preview_content_type"] = "application/json"
	res.Meta["preview_truncated"] = fmt.Sprintf("%v", len(records) > n)
	res.Meta["preview_body"] = b.String()
}
