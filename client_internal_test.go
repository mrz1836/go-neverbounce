package neverbounce

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// errCFGuard is the error a failing transport returns under the guard.
var errCFGuard = errors.New("cf: transport failed")

// cfIdleTransport counts CloseIdleConnections calls.
type cfIdleTransport struct {
	http.RoundTripper

	closed int
}

func (t *cfIdleTransport) CloseIdleConnections() { t.closed++ }

// cfGuard returns the redirect guard that wraps the transport of c, failing
// tb when the transport is not wrapped.
func cfGuard(tb testing.TB, c *http.Client) redirectGuard {
	tb.Helper()
	g, ok := c.Transport.(redirectGuard)
	require.True(tb, ok, "the transport is wrapped in the redirect guard, got %T", c.Transport)
	return g
}

func TestClientRefuseRedirect(t *testing.T) {
	require.ErrorIs(t, refuseRedirect(nil, nil), http.ErrUseLastResponse)
}

func TestClientGuardForwardsCloseIdleConnections(t *testing.T) {
	idle := &cfIdleTransport{}
	o := options{httpClient: &http.Client{Transport: idle}, httpClientSet: true}
	c := o.client()
	c.CloseIdleConnections()
	assert.Equal(t, 1, idle.closed, "(*http.Client).CloseIdleConnections reaches the wrapped transport")
	cfGuard(t, c).CloseIdleConnections()
	assert.Equal(t, 2, idle.closed)

	assert.NotPanics(t, func() {
		redirectGuard{next: testutil.RoundTripFunc(nil)}.CloseIdleConnections()
	}, "a transport without the method is tolerated")
}

func TestClientDefaultTransportClosesIdleConnections(t *testing.T) {
	c := (&options{}).client()
	assert.NotPanics(t, c.CloseIdleConnections)
}

func TestClientRedirectBackstopInstalled(t *testing.T) {
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	permissive := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
		Jar:           jar,
		Transport:     &cfIdleTransport{},
		Timeout:       7 * time.Second,
	}
	tests := []struct {
		name    string
		o       options
		timeout time.Duration
	}{
		{name: "default", o: options{}},
		{name: "injected", o: options{httpClient: permissive, httpClientSet: true}, timeout: 7 * time.Second},
		{name: "injected without transport", o: options{httpClient: &http.Client{}, httpClientSet: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.o.client()
			require.NotNil(t, c.CheckRedirect)
			require.ErrorIs(t, c.CheckRedirect(nil, nil), http.ErrUseLastResponse)
			assert.Nil(t, c.Jar)
			assert.Equal(t, tt.timeout, c.Timeout, "an injected Timeout is kept")
			cfGuard(t, c)
			if tt.o.httpClientSet {
				assert.NotSame(t, tt.o.httpClient, c, "the SDK uses its own copy")
			}
		})
	}
	assert.Same(t, jar, permissive.Jar, "the caller's client is never modified")
	assert.NotNil(t, permissive.CheckRedirect)
	assert.IsType(t, &cfIdleTransport{}, permissive.Transport)
}

func TestClientInjectedTransportWrapped(t *testing.T) {
	inner := &cfIdleTransport{}
	c := (&options{httpClient: &http.Client{Transport: inner}, httpClientSet: true}).client()
	assert.Same(t, inner, cfGuard(t, c).next)
}

func TestClientNilTransportMeansDefaultTransport(t *testing.T) {
	c := (&options{httpClient: &http.Client{}, httpClientSet: true}).client()
	assert.Equal(t, http.DefaultTransport, cfGuard(t, c).next)
}

func TestClientGuardStripsLocationOnlyOn3xx(t *testing.T) {
	const location = "https://elsewhere.example.com/?key=" + internalKey
	for _, status := range []int{100, 101, 199, 200, 204, 299, 300, 301, 302, 303, 304, 305, 307, 308, 399, 400, 404, 500, 999} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			header := http.Header{"Location": {location}, "X-Other": {"kept"}}
			g := redirectGuard{next: testutil.TransportWith(status, header, 0, http.NoBody)}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://127.0.0.1/", http.NoBody)
			require.NoError(t, err)
			resp, err := g.RoundTrip(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			if status >= 300 && status < 400 {
				assert.Empty(t, resp.Header.Values("Location"), "a 3xx loses its Location")
			} else {
				assert.Equal(t, location, resp.Header.Get("Location"), "any other status keeps it")
			}
			assert.Equal(t, "kept", resp.Header.Get("X-Other"))
		})
	}
}

func TestClientGuardPassesErrorsThrough(t *testing.T) {
	g := redirectGuard{next: testutil.RoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errCFGuard
	})}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://127.0.0.1/", http.NoBody)
	require.NoError(t, err)
	resp, err := g.RoundTrip(req) //nolint:bodyclose // the response is nil
	assert.Nil(t, resp)
	require.ErrorIs(t, err, errCFGuard)
}

// TestClientDefaultTransport pins the transport the SDK builds when no client
// is injected: its own pool, never the shared http.DefaultTransport, the
// standard proxy behavior, a 64 KiB response header bound, and HTTP/2.
func TestClientDefaultTransport(t *testing.T) {
	c := (&options{}).client()
	assert.Zero(t, c.Timeout, "the per-call context governs")
	tr, ok := cfGuard(t, c).next.(*http.Transport)
	require.True(t, ok)
	assert.NotSame(t, http.DefaultTransport, tr)
	require.NotNil(t, tr.Proxy)
	assert.Equal(t, reflect.ValueOf(http.ProxyFromEnvironment).Pointer(), reflect.ValueOf(tr.Proxy).Pointer())
	assert.Equal(t, int64(64<<10), tr.MaxResponseHeaderBytes)
	assert.True(t, tr.ForceAttemptHTTP2)
	assert.Equal(t, 100, tr.MaxIdleConns)
	assert.Equal(t, 90*time.Second, tr.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	assert.Equal(t, time.Second, tr.ExpectContinueTimeout)
	assert.NotNil(t, tr.DialContext)
	assert.Nil(t, tr.TLSClientConfig, "the standard library's TLS defaults apply")
	assert.False(t, tr.DisableCompression, "the transport decodes gzip itself")

	other, ok := cfGuard(t, (&options{}).client()).next.(*http.Transport)
	require.True(t, ok)
	assert.NotSame(t, tr, other, "each client has its own pool")
}

func TestClientResponseLimit(t *testing.T) {
	ops := []Operation{OpCheckEmail, OpCreateJob, OpParseJob, OpStartJob, OpJobStatus, OpDownloadJob}
	tests := []struct {
		configured int64
		small      int64
	}{
		{configured: 1, small: 1},
		{configured: 1<<20 - 1, small: 1<<20 - 1},
		{configured: 1 << 20, small: 1 << 20},
		{configured: 1<<20 + 1, small: 1 << 20},
		{configured: DefaultMaxResponseBytes, small: 1 << 20},
		{configured: 64 << 20, small: 1 << 20},
	}
	for _, tt := range tests {
		c := &Client{maxResponseBytes: tt.configured}
		assert.Equal(t, tt.configured, c.responseLimit(OpJobResults), "jobs/results gets the configured limit")
		for _, op := range ops {
			assert.Equal(t, tt.small, c.responseLimit(op), "%s with %d", op, tt.configured)
		}
	}
}

func TestClientReadyNeedsEveryPart(t *testing.T) {
	full := newInternalClient(t)
	tests := []struct {
		name string
		c    *Client
	}{
		{name: "nil", c: nil},
		{name: "zero", c: &Client{}},
		{name: "no sender", c: &Client{newBody: full.newBody, scrub: full.scrub}},
		{name: "no body builder", c: &Client{send: full.send, scrub: full.scrub}},
		{name: "no scrubber", c: &Client{send: full.send, newBody: full.newBody}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.ready()
			require.ErrorIs(t, err, ErrInvalidConfig)
			assert.Equal(t, &ValidationError{Err: ErrInvalidConfig, Field: "Client", Reason: reasonNotInitialized}, err)
			require.ErrorIs(t, tt.c.prepare(context.Background()), ErrInvalidConfig)
		})
	}
	require.NoError(t, full.ready())
	var nilCtx context.Context
	assert.Equal(t, &ValidationError{Err: ErrInvalidRequest, Field: "ctx", Reason: reasonNil}, full.prepare(nilCtx))
}

// TestClientKeyHeldOnlyInClosures proves that the API key is reachable only
// through function values: no field of a Client holds it as data.
func TestClientKeyHeldOnlyInClosures(t *testing.T) {
	c := newInternalClient(t, WithTestBaseURL("https://127.0.0.1:51234"), WithProviderDiagnostics())
	testutil.AssertRedacted(t, strings.Join(testutil.ReflectStrings(c), "\n"), []string{internalKey, "51234"}, "reflection walk")
	v := reflect.ValueOf(c).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		assert.Contains(t, []reflect.Kind{reflect.Func, reflect.Bool, reflect.Int64}, f.Type.Kind(),
			"field %s: only functions and scalars may hold configuration", f.Name)
	}

	b := c.newBody(8)
	assert.Len(t, b, len(`{"key":"`+internalKey+`"`), "the body reserves room for the key member")
	assert.Equal(t, make([]byte, len(b)), b, "the encoders never see the key")
	assert.Equal(t, "a [redacted] b", c.scrub("a "+internalKey+" b"))
}

func TestClientOptionsValidateOrigin(t *testing.T) {
	base := options{
		timeout: DefaultTimeout, maxResponseBytes: DefaultMaxResponseBytes,
		downloadTimeout: DefaultDownloadTimeout, maxDownloadBytes: DefaultMaxDownloadBytes,
	}
	origin, err := base.validate()
	require.NoError(t, err)
	assert.Equal(t, "https://api.neverbounce.com", origin, "production is the only origin without a test URL")

	tests := []struct{ raw, want string }{
		{raw: "https://127.0.0.1", want: "https://127.0.0.1"},
		{raw: "https://127.0.0.1/", want: "https://127.0.0.1"},
		{raw: "https://127.0.0.1:8443/", want: "https://127.0.0.1:8443"},
		{raw: "https://[::1]:8443", want: "https://[::1]:8443"},
	}
	for _, tt := range tests {
		o := base
		o.testBaseURL, o.testBaseURLSet = tt.raw, true
		origin, err = o.validate()
		require.NoError(t, err)
		assert.Equal(t, tt.want, origin)
	}

	o := base
	o.testBaseURLSet = true
	_, err = o.validate()
	assert.Equal(t, &ValidationError{Err: ErrInvalidConfig, Field: "TestBaseURL", Reason: reasonInvalidURL}, err,
		"an explicitly empty test URL is rejected, not ignored")
}

func TestClientLoopbackHostPort(t *testing.T) {
	tests := []struct {
		hostport string
		want     bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:1", true},
		{"127.0.0.1:65535", true},
		{"[::1]", true},
		{"[::1]:443", true},
		{"127.0.0.1:", false},
		{"127.0.0.1:0", false},
		{"127.0.0.1:65536", false},
		{"127.0.0.1:080", false},
		{"127.0.0.1:-1", false},
		{"[::1]443", false},
		{"[::1]:", false},
		{"[::2]", false},
		{"[::1", false},
		{"::1", false},
		{"[127.0.0.1]", false},
		{"127.1", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, loopbackHostPort(tt.hostport), "%q", tt.hostport)
	}
}
