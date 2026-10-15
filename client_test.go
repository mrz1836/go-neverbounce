package neverbounce_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// cfMarker prefixes every rejected value, so a test can prove that no error
// ever quotes it. It is synthetic.
const cfMarker = "CFMARKER" // gitleaks:allow (synthetic test marker)

// cfContentType is the Content-Type header's canonical name.
const cfContentType = "Content-Type"

// cfDefaultString is how a Client built with no option describes itself.
const cfDefaultString = "neverbounce.Client{timeout: 1m0s, maxResponseBytes: 8388608, downloadTimeout: 30m0s, " +
	"maxDownloadBytes: 1073741824, testBaseURL: false, providerDiagnostics: false}"

// cfConfigError asserts that NewClient failed with a configuration error for
// field and reason, returned no Client, and quoted none of the secrets.
func cfConfigError(tb testing.TB, c *neverbounce.Client, err error, field, reason string, secrets ...string) {
	tb.Helper()
	assert.Nil(tb, c)
	require.ErrorIs(tb, err, neverbounce.ErrInvalidConfig)
	require.NotErrorIs(tb, err, neverbounce.ErrInvalidRequest)
	ve := validationError(tb, err)
	assert.Equal(tb, neverbounce.ValidationError{Err: neverbounce.ErrInvalidConfig, Field: field, Reason: reason}, *ve)
	assert.Equal(tb, "neverbounce: invalid configuration: "+field+": "+reason, err.Error())
	secrets = append(secrets, cfMarker, testKey)
	testutil.AssertRedacted(tb, err.Error(), secrets)
	assertErrorClean(tb, err, secrets...)
}

func TestClientKeyRejected(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		reason string
	}{
		{name: "empty", key: "", reason: reasonRequired},
		{name: "257 bytes", key: cfMarker + strings.Repeat("k", 257-len(cfMarker)), reason: reasonInvalidFormat},
		{name: "1 KiB", key: cfMarker + strings.Repeat("k", 1024), reason: reasonInvalidFormat},
		{name: "inner space", key: cfMarker + " k", reason: reasonInvalidFormat},
		{name: "leading space", key: " " + cfMarker, reason: reasonInvalidFormat},
		{name: "trailing space", key: cfMarker + " ", reason: reasonInvalidFormat},
		{name: "only a space", key: " ", reason: reasonInvalidFormat},
		{name: "DEL", key: cfMarker + "\x7f", reason: reasonInvalidFormat},
		{name: "NUL", key: cfMarker + "\x00k", reason: reasonInvalidFormat},
		{name: "tab", key: cfMarker + "\tk", reason: reasonInvalidFormat},
		{name: "line feed", key: cfMarker + "\nk", reason: reasonInvalidFormat},
		{name: "carriage return", key: cfMarker + "\rk", reason: reasonInvalidFormat},
		{name: "unit separator", key: cfMarker + "\x1fk", reason: reasonInvalidFormat},
		{name: "non-ASCII letter", key: cfMarker + "\xc3\xa9", reason: reasonInvalidFormat},
		{name: "byte 0x80", key: cfMarker + "\x80", reason: reasonInvalidFormat},
		{name: "byte 0xff", key: cfMarker + "\xff", reason: reasonInvalidFormat},
		{name: "double quote", key: cfMarker + `"`, reason: reasonInvalidFormat},
		{name: "backslash", key: cfMarker + `\`, reason: reasonInvalidFormat},
		{name: "JSON injection", key: cfMarker + `","key":"other`, reason: reasonInvalidFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := neverbounce.NewClient(tt.key)
			secrets := []string{}
			if tt.key != "" && strings.TrimSpace(tt.key) != "" {
				secrets = append(secrets, tt.key)
			}
			cfConfigError(t, c, err, "apiKey", tt.reason, secrets...)
		})
	}
}

func TestClientKeyEveryByte(t *testing.T) {
	for b := range 256 {
		key := "k" + string([]byte{byte(b)}) + "k"
		_, err := neverbounce.NewClient(key)
		want := b >= 0x21 && b <= 0x7e && b != '"' && b != '\\'
		if want {
			require.NoError(t, err, "byte 0x%02x must be accepted", b)
			continue
		}
		require.ErrorIs(t, err, neverbounce.ErrInvalidConfig, "byte 0x%02x must be rejected", b)
		assert.Equal(t, reasonInvalidFormat, validationError(t, err).Reason, "byte 0x%02x", b)
	}
}

func TestClientKeyAccepted(t *testing.T) {
	var visible []byte
	for b := byte(0x21); b <= 0x7e; b++ {
		if b != '"' && b != '\\' {
			visible = append(visible, b)
		}
	}
	tests := []struct {
		name string
		key  string
	}{
		{name: "one byte", key: "k"},
		{name: "exactly 256 bytes", key: strings.Repeat("k", 256)},
		{name: "every visible ASCII byte but quote and backslash", key: string(visible)},
		{name: "no secret_ prefix needed", key: "public-looking-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testutil.NewScript(testutil.Step{Body: checkOK})
			c, err := neverbounce.NewClient(tt.key, neverbounce.WithTestBaseURL(loopbackOrigin),
				neverbounce.WithHTTPClient(&http.Client{Transport: s}))
			require.NoError(t, err)
			require.NotNil(t, c)
			_, err = c.CheckEmail(t.Context(), checkRequest())
			require.NoError(t, err)
			r := testutil.OnlyRequest(t, s)
			assertBody(t, `{"key":"`+tt.key+`","email":"`+testEmail+`","address_info":false,"credits_info":false}`, r.Body)
			assert.Equal(t, 1, strings.Count(string(r.Body), `"key"`))
		})
	}
}

func TestClientOptionRejected(t *testing.T) {
	tests := []struct {
		name    string
		opt     neverbounce.Option
		field   string
		reason  string
		secrets []string
	}{
		{name: "nil HTTP client", opt: neverbounce.WithHTTPClient(nil), field: "HTTPClient", reason: "nil"},
		{name: "zero timeout", opt: neverbounce.WithTimeout(0), field: "Timeout", reason: reasonOutOfRange},
		{name: "negative timeout", opt: neverbounce.WithTimeout(-time.Second), field: "Timeout", reason: reasonOutOfRange},
		{
			name: "timeout over 10m", opt: neverbounce.WithTimeout(10*time.Minute + time.Nanosecond),
			field: "Timeout", reason: reasonOutOfRange, secrets: []string{"10m0", "600000000001"},
		},
		{name: "zero max response bytes", opt: neverbounce.WithMaxResponseBytes(0), field: "MaxResponseBytes", reason: reasonOutOfRange},
		{name: "negative max response bytes", opt: neverbounce.WithMaxResponseBytes(-1), field: "MaxResponseBytes", reason: reasonOutOfRange},
		{
			name: "max response bytes over 64 MiB", opt: neverbounce.WithMaxResponseBytes(64<<20 + 1),
			field: "MaxResponseBytes", reason: reasonOutOfRange, secrets: []string{"67108865"},
		},
		{name: "zero download timeout", opt: neverbounce.WithDownloadTimeout(0), field: "DownloadTimeout", reason: reasonOutOfRange},
		{name: "negative download timeout", opt: neverbounce.WithDownloadTimeout(-time.Nanosecond), field: "DownloadTimeout", reason: reasonOutOfRange},
		{
			name: "download timeout over 12h", opt: neverbounce.WithDownloadTimeout(12*time.Hour + time.Nanosecond),
			field: "DownloadTimeout", reason: reasonOutOfRange, secrets: []string{"12h0", "43200000000001"},
		},
		{name: "zero max download bytes", opt: neverbounce.WithMaxDownloadBytes(0), field: "MaxDownloadBytes", reason: reasonOutOfRange},
		{name: "negative max download bytes", opt: neverbounce.WithMaxDownloadBytes(-1), field: "MaxDownloadBytes", reason: reasonOutOfRange},
		{
			name: "max download bytes over 64 GiB", opt: neverbounce.WithMaxDownloadBytes(64<<30 + 1),
			field: "MaxDownloadBytes", reason: reasonOutOfRange, secrets: []string{"68719476737"},
		},
		{
			name: "invalid test URL", opt: neverbounce.WithTestBaseURL("http://" + cfMarker + ".example.com"),
			field: "TestBaseURL", reason: reasonInvalidURL, secrets: []string{"example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := neverbounce.NewClient(testKey, tt.opt)
			cfConfigError(t, c, err, tt.field, tt.reason, tt.secrets...)
		})
	}
}

func TestClientOptionBoundariesAccepted(t *testing.T) {
	tests := []struct {
		name string
		opts []neverbounce.Option
		want string // the Client's String
	}{
		{name: "no options", want: cfDefaultString},
		{name: "nil option", opts: []neverbounce.Option{nil}, want: cfDefaultString},
		{
			name: "timeout of one nanosecond", opts: []neverbounce.Option{neverbounce.WithTimeout(time.Nanosecond)},
			want: strings.Replace(cfDefaultString, "timeout: 1m0s", "timeout: 1ns", 1),
		},
		{
			name: "timeout of exactly 10m", opts: []neverbounce.Option{neverbounce.WithTimeout(10 * time.Minute)},
			want: strings.Replace(cfDefaultString, "timeout: 1m0s", "timeout: 10m0s", 1),
		},
		{
			name: "max response bytes of 1", opts: []neverbounce.Option{neverbounce.WithMaxResponseBytes(1)},
			want: strings.Replace(cfDefaultString, "maxResponseBytes: 8388608", "maxResponseBytes: 1", 1),
		},
		{
			name: "max response bytes of exactly 64 MiB", opts: []neverbounce.Option{neverbounce.WithMaxResponseBytes(64 << 20)},
			want: strings.Replace(cfDefaultString, "maxResponseBytes: 8388608", "maxResponseBytes: 67108864", 1),
		},
		{
			name: "download timeout of one nanosecond", opts: []neverbounce.Option{neverbounce.WithDownloadTimeout(time.Nanosecond)},
			want: strings.Replace(cfDefaultString, "downloadTimeout: 30m0s", "downloadTimeout: 1ns", 1),
		},
		{
			name: "download timeout of exactly 12h", opts: []neverbounce.Option{neverbounce.WithDownloadTimeout(12 * time.Hour)},
			want: strings.Replace(cfDefaultString, "downloadTimeout: 30m0s", "downloadTimeout: 12h0m0s", 1),
		},
		{
			name: "max download bytes of 1", opts: []neverbounce.Option{neverbounce.WithMaxDownloadBytes(1)},
			want: strings.Replace(cfDefaultString, "maxDownloadBytes: 1073741824", "maxDownloadBytes: 1", 1),
		},
		{
			name: "max download bytes of exactly 64 GiB", opts: []neverbounce.Option{neverbounce.WithMaxDownloadBytes(64 << 30)},
			want: strings.Replace(cfDefaultString, "maxDownloadBytes: 1073741824", "maxDownloadBytes: 68719476736", 1),
		},
		{name: "HTTP client", opts: []neverbounce.Option{neverbounce.WithHTTPClient(&http.Client{})}, want: cfDefaultString},
		{
			name: "test URL", opts: []neverbounce.Option{neverbounce.WithTestBaseURL("https://127.0.0.1:8443")},
			want: strings.Replace(cfDefaultString, "testBaseURL: false", "testBaseURL: true", 1),
		},
		{
			name: "provider diagnostics", opts: []neverbounce.Option{neverbounce.WithProviderDiagnostics()},
			want: strings.Replace(cfDefaultString, "providerDiagnostics: false", "providerDiagnostics: true", 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Construction performs no I/O, so the default transport is safe here.
			c, err := neverbounce.NewClient(testKey, tt.opts...)
			require.NoError(t, err)
			require.NotNil(t, c)
			assert.Equal(t, tt.want, c.String())
		})
	}
}

func TestClientValidationOrder(t *testing.T) {
	var (
		badClient          = neverbounce.WithHTTPClient(nil)
		badTimeout         = neverbounce.WithTimeout(0)
		badResponseBytes   = neverbounce.WithMaxResponseBytes(0)
		badDownloadTimeout = neverbounce.WithDownloadTimeout(0)
		badDownloadBytes   = neverbounce.WithMaxDownloadBytes(0)
		badURL             = neverbounce.WithTestBaseURL("http://127.0.0.1")
	)
	tests := []struct {
		name  string
		key   string
		opts  []neverbounce.Option
		field string
	}{
		{
			name: "key before every option", key: "",
			opts:  []neverbounce.Option{badURL, badDownloadBytes, badDownloadTimeout, badResponseBytes, badTimeout, badClient},
			field: "apiKey",
		},
		{
			name: "HTTP client before timeout", key: testKey,
			opts:  []neverbounce.Option{badURL, badDownloadBytes, badDownloadTimeout, badResponseBytes, badTimeout, badClient},
			field: "HTTPClient",
		},
		{
			name: "timeout before max response bytes", key: testKey,
			opts:  []neverbounce.Option{badURL, badDownloadBytes, badDownloadTimeout, badResponseBytes, badTimeout},
			field: "Timeout",
		},
		{
			name: "max response bytes before download timeout", key: testKey,
			opts:  []neverbounce.Option{badURL, badDownloadBytes, badDownloadTimeout, badResponseBytes},
			field: "MaxResponseBytes",
		},
		{
			name: "download timeout before max download bytes", key: testKey,
			opts:  []neverbounce.Option{badURL, badDownloadBytes, badDownloadTimeout},
			field: "DownloadTimeout",
		},
		{
			name: "max download bytes before test URL", key: testKey,
			opts:  []neverbounce.Option{badURL, badDownloadBytes},
			field: "MaxDownloadBytes",
		},
		{name: "test URL last", key: testKey, opts: []neverbounce.Option{badURL}, field: "TestBaseURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := neverbounce.NewClient(tt.key, tt.opts...)
			assert.Nil(t, c)
			require.ErrorIs(t, err, neverbounce.ErrInvalidConfig)
			assert.Equal(t, tt.field, validationError(t, err).Field)
		})
	}
}

func TestClientNilOptionSkipped(t *testing.T) {
	c, err := neverbounce.NewClient(testKey, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, cfDefaultString, c.String())

	c, err = neverbounce.NewClient(testKey, nil, neverbounce.WithTimeout(time.Second), nil)
	require.NoError(t, err)
	assert.Contains(t, c.String(), "timeout: 1s,")

	_, err = neverbounce.NewClient(testKey, nil, neverbounce.WithTimeout(0), nil)
	require.ErrorIs(t, err, neverbounce.ErrInvalidConfig)
	assert.Equal(t, "Timeout", validationError(t, err).Field)
}

func TestClientLastOptionWins(t *testing.T) {
	tests := []struct {
		name string
		opts []neverbounce.Option
		want string
	}{
		{
			name: "timeout",
			opts: []neverbounce.Option{neverbounce.WithTimeout(0), neverbounce.WithTimeout(3 * time.Second)},
			want: "timeout: 3s,",
		},
		{
			name: "max response bytes",
			opts: []neverbounce.Option{neverbounce.WithMaxResponseBytes(-5), neverbounce.WithMaxResponseBytes(77)},
			want: "maxResponseBytes: 77,",
		},
		{
			name: "download timeout",
			opts: []neverbounce.Option{neverbounce.WithDownloadTimeout(13 * time.Hour), neverbounce.WithDownloadTimeout(time.Minute)},
			want: "downloadTimeout: 1m0s,",
		},
		{
			name: "max download bytes",
			opts: []neverbounce.Option{neverbounce.WithMaxDownloadBytes(0), neverbounce.WithMaxDownloadBytes(99)},
			want: "maxDownloadBytes: 99,",
		},
		{
			name: "test URL",
			opts: []neverbounce.Option{neverbounce.WithTestBaseURL("http://" + cfMarker), neverbounce.WithTestBaseURL(loopbackOrigin)},
			want: "testBaseURL: true,",
		},
		{
			name: "HTTP client",
			opts: []neverbounce.Option{neverbounce.WithHTTPClient(nil), neverbounce.WithHTTPClient(&http.Client{})},
			want: "neverbounce.Client{",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := neverbounce.NewClient(testKey, tt.opts...)
			require.NoError(t, err, "an invalid value overridden by a later valid one is never seen")
			assert.Contains(t, c.String(), tt.want)

			reversed := []neverbounce.Option{tt.opts[1], tt.opts[0]}
			c, err = neverbounce.NewClient(testKey, reversed...)
			require.ErrorIs(t, err, neverbounce.ErrInvalidConfig, "the invalid value given last wins")
			assert.Nil(t, c)
		})
	}
}

// TestClientLastOptionWinsOnTheWire proves that the last WithHTTPClient,
// WithTestBaseURL and WithMaxResponseBytes are the ones calls use.
func TestClientLastOptionWinsOnTheWire(t *testing.T) {
	first := testutil.NewScript(testutil.Step{Body: checkOK})
	last := testutil.NewScript(testutil.Step{Body: checkOK}, testutil.Step{Body: checkOK})
	c := newClient(t,
		neverbounce.WithHTTPClient(&http.Client{Transport: first}),
		neverbounce.WithTestBaseURL("https://127.0.0.2:9443"),
		neverbounce.WithHTTPClient(&http.Client{Transport: last}),
		neverbounce.WithTestBaseURL("https://[::1]:8443/"),
		neverbounce.WithMaxResponseBytes(int64(len(checkOK))-1),
		neverbounce.WithMaxResponseBytes(int64(len(checkOK))),
	)
	_, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Zero(t, first.Calls())
	assert.Equal(t, "https://[::1]:8443"+pathCheck, testutil.OnlyRequest(t, last).URL)
}

// cfNeverCalled is a transport that fails the test if anything uses it.
func cfNeverCalled(tb testing.TB) testutil.RoundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		assert.Fail(tb, "unexpected round trip to "+r.URL.Path)
		return nil, http.ErrHandlerTimeout
	}
}

func TestClientNewClientDoesNoIO(t *testing.T) {
	ct := &testutil.CountingTransport{Next: cfNeverCalled(t)}
	hc := &http.Client{Transport: ct}
	for range 3 {
		_, err := neverbounce.NewClient(testKey, neverbounce.WithHTTPClient(hc), neverbounce.WithTestBaseURL(loopbackOrigin),
			neverbounce.WithProviderDiagnostics())
		require.NoError(t, err)
		_, err = neverbounce.NewClient(testKey, neverbounce.WithHTTPClient(hc))
		require.NoError(t, err)
	}
	assert.Zero(t, ct.Attempts(), "NewClient never sends anything")
}

// cfTestURLAccepted lists accepted test base URLs and the URL a single/check
// call then goes to.
func cfTestURLAccepted() []struct{ raw, want string } {
	return []struct{ raw, want string }{
		{"https://127.0.0.1", "https://127.0.0.1/v4.2/single/check"},
		{"https://127.0.0.1/", "https://127.0.0.1/v4.2/single/check"},
		{"https://127.0.0.1:1", "https://127.0.0.1:1/v4.2/single/check"},
		{"https://127.0.0.1:443", "https://127.0.0.1:443/v4.2/single/check"},
		{"https://127.0.0.1:8443/", "https://127.0.0.1:8443/v4.2/single/check"},
		{"https://127.0.0.1:65535", "https://127.0.0.1:65535/v4.2/single/check"},
		{"https://127.0.0.0", "https://127.0.0.0/v4.2/single/check"},
		{"https://127.10.20.30:4000", "https://127.10.20.30:4000/v4.2/single/check"},
		{"https://127.255.255.255", "https://127.255.255.255/v4.2/single/check"},
		{"https://[::1]", "https://[::1]/v4.2/single/check"},
		{"https://[::1]/", "https://[::1]/v4.2/single/check"},
		{"https://[::1]:8443", "https://[::1]:8443/v4.2/single/check"},
		{"https://[::1]:65535/", "https://[::1]:65535/v4.2/single/check"},
	}
}

func TestClientTestBaseURLAccepted(t *testing.T) {
	for _, tt := range cfTestURLAccepted() {
		t.Run(tt.raw, func(t *testing.T) {
			s := testutil.NewScript(testutil.Step{Body: checkOK})
			c := newClient(t, neverbounce.WithHTTPClient(&http.Client{Transport: s}), neverbounce.WithTestBaseURL(tt.raw))
			_, err := c.CheckEmail(t.Context(), checkRequest())
			require.NoError(t, err)
			assert.Equal(t, tt.want, testutil.OnlyRequest(t, s).URL)
			assert.NotContains(t, c.String(), strings.TrimPrefix(tt.raw, "https://"), "String never prints the URL")
		})
	}
}

// cfTestURLRejected lists test base URLs NewClient rejects.
func cfTestURLRejected() []struct{ name, raw string } {
	return []struct{ name, raw string }{
		{"empty", ""},
		{"http", "http://127.0.0.1"},
		{"websocket", "wss://127.0.0.1"},
		{"no scheme", "//127.0.0.1"},
		{"bare host", "127.0.0.1"},
		{"opaque", "https:127.0.0.1"},
		{"no host", "https://"},
		{"no host with path", "https:///v4.2"},
		{"localhost", "https://localhost"},
		{"localhost with port", "https://localhost:8443"},
		{"name", "https://" + cfMarker + ".example.com"},
		{"loopback-looking name", "https://127.0.0.1." + cfMarker + ".example"},
		{"userinfo", "https://" + cfMarker + "@127.0.0.1"},
		{"userinfo with password", "https://user:" + cfMarker + "@127.0.0.1:8443"},
		{"empty userinfo", "https://@127.0.0.1"},
		{"query", "https://127.0.0.1/?key=" + cfMarker},
		{"query without path", "https://127.0.0.1?" + cfMarker},
		{"empty query", "https://127.0.0.1/?"},
		{"fragment", "https://127.0.0.1#" + cfMarker},
		{"empty fragment", "https://127.0.0.1/#"},
		{"path", "https://127.0.0.1/" + cfMarker},
		{"API path", "https://127.0.0.1/v4.2"},
		{"path with trailing slash", "https://127.0.0.1/v4.2/"},
		{"double slash", "https://127.0.0.1//"},
		{"escaped slash", "https://127.0.0.1/%2F"},
		{"escaped path", "https://127.0.0.1/%7E"},
		{"backslash", "https://127.0.0.1\\" + cfMarker},
		{"space in host", "https://127.0.0.1 "},
		{"leading space", " https://127.0.0.1"},
		{"IPv4-mapped IPv6", "https://[::ffff:127.0.0.1]"},
		{"IPv4-mapped IPv6 with port", "https://[::ffff:127.0.0.1]:8443"},
		{"short IPv4", "https://127.1"},
		{"leading zero in an octet", "https://127.0.0.01"},
		{"leading zero in the first octet", "https://0127.0.0.1"},
		{"octal-looking address", "https://0177.0.0.1"},
		{"hex address", "https://0x7f.0.0.1"},
		{"decimal address", "https://2130706433"},
		{"port 0", "https://127.0.0.1:0"},
		{"port 65536", "https://127.0.0.1:65536"},
		{"port with a leading zero", "https://127.0.0.1:080"},
		{"port 00443", "https://127.0.0.1:00443"},
		{"empty port", "https://127.0.0.1:"},
		{"signed port", "https://127.0.0.1:+80"},
		{"two ports", "https://127.0.0.1:80:80"},
		{"IPv6 port 0", "https://[::1]:0"},
		{"IPv6 port 65536", "https://[::1]:65536"},
		{"IPv6 port without colon", "https://[::1]8443"},
		{"IPv6 empty port", "https://[::1]:"},
		{"unbracketed IPv6", "https://::1"},
		{"expanded IPv6 loopback", "https://[0:0:0:0:0:0:0:1]"},
		{"IPv6 loopback with zone", "https://[::1%25lo0]"},
		{"IPv6 unspecified", "https://[::]"},
		{"IPv6 non-loopback", "https://[::2]"},
		{"IPv6 documentation", "https://[2001:db8::1]"},
		{"unspecified IPv4", "https://0.0.0.0"},
		{"private IPv4", "https://10.0.0.1"},
		{"documentation IPv4", "https://192.0.2.1:8443"},
		{"just past 127/8", "https://128.0.0.1"},
		{"just before 127/8", "https://126.255.255.255"},
		{"link-local", "https://169.254.169.254"},
	}
}

func TestClientTestBaseURLRejected(t *testing.T) {
	for _, tt := range cfTestURLRejected() {
		t.Run(tt.name, func(t *testing.T) {
			c, err := neverbounce.NewClient(testKey, neverbounce.WithTestBaseURL(tt.raw))
			var secrets []string
			if host := strings.TrimSpace(strings.TrimPrefix(tt.raw, "https://")); len(host) > 3 {
				secrets = append(secrets, tt.raw, host)
			}
			cfConfigError(t, c, err, "TestBaseURL", reasonInvalidURL, secrets...)
		})
	}
}

// cfJar is a cookie jar that counts every use.
type cfJar struct {
	uses atomic.Int32
}

func (j *cfJar) SetCookies(*url.URL, []*http.Cookie) { j.uses.Add(1) }

func (j *cfJar) Cookies(*url.URL) []*http.Cookie {
	j.uses.Add(1)
	return []*http.Cookie{{Name: "session", Value: cfMarker, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}}
}

func TestClientInjectedClientNeverMutated(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	jar := &cfJar{}
	var redirects atomic.Int32
	checkRedirect := func(*http.Request, []*http.Request) error {
		redirects.Add(1)
		return nil
	}
	hc := &http.Client{Transport: ct, CheckRedirect: checkRedirect, Jar: jar, Timeout: 45 * time.Second}
	c := newClient(t, neverbounce.WithTestBaseURL(loopbackOrigin), neverbounce.WithHTTPClient(hc))

	_, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Same(t, ct, hc.Transport)
	assert.Equal(t, reflect.ValueOf(checkRedirect).Pointer(), reflect.ValueOf(hc.CheckRedirect).Pointer())
	assert.Same(t, jar, hc.Jar)
	assert.Equal(t, 45*time.Second, hc.Timeout)
	assert.Equal(t, 1, ct.Attempts())
	assert.Zero(t, jar.uses.Load(), "the injected jar is never consulted")
	assert.Zero(t, redirects.Load())
}

func TestClientInjectedNilFieldsStayNil(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	hc := &http.Client{Transport: ct}
	c := newClient(t, neverbounce.WithTestBaseURL(loopbackOrigin), neverbounce.WithHTTPClient(hc))
	_, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Nil(t, hc.CheckRedirect)
	assert.Nil(t, hc.Jar)
	assert.Zero(t, hc.Timeout)
	assert.Same(t, ct, hc.Transport)
}

// TestClientKeepsItsOwnCopy proves that changing the injected client after
// NewClient changes nothing about the calls.
func TestClientKeepsItsOwnCopy(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	hc := &http.Client{Transport: ct}
	c := newClient(t, neverbounce.WithTestBaseURL(loopbackOrigin), neverbounce.WithHTTPClient(hc))
	hc.Transport = cfNeverCalled(t)
	hc.Timeout = time.Nanosecond
	hc.Jar = &cfJar{}
	_, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Equal(t, 1, ct.Attempts())
}

func TestClientCookieJarNeverConsulted(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			srv := testutil.NewServerProto(t, proto, func(w http.ResponseWriter, _ *http.Request) {
				http.SetCookie(w, &http.Cookie{
					Name: "session", Value: cfMarker, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				respond(http.StatusOK, checkOK)(w, nil)
			})
			jar := &cfJar{}
			hc := srv.Client()
			hc.Jar = jar
			c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(hc))
			for range 2 {
				_, err := c.CheckEmail(t.Context(), checkRequest())
				require.NoError(t, err)
			}
			assert.Zero(t, jar.uses.Load(), "the jar is neither read nor written")
			reqs := srv.Requests()
			require.Len(t, reqs, 2)
			for _, r := range reqs {
				assert.Empty(t, r.Header.Values("Cookie"))
			}
		})
	}
}

// TestClientRedirectRefusedSmoke is the one redirect case here; the full
// matrix lives with the redirect tests.
func TestClientRedirectRefusedSmoke(t *testing.T) {
	c, srv := newServerClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v4.2/elsewhere?key="+cfMarker, http.StatusFound)
	})
	_, err := c.CheckEmail(t.Context(), checkRequest())
	assertCallError(t, err, wantCall{
		Kind: neverbounce.FailureUnexpectedResponse, Op: neverbounce.OpCheckEmail, Status: http.StatusFound,
	}, cfMarker, "elsewhere")
	assert.Equal(t, 1, srv.Hits(), "the redirect is never followed")
}

func TestClientString(t *testing.T) {
	const raw = "https://127.0.0.1:51234/"
	c := newClient(t, neverbounce.WithTestBaseURL(raw), neverbounce.WithTimeout(45*time.Second),
		neverbounce.WithMaxResponseBytes(4096), neverbounce.WithDownloadTimeout(2*time.Hour),
		neverbounce.WithMaxDownloadBytes(1<<20), neverbounce.WithProviderDiagnostics())
	want := "neverbounce.Client{timeout: 45s, maxResponseBytes: 4096, downloadTimeout: 2h0m0s, " +
		"maxDownloadBytes: 1048576, testBaseURL: true, providerDiagnostics: true}"
	assert.Equal(t, want, c.String())
	assert.Equal(t, want, c.GoString())
	assert.Equal(t, want, fmt.Sprintf("%v", c))
	assert.Equal(t, want, fmt.Sprintf("%#v", c))

	secrets := []string{testKey, raw, "51234", "127.0.0.1", "api.neverbounce.com"}
	testutil.AssertShapesNeverPrint(t, c, secrets...)
	testutil.AssertRedacted(t, strings.Join(testutil.ReflectStrings(c), "\n"), secrets, "reflection walk")

	other := newClient(t)
	testutil.AssertDiffRedacted(t, c, other, secrets...)
}

func TestClientStringNilAndZero(t *testing.T) {
	var nilClient *neverbounce.Client
	assert.Equal(t, "neverbounce.Client(nil)", nilClient.String())
	assert.Equal(t, "neverbounce.Client(nil)", nilClient.GoString())
	assert.Equal(t, "neverbounce.Client(nil)", fmt.Sprintf("%v", nilClient))

	var zero neverbounce.Client
	want := "neverbounce.Client{timeout: 0s, maxResponseBytes: 0, downloadTimeout: 0s, maxDownloadBytes: 0, " +
		"testBaseURL: false, providerDiagnostics: false}"
	assert.Equal(t, want, zero.String())
	assert.Equal(t, want, zero.GoString())
}

// cfMethod is an operation case together with a call of the same method with
// a request it refuses.
type cfMethod struct {
	opCase

	invalid func(ctx context.Context, c *neverbounce.Client) error
}

// with returns m's valid call, or its invalid one.
func (m cfMethod) with(valid bool) func(ctx context.Context, c *neverbounce.Client) error {
	if valid {
		return m.call
	}
	return m.invalid
}

// cfMethods returns every method's case, with a call it refuses: the zero
// request for CheckEmail and CreateJob, and job 0 for every other method.
func cfMethods() []cfMethod {
	zeroRequest := map[neverbounce.Operation]func(context.Context, *neverbounce.Client) error{
		neverbounce.OpCheckEmail: func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.CheckEmail(ctx, neverbounce.CheckEmailRequest{})
			return err
		},
		neverbounce.OpCreateJob: func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.CreateJob(ctx, neverbounce.CreateJobRequest{})
			return err
		},
	}
	jobZero := opCasesFor(0)
	methods := make([]cfMethod, 0, len(jobZero))
	for i, o := range opCases() {
		invalid := jobZero[i].call
		if call, ok := zeroRequest[o.op]; ok {
			invalid = call
		}
		methods = append(methods, cfMethod{opCase: o, invalid: invalid})
	}
	return methods
}

func TestClientMethodsNotInitialized(t *testing.T) {
	var (
		zero      neverbounce.Client
		nilClient *neverbounce.Client
		nilCtx    context.Context
	)
	clients := []struct {
		name   string
		client *neverbounce.Client
	}{
		{name: "nil client", client: nilClient},
		{name: "zero client", client: &zero},
	}
	for _, m := range cfMethods() {
		for _, cl := range clients {
			for _, valid := range []bool{true, false} {
				for _, ctx := range []context.Context{t.Context(), nilCtx} {
					name := m.name + "/" + cl.name + "/valid=" + strconv.FormatBool(valid) + "/nil ctx=" + strconv.FormatBool(ctx == nil)
					t.Run(name, func(t *testing.T) {
						err := m.with(valid)(ctx, cl.client)
						require.ErrorIs(t, err, neverbounce.ErrInvalidConfig)
						ve := validationError(t, err)
						assert.Equal(t, neverbounce.ValidationError{
							Err: neverbounce.ErrInvalidConfig, Field: "Client", Reason: reasonNotInitialized,
						}, *ve)
					})
				}
			}
		}
	}
}

func TestClientMethodsNilContext(t *testing.T) {
	var nilCtx context.Context
	for _, m := range cfMethods() {
		for _, valid := range []bool{true, false} {
			t.Run(m.name+"/valid="+strconv.FormatBool(valid), func(t *testing.T) {
				ct := &testutil.CountingTransport{Next: cfNeverCalled(t)}
				c := newTransportClient(t, ct)
				err := m.with(valid)(nilCtx, c)
				require.ErrorIs(t, err, neverbounce.ErrInvalidRequest)
				ve := validationError(t, err)
				assert.Equal(t, neverbounce.ValidationError{Err: neverbounce.ErrInvalidRequest, Field: "ctx", Reason: "nil"}, *ve)
				assert.Zero(t, ct.Attempts())
			})
		}
	}
}

// cfDoneContexts returns a canceled context and an expired one, with the
// failure kind each must produce.
func cfDoneContexts(tb testing.TB) []struct {
	name string
	ctx  context.Context //nolint:containedctx // each case runs with its own context
	kind neverbounce.FailureKind
	is   error
} {
	tb.Helper()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	tb.Cleanup(cancelExpired)
	return []struct {
		name string
		ctx  context.Context //nolint:containedctx // each case runs with its own context
		kind neverbounce.FailureKind
		is   error
	}{
		{name: "canceled", ctx: canceled, kind: neverbounce.FailureCanceled, is: context.Canceled},
		{name: "expired", ctx: expired, kind: neverbounce.FailureDeadlineExceeded, is: context.DeadlineExceeded},
	}
}

func TestClientMethodsDoneContextSendNothing(t *testing.T) {
	for _, m := range cfMethods() {
		for _, dc := range cfDoneContexts(t) {
			t.Run(m.name+"/"+dc.name, func(t *testing.T) {
				ct := &testutil.CountingTransport{Next: cfNeverCalled(t)}
				c := newTransportClient(t, ct)
				err := m.call(dc.ctx, c)
				assertCallError(t, err, m.want(wantCall{Kind: dc.kind}))
				assert.Equal(t, dc.is, errors.Unwrap(callError(t, err)), "the cause is the context error")
				assert.Zero(t, ct.Attempts(), "a done context sends nothing")
			})
		}
	}
}

// TestClientMethodsRefuseInvalidJobIDs proves that every method whose request
// names a job refuses an ID that is not greater than 0, naming JobID, and sends
// nothing.
func TestClientMethodsRefuseInvalidJobIDs(t *testing.T) {
	tests := []struct {
		id     neverbounce.JobID
		reason string
	}{
		{0, reasonRequired},
		{-1, reasonOutOfRange},
		{math.MinInt64, reasonOutOfRange},
	}
	for _, tt := range tests {
		for _, o := range opCasesFor(tt.id) {
			if !o.takesJob() {
				continue
			}
			t.Run(o.name+"/"+tt.id.String(), func(t *testing.T) {
				ct := &testutil.CountingTransport{Next: cfNeverCalled(t)}
				err := o.call(t.Context(), newTransportClient(t, ct))
				assertValidation(t, err, "JobID", tt.reason)
				assert.Equal(t, "neverbounce: invalid request: JobID: "+tt.reason, err.Error())
				assert.Zero(t, ct.Attempts(), "nothing is sent")
			})
		}
	}
}

// TestClientMethodsValidateBeforeContext proves that request validation runs
// before the context check: an invalid request with a done context is a
// validation error, and nothing is sent.
func TestClientMethodsValidateBeforeContext(t *testing.T) {
	for _, m := range cfMethods() {
		for _, dc := range cfDoneContexts(t) {
			t.Run(m.name+"/"+dc.name, func(t *testing.T) {
				ct := &testutil.CountingTransport{Next: cfNeverCalled(t)}
				c := newTransportClient(t, ct)
				err := m.invalid(dc.ctx, c)
				require.ErrorIs(t, err, neverbounce.ErrInvalidRequest)
				require.NotErrorIs(t, err, dc.is)
				assert.Zero(t, ct.Attempts())
			})
		}
	}
}
