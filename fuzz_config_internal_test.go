package neverbounce

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// bzRequireLoopbackHost checks the host of an accepted test origin: a
// canonical IPv4 address in 127.0.0.0/8 or exactly [::1], with an optional
// canonical port from 1 to 65535.
func bzRequireLoopbackHost(t *testing.T, u *url.URL) {
	t.Helper()
	host, port := u.Hostname(), u.Port()
	if host == "::1" {
		require.True(t, strings.HasPrefix(u.Host, "[::1]"), "IPv6 loopback is spelled [::1]")
	} else {
		addr, err := netip.ParseAddr(host)
		require.NoError(t, err)
		require.True(t, addr.Is4(), "only IPv4 or [::1]")
		require.True(t, addr.IsLoopback(), "only 127.0.0.0/8")
		require.Equal(t, addr.String(), host, "the address is canonical")
	}
	if strings.HasSuffix(u.Host, "]") || !strings.Contains(strings.TrimPrefix(u.Host, "[::1]"), ":") {
		require.Empty(t, port)
		return
	}
	n, err := strconv.ParseUint(port, 10, 16)
	require.NoError(t, err, "the port is a number")
	require.NotZero(t, n)
	require.Equal(t, strconv.FormatUint(n, 10), port, "the port is canonical")
}

func FuzzTestBaseURL(f *testing.F) {
	for _, seed := range []string{
		"https://127.0.0.1", "https://127.0.0.1/", "https://127.0.0.1:8443", "https://127.255.255.254:1",
		"https://[::1]", "https://[::1]:443", "https://[::1]:65535/", "HTTPS://127.0.0.1",
		"https://localhost", "http://127.0.0.1", "https://user@127.0.0.1", "https://user:pw@127.0.0.1",
		"https://127.1", "https://0177.0.0.1", "https://127.000.0.1", "https://[::ffff:127.0.0.1]",
		"https://[0:0:0:0:0:0:0:1]", "https://[::1%25lo0]", "https://128.0.0.1", "https://10.0.0.1",
		"https://127.0.0.1:0", "https://127.0.0.1:0443", "https://127.0.0.1:65536", "https://127.0.0.1:",
		"https://127.0.0.1:+80", "https://127.0.0.1?q", "https://127.0.0.1?", "https://127.0.0.1#f",
		"https://127.0.0.1/p", "https://127.0.0.1//", "https://127.0.0.1/%2F", "https:127.0.0.1",
		"https:///127.0.0.1", "//127.0.0.1", "https://127.0.0.1\\@example.com", "https://[::1", "",
		"https://127.0.0.1 ", " https://127.0.0.1", "https://127.0.0.1\t", "https://127%2E0.0.1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > bzMaxFuzzInput {
			return
		}
		origin, ok := testOrigin(raw)
		s := testutil.NewScript(testutil.Step{Body: bzCheckOK})
		c, err := NewClient(internalKey, WithTestBaseURL(raw), WithHTTPClient(&http.Client{Transport: s}))
		if !ok {
			require.Empty(t, origin)
			require.Nil(t, c)
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.Equal(t, "neverbounce: invalid configuration: TestBaseURL: invalid_url", err.Error(),
				"the error never quotes the URL")
			return
		}
		require.NoError(t, err)

		u, perr := url.Parse(origin)
		require.NoError(t, perr)
		require.Equal(t, "https", u.Scheme)
		require.Nil(t, u.User, "no userinfo")
		require.Empty(t, u.Opaque)
		require.Empty(t, u.Path, "no path")
		require.Empty(t, u.RawPath)
		require.Empty(t, u.RawQuery, "no query")
		require.False(t, u.ForceQuery)
		require.Empty(t, u.Fragment, "no fragment")
		require.Equal(t, "https://"+u.Host, origin)
		bzRequireLoopbackHost(t, u)

		again, ok := testOrigin(origin)
		require.True(t, ok, "an accepted origin is accepted again")
		require.Equal(t, origin, again, "testOrigin is idempotent")

		// Calls go to the origin and nowhere else.
		_, err = c.CheckEmail(t.Context(), CheckEmailRequest{Email: "avery.example@example.com"})
		require.NoError(t, err)
		require.Equal(t, origin+"/v4.2/single/check", testutil.OnlyRequest(t, s).URL)
	})
}

// bzKeyAllowed reports whether key meets the documented API key rule: 1 to
// 256 bytes of visible ASCII (0x21 to 0x7E) other than '"' and '\'.
func bzKeyAllowed(key string) bool {
	if key == "" || len(key) > 256 {
		return false
	}
	for _, c := range []byte(key) {
		if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

func FuzzAPIKey(f *testing.F) {
	for _, seed := range []string{
		internalKey, "k", "secret_" + strings.Repeat("a", 249), strings.Repeat("k", 257), "",
		"!#$%&'()*+,-./0123456789:;<=>?@ABCXYZ[]^_`abcxyz{|}~", `key"quote`, `key\backslash`,
		"key space", " lead", "trail ", "tab\tkey", "nl\nkey", "nul\x00key", "del\x7fkey", "pässwörd", "\xff",
		"secret_</script>", `secret_"},"email":"x`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if len(key) > bzMaxFuzzInput {
			return
		}
		err := checkKey(key)
		want := bzKeyAllowed(key)
		require.Equal(t, want, err == nil, "accepted = %v", err == nil)

		s := testutil.NewScript(testutil.Step{Body: bzCheckOK})
		c, cerr := NewClient(key, WithTestBaseURL(bzLoopback), WithHTTPClient(&http.Client{Transport: s}))
		if !want {
			reason := "invalid_format"
			if key == "" {
				reason = bzReasonRequired
			}
			for _, e := range []error{err, cerr} {
				requireValidation(t, e, ErrInvalidConfig, "apiKey", reason)
			}
			require.Nil(t, c)
			return
		}
		require.NoError(t, cerr)

		_, err = c.CheckEmail(t.Context(), CheckEmailRequest{Email: "avery.example@example.com"})
		require.NoError(t, err)
		r := testutil.OnlyRequest(t, s)
		members := bzRequireBody(t, key, r.Body)
		require.Equal(t, []string{"key", "email", "address_info", "credits_info"}, bzNames(members))
		require.Equal(t, "avery.example@example.com", bzDecodeString(t, members[1].value))
		// The key travels only in the body: the URL and headers are fixed.
		require.Equal(t, bzLoopback+"/v4.2/single/check", r.URL)
		const jsonMedia = "application/json"
		require.Equal(t, http.Header{
			"Content-Type": {jsonMedia},
			"Accept":       {jsonMedia},
			"User-Agent":   {"go-neverbounce"},
		}, r.Header)
	})
}
