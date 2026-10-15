package neverbounce_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Planted transport errors. Their text holds cfMarker, which must never
// surface.
var (
	errCFPlanted = errors.New("transport broke " + cfMarker)
	errCFRefused = errors.New("connection refused " + cfMarker)
)

// cfTCP is the network of every planted net.OpError.
const cfTCP = "tcp"

// cfWireOp is an operation case as it travels: the exact request body after
// the key member, and the Accept header.
type cfWireOp struct {
	opCase

	body   string
	accept string
}

// cfWireOps returns the seven operations as they travel.
func cfWireOps() []cfWireOp {
	job := `,"job_id":` + opJobID.String()
	wire := map[string]struct{ body, accept string }{
		"CheckEmail": {`,"email":"` + testEmail + `","address_info":false,"credits_info":false}`, mediaJSON},
		"CreateJob": {
			`,"input_location":"supplied","input":[{"email":"` + testEmail + `"}],` +
				`"auto_parse":false,"auto_start":false,"run_sample":false,"allow_manual_review":false}`,
			mediaJSON,
		},
		"ParseJob":    {job + `,"auto_start":false}`, mediaJSON},
		"StartJob":    {job + `,"run_sample":true}`, mediaJSON},
		"JobStatus":   {job + `}`, mediaJSON},
		"JobResults":  {job + `,"page":2,"items_per_page":10}`, mediaJSON},
		"DownloadJob": {job + `,"line_feed_type":"LINEFEED_0D0A"}`, "*/*"},
	}
	ops := operations()
	out := make([]cfWireOp, len(ops))
	for i, o := range ops {
		w := wire[o.name]
		out[i] = cfWireOp{opCase: o, body: w.body, accept: w.accept}
	}
	return out
}

// cfAssertKeyOnce asserts that body starts with the key member and holds the
// key exactly once.
func cfAssertKeyOnce(tb testing.TB, body []byte) {
	tb.Helper()
	assert.True(tb, bytes.HasPrefix(body, []byte(keyMember)), "the key is the first member")
	assert.Equal(tb, 1, bytes.Count(body, []byte(testKey)), "the key appears exactly once")
	assert.Equal(tb, 1, bytes.Count(body, []byte(`"key"`)), "one key member")
}

func TestTransportWireOverTLS(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		for _, o := range cfWireOps() {
			t.Run(proto.Name()+"/"+o.name, func(t *testing.T) {
				c, srv := newServerClientProto(t, proto, testutil.Serve(o.ok))
				require.NoError(t, o.call(t.Context(), c))

				r := testutil.OnlyRequest(t, srv)
				assertWireRequest(t, r, o.path, o.accept)
				assert.Equal(t, string(proto), r.Proto)
				assert.Equal(t, o.path, r.URL, "exact path and no query")
				assertBody(t, keyMember+o.body, r.Body)
				cfAssertKeyOnce(t, r.Body)
				assert.Equal(t, []string{strconv.Itoa(len(r.Body))}, r.Header.Values("Content-Length"))
				// The transport adds Accept-Encoding itself; nothing else may appear.
				keys := slices.Sorted(func(yield func(string) bool) {
					for k := range r.Header {
						if k != "Accept-Encoding" && !yield(k) {
							return
						}
					}
				})
				assert.Equal(t, []string{"Accept", "Content-Length", cfContentType, "User-Agent"}, keys)
			})
		}
	}
}

func TestTransportWireInMemory(t *testing.T) {
	for _, o := range cfWireOps() {
		t.Run(o.name, func(t *testing.T) {
			c, s := newScriptClient(t, o.ok)
			require.NoError(t, o.call(t.Context(), c))

			r := testutil.OnlyRequest(t, s)
			assertWireRequest(t, r, o.path, o.accept)
			assert.Equal(t, loopbackOrigin+o.path, r.URL)
			assertBody(t, keyMember+o.body, r.Body)
			cfAssertKeyOnce(t, r.Body)
			assert.Equal(t, http.Header{
				cfContentType: {mediaJSON},
				"Accept":      {o.accept},
				"User-Agent":  {"go-neverbounce"},
			}, r.Header, "exactly the SDK's three headers")
		})
	}
}

// TestTransportProductionOrigin proves that without WithTestBaseURL every call
// goes to the documented production endpoint. The in-memory transport never
// dials.
func TestTransportProductionOrigin(t *testing.T) {
	for _, o := range cfWireOps() {
		t.Run(o.name, func(t *testing.T) {
			s := testutil.NewScript(o.ok)
			c := newClient(t, neverbounce.WithHTTPClient(&http.Client{Transport: s}))
			require.NoError(t, o.call(t.Context(), c))
			assert.Equal(t, "https://api.neverbounce.com"+o.path, testutil.OnlyRequest(t, s).URL)
		})
	}
}

func TestTransportHTTPStatusTable(t *testing.T) {
	statuses := make([]int, 0, 502)
	for code := 100; code <= 599; code++ {
		statuses = append(statuses, code)
	}
	statuses = append(statuses, 600, 999)
	const envelope = `{"status":"auth_failure","message":"cf note ` + testKey + `"}`
	for _, code := range statuses {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			content := envelope
			if code == http.StatusOK {
				content = checkOK
			}
			body := testutil.NewBody(content)
			ct := &testutil.CountingTransport{Next: testutil.TransportWith(code, nil, int64(len(content)), body)}
			c := newTransportClient(t, ct)
			res, err := c.CheckEmail(t.Context(), checkRequest())
			assert.Equal(t, 1, ct.Attempts())
			assert.Equal(t, 1, body.Closes(), "the body is closed exactly once")
			if code == http.StatusOK {
				require.NoError(t, err)
				assert.Equal(t, neverbounce.ResultValid, res.Result)
				return
			}
			assert.Nil(t, res)
			want := wantCall{Kind: statusKind(code), Op: neverbounce.OpCheckEmail, Status: code}
			if code < http.StatusOK {
				assert.Zero(t, body.BytesRead(), "a 1xx body is never read")
			} else {
				want.API = neverbounce.APIStatusAuthFailure
			}
			assertCallError(t, err, want, "cf note")
			ce := callError(t, err)
			assert.Empty(t, ce.Diagnostics().Message, "diagnostics are opt-in")
			assert.Equal(t, "neverbounce: unexpected HTTP status", errors.Unwrap(ce).Error())
		})
	}
}

func TestTransportStatusDiagnosticsOptIn(t *testing.T) {
	const envelope = `{"status":"throttle_triggered","message":"cf note ` + testKey + ` for ` + testEmail + `"}`
	for _, code := range []int{400, 401, 403, 413, 429, 500, 502, 503, 302, 204} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c := newTransportClient(t, testutil.StaticTransport(code, envelope), neverbounce.WithProviderDiagnostics())
			_, err := c.CheckEmail(t.Context(), checkRequest())
			// The HTTP status alone decides the kind.
			assertCallError(t, err, wantCall{
				Kind: statusKind(code), Op: neverbounce.OpCheckEmail,
				Status: code, API: neverbounce.APIStatusThrottleTriggered,
			}, "cf note")
			assert.Equal(t, "cf note [redacted] for "+testEmail, callError(t, err).Diagnostics().Message)
		})
	}
}

func TestTransportStatusBodyEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  neverbounce.APIStatus
		message string // with WithProviderDiagnostics
	}{
		{name: "documented status", body: `{"status":"general_failure","message":"cf note"}`, status: "general_failure", message: "cf note"},
		{name: "undocumented token", body: `{"status":"maintenance_mode"}`, status: "maintenance_mode"},
		{name: "success token", body: `{"status":"success"}`, status: "success"},
		{name: "escaped token", body: testutil.Esc(`{"status":"~u0061uth_failure"}`), status: "auth_failure"},
		{name: "64-byte token", body: `{"status":"` + strings.Repeat("a", 64) + `"}`, status: neverbounce.APIStatus(strings.Repeat("a", 64))},
		{name: "65-byte token", body: `{"status":"` + strings.Repeat("a", 65) + `","message":"cf note"}`, message: "cf note"},
		{name: "upper case", body: `{"status":"SUCCESS"}`},
		{name: "digit", body: `{"status":"temp_unavail2"}`},
		{name: "space", body: `{"status":"temp unavail"}`},
		{name: "hyphen", body: `{"status":"temp-unavail"}`},
		{name: "empty string", body: `{"status":""}`},
		{name: "number", body: `{"status":42,"message":"cf note"}`, message: "cf note"},
		{name: "null", body: `{"status":null}`},
		{name: "array", body: `{"status":["auth_failure"]}`},
		{name: "object", body: `{"status":{"auth_failure":true}}`},
		{name: "absent", body: `{"message":"cf note"}`, message: "cf note"},
		{name: "message not a string", body: `{"status":"auth_failure","message":7}`, status: "auth_failure"},
		{name: "duplicate key", body: `{"status":"auth_failure","status":"auth_failure","message":"cf note"}`},
		{name: "trailing data", body: `{"status":"auth_failure"} x`},
		{name: "top-level array", body: `["auth_failure"]`},
		{name: "top-level string", body: `"auth_failure"`},
		{name: "empty body", body: ``},
		{name: "not JSON", body: `auth_failure`},
		{name: "HTML", body: `<html><body>502 Bad Gateway</body></html>`},
		{name: "invalid UTF-8", body: "{\"status\":\"auth_failure\",\"message\":\"\xff\"}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, diagnostics := range []bool{false, true} {
				opts := []neverbounce.Option{}
				if diagnostics {
					opts = append(opts, neverbounce.WithProviderDiagnostics())
				}
				c := newTransportClient(t, testutil.StaticTransport(http.StatusBadGateway, tt.body), opts...)
				_, err := c.CheckEmail(t.Context(), checkRequest())
				// The body never changes the kind.
				assertCallError(t, err, wantCall{
					Kind: neverbounce.FailureUpstream, Op: neverbounce.OpCheckEmail,
					Status: http.StatusBadGateway, API: tt.status,
				}, "cf note")
				message := callError(t, err).Diagnostics().Message
				if diagnostics {
					assert.Equal(t, tt.message, message)
				} else {
					assert.Empty(t, message)
				}
			}
		})
	}
}

// cfPadded returns envelope with spaces before its closing brace, so that it
// is exactly n bytes long.
func cfPadded(tb testing.TB, envelope string, n int) string {
	tb.Helper()
	require.GreaterOrEqual(tb, n, len(envelope))
	return envelope[:len(envelope)-1] + strings.Repeat(" ", n-len(envelope)) + "}"
}

func TestTransportStatusExcerptBound(t *testing.T) {
	const limit = 64 << 10
	envelope := `{"status":"temp_unavail","message":"cf note"}`
	tests := []struct {
		name     string
		size     int
		declared bool
		want     neverbounce.APIStatus
		read     int64 // bytes read from the body
	}{
		{name: "exactly 64 KiB declared", size: limit, declared: true, want: "temp_unavail", read: limit},
		{name: "exactly 64 KiB unknown length", size: limit, want: "temp_unavail", read: limit},
		{name: "64 KiB plus one declared", size: limit + 1, declared: true, read: 0},
		{name: "64 KiB plus one unknown length", size: limit + 1, read: limit + 1},
		{name: "1 MiB unknown length", size: 1 << 20, read: limit + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.NewBody(cfPadded(t, envelope, tt.size))
			cl := int64(-1)
			if tt.declared {
				cl = int64(tt.size)
			}
			c := newTransportClient(t, testutil.TransportWith(http.StatusServiceUnavailable, nil, cl, body),
				neverbounce.WithProviderDiagnostics())
			_, err := c.CheckEmail(t.Context(), checkRequest())
			// An oversized excerpt never changes the kind.
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureUnavailable, Op: neverbounce.OpCheckEmail,
				Status: http.StatusServiceUnavailable, API: tt.want,
			}, "cf note")
			message := callError(t, err).Diagnostics().Message
			if tt.want == "" {
				assert.Empty(t, message)
			} else {
				assert.Equal(t, "cf note", message)
			}
			assert.Equal(t, tt.read, body.BytesRead())
			assert.Equal(t, 1, body.Closes())
		})
	}
}

// cfFailingBody returns a body that serves content, then fails with end.
func cfFailingBody(content string, end error) *testutil.Body {
	body := testutil.NewBody(content)
	body.End = end
	return body
}

func TestTransportStatusExcerptReadError(t *testing.T) {
	for _, cl := range []int64{-1, 200} {
		t.Run(strconv.FormatInt(cl, 10), func(t *testing.T) {
			body := cfFailingBody(`{"status":"temp_unavail"}`, errCFPlanted)
			c := newTransportClient(t, testutil.TransportWith(http.StatusTooManyRequests, nil, cl, body))
			_, err := c.CheckEmail(t.Context(), checkRequest())
			// A failed read yields no excerpt, so no APIStatus.
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureRateLimited, Op: neverbounce.OpCheckEmail, Status: http.StatusTooManyRequests,
			}, cfMarker)
			assert.Equal(t, 1, body.Closes())
		})
	}
}

func TestTransportContentEncoding(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		refused bool
	}{
		{name: "gzip", values: []string{"gzip"}, refused: true},
		{name: "upper-case gzip", values: []string{"GZIP"}, refused: true},
		{name: "x-gzip", values: []string{"x-gzip"}, refused: true},
		{name: "br", values: []string{"br"}, refused: true},
		{name: "deflate", values: []string{"deflate"}, refused: true},
		{name: "zstd", values: []string{"zstd"}, refused: true},
		{name: "unknown coding", values: []string{"x-" + strings.ToLower(cfMarker)}, refused: true},
		{name: "list gzip then identity", values: []string{"gzip, identity"}, refused: true},
		{name: "list identity then gzip", values: []string{"identity, gzip"}, refused: true},
		{name: "list without spaces", values: []string{"identity,br"}, refused: true},
		{name: "list with tabs", values: []string{"identity,\tgzip\t"}, refused: true},
		{name: "two values", values: []string{"identity", "br"}, refused: true},
		{name: "identity", values: []string{"identity"}},
		{name: "upper-case identity", values: []string{"IDENTITY"}},
		{name: "mixed-case identity", values: []string{"Identity"}},
		{name: "padded identity", values: []string{" identity\t"}},
		{name: "identity twice", values: []string{"identity, identity"}},
		{name: "identity in two values", values: []string{"identity", "identity"}},
		{name: "empty value", values: []string{""}},
		{name: "empty list elements", values: []string{" , identity ,"}},
		{name: "only commas", values: []string{",,"}},
		{name: "no header", values: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.NewBody(checkOK)
			header := http.Header{cfContentType: {mediaJSON}, "Content-Encoding": tt.values}
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, header, int64(len(checkOK)), body))
			res, err := c.CheckEmail(t.Context(), checkRequest())
			assert.Equal(t, 1, body.Closes())
			if !tt.refused {
				require.NoError(t, err)
				assert.Equal(t, neverbounce.ResultValid, res.Result)
				return
			}
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureUnexpectedResponse, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
			})
			cause := errors.Unwrap(callError(t, err))
			assert.Equal(t, "neverbounce: response has an undecoded content encoding", cause.Error())
			assert.Zero(t, body.BytesRead(), "an encoded body is never read")
		})
	}
}

// cfGzipHandler always answers with a gzip-encoded checkOK, whatever the
// request accepts.
func cfGzipHandler(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(checkOK))
	_ = zw.Close()
	w.Header().Set(cfContentType, mediaJSON)
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(buf.Bytes())
}

func TestTransportGzipDecodedByTransport(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			c, srv := newServerClientProto(t, proto, cfGzipHandler)
			res, err := c.CheckEmail(t.Context(), checkRequest())
			require.NoError(t, err, "the transport decodes gzip and removes the header")
			assert.Equal(t, neverbounce.ResultValid, res.Result)
			r := testutil.OnlyRequest(t, srv)
			assert.Equal(t, "gzip", r.Header.Get("Accept-Encoding"), "the transport asked for gzip")
		})
	}
}

func TestTransportGzipUndecoded(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			srv := testutil.NewServerProto(t, proto, cfGzipHandler)
			tr, ok := srv.Client().Transport.(*http.Transport)
			require.True(t, ok)
			tr = tr.Clone()
			tr.DisableCompression = true
			t.Cleanup(tr.CloseIdleConnections)
			c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(&http.Client{Transport: tr}))
			_, err := c.CheckEmail(t.Context(), checkRequest())
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureUnexpectedResponse, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
			})
			assert.Empty(t, testutil.OnlyRequest(t, srv).Header.Values("Accept-Encoding"))
		})
	}
}

// cfAnswer returns op's successful JSON answer padded to exactly n bytes.
func cfAnswer(tb testing.TB, o opCase, n int) string {
	tb.Helper()
	return cfPadded(tb, o.ok.Body, n)
}

func TestTransportDeclaredLengthOverLimit(t *testing.T) {
	tests := []struct {
		name     string
		opts     []neverbounce.Option
		declared func(op neverbounce.Operation) int64
	}{
		{
			name: "default limits",
			declared: func(op neverbounce.Operation) int64 {
				if op == neverbounce.OpJobResults {
					return neverbounce.DefaultMaxResponseBytes + 1
				}
				return 1<<20 + 1
			},
		},
		{
			name:     "configured limit",
			opts:     []neverbounce.Option{neverbounce.WithMaxResponseBytes(100)},
			declared: func(neverbounce.Operation) int64 { return 101 },
		},
	}
	for _, tt := range tests {
		for _, o := range jsonOperations() {
			t.Run(tt.name+"/"+o.name, func(t *testing.T) {
				body := testutil.NewBody(o.ok.Body)
				rt := testutil.TransportWith(http.StatusOK, nil, tt.declared(o.op), body)
				err := o.call(t.Context(), newTransportClient(t, rt, tt.opts...))
				assertCallError(t, err, o.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
				cause := errors.Unwrap(callError(t, err))
				assert.Equal(t, "neverbounce: response body exceeds the size limit", cause.Error())
				assert.Zero(t, body.BytesRead(), "a declared length over the limit is refused unread")
				assert.Equal(t, 1, body.Closes())
			})
		}
	}
}

// cfSizedCall calls o through a body of exactly n bytes, declared or of
// unknown length, and returns the body and the error.
func cfSizedCall(t *testing.T, o opCase, n int, declared bool, opts ...neverbounce.Option) (*testutil.Body, error) {
	t.Helper()
	body := testutil.NewBody(cfAnswer(t, o, n))
	cl := int64(-1)
	if declared {
		cl = int64(n)
	}
	c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, cl, body), opts...)
	return body, o.call(t.Context(), c)
}

func TestTransportExactLimit(t *testing.T) {
	check := opCaseNamed("CheckEmail")
	for _, n := range []int{len(checkOK), 100, 4096} {
		for _, declared := range []bool{true, false} {
			t.Run(strconv.Itoa(n)+"/declared="+strconv.FormatBool(declared), func(t *testing.T) {
				opt := neverbounce.WithMaxResponseBytes(int64(n))
				body, err := cfSizedCall(t, check, n, declared, opt)
				require.NoError(t, err, "exactly n bytes is within the limit")
				assert.Equal(t, int64(n), body.BytesRead())

				// n+1 bytes is over it.
				body, err = cfSizedCall(t, check, n+1, declared, opt)
				assertCallError(t, err, check.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
				assert.LessOrEqual(t, body.BytesRead(), int64(n+1), "never more than n+1 bytes are read")
				assert.Equal(t, 1, body.Closes())
			})
		}
	}
}

func TestTransportSmallOperationsCappedAt1MiB(t *testing.T) {
	const capBytes = 1 << 20
	for _, o := range jsonOperations() {
		if o.op == neverbounce.OpJobResults {
			continue
		}
		for _, declared := range []bool{true, false} {
			t.Run(o.name+"/declared="+strconv.FormatBool(declared), func(t *testing.T) {
				opt := neverbounce.WithMaxResponseBytes(8 << 20)
				_, err := cfSizedCall(t, o, capBytes, declared, opt)
				require.NoError(t, err, "1 MiB is within the cap")

				// The 1 MiB cap applies above any configured limit.
				body, err := cfSizedCall(t, o, capBytes+1, declared, opt)
				assertCallError(t, err, o.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
				assert.LessOrEqual(t, body.BytesRead(), int64(capBytes+1))
			})
		}
	}
}

func TestTransportResultsUseConfiguredLimit(t *testing.T) {
	results := opCaseNamed("JobResults")
	require.Equal(t, neverbounce.OpJobResults, results.op)
	const limit = 2 << 20
	opt := neverbounce.WithMaxResponseBytes(limit)
	for _, declared := range []bool{true, false} {
		t.Run("declared="+strconv.FormatBool(declared), func(t *testing.T) {
			for _, n := range []int{1<<20 + 1, limit} {
				_, err := cfSizedCall(t, results, n, declared, opt)
				require.NoError(t, err, "%d bytes is within the configured limit", n)
			}
			body, err := cfSizedCall(t, results, limit+1, declared, opt)
			assertCallError(t, err, results.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
			assert.LessOrEqual(t, body.BytesRead(), int64(limit+1))
		})
	}
	t.Run("default", func(t *testing.T) {
		_, err := cfSizedCall(t, results, neverbounce.DefaultMaxResponseBytes, false)
		require.NoError(t, err, "8 MiB is within the default")
		_, err = cfSizedCall(t, results, neverbounce.DefaultMaxResponseBytes+1, false)
		assertCallError(t, err, results.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
	})
}

// cfAssertTruncated asserts that err is the transport failure, with status
// 200, of a call of o, whose cause is the fixed "closed early" class.
func cfAssertTruncated(tb testing.TB, err error, o opCase) {
	tb.Helper()
	assertCallError(tb, err, o.want(wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK}))
	assert.Equal(tb, cfCauseClosed, errors.Unwrap(callError(tb, err)).Error())
}

func TestTransportTruncatedInMemory(t *testing.T) {
	tests := []struct {
		name     string
		op       int // index into cfJSONOps
		content  string
		declared int64
		opts     []neverbounce.Option
	}{
		{name: "one byte short", content: checkOK[:len(checkOK)-1], declared: int64(len(checkOK))},
		{name: "empty body declared", content: "", declared: int64(len(checkOK))},
		{name: "body longer than declared", content: checkOK, declared: int64(len(checkOK) - 1)},
		{name: "much longer than declared", content: checkOK, declared: 2},
		{
			name: "large declared length", op: 5, content: opCaseNamed("JobResults").ok.Body, declared: 2 << 20,
			opts: []neverbounce.Option{neverbounce.WithMaxResponseBytes(4 << 20)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.NewBody(tt.content)
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, tt.declared, body), tt.opts...)
			o := jsonOperations()[tt.op]
			cfAssertTruncated(t, o.call(t.Context(), c), o)
			assert.Equal(t, 1, body.Closes())
		})
	}
}

func TestTransportReadErrorMidBody(t *testing.T) {
	for _, cl := range []int64{-1, int64(len(checkOK))} {
		t.Run(strconv.FormatInt(cl, 10), func(t *testing.T) {
			body := cfFailingBody(checkOK[:10], io.ErrUnexpectedEOF)
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, cl, body))
			_, err := c.CheckEmail(t.Context(), checkRequest())
			cfAssertTruncated(t, err, opCaseNamed("CheckEmail"))
			assert.Equal(t, 1, body.Closes())
		})
	}
}

func TestTransportTruncatedByServer(t *testing.T) {
	t.Run("http1 hijack", func(t *testing.T) {
		c, _ := newServerClient(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.Itoa(len(checkOK)) + "\r\n\r\n" + checkOK[:10])
			_ = buf.Flush()
			_ = conn.Close()
		})
		_, err := c.CheckEmail(t.Context(), checkRequest())
		cfAssertTruncated(t, err, opCaseNamed("CheckEmail"))
	})
	t.Run("http2 aborted stream", func(t *testing.T) {
		c, _ := newServerClientProto(t, testutil.HTTP2, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(cfContentType, mediaJSON)
			w.Header().Set("Content-Length", strconv.Itoa(len(checkOK)))
			_, _ = io.WriteString(w, checkOK[:10])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		})
		_, err := c.CheckEmail(t.Context(), checkRequest())
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureTransport, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
		}, "INTERNAL_ERROR", "stream")
	})
}

// The text of each fixed transport cause.
const (
	cfCauseDNS     = "neverbounce: transport: DNS lookup failed"
	cfCauseProxy   = "neverbounce: transport: proxy connection failed"
	cfCauseDial    = "neverbounce: transport: connection failed"
	cfCauseTLS     = "neverbounce: transport: TLS failed"
	cfCauseReset   = "neverbounce: transport: connection reset"
	cfCauseClosed  = "neverbounce: transport: connection closed early"
	cfCauseStalled = "neverbounce: transport: body read made no progress"
	cfCauseOther   = "neverbounce: transport: request failed"
	cfCauseTimeout = "context deadline exceeded"
)

// cfTransportErrors lists transport errors and the class each reduces to.
func cfTransportErrors() []struct {
	name  string
	err   error
	cause string
} {
	dnsErr := &net.DNSError{Err: "no such host " + cfMarker, Name: cfMarker + ".example.com", Server: cfMarker, IsNotFound: true}
	timeout := testutil.TimeoutError{Msg: "synthetic network timeout " + cfMarker}
	reset := os.NewSyscallError("read", syscall.ECONNRESET)
	tlsCert := &tls.CertificateVerificationError{Err: errCFPlanted}
	return []struct {
		name  string
		err   error
		cause string
	}{
		{"dns", dnsErr, cfCauseDNS},
		{"dns inside dial", &net.OpError{Op: "dial", Net: cfTCP, Err: dnsErr}, cfCauseDNS},
		{"dns wrapped", fmt.Errorf("lookup %s: %w", cfMarker, dnsErr), cfCauseDNS},
		{"proxy", &net.OpError{Op: "proxyconnect", Net: cfTCP, Err: errCFRefused}, cfCauseProxy},
		{"proxy reset", &net.OpError{Op: "proxyconnect", Net: cfTCP, Err: reset}, cfCauseProxy},
		{"proxy TLS", &net.OpError{Op: "proxyconnect", Net: cfTCP, Err: tlsCert}, cfCauseProxy},
		{"tls record header", tls.RecordHeaderError{Msg: "bad record " + cfMarker, RecordHeader: [5]byte{'X', 'Y', 'Z', 0, 0}}, cfCauseTLS},
		{"tls alert", tls.AlertError(40), cfCauseTLS},
		{"tls certificate", tlsCert, cfCauseTLS},
		{"tls inside a read", &net.OpError{Op: "remote error", Net: cfTCP, Err: tls.AlertError(42)}, cfCauseTLS},
		{"connection reset", &net.OpError{Op: "read", Net: cfTCP, Err: reset}, cfCauseReset},
		{"connection aborted", &net.OpError{Op: "read", Net: cfTCP, Err: os.NewSyscallError("read", syscall.ECONNABORTED)}, cfCauseReset},
		{"broken pipe", &net.OpError{Op: "write", Net: cfTCP, Err: os.NewSyscallError("write", syscall.EPIPE)}, cfCauseReset},
		{"bare reset errno", syscall.ECONNRESET, cfCauseReset},
		{"reset while dialing", &net.OpError{Op: "dial", Net: cfTCP, Err: reset}, cfCauseReset},
		{"dial refused", &net.OpError{Op: "dial", Net: cfTCP, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, cfCauseDial},
		{"dial other", &net.OpError{Op: "dial", Net: cfTCP, Err: errCFRefused}, cfCauseDial},
		{"eof", io.EOF, cfCauseClosed},
		{"unexpected eof", io.ErrUnexpectedEOF, cfCauseClosed},
		{"wrapped eof", fmt.Errorf("read %s: %w", cfMarker, io.EOF), cfCauseClosed},
		{"closed connection", net.ErrClosed, cfCauseClosed},
		{"closed inside a read", &net.OpError{Op: "read", Net: cfTCP, Err: net.ErrClosed}, cfCauseClosed},
		{"other", errCFPlanted, cfCauseOther},
		{"other inside a read", &net.OpError{Op: "read", Net: cfTCP, Err: errCFPlanted}, cfCauseOther},
		{"canceled by the transport alone", context.Canceled, cfCauseOther},
		{"url error from the transport", &url.Error{Op: "Post", URL: "https://127.0.0.1/?key=" + cfMarker, Err: errCFPlanted}, cfCauseOther},
		{"network timeout", timeout, cfCauseTimeout},
		{"dial timeout", &net.OpError{Op: "dial", Net: cfTCP, Err: timeout}, cfCauseTimeout},
		{"deadline from the transport", context.DeadlineExceeded, cfCauseTimeout},
		{"wrapped deadline", fmt.Errorf("%s: %w", cfMarker, context.DeadlineExceeded), cfCauseTimeout},
	}
}

func TestTransportErrorClasses(t *testing.T) {
	for _, tt := range cfTransportErrors() {
		t.Run(tt.name, func(t *testing.T) {
			ct := &testutil.CountingTransport{Next: testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				testutil.Drain(r)
				return nil, tt.err
			})}
			kind := neverbounce.FailureTransport
			if tt.cause == cfCauseTimeout {
				kind = neverbounce.FailureTimeout
			}
			for _, o := range cfWireOps() {
				c := newTransportClient(t, ct)
				err := o.call(t.Context(), c)
				assertCallError(t, err, o.want(wantCall{Kind: kind}), cfMarker, "127.0.0.1", "/v4.2/")
				assert.Equal(t, tt.cause, errors.Unwrap(callError(t, err)).Error(), "the fixed class of the cause")
			}
			assert.Equal(t, len(cfWireOps()), ct.Attempts(), "exactly one attempt per call")
		})
	}
}

func TestTransportRealTLSFailure(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			srv := testutil.NewServerProto(t, proto, respond(http.StatusOK, checkOK))
			// A client without a transport uses http.DefaultTransport, which
			// does not trust the test server's certificate.
			c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(&http.Client{}))
			_, err := c.CheckEmail(t.Context(), checkRequest())
			assertCallError(t, err, wantCall{Kind: neverbounce.FailureTransport, Op: neverbounce.OpCheckEmail},
				srv.URL(), strings.TrimPrefix(srv.URL(), "https://"), "x509", "certificate")
			assert.Equal(t, cfCauseTLS, errors.Unwrap(callError(t, err)).Error())
			assert.Zero(t, srv.Hits(), "the handshake fails before any request is sent")
		})
	}
}

func TestTransportRealDialFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	base, hc := srv.URL, srv.Client()
	srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(base, "https://"))
	require.NoError(t, err)

	ct := &testutil.CountingTransport{Next: hc.Transport}
	c := newClient(t, neverbounce.WithTestBaseURL(base), neverbounce.WithHTTPClient(&http.Client{Transport: ct}))
	_, err = c.CheckEmail(t.Context(), checkRequest())
	assertCallError(t, err, wantCall{Kind: neverbounce.FailureTransport, Op: neverbounce.OpCheckEmail},
		":"+port, "127.0.0.1", "refused")
	assert.Equal(t, cfCauseDial, errors.Unwrap(callError(t, err)).Error())
	assert.Equal(t, 1, ct.Attempts())
}
