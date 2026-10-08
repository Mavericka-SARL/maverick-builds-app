package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

func TestValidateSFTPHost(t *testing.T) {
	cases := []struct {
		host     string
		insecure bool
		want     string // dial address; "" = refused
	}{
		{"sftp.example.com", false, "sftp.example.com:22"},
		{"sftp.example.com:22", false, "sftp.example.com:22"},
		{"  sftp.example.com ", false, "sftp.example.com:22"},
		{"[2606:4700::1111]", false, "[2606:4700::1111]:22"},
		{"2606:4700::1111", false, "[2606:4700::1111]:22"},
		{"sftp.example.com:2222", false, ""},
		{"sftp.example.com:443", false, ""},
		{"sftp.example.com:", false, ""},
		{"sftp://sftp.example.com", false, ""},
		{"user@sftp.example.com", false, ""},
		{"sftp.example.com/exports", false, ""},
		{"", false, ""},
		{"10.0.0.5", false, ""},
		{"169.254.169.254", false, ""},
		{"localhost", false, ""},
		{"files.svc.cluster.local", false, ""},
		{"nas.local", false, ""},
		{"0x7f000001", false, ""},
		// Local fixtures only: a port and loopback.
		{"127.0.0.1:2222", true, "127.0.0.1:2222"},
		{"sftp.example.com:2222", true, "sftp.example.com:2222"},
	}
	for _, c := range cases {
		got, err := ValidateSFTPHost(c.host, c.insecure)
		if c.want == "" {
			var blocked *BlockedDestinationError
			if err == nil || !errors.As(err, &blocked) {
				t.Errorf("%q (insecure=%v): want a blocked destination, got %q, %v", c.host, c.insecure, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q (insecure=%v): got %q, %v; want %q", c.host, c.insecure, got, err, c.want)
		}
	}
}

func sftpConfig(mutate func(*Config)) *Config {
	c := &Config{
		Kind: ConfigKind, Protocol: ProtocolSFTP, Direction: DirectionPull,
		TargetType: TargetGrid, TargetID: "g", ImportMode: ModeIncremental,
		SFTP:    &SFTPSource{Host: "sftp.example.com", Select: FileFixed, Path: "/exports/sales.xlsx"},
		Auth:    AuthPlacement{Type: "basic"},
		Mapping: MappingConfig{Fields: []FieldMap{{Source: "$.Region", Target: "region"}}},
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestValidate_SFTP(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	hostKey := AuthorizedKeyLine(signer.PublicKey())

	ok := []struct {
		name   string
		mutate func(*Config)
	}{
		{"fixed path, password", nil},
		{"fixed path, ssh key", func(c *Config) { c.Auth.Type = AuthTypeSSHKey }},
		{"pinned host key", func(c *Config) { c.SFTP.HostKey = hostKey }},
		{"workbook sheet + reshape", func(c *Config) {
			c.SFTP.Sheet = "Data"
			c.SFTP.Reshape = &importpkg.Reshape{HeaderRow: 2}
		}},
		{"newest in folder", func(c *Config) {
			c.SFTP.Select, c.SFTP.Path, c.SFTP.Folder, c.SFTP.Pattern = FileNewest, "", "/exports", "sales_*.xlsx"
		}},
		{"newest in the login folder", func(c *Config) {
			c.SFTP.Select, c.SFTP.Path, c.SFTP.Pattern = FileNewest, "", "*.csv"
		}},
		// Request/response/pagination are not used by an SFTP run.
		{"stale https fields ignored", func(c *Config) {
			c.Request = RequestConfig{Method: "GET", URL: "https://api.example.com"}
			c.Pagination = PaginationConfig{Mode: PageCursor}
		}},
	}
	for _, c := range ok {
		if err := sftpConfig(c.mutate).Validate(false); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	bad := []struct {
		name, want string
		mutate     func(*Config)
	}{
		{"no sftp block", "sftp settings are required", func(c *Config) { c.SFTP = nil }},
		{"push", "pull", func(c *Config) { c.Direction = DirectionPush }},
		{"port", "22 only", func(c *Config) { c.SFTP.Host = "sftp.example.com:2222" }},
		{"private address", "forbidden", func(c *Config) { c.SFTP.Host = "192.168.1.10" }},
		{"bad host key", "host_key", func(c *Config) { c.SFTP.HostKey = "not a key" }},
		{"two host keys", "one key only", func(c *Config) { c.SFTP.HostKey = hostKey + "\n" + hostKey }},
		{"no path", "path", func(c *Config) { c.SFTP.Path = "" }},
		{"not a spreadsheet", ".csv, .xlsx or .xlsm", func(c *Config) { c.SFTP.Path = "/exports/sales.txt" }},
		{"sheet on csv", "sheet applies", func(c *Config) { c.SFTP.Path, c.SFTP.Sheet = "/x/sales.csv", "Data" }},
		{"fixed with pattern", "newest", func(c *Config) { c.SFTP.Pattern = "*.csv" }},
		{"control character", "control", func(c *Config) { c.SFTP.Path = "/x/a\nb.csv" }},
		{"newest without pattern", "pattern", func(c *Config) { c.SFTP.Select, c.SFTP.Path = FileNewest, "" }},
		{"pattern with folder", "pattern", func(c *Config) {
			c.SFTP.Select, c.SFTP.Path, c.SFTP.Pattern = FileNewest, "", "exports/*.csv"
		}},
		{"malformed pattern", "pattern", func(c *Config) { c.SFTP.Select, c.SFTP.Path, c.SFTP.Pattern = FileNewest, "", "[a" }},
		{"newest with path", "fixed", func(c *Config) { c.SFTP.Select, c.SFTP.Pattern = FileNewest, "*.csv" }},
		{"unknown select", "select", func(c *Config) { c.SFTP.Select = "oldest" }},
		{"bad reshape", "reshape", func(c *Config) { c.SFTP.Reshape = &importpkg.Reshape{Delimiter: "x"} }},
		{"bearer auth", "password (basic) or an SSH key", func(c *Config) { c.Auth.Type = "bearer" }},
		{"no auth", "password (basic) or an SSH key", func(c *Config) { c.Auth.Type = "none" }},
		{"unknown protocol", "protocol", func(c *Config) { c.Protocol = "ftp" }},
	}
	for _, c := range bad {
		err := sftpConfig(c.mutate).Validate(false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}

	// The HTTPS side refuses SFTP-only pieces.
	https := &Config{
		Kind: ConfigKind, Direction: DirectionPull, TargetType: TargetGrid, TargetID: "g",
		ImportMode: ModeIncremental,
		Request:    RequestConfig{Method: "GET", URL: "https://api.example.com/v1", BodyMode: BodyNone},
		Auth:       AuthPlacement{Type: AuthTypeSSHKey},
		Response:   ResponseConfig{Format: FormatJSON},
	}
	if err := https.Validate(false); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("https + ssh_key: %v", err)
	}
	https.Auth.Type = "none"
	https.SFTP = &SFTPSource{Host: "sftp.example.com"}
	if err := https.Validate(false); err == nil || !strings.Contains(err.Error(), "sftp protocol only") {
		t.Errorf("https + sftp block: %v", err)
	}
}

// TestConfigHash_HTTPSUnchanged: adding the protocol fields must not change
// any existing HTTPS integration's hash (it would mark every tested
// integration untested).
func TestConfigHash_HTTPSUnchanged(t *testing.T) {
	c := &Config{
		Kind: ConfigKind, Direction: DirectionPull, TargetType: TargetGrid, TargetID: "g",
		Request:    RequestConfig{Method: "GET", URL: "https://api.example.com/v1"},
		Auth:       AuthPlacement{Type: "none"},
		Response:   ResponseConfig{Format: FormatJSON, RecordsPath: "$.data"},
		Pagination: PaginationConfig{Mode: PageNone},
	}
	before := struct {
		Direction  Direction        `json:"d"`
		TargetType TargetType       `json:"tt"`
		TargetID   string           `json:"t"`
		Request    RequestConfig    `json:"r"`
		Auth       AuthPlacement    `json:"a"`
		Response   ResponseConfig   `json:"re"`
		Pagination PaginationConfig `json:"p"`
	}{c.Direction, c.TargetType, c.TargetID, c.Request, c.Auth, c.Response, c.Pagination}
	raw, _ := json.Marshal(before)
	sum := sha256.Sum256(raw)
	if got, want := ConfigHash(c), hex.EncodeToString(sum[:8]); got != want {
		t.Fatalf("HTTPS hash changed: %s, was %s", got, want)
	}
	// An SFTP change (a newly trusted host key) does invalidate the test.
	s := sftpConfig(nil)
	h1 := ConfigHash(s)
	s.SFTP.HostKey = "ssh-ed25519 AAAA"
	if ConfigHash(s) == h1 {
		t.Fatal("trusting a host key must change the hash")
	}
}

func TestParseSSHPrivateKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	plain, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	locked, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	plainPEM, lockedPEM := string(pem.EncodeToMemory(plain)), string(pem.EncodeToMemory(locked))

	if _, err := ParseSSHPrivateKey(plainPEM, ""); err != nil {
		t.Errorf("plain key: %v", err)
	}
	if _, err := ParseSSHPrivateKey(lockedPEM, "s3cret"); err != nil {
		t.Errorf("encrypted key with passphrase: %v", err)
	}
	for _, c := range []struct{ key, pass, want string }{
		{lockedPEM, "", "encrypted"},
		{lockedPEM, "wrong", "passphrase is wrong"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----", "", "does not parse"},
		{"", "", "no private key"},
	} {
		_, err := ParseSSHPrivateKey(c.key, c.pass)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "AAAA") {
			t.Errorf("error quotes the key: %v", err)
		}
	}
}
