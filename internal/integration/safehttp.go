package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// safehttp is the ONLY way connector traffic leaves the process. Every
// protection here is load-bearing; none is optional:
//
//   - absolute HTTPS URLs only in production (plain HTTP allowed solely when
//     allowInsecure, i.e. dev/test against local fixtures);
//   - no userinfo, no unusual ports, no credentials in the URL;
//   - hostname suffixes that can only mean in-cluster or mDNS targets
//     (.local, .localhost, .svc, .cluster.local, .internal) are rejected
//     before DNS is even consulted;
//   - DNS is resolved through the standard resolver ONCE, every answer is
//     checked against the forbidden ranges, and the connection dials the
//     validated IP directly (SNI/Host header keep the original name) — a
//     rebinding server that answers a public A record first and a private
//     one later never gets a second resolution to poison;
//   - redirects are re-validated hop by hop with the same rules, and
//     Authorization/Cookie never cross origins;
//   - environment proxies are ignored (a proxy would bypass the IP checks);
//   - TLS verification cannot be disabled;
//   - response bodies are hard-capped AFTER decompression.
//
// Forbidden destination ranges, across IPv4 and IPv6 (including mapped
// forms): loopback, RFC1918 private, link-local (incl. 169.254.169.254 and
// fd00::/8 cloud metadata variants), CGNAT 100.64/10, multicast, unspecified,
// and documentation/benchmark ranges.

// ErrForbiddenDestination marks SSRF-blocked targets; callers map it to the
// distinct "blocked-host" run error code.
type BlockedDestinationError struct{ Reason string }

func (e *BlockedDestinationError) Error() string { return "destination not allowed: " + e.Reason }

var forbiddenV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // unspecified/this-net
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local + cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/3"),     // multicast + reserved + broadcast
}

var forbiddenV6 = []netip.Prefix{
	netip.MustParsePrefix("::/128"),      // unspecified
	netip.MustParsePrefix("::1/128"),     // loopback
	netip.MustParsePrefix("::ffff:0:0/96"), // v4-mapped — checked as v4 too, belt and braces
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64
	netip.MustParsePrefix("100::/64"),    // discard
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("fc00::/7"),    // unique-local (incl. fd00::/8 metadata variants)
	netip.MustParsePrefix("fe80::/10"),   // link-local
	netip.MustParsePrefix("ff00::/8"),    // multicast
}

var forbiddenHostSuffixes = []string{
	".local", ".localhost", ".svc", ".cluster.local", ".internal",
}

// forbiddenHeaders can smuggle routing or identity through the connector.
var forbiddenHeaders = map[string]bool{
	"host": true, "x-forwarded-for": true, "x-forwarded-host": true,
	"x-real-ip": true, "forwarded": true, "via": true,
	"proxy-authorization": true, "transfer-encoding": true, "connection": true,
	"upgrade": true, "te": true, "trailer": true, "content-length": true,
}

func ipForbidden(ip netip.Addr) string {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if ip.Is4() {
		for _, p := range forbiddenV4 {
			if p.Contains(ip) {
				return fmt.Sprintf("address %s is in forbidden range %s", ip, p)
			}
		}
		return ""
	}
	for _, p := range forbiddenV6 {
		if p.Contains(ip) {
			return fmt.Sprintf("address %s is in forbidden range %s", ip, p)
		}
	}
	return ""
}

// ValidateURL enforces the static (pre-DNS) rules. Returned host is lowercase.
func ValidateURL(raw string, allowInsecure bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &BlockedDestinationError{Reason: "URL does not parse"}
	}
	if !u.IsAbs() {
		return nil, &BlockedDestinationError{Reason: "URL must be absolute"}
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure {
			return nil, &BlockedDestinationError{Reason: "only https destinations are allowed"}
		}
	default:
		return nil, &BlockedDestinationError{Reason: "scheme " + u.Scheme + " is not allowed"}
	}
	if u.User != nil {
		return nil, &BlockedDestinationError{Reason: "userinfo in URL is not allowed"}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, &BlockedDestinationError{Reason: "empty host"}
	}
	if strings.Contains(raw, "{{") && strings.Contains(strings.SplitN(raw, "?", 2)[0], "{{") {
		// Variables are allowed in query/body only; a template in the
		// scheme/host/path portion defeats static origin verification.
		beforeQuery := strings.SplitN(raw, "?", 2)[0]
		if idx := strings.Index(beforeQuery, "{{"); idx >= 0 {
			// Allow templates in the PATH (after the host). Find where the
			// host ends: scheme://host[:port]/
			hostEnd := strings.Index(beforeQuery, "://")
			if hostEnd >= 0 {
				rest := beforeQuery[hostEnd+3:]
				slash := strings.Index(rest, "/")
				if slash < 0 || idx < hostEnd+3+slash {
					return nil, &BlockedDestinationError{Reason: "template variables are not allowed in the URL scheme or host"}
				}
			}
		}
	}
	if host != "localhost" { // caught below anyway; explicit for clarity
		for _, suf := range forbiddenHostSuffixes {
			if strings.HasSuffix(host, suf) {
				return nil, &BlockedDestinationError{Reason: "hostname suffix " + suf + " is not allowed"}
			}
		}
	}
	if host == "localhost" && !allowInsecure {
		return nil, &BlockedDestinationError{Reason: "localhost is not allowed"}
	}
	if p := u.Port(); p != "" && p != "443" && p != "80" && !allowInsecure {
		return nil, &BlockedDestinationError{Reason: "port " + p + " is not allowed (443/80 only)"}
	}
	// Literal IP hosts skip DNS but not the range checks. legacyIPAddr also
	// catches inet_aton shorthand/octal/hex spellings ("127.1", "0x7f000001",
	// "017700000001") that netip refuses but real resolvers happily expand.
	bare := strings.Trim(host, "[]")
	ip, perr := netip.ParseAddr(bare)
	if perr != nil {
		legacy, ok := legacyIPAddr(bare)
		if !ok {
			return u, nil // a genuine hostname; the dial hook validates its resolution
		}
		ip = legacy
	}
	if allowInsecure && (ip.IsLoopback() || ip.IsPrivate()) {
		return u, nil // test fixtures listen on 127.0.0.1
	}
	if reason := ipForbidden(ip); reason != "" {
		return nil, &BlockedDestinationError{Reason: reason}
	}
	return u, nil
}

// legacyIPAddr interprets C inet_aton notations: 1–4 dot-separated parts,
// each decimal, octal (leading 0) or hex (0x); missing parts fold into the
// last one. "127.1" → 127.0.0.1, "0x7f000001" → 127.0.0.1.
func legacyIPAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, 0, 4)
	for _, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		var n uint64
		var err error
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			_, err = fmt.Sscanf(strings.ToLower(p), "0x%x", &n)
		case len(p) > 1 && p[0] == '0':
			_, err = fmt.Sscanf(p, "%o", &n)
		default:
			_, err = fmt.Sscanf(p, "%d", &n)
		}
		if err != nil {
			return netip.Addr{}, false
		}
		nums = append(nums, n)
	}
	var v uint64
	switch len(nums) {
	case 1:
		v = nums[0]
	case 2:
		if nums[0] > 255 || nums[1] > 0xFFFFFF {
			return netip.Addr{}, false
		}
		v = nums[0]<<24 | nums[1]
	case 3:
		if nums[0] > 255 || nums[1] > 255 || nums[2] > 0xFFFF {
			return netip.Addr{}, false
		}
		v = nums[0]<<24 | nums[1]<<16 | nums[2]
	case 4:
		for _, n := range nums {
			if n > 255 {
				return netip.Addr{}, false
			}
		}
		v = nums[0]<<24 | nums[1]<<16 | nums[2]<<8 | nums[3]
	}
	if v > 0xFFFFFFFF {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// Limits bounds one connector request/run at the transport level.
type Limits struct {
	RequestTimeout  time.Duration // per attempt; default 30s
	MaxResponseSize int64         // decompressed; default 16MB
	MaxRedirects    int           // default 5
}

func (l *Limits) withDefaults() Limits {
	out := Limits{RequestTimeout: 30 * time.Second, MaxResponseSize: 16 << 20, MaxRedirects: 5}
	if l != nil {
		if l.RequestTimeout > 0 {
			out.RequestTimeout = l.RequestTimeout
		}
		if l.MaxResponseSize > 0 {
			out.MaxResponseSize = l.MaxResponseSize
		}
		if l.MaxRedirects > 0 {
			out.MaxRedirects = l.MaxRedirects
		}
	}
	return out
}

// NewSafeClient builds the pinned-dial HTTP client. allowInsecure loosens
// scheme/loopback rules for tests and local development ONLY — the worker
// sets it from an env var that production never defines.
func NewSafeClient(limits *Limits, allowInsecure bool) *http.Client {
	l := limits.withDefaults()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // NEVER honor environment proxies — they bypass IP validation
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Literal IPs were validated in ValidateURL; names resolve here,
			// get validated, and the dial goes to the validated IP — the
			// single-resolution property that defeats rebinding.
			if ip, perr := netip.ParseAddr(host); perr == nil {
				if !allowInsecure {
					if reason := ipForbidden(ip); reason != "" {
						return nil, &BlockedDestinationError{Reason: reason}
					}
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
			}
			// One unsafe answer poisons the whole set: a mixed response is
			// exactly what a rebinding/split-horizon attack looks like, so
			// every address is validated before the first one is dialed.
			if !allowInsecure {
				for _, ip := range ips {
					if reason := ipForbidden(ip); reason != "" {
						return nil, &BlockedDestinationError{Reason: reason + " (resolved from " + host + ")"}
					}
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: l.RequestTimeout,
		// TLSClientConfig deliberately left nil: full verification, no knob.
	}
	return &http.Client{
		Timeout:   l.RequestTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > l.MaxRedirects {
				return &BlockedDestinationError{Reason: fmt.Sprintf("more than %d redirects", l.MaxRedirects)}
			}
			if _, err := ValidateURL(req.URL.String(), allowInsecure); err != nil {
				return err
			}
			// Never forward credentials across origins.
			prev := via[len(via)-1]
			if req.URL.Host != prev.URL.Host || req.URL.Scheme != prev.URL.Scheme {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
				req.Header.Del("Proxy-Authorization")
			}
			return nil
		},
	}
}

// CheckHeaderSafe rejects header names the connector must not let a
// configuration set, and CRLF/control characters in names or values.
func CheckHeaderSafe(name, value string) error {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return fmt.Errorf("empty header name")
	}
	if forbiddenHeaders[n] {
		return fmt.Errorf("header %q is not allowed", name)
	}
	for _, s := range []string{name, value} {
		for _, r := range s {
			if r == '\r' || r == '\n' || r == 0 {
				return fmt.Errorf("control characters are not allowed in headers")
			}
		}
	}
	return nil
}
