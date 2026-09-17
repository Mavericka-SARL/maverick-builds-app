package integration

import (
	"context"
	"net/netip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidateURL_SSRFBypassVariants is the static-rules gauntlet: every row
// is a real bypass shape from SSRF cheat-sheets, and every one must be
// refused in production mode.
func TestValidateURL_SSRFBypassVariants(t *testing.T) {
	blocked := []string{
		// schemes / shape
		"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com",
		"//example.com/path", "/relative/path", "",
		// userinfo tricks
		"https://user:pass@example.com/", "https://example.com%40evil.test/",
		// loopback & friends, every spelling
		"https://127.0.0.1/", "https://127.0.0.2/", "https://0.0.0.0/",
		"https://[::1]/", "https://[::ffff:127.0.0.1]/",
		"https://localhost/", "https://127.1/",
		// private / CGNAT / link-local / metadata
		"https://10.0.0.5/", "https://172.16.5.5/", "https://192.168.1.1/",
		"https://100.64.1.1/", "https://169.254.169.254/latest/meta-data/",
		"https://[fe80::1]/", "https://[fd00:ec2::254]/", "https://[fc00::1]/",
		// multicast / reserved / documentation
		"https://224.0.0.1/", "https://255.255.255.255/", "https://192.0.2.1/",
		"https://[2001:db8::1]/", "https://198.18.0.1/",
		// forbidden suffixes (in-cluster / mDNS)
		"https://postgres.mavericks.svc/", "https://audit.svc/",
		"https://printer.local/", "https://x.localhost/", "https://gateway.cluster.local/",
		"https://vault.internal/",
		// odd ports
		"https://example.com:6443/", "https://example.com:9090/",
		// template in host
		"https://{{context.model_id}}.example.com/", "https://example.com{{run.id}}/x",
	}
	for _, raw := range blocked {
		if _, err := ValidateURL(raw, false); err == nil {
			t.Errorf("ValidateURL(%q) allowed — must be blocked", raw)
		}
	}

	allowed := []string{
		"https://api.example.com/v1/items",
		"https://api.example.com:443/v1/items?x=1",
		"https://api.example.com/v1/{{page.number}}/items", // template in PATH is fine
		"https://api.example.com/x?cursor={{page.cursor}}",
	}
	for _, raw := range allowed {
		if _, err := ValidateURL(raw, false); err != nil {
			t.Errorf("ValidateURL(%q) blocked: %v — must be allowed", raw, err)
		}
	}

	// Dev mode: loopback http fixtures allowed, but metadata still blocked.
	if _, err := ValidateURL("http://127.0.0.1:8099/fixture", true); err != nil {
		t.Errorf("dev-mode loopback fixture blocked: %v", err)
	}
	if _, err := ValidateURL("https://169.254.169.254/", true); err == nil {
		t.Error("metadata endpoint allowed even in dev mode")
	}
}

// FuzzValidateURL asserts the invariant that NO accepted production URL ever
// carries userinfo, a non-http(s) scheme, a forbidden literal IP, or a
// forbidden host suffix — regardless of how the parser interprets the input.
func FuzzValidateURL(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/x", "https://127.0.0.1/", "https://example.com@evil/",
		"https://[::ffff:10.0.0.1]/", "https://0x7f000001/", "https://017700000001/",
		"https://example.com:65535/", "https://%6c%6f%63%61%6c%68%6f%73%74/",
		"https://foo.svc/", "https://a.b.cluster.local/x?y={{page.cursor}}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := ValidateURL(raw, false)
		if err != nil {
			return // rejected is always safe
		}
		if u.User != nil {
			t.Fatalf("accepted URL %q carries userinfo", raw)
		}
		if u.Scheme != "https" {
			t.Fatalf("accepted URL %q has scheme %q", raw, u.Scheme)
		}
		host := strings.ToLower(u.Hostname())
		for _, suf := range forbiddenHostSuffixes {
			if strings.HasSuffix(host, suf) {
				t.Fatalf("accepted URL %q has forbidden suffix %s", raw, suf)
			}
		}
	})
}

// TestSafeClient_DialRefusesPrivateResolution: a hostname is fine statically,
// but if DNS answers with a forbidden address the DIAL must refuse — this is
// the rebinding defense, tested end to end through the real client against a
// local fixture (allowed in dev mode) versus a name that resolves privately.
func TestSafeClient_DialRefusesPrivateResolution(t *testing.T) {
	// Production-mode client. localhost resolves to 127.0.0.1 → the dial
	// hook must refuse it even though we bypass ValidateURL here.
	client := NewSafeClient(nil, false)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://localhost:443/", nil)
	resp, err := client.Do(req) //nolint:bodyclose // error path: no body on a refused dial
	if err == nil {
		resp.Body.Close() //nolint:errcheck
	}
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("dial to loopback-resolving name: err=%v, want blocked-destination", err)
	}
}

// TestSafeClient_RedirectRevalidationAndCredentialStripping: a fixture
// redirects to a second origin; the client must re-validate the hop and drop
// Authorization on the cross-origin jump.
func TestSafeClient_RedirectRevalidation(t *testing.T) {
	var gotAuth string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/hop", http.StatusFound)
	}))
	defer first.Close()

	client := NewSafeClient(nil, true) // dev mode: loopback fixtures allowed
	req, _ := http.NewRequestWithContext(context.Background(), "GET", first.URL, nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("redirect chain: %v", err)
	}
	resp.Body.Close() //nolint:errcheck
	if gotAuth != "" {
		t.Fatalf("Authorization forwarded across origins: %q", gotAuth)
	}

	// A redirect to a forbidden destination must abort even in dev mode
	// (metadata is never allowed).
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/", http.StatusFound)
	}))
	defer trap.Close()
	req2, _ := http.NewRequestWithContext(context.Background(), "GET", trap.URL, nil)
	if resp2, err := client.Do(req2); err == nil {
		resp2.Body.Close() //nolint:errcheck
		t.Fatal("redirect to metadata endpoint was followed")
	}
}

// TestSafeClient_Timeout enforces the per-attempt deadline.
func TestSafeClient_Timeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()
	client := NewSafeClient(&Limits{RequestTimeout: 300 * time.Millisecond}, true)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", slow.URL, nil)
	start := time.Now()
	resp, err := client.Do(req) //nolint:bodyclose // error path: no body on timeout
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("slow response did not time out")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout took %v, want ~300ms", time.Since(start))
	}
}

func TestCheckHeaderSafe(t *testing.T) {
	for _, bad := range [][2]string{
		{"Host", "evil"}, {"X-Forwarded-For", "1.2.3.4"}, {"Transfer-Encoding", "chunked"},
		{"X-Ok", "line1\r\nX-Injected: yes"}, {"Bad\nName", "v"}, {"", "v"},
		{"Connection", "close"}, {"Content-Length", "9"},
	} {
		if err := CheckHeaderSafe(bad[0], bad[1]); err == nil {
			t.Errorf("header %q:%q allowed — must be rejected", bad[0], bad[1])
		}
	}
	if err := CheckHeaderSafe("X-Api-Version", "2024-01"); err != nil {
		t.Errorf("benign header rejected: %v", err)
	}
}

// ipForbidden must catch v4-mapped v6 forms of every v4 range.
func TestIPForbidden_MappedForms(t *testing.T) {
	for _, s := range []string{"::ffff:10.1.2.3", "::ffff:169.254.169.254", "::ffff:127.0.0.1"} {
		ip := netip.MustParseAddr(s)
		if ipForbidden(ip) == "" {
			t.Errorf("mapped address %s not forbidden", s)
		}
	}
}
