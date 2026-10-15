package neverbounce

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults and limits.
const (
	// DefaultTimeout bounds each JSON call, including reading the whole
	// response body. WithTimeout accepts more than 0 and at most 10 minutes.
	DefaultTimeout = 60 * time.Second

	// DefaultMaxResponseBytes limits a JSON response body. WithMaxResponseBytes
	// accepts 1 byte to 64 MiB. Every operation except jobs/results is also
	// capped at 1 MiB, far above any documented answer.
	DefaultMaxResponseBytes = 8 << 20

	// DefaultDownloadTimeout bounds a whole DownloadJob call, streaming
	// included. WithDownloadTimeout accepts more than 0 and at most 12 hours.
	DefaultDownloadTimeout = 30 * time.Minute

	// DefaultMaxDownloadBytes limits the bytes DownloadJob copies to its
	// writer. WithMaxDownloadBytes accepts 1 byte to 64 GiB.
	DefaultMaxDownloadBytes = 1 << 30

	// MaxJobRequestBytes is the provider's 25 MB jobs/create limit, read as
	// 25,000,000 bytes (the smaller reading) for the whole encoded request
	// body, the API key included. CreateJob rejects a larger body locally.
	MaxJobRequestBytes = 25_000_000

	// MaxItemsPerPage is the largest items_per_page jobs/results accepts.
	MaxItemsPerPage = 1000

	// DefaultWaitInterval is WaitForJob's pause between polls when
	// WaitForJobRequest.Interval is 0.
	DefaultWaitInterval = 10 * time.Second

	// DefaultPageSize is the items_per_page JobResultPages sends when
	// JobResultPagesRequest.ItemsPerPage is 0.
	DefaultPageSize = 100
)

// Internal bounds.
const (
	// productionOrigin is the only server the provider documents. Calls to it
	// are live and may be billed; there is no documented sandbox.
	productionOrigin = "https://api.neverbounce.com"

	// apiRoot is the path prefix of every operation: API version 4.2 only.
	apiRoot = "/v4.2/"

	maxTimeout            = 10 * time.Minute
	maxResponseBytesLimit = 64 << 20
	smallResponseCap      = 1 << 20
	maxDownloadTimeout    = 12 * time.Hour
	maxDownloadBytesLimit = 64 << 30
	maxKeyBytes           = 256
)

// Client calls the NeverBounce API v4.2. Create it with NewClient; the zero
// Client and a nil *Client return ErrInvalidConfig without any I/O. A Client is
// immutable and safe for concurrent use.
//
// Its fields are unexported and the API key is held only inside function
// values, so formatting a Client, or a struct holding one, with any fmt verb or
// a reflection-based dumper never prints it.
type Client struct {
	send    sender
	newBody func(capHint int) []byte
	scrub   func(string) string

	timeout          time.Duration
	maxResponseBytes int64
	downloadTimeout  time.Duration
	maxDownloadBytes int64
	testBaseURL      bool
	diagnostics      bool
}

// NewClient returns a Client that authenticates with a NeverBounce v4 API key.
//
// Parameters:
//   - apiKey: the key from the NeverBounce dashboard, sent as the first member
//     ("key") of every JSON request body, never in a URL or header
//   - opts: optional settings; see the With functions
//
// The key must be 1 to 256 bytes of visible ASCII (0x21 to 0x7E) other than
// '"' and '\'. Its "secret_" prefix is not enforced. The application decides
// where the key comes from: NewClient reads no environment variables or files
// and performs no I/O.
//
// Errors are *ValidationError values matching ErrInvalidConfig. They name the
// argument or option, never its value.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if err := checkKey(apiKey); err != nil {
		return nil, err
	}
	o := options{
		timeout:          DefaultTimeout,
		maxResponseBytes: DefaultMaxResponseBytes,
		downloadTimeout:  DefaultDownloadTimeout,
		maxDownloadBytes: DefaultMaxDownloadBytes,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	origin, err := o.validate()
	if err != nil {
		return nil, err
	}
	keyMember := `{"key":"` + apiKey + `"`
	return &Client{
		send: newSender(o.client(), origin, keyMember),
		newBody: func(capHint int) []byte {
			return make([]byte, len(keyMember), len(keyMember)+capHint)
		},
		scrub:            func(s string) string { return strings.ReplaceAll(s, apiKey, "[redacted]") },
		timeout:          o.timeout,
		maxResponseBytes: o.maxResponseBytes,
		downloadTimeout:  o.downloadTimeout,
		maxDownloadBytes: o.maxDownloadBytes,
		testBaseURL:      o.testBaseURLSet,
		diagnostics:      o.diagnostics,
	}, nil
}

// String describes the Client's settings, such as
// "neverbounce.Client{timeout: 1m0s, maxResponseBytes: 8388608, ...}". It
// never includes the API key or the base URL.
func (c *Client) String() string {
	if c == nil {
		return "neverbounce.Client(nil)"
	}
	var b strings.Builder
	b.WriteString("neverbounce.Client{timeout: ")
	b.WriteString(c.timeout.String())
	b.WriteString(", maxResponseBytes: ")
	b.WriteString(strconv.FormatInt(c.maxResponseBytes, 10))
	b.WriteString(", downloadTimeout: ")
	b.WriteString(c.downloadTimeout.String())
	b.WriteString(", maxDownloadBytes: ")
	b.WriteString(strconv.FormatInt(c.maxDownloadBytes, 10))
	b.WriteString(", testBaseURL: ")
	b.WriteString(strconv.FormatBool(c.testBaseURL))
	b.WriteString(", providerDiagnostics: ")
	b.WriteString(strconv.FormatBool(c.diagnostics))
	b.WriteString("}")
	return b.String()
}

// GoString returns the same text as String, so that %#v prints settings only.
func (c *Client) GoString() string { return c.String() }

// ready reports ErrInvalidConfig for a nil Client or one not built by
// NewClient.
func (c *Client) ready() error {
	if c == nil || c.send == nil || c.newBody == nil || c.scrub == nil {
		return invalidConfig("Client", reasonNotInitialized)
	}
	return nil
}

// responseLimit returns the body limit for op: the configured limit for
// jobs/results, and at most 1 MiB for every other JSON operation.
func (c *Client) responseLimit(op Operation) int64 {
	if op == OpJobResults {
		return c.maxResponseBytes
	}
	return min(c.maxResponseBytes, smallResponseCap)
}

// checkKey validates the API key. The key is spliced into each request body
// as a JSON string, so it must need no escaping: visible ASCII without '"'
// and '\'. The error names the argument, never its value.
func checkKey(key string) error {
	if key == "" {
		return invalidConfig("apiKey", reasonRequired)
	}
	if len(key) > maxKeyBytes || !asciiRange(key, 0x21, 0x7e) || strings.ContainsAny(key, `"\`) {
		return invalidConfig("apiKey", reasonInvalidFormat)
	}
	return nil
}

// client returns the HTTP client calls use: a copy of the injected client, or a
// new one on a fresh transport, that never follows a redirect and keeps no
// cookies.
func (o *options) client() *http.Client {
	var c http.Client
	if o.httpClientSet {
		c = *o.httpClient
	} else {
		c.Transport = newTransport()
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.Transport = redirectGuard{next: next}
	c.CheckRedirect = refuseRedirect
	c.Jar = nil
	return &c
}

// newTransport returns a fresh transport with the standard library's default
// settings, a bound on response headers, and its own connection pool. It uses
// http.ProxyFromEnvironment, as http.DefaultTransport does.
func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
}

// refuseRedirect stops the client at the first response, so the API key is
// never sent to another URL.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// redirectGuard removes the Location header from every 3xx response before the
// http.Client sees it. The client then returns the response as it is: it
// neither follows the redirect, which would resend the body, API key included,
// to the new URL, nor fails on an unparsable Location with an error that
// quotes it.
type redirectGuard struct {
	next http.RoundTripper
}

// RoundTrip passes r to the wrapped transport and strips Location from a 3xx.
func (g redirectGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(r)
	if resp != nil && resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		resp.Header.Del("Location")
	}
	return resp, err
}

// CloseIdleConnections closes the wrapped transport's idle connections, if it
// has any, so that (*http.Client).CloseIdleConnections still reaches it.
func (g redirectGuard) CloseIdleConnections() {
	if c, ok := g.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// testOrigin validates a loopback test URL and returns it as
// "https://host[:port]".
func testOrigin(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "?#@\\") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", false
	}
	if !loopbackHostPort(u.Host) {
		return "", false
	}
	return "https://" + u.Host, true
}

// loopbackHostPort accepts "ip[:port]" for a canonical IPv4 address in
// 127.0.0.0/8, or "[::1][:port]", with a canonical port from 1 to 65535.
func loopbackHostPort(hostport string) bool {
	host, port := hostport, ""
	if end := strings.LastIndexByte(hostport, ']'); end >= 0 {
		host, port = hostport[:end+1], hostport[end+1:]
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, port = hostport[:i], hostport[i:]
	}
	if host != "[::1]" {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is4() || !ip.IsLoopback() || ip.String() != host {
			return false
		}
	}
	if port == "" {
		return true
	}
	digits, ok := strings.CutPrefix(port, ":")
	if !ok {
		return false
	}
	n, err := strconv.ParseUint(digits, 10, 16)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == digits
}
