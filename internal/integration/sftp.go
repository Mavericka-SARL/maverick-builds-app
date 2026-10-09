package integration

// SFTP source: a pull that reads one spreadsheet file (.csv, .xlsx, .xlsm)
// from an SFTP server instead of calling an HTTPS API. The connection takes
// the same destination rules as safehttp — names resolve once, every answer
// is checked against the forbidden ranges, the dial goes to the checked
// address — on port 22 only, and a file is read only from a server whose
// host key the developer pinned. The file is left where it is.

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// AuthTypeSSHKey authenticates an SFTP connection with a private key; the
// secret holds username, private_key (PEM/OpenSSH) and an optional
// passphrase. A password login over SFTP is the "basic" type.
const AuthTypeSSHKey = "ssh_key"

const sftpPort = "22"

// sftpSessionTimeout bounds one whole SFTP session: connect, handshake,
// listing and download.
const sftpSessionTimeout = 2 * time.Minute

// Run error codes only an SFTP pull produces.
const (
	ErrCodeHostKey  = "host_key"  // untrusted or changed server key
	ErrCodeNotFound = "not_found" // no file at the path / none matching
)

// HostKeyError stops a session whose server key is not the pinned one.
// Presented is what the server offered, reported so the developer can
// compare its fingerprint and trust it.
type HostKeyError struct {
	Presented ssh.PublicKey
	Changed   bool // a key was pinned and the server presented another
}

func (e *HostKeyError) Error() string {
	key := HostKeyLabel(e.Presented)
	if e.Changed {
		return "the server's " + key + " does not match the trusted key: the server was re-keyed, or the connection is being intercepted"
	}
	return "the server's " + key + " is not trusted yet: compare it with the server's, trust it, then test again"
}

// HostKeyLabel names a host key the way an administrator compares it:
// its type as ssh-keygen -l prints it, and its SHA256 fingerprint.
func HostKeyLabel(key ssh.PublicKey) string {
	kind := strings.ToUpper(strings.TrimPrefix(key.Type(), "ssh-"))
	switch {
	case strings.HasPrefix(key.Type(), "ecdsa-"):
		kind = "ECDSA"
	case key.Type() == ssh.KeyAlgoRSA:
		kind = "RSA"
	}
	return kind + " host key " + ssh.FingerprintSHA256(key)
}

// FileNotFoundError is a missing file, or a folder with no matching file.
type FileNotFoundError struct{ Reason string }

func (e *FileNotFoundError) Error() string { return e.Reason }

// ValidateSFTPHost checks an SFTP host with the rules ValidateURL applies to
// a URL's host and returns the address to dial. Only port 22; a host:port
// is accepted solely when allowInsecure (local fixtures).
func ValidateSFTPHost(host string, allowInsecure bool) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", &BlockedDestinationError{Reason: "empty host"}
	}
	if strings.ContainsAny(host, "/@?#\\ \t") {
		return "", &BlockedDestinationError{Reason: "the host is a server name or address only: no scheme, user name, path or spaces"}
	}
	name, port := host, sftpPort
	if h, p, err := net.SplitHostPort(host); err == nil {
		if p == "" {
			return "", &BlockedDestinationError{Reason: "host does not parse"}
		}
		name, port = h, p
	}
	name = strings.Trim(name, "[]")
	if port != sftpPort && !allowInsecure {
		return "", &BlockedDestinationError{Reason: "port " + port + " is not allowed (22 only)"}
	}
	if _, err := ValidateURL("https://"+net.JoinHostPort(name, "443")+"/", allowInsecure); err != nil {
		return "", err
	}
	return net.JoinHostPort(name, port), nil
}

// sftpHostName is the host without a port, for run history.
func sftpHostName(host string) string {
	if h, _, err := net.SplitHostPort(strings.TrimSpace(host)); err == nil {
		return h
	}
	return strings.Trim(strings.TrimSpace(host), "[]")
}

// ParseHostKey reads a pinned host key in authorized_keys form.
func ParseHostKey(s string) (ssh.PublicKey, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(s)))
	if err != nil {
		return nil, fmt.Errorf("not a public key in authorized_keys form (\"ssh-ed25519 AAAA…\")")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("one key only")
	}
	return key, nil
}

// AuthorizedKeyLine renders a key the way HostKey stores it.
func AuthorizedKeyLine(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// ParseSSHPrivateKey reads a connection's private key. Errors never quote
// the key.
func ParseSSHPrivateKey(pemText, passphrase string) (ssh.Signer, error) {
	if strings.TrimSpace(pemText) == "" {
		return nil, fmt.Errorf("the connection has no private key")
	}
	var signer ssh.Signer
	var err error
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(pemText), []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(pemText))
	}
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, fmt.Errorf("the private key is encrypted: add its passphrase to the connection")
	case errors.Is(err, x509.IncorrectPasswordError):
		return nil, fmt.Errorf("the private key's passphrase is wrong")
	case err != nil:
		return nil, fmt.Errorf("the private key does not parse (PEM or OpenSSH format expected)")
	}
	return signer, nil
}

// sshAuthMethods turns a connection's secret into SSH logins.
func sshAuthMethods(authType string, secret map[string]string) ([]ssh.AuthMethod, error) {
	if secret["username"] == "" {
		return nil, fmt.Errorf("the connection has no username")
	}
	switch authType {
	case "basic":
		pw := secret["password"]
		// Servers that disable "password" but prompt once for it through
		// keyboard-interactive get the same password; any other prompt
		// (a second factor) is refused, not answered with it.
		prompt := func(_, _ string, questions []string, _ []bool) ([]string, error) {
			if len(questions) > 1 {
				return nil, fmt.Errorf("the server asks %d questions; only a password login is supported", len(questions))
			}
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = pw
			}
			return answers, nil
		}
		return []ssh.AuthMethod{ssh.Password(pw), ssh.KeyboardInteractive(prompt)}, nil
	case AuthTypeSSHKey:
		signer, err := ParseSSHPrivateKey(secret["private_key"], secret["passphrase"])
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	return nil, fmt.Errorf("sftp signs in with a password (basic) or an SSH key (ssh_key), not %s", authType)
}

// hostKeyAlgorithms asks the server for the pinned key's own type, so a
// server holding several keys presents the one that was trusted. Before a
// key is pinned, Ed25519 comes first: it is the key administrators most
// often publish (Go's default order would present ECDSA).
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key == nil {
		return []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{key.Type()}
}

// validateSFTP checks an SFTP pull: the server, the file choice, and the
// sign-in. Request, Response and Pagination do not apply and are ignored.
func (c *Config) validateSFTP(allowInsecure bool) error {
	s := c.SFTP
	if s == nil {
		return fmt.Errorf("sftp settings are required for the sftp protocol")
	}
	if c.Model != nil {
		return fmt.Errorf("model settings apply to the model protocol only")
	}
	if c.Direction != DirectionPull {
		return fmt.Errorf("sftp reads files only (direction pull)")
	}
	if _, err := ValidateSFTPHost(s.Host, allowInsecure); err != nil {
		return err
	}
	if s.HostKey != "" {
		if _, err := ParseHostKey(s.HostKey); err != nil {
			return fmt.Errorf("host_key: %w", err)
		}
	}
	switch s.Select {
	case FileFixed:
		if err := checkRemotePath(s.Path, true); err != nil {
			return fmt.Errorf("path: %w", err)
		}
		if !importpkg.IsTabularFile(s.Path) {
			return fmt.Errorf("path must name a .csv, .xlsx or .xlsm file")
		}
		if s.Sheet != "" && strings.EqualFold(filepath.Ext(s.Path), ".csv") {
			return fmt.Errorf("sheet applies to a workbook, not a .csv file")
		}
		if s.Folder != "" || s.Pattern != "" {
			return fmt.Errorf("folder and pattern apply to select \"newest\" only")
		}
	case FileNewest:
		if err := checkRemotePath(s.Folder, false); err != nil {
			return fmt.Errorf("folder: %w", err)
		}
		if strings.TrimSpace(s.Pattern) == "" || strings.Contains(s.Pattern, "/") {
			return fmt.Errorf("pattern is a file-name pattern such as sales_*.xlsx")
		}
		if _, err := path.Match(s.Pattern, ""); err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
		if s.Path != "" {
			return fmt.Errorf("path applies to select \"fixed\" only")
		}
	default:
		return fmt.Errorf("select must be fixed or newest")
	}
	if err := s.Reshape.Validate(); err != nil {
		return fmt.Errorf("reshape: %w", err)
	}
	switch c.Auth.Type {
	case "basic", AuthTypeSSHKey:
	default:
		return fmt.Errorf("sftp signs in with a password (basic) or an SSH key (ssh_key)")
	}
	return nil
}

func checkRemotePath(p string, required bool) error {
	if strings.TrimSpace(p) == "" {
		if required {
			return fmt.Errorf("required")
		}
		return nil
	}
	if len(p) > 1024 {
		return fmt.Errorf("longer than 1024 characters")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

// remoteFile is the file an SFTP run read.
type remoteFile struct {
	Path     string
	Size     int64
	Modified time.Time
	Data     []byte
}

// fetchSFTPFile connects, checks the host key, signs in, picks the file and
// downloads it (at most maxSize bytes).
func fetchSFTPFile(ctx context.Context, src *SFTPSource, authType string, secret map[string]string, maxSize int64, allowInsecure bool) (*remoteFile, error) {
	addr, err := ValidateSFTPHost(src.Host, allowInsecure)
	if err != nil {
		return nil, err
	}
	auth, err := sshAuthMethods(authType, secret)
	if err != nil {
		return nil, err
	}
	var pinned ssh.PublicKey
	if src.HostKey != "" {
		if pinned, err = ParseHostKey(src.HostKey); err != nil {
			return nil, fmt.Errorf("host_key: %w", err)
		}
	}
	cfg := &ssh.ClientConfig{
		User: secret["username"],
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if pinned == nil {
				return &HostKeyError{Presented: key}
			}
			if !bytes.Equal(key.Marshal(), pinned.Marshal()) {
				return &HostKeyError{Presented: key, Changed: true}
			}
			return nil
		},
	}
	cfg.HostKeyAlgorithms = hostKeyAlgorithms(pinned)

	ctx, cancel := context.WithTimeout(ctx, sftpSessionTimeout)
	defer cancel()
	conn, err := safeDialContext(&net.Dialer{Timeout: 10 * time.Second}, allowInsecure)(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck
	// SSH and SFTP calls take no context: the deadline bounds the whole
	// session, and cancellation closes the connection under them.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close() //nolint:errcheck
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("the server does not offer SFTP: %w", err)
	}
	defer sc.Close() //nolint:errcheck

	name, info, err := pickRemoteFile(sc, src)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSize {
		return nil, &fileTooLargeError{name: name, max: maxSize}
	}
	f, err := sc.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(data)) > maxSize {
		return nil, &fileTooLargeError{name: name, max: maxSize}
	}
	return &remoteFile{Path: name, Size: int64(len(data)), Modified: info.ModTime(), Data: data}, nil
}

type fileTooLargeError struct {
	name string
	max  int64
}

func (e *fileTooLargeError) Error() string {
	return fmt.Sprintf("%s is larger than %d MB", e.name, e.max>>20)
}

// pickRemoteFile resolves Select: the fixed path, or the most recently
// modified regular .csv/.xlsx/.xlsm file in Folder whose name matches
// Pattern (a tie goes to the later name).
func pickRemoteFile(sc *sftp.Client, src *SFTPSource) (string, os.FileInfo, error) {
	if src.Select == FileFixed {
		info, err := sc.Stat(src.Path)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, &FileNotFoundError{Reason: "no file at " + src.Path}
		}
		if err != nil {
			return "", nil, fmt.Errorf("stat %s: %w", src.Path, err)
		}
		if !info.Mode().IsRegular() {
			return "", nil, &FileNotFoundError{Reason: src.Path + " is not a file"}
		}
		return src.Path, info, nil
	}
	folder := src.Folder
	if folder == "" {
		folder = "."
	}
	entries, err := sc.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, &FileNotFoundError{Reason: "no folder " + folder}
	}
	if err != nil {
		return "", nil, fmt.Errorf("list %s: %w", folder, err)
	}
	var best os.FileInfo
	for _, e := range entries {
		if !e.Mode().IsRegular() || !importpkg.IsTabularFile(e.Name()) {
			continue
		}
		if ok, _ := path.Match(src.Pattern, e.Name()); !ok {
			continue
		}
		if best == nil || e.ModTime().After(best.ModTime()) ||
			(e.ModTime().Equal(best.ModTime()) && e.Name() > best.Name()) {
			best = e
		}
	}
	if best == nil {
		return "", nil, &FileNotFoundError{Reason: "no .csv, .xlsx or .xlsm file in " + folder + " matches " + src.Pattern}
	}
	return path.Join(folder, best.Name()), best, nil
}

// classifySFTPErr maps an SFTP session failure onto the run error codes.
func classifySFTPErr(err error) string {
	var hk *HostKeyError
	var nf *FileNotFoundError
	var big *fileTooLargeError
	switch {
	case errors.As(err, &hk):
		return ErrCodeHostKey
	case errors.As(err, &nf):
		return ErrCodeNotFound
	case errors.As(err, &big):
		return ErrCodeTooLarge
	case errors.Is(err, os.ErrPermission),
		strings.Contains(err.Error(), "unable to authenticate"),
		strings.Contains(err.Error(), "the connection has no"),
		strings.Contains(err.Error(), "private key"):
		return ErrCodeAuth
	}
	return classifyErr(err)
}
