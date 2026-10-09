package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/xuri/excelize/v2"
	"golang.org/x/crypto/ssh"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/integration"
)

// sftpFixture is a real SSH + SFTP server on loopback serving a temp
// folder: password login for sftpUser/sftpPassword, key login for the
// authorized key.
type sftpFixture struct {
	Addr    string
	HostKey ssh.PublicKey
	Dir     string
}

const sftpUser, sftpPassword = "loader", "pw-123"

func startSFTPServer(t *testing.T, authorized ssh.PublicKey) *sftpFixture {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == sftpUser && string(pw) == sftpPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && c.User() == sftpUser && bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dir := t.TempDir()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTP(nc, cfg, dir)
		}
	}()
	return &sftpFixture{Addr: ln.Addr().String(), HostKey: hostSigner.PublicKey(), Dir: dir}
}

func serveSFTP(nc net.Conn, cfg *ssh.ServerConfig, dir string) {
	defer nc.Close() //nolint:errcheck
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, in, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				_ = req.Reply(req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp", nil)
			}
		}()
		srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(dir))
		if err != nil {
			return
		}
		_ = srv.Serve()
		_ = srv.Close()
	}
}

// writeFile writes a fixture file and sets its modified time.
func (f *sftpFixture) writeFile(t *testing.T, rel string, data []byte, mod time.Time) {
	t.Helper()
	p := filepath.Join(f.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// salesWorkbook: a "Notes" sheet first, then "Data" with a title row above
// the header — the layout a sheet + reshape.header_row exists for.
func salesWorkbook(t *testing.T, a, b float64) []byte {
	t.Helper()
	x := excelize.NewFile()
	defer x.Close() //nolint:errcheck
	_ = x.SetSheetName("Sheet1", "Notes")
	_ = x.SetCellValue("Notes", "A1", "exported nightly")
	if _, err := x.NewSheet("Data"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cell string
		v    any
	}{{"A1", "Sales by region"}, {"A2", "Region"}, {"B2", "Amount"}, {"A3", "A"}, {"B3", a}, {"A4", "B"}, {"B4", b}} {
		_ = x.SetCellValue("Data", c.cell, c.v)
	}
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func runMeta(t *testing.T, run *integration.Run) map[string]string {
	t.Helper()
	m := map[string]string{}
	if len(run.Meta) > 0 {
		if err := json.Unmarshal(run.Meta, &m); err != nil {
			t.Fatalf("meta: %v", err)
		}
	}
	return m
}

// TestRunner_SFTPPull drives the worker against a real SFTP server: host
// key trust, a workbook sheet with a title row, a commit, the scheduled
// skip of an unchanged file, a changed host key, newest-file selection, a
// missing file, a wrong password and key login.
func TestRunner_SFTPPull(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)
	ctx := context.Background()
	var appID string
	if err := st.Pool().QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		t.Fatal(err)
	}

	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	fx := startSFTPServer(t, clientSigner.PublicKey())
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	fx.writeFile(t, "exports/sales.xlsx", salesWorkbook(t, 10, 20), base)

	pwConn, err := st.CreateConnection(ctx, appID, "sftp password", "basic", nil,
		[]byte(fmt.Sprintf(`{"username":%q,"password":%q}`, sftpUser, sftpPassword)), "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &integration.Config{
		Kind: integration.ConfigKind, Protocol: integration.ProtocolSFTP, Direction: integration.DirectionPull,
		TargetType: integration.TargetGrid, TargetID: gridID, ImportMode: integration.ModeIncremental,
		SFTP: &integration.SFTPSource{
			Host: fx.Addr, Select: integration.FileFixed, Path: "exports/sales.xlsx",
			Sheet: "Data", Reshape: &importpkg.Reshape{HeaderRow: 2},
		},
		Auth: integration.AuthPlacement{Type: "basic"},
		Mapping: integration.MappingConfig{Fields: []integration.FieldMap{
			{Source: "$.Region", Target: "region"},
			{Source: "$.Amount", Target: "amount", Transforms: []integration.Transform{{Kind: integration.TransformToNumber}}},
		}},
	}
	def, err := st.CreateDefinition(ctx, modelID, revID, "Sales over SFTP", "", nil, "draft", pwConn.ID, cfg, true)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	update := func(mutate func(*integration.Config), connID string) {
		t.Helper()
		c := *def.Config
		s := *c.SFTP
		c.SFTP = &s
		mutate(&c)
		var conn *string
		if connID != "" {
			conn = &connID
		}
		d, uerr := st.UpdateDefinition(ctx, modelID, def.ID, nil, nil, nil, nil, conn, &c, true)
		if uerr != nil {
			t.Fatalf("update def: %v", uerr)
		}
		def = d
	}
	factCount := func() int {
		var n int
		_ = st.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, revID).Scan(&n)
		return n
	}

	// 1. No key pinned: the test run stops at the handshake and reports it.
	run := claimAndRun(t, rn, st, def.ID, "test", false)
	m := runMeta(t, run)
	if run.Status != "failed" || run.ErrorCode != integration.ErrCodeHostKey || m["host_key"] != integration.AuthorizedKeyLine(fx.HostKey) ||
		m["host_key_fingerprint"] != "ED25519 host key "+ssh.FingerprintSHA256(fx.HostKey) || m["host_key_changed"] != "" ||
		!strings.HasPrefix(run.Message, "the server's ED25519 host key SHA256:") {
		t.Fatalf("untrusted key: %s %s %q meta=%v", run.Status, run.ErrorCode, run.Message, m)
	}

	// 2. Trusted: the test run previews the Data sheet below its title row.
	update(func(c *integration.Config) { c.SFTP.HostKey = m["host_key"] }, "")
	run = claimAndRun(t, rn, st, def.ID, "test", false)
	m = runMeta(t, run)
	if run.Status != "success" || run.RecordsRead != 2 || run.RecordsWritten != 0 {
		t.Fatalf("test run: %s %s %q read=%d written=%d", run.Status, run.ErrorCode, run.Message, run.RecordsRead, run.RecordsWritten)
	}
	if m["preview_body"] != `[{"Region":"A","Amount":"10"},{"Region":"B","Amount":"20"}]` || m["sheets"] != `["Notes","Data"]` ||
		m["file"] != "exports/sales.xlsx" || len(m["file_sha256"]) != 64 {
		t.Fatalf("test meta: %v", m)
	}
	if factCount() != 0 {
		t.Fatal("a test run wrote facts")
	}

	// 3. Manual run commits.
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if run.Status != "success" || run.RecordsWritten != 2 {
		t.Fatalf("manual run: %s %s %q written=%d", run.Status, run.ErrorCode, run.Message, run.RecordsWritten)
	}
	facts := factCount()
	if facts != 2 {
		t.Fatalf("facts after import: %d", facts)
	}
	var sum float64
	_ = st.Pool().QueryRow(ctx, `SELECT COALESCE(SUM(value),0) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&sum)
	if sum != 30 {
		t.Fatalf("fact sum %v, want 30", sum)
	}

	// 4. A scheduled run over the same file writes nothing; a manual run
	// would (it always imports), a changed file does. Only an active
	// connector's schedule runs.
	if _, err := st.Pool().Exec(ctx, `UPDATE model.integration_def SET status='active' WHERE id=$1::uuid`, def.ID); err != nil {
		t.Fatal(err)
	}
	run = claimAndRun(t, rn, st, def.ID, "schedule", false)
	if m = runMeta(t, run); run.Status != "success" || m["unchanged"] != "true" || run.RecordsWritten != 0 || factCount() != facts {
		t.Fatalf("unchanged schedule run: %s %q written=%d meta=%v", run.Status, run.Message, run.RecordsWritten, m)
	}
	fx.writeFile(t, "exports/sales.xlsx", salesWorkbook(t, 11, 22), base.Add(time.Minute))
	run = claimAndRun(t, rn, st, def.ID, "schedule", false)
	if m = runMeta(t, run); run.Status != "success" || m["unchanged"] != "" || run.RecordsWritten != 2 {
		t.Fatalf("changed-file schedule run: %s %s %q written=%d meta=%v", run.Status, run.ErrorCode, run.Message, run.RecordsWritten, m)
	}
	// Back to a draft, so the edits below need no new test.
	if _, err := st.Pool().Exec(ctx, `UPDATE model.integration_def SET status='draft' WHERE id=$1::uuid`, def.ID); err != nil {
		t.Fatal(err)
	}

	// 5. A different pinned key: refused as changed, nothing read.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherPriv)
	trusted := def.Config.SFTP.HostKey
	update(func(c *integration.Config) { c.SFTP.HostKey = integration.AuthorizedKeyLine(otherSigner.PublicKey()) }, "")
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if m = runMeta(t, run); run.ErrorCode != integration.ErrCodeHostKey || m["host_key_changed"] != "true" || m["file"] != "" {
		t.Fatalf("changed key: %s %s meta=%v", run.Status, run.ErrorCode, m)
	}

	// 6. Newest file matching the pattern; a newer non-matching file and a
	// non-spreadsheet are ignored.
	csv := func(a int) []byte { return []byte(fmt.Sprintf("Region,Amount\nA,%d\nB,1\n", a)) }
	fx.writeFile(t, "drop/sales_0901.csv", csv(1), base)
	fx.writeFile(t, "drop/sales_0902.csv", csv(2), base.Add(2*time.Minute))
	fx.writeFile(t, "drop/sales_0903.txt", csv(3), base.Add(3*time.Minute))
	fx.writeFile(t, "drop/costs_0904.csv", csv(4), base.Add(4*time.Minute))
	update(func(c *integration.Config) {
		c.SFTP.HostKey = trusted
		c.SFTP.Select, c.SFTP.Path, c.SFTP.Folder, c.SFTP.Pattern = integration.FileNewest, "", "drop", "sales_*"
		c.SFTP.Sheet, c.SFTP.Reshape = "", nil
	}, "")
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if m = runMeta(t, run); run.Status != "success" || m["file"] != "drop/sales_0902.csv" || run.RecordsWritten != 2 || m["sheets"] != "" {
		t.Fatalf("newest: %s %s %q meta=%v", run.Status, run.ErrorCode, run.Message, m)
	}
	var attempt string
	_ = st.Pool().QueryRow(ctx, `SELECT url_sanitized FROM model.integration_attempt WHERE run_id=$1::uuid`, run.ID).Scan(&attempt)
	if attempt != "sftp://127.0.0.1/drop/sales_0902.csv" {
		t.Fatalf("attempt target %q", attempt)
	}

	// 7. Nothing matches.
	update(func(c *integration.Config) { c.SFTP.Pattern = "budget_*.xlsx" }, "")
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if run.ErrorCode != integration.ErrCodeNotFound || !strings.Contains(run.Message, "budget_*.xlsx") {
		t.Fatalf("no match: %s %s %q", run.Status, run.ErrorCode, run.Message)
	}

	// 8. Wrong password.
	if _, err := st.UpdateConnection(ctx, appID, pwConn.ID, "", "", nil,
		[]byte(fmt.Sprintf(`{"username":%q,"password":"nope"}`, sftpUser))); err != nil {
		t.Fatal(err)
	}
	update(func(c *integration.Config) { c.SFTP.Pattern = "sales_*" }, "")
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if run.ErrorCode != integration.ErrCodeAuth || strings.Contains(run.Message, "nope") {
		t.Fatalf("wrong password: %s %s %q", run.Status, run.ErrorCode, run.Message)
	}

	// 9. Key login, through an ssh_key connection.
	block, _ := ssh.MarshalPrivateKey(clientPriv, "")
	secret, _ := json.Marshal(map[string]string{"username": sftpUser, "private_key": string(pem.EncodeToMemory(block))})
	keyConn, err := st.CreateConnection(ctx, appID, "sftp key", integration.AuthTypeSSHKey, nil, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	update(func(c *integration.Config) { c.Auth.Type = integration.AuthTypeSSHKey }, keyConn.ID)
	run = claimAndRun(t, rn, st, def.ID, "manual", false)
	if m = runMeta(t, run); run.Status != "success" || m["file"] != "drop/sales_0902.csv" {
		t.Fatalf("key login: %s %s %q", run.Status, run.ErrorCode, run.Message)
	}
}
