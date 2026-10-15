package neverbounce

import (
	"net/http"
	"time"
)

// Option configures NewClient. Options only record their argument: NewClient
// validates the result. A nil Option is skipped, and when an option is given
// more than once the last one wins.
type Option func(*options)

// options is the configuration options record.
type options struct {
	httpClient       *http.Client
	httpClientSet    bool
	timeout          time.Duration
	maxResponseBytes int64
	downloadTimeout  time.Duration
	maxDownloadBytes int64
	testBaseURL      string
	testBaseURLSet   bool
	diagnostics      bool
}

// WithHTTPClient makes calls through a copy of hc. The copy refuses redirects
// (its CheckRedirect returns http.ErrUseLastResponse), has no cookie jar, keeps
// hc.Timeout, and wraps hc's transport, without modifying it, so that every
// redirect comes back as a plain response with its Location removed and is
// never followed. A nil transport means http.DefaultTransport. hc itself is
// never modified. hc.Timeout applies to every call, DownloadJob included: leave
// it 0 when downloading large lists.
//
// The transport is trusted: it sees the API key and the email addresses in
// every request body, and it may retry on its own, which the SDK cannot
// prevent. A nil hc is rejected.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) {
		o.httpClient = hc
		o.httpClientSet = true
	}
}

// WithTimeout bounds each JSON call, including reading the whole response
// body. The default is DefaultTimeout (60 seconds); d must be greater than 0
// and at most 10 minutes. The caller's context deadline applies as well. It
// does not apply to DownloadJob (see WithDownloadTimeout).
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithMaxResponseBytes limits a JSON response body, counted after any
// transport decompression. The default is DefaultMaxResponseBytes (8 MiB); n
// must be from 1 byte to 64 MiB. Every operation except jobs/results is also
// capped at 1 MiB. A larger HTTP 200 body fails with ErrResponseTooLarge,
// after the provider may already have processed the call.
func WithMaxResponseBytes(n int64) Option {
	return func(o *options) { o.maxResponseBytes = n }
}

// WithDownloadTimeout bounds a whole DownloadJob call, streaming included. The
// default is DefaultDownloadTimeout (30 minutes); d must be greater than 0 and
// at most 12 hours.
func WithDownloadTimeout(d time.Duration) Option {
	return func(o *options) { o.downloadTimeout = d }
}

// WithMaxDownloadBytes limits the bytes DownloadJob copies to its writer. The
// default is DefaultMaxDownloadBytes (1 GiB); n must be from 1 byte to 64 GiB.
// A longer download fails with ErrResponseTooLarge after exactly n bytes.
func WithMaxDownloadBytes(n int64) Option {
	return func(o *options) { o.maxDownloadBytes = n }
}

// WithTestBaseURL sends calls to a local test server instead of the provider.
// It exists for tests built on httptest.NewTLSServer, together with
// WithHTTPClient(srv.Client()). The SDK appends /v4.2/<operation> to it.
//
// The URL must use https and have a host that is a canonical IPv4 address in
// 127.0.0.0/8 or exactly [::1], with an optional port and no userinfo, query,
// fragment or path (a lone trailing "/" is allowed). Names such as "localhost"
// are rejected, because they resolve through the system resolver. Errors never
// quote the URL. There is no option for any other base URL, so the API key
// reaches only the provider or loopback.
func WithTestBaseURL(rawURL string) Option {
	return func(o *options) {
		o.testBaseURL = rawURL
		o.testBaseURLSet = true
	}
}

// WithProviderDiagnostics keeps the provider's "message" on errors, reachable
// only through (*CallError).Diagnostics, cut to 1 KiB and with the API key
// replaced by "[redacted]". It is off by default, because errors are routinely
// logged and passed on, and provider text may echo request data such as an
// email address.
func WithProviderDiagnostics() Option {
	return func(o *options) { o.diagnostics = true }
}

// validate checks the options in a fixed order and returns the API origin.
func (o *options) validate() (string, error) {
	switch {
	case o.httpClientSet && o.httpClient == nil:
		return "", invalidConfig("HTTPClient", reasonNil)
	case o.timeout <= 0 || o.timeout > maxTimeout:
		return "", invalidConfig("Timeout", reasonOutOfRange)
	case o.maxResponseBytes < 1 || o.maxResponseBytes > maxResponseBytesLimit:
		return "", invalidConfig("MaxResponseBytes", reasonOutOfRange)
	case o.downloadTimeout <= 0 || o.downloadTimeout > maxDownloadTimeout:
		return "", invalidConfig("DownloadTimeout", reasonOutOfRange)
	case o.maxDownloadBytes < 1 || o.maxDownloadBytes > maxDownloadBytesLimit:
		return "", invalidConfig("MaxDownloadBytes", reasonOutOfRange)
	case !o.testBaseURLSet:
		return productionOrigin, nil
	}
	origin, ok := testOrigin(o.testBaseURL)
	if !ok {
		return "", invalidConfig("TestBaseURL", reasonInvalidURL)
	}
	return origin, nil
}
