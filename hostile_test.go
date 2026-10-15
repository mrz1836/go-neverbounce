package neverbounce_test

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// TestHostileInformationalStatusNeverRead proves that a 1xx response that
// reaches the SDK, such as 101 Switching Protocols with the raw connection as
// its body, is classified at once, for every operation, without reading that
// body. The bubble's clock shows that no time passes.
func TestHostileInformationalStatusNeverRead(t *testing.T) {
	for _, status := range []int{http.StatusContinue, http.StatusSwitchingProtocols, http.StatusEarlyHints, 199} {
		for _, o := range cfWireOps() {
			t.Run(strconv.Itoa(status)+"/"+o.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// The raw connection ignores every context: a Read blocks until Close.
					body := testutil.NewBody("")
					body.Block = true
					header := http.Header{cfContentType: {mediaOctet}}
					ct := &testutil.CountingTransport{Next: testutil.TransportWith(status, header, -1, body)}
					c := newTransportClient(t, ct)
					elapsed, err := cfTimed(func() error { return o.call(t.Context(), c) })
					assertCallError(t, err, o.want(wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: status}))
					assert.Zero(t, elapsed, "the call returns at once")
					assert.Zero(t, body.Reads(), "a 1xx body is never read")
					assert.Equal(t, 1, body.Closes())
					assert.Equal(t, 1, ct.Attempts())
				})
			})
		}
	}
}

func TestHostileSwitchingProtocolsOverTheWire(t *testing.T) {
	for _, o := range []cfWireOp{cfWireOps()[0], cfWireOps()[6]} {
		t.Run(o.name, func(t *testing.T) {
			release := make(chan struct{})
			c, srv := newServerClient(t, func(w http.ResponseWriter, _ *http.Request) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: synthetic\r\nConnection: Upgrade\r\n\r\n")
				_ = buf.Flush()
				// Hold the upgraded connection open, as a server would.
				<-release
			}, neverbounce.WithTimeout(time.Minute), neverbounce.WithDownloadTimeout(time.Minute))
			// Cleanups run last first, so the handler is released before the
			// server closes.
			t.Cleanup(func() { close(release) })

			start := time.Now()
			err := o.call(t.Context(), c)
			assertCallError(t, err, o.want(wantCall{
				Kind: neverbounce.FailureUnexpectedResponse, Status: http.StatusSwitchingProtocols,
			}))
			assert.Less(t, time.Since(start), 10*time.Second, "the call must not wait for the timeout")
			assert.Equal(t, 1, srv.Hits())
		})
	}
}

func TestHostileEndlessBodyBounded(t *testing.T) {
	const limit = 4096
	for _, o := range jsonOperations() {
		t.Run(o.name, func(t *testing.T) {
			body := &testutil.EndlessBody{}
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, -1, body),
				neverbounce.WithMaxResponseBytes(limit))
			err := o.call(t.Context(), c)
			assertCallError(t, err, o.want(wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}))
			assert.Equal(t, int64(limit+1), body.BytesRead(), "exactly one byte past the limit is read")
			assert.Equal(t, 1, body.Closes())
		})
	}
	tooLarge := wantCall{Kind: neverbounce.FailureResponseTooLarge, Op: neverbounce.OpCheckEmail, Status: http.StatusOK}
	t.Run("default cap", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, -1, body))
		_, err := c.CheckEmail(t.Context(), checkRequest())
		assertCallError(t, err, tooLarge)
		assert.Equal(t, int64(1<<20+1), body.BytesRead())
	})
	t.Run("declared small", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, 10, body),
			neverbounce.WithMaxResponseBytes(limit))
		_, err := c.CheckEmail(t.Context(), checkRequest())
		assertCallError(t, err, tooLarge)
		assert.Equal(t, int64(limit+1), body.BytesRead())
	})
	t.Run("declared huge", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, 1<<62, body))
		_, err := c.CheckEmail(t.Context(), checkRequest())
		assertCallError(t, err, tooLarge)
		assert.Zero(t, body.BytesRead())
	})
	t.Run("non-200 excerpt", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		c := newTransportClient(t, testutil.TransportWith(http.StatusInternalServerError, nil, -1, body))
		_, err := c.CheckEmail(t.Context(), checkRequest())
		// An excerpt cut at its bound yields no APIStatus.
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureUpstream, Op: neverbounce.OpCheckEmail, Status: http.StatusInternalServerError,
		})
		assert.Equal(t, int64(64<<10+1), body.BytesRead())
		assert.Equal(t, 1, body.Closes())
	})
	t.Run("download", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		header := http.Header{cfContentType: {mediaOctet}}
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, header, -1, body),
			neverbounce.WithMaxDownloadBytes(100_000))
		w := &testutil.CountingWriter{}
		n, err := c.DownloadJob(t.Context(), neverbounce.DownloadJobRequest{JobID: opJobID}, w)
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureResponseTooLarge, Op: neverbounce.OpDownloadJob, Status: http.StatusOK,
			JobID: opJobID, Written: 100_000,
		})
		assert.Equal(t, int64(100_000), n)
		assert.Equal(t, 100_000, w.Len())
		assert.LessOrEqual(t, body.BytesRead(), int64(100_000+64<<10+1))
		assert.Equal(t, 1, body.Closes())
	})
}

// cfClosedOnce is one response, for TestHostileBodyClosedExactlyOnce: the
// operation, the answer, and the failure expected: its kind ("" for success)
// and the CallError fields that the answer sets.
type cfClosedOnce struct {
	name     string
	op       int // index into cfWireOps
	status   int
	header   http.Header
	content  string
	declared int64 // -2 declares len(content)
	opts     []neverbounce.Option
	kind     neverbounce.FailureKind
	api      neverbounce.APIStatus // the CallError's APIStatus
	field    string                // the CallError's Field
	written  int64                 // the CallError's BytesWritten
}

// cfClosedOnceCases lists a response for every path a call can take.
func cfClosedOnceCases() []cfClosedOnce {
	const exact = -2
	octet := http.Header{cfContentType: {mediaOctet}}
	return []cfClosedOnce{
		{name: "success", content: checkOK, declared: exact},
		{name: "success of unknown length", content: checkOK, declared: -1},
		{name: "success declared with a negative length", content: checkOK, declared: -5},
		{
			name: "non-200", status: 500, content: `{"status":"temp_unavail"}`, declared: exact,
			kind: neverbounce.FailureUpstream, api: neverbounce.APIStatusTempUnavail,
		},
		{name: "non-200 declared oversize", status: 404, content: "{}", declared: 1 << 20, kind: neverbounce.FailureRejected},
		{name: "non-200 streamed oversize", status: 502, content: strings.Repeat(" ", 70_000), declared: -1, kind: neverbounce.FailureUpstream},
		{name: "redirect", status: 307, content: "", declared: exact, kind: neverbounce.FailureUnexpectedResponse},
		{name: "informational", status: 101, content: checkOK, declared: -1, kind: neverbounce.FailureUnexpectedResponse},
		{name: "declared oversize", content: checkOK, declared: 2 << 20, kind: neverbounce.FailureResponseTooLarge},
		{
			name: "streamed oversize", content: checkOK + "      ", declared: -1, kind: neverbounce.FailureResponseTooLarge,
			opts: []neverbounce.Option{neverbounce.WithMaxResponseBytes(int64(len(checkOK)))},
		},
		{
			name: "content encoding", header: http.Header{"Content-Encoding": {"gzip"}}, content: checkOK,
			declared: exact, kind: neverbounce.FailureUnexpectedResponse,
		},
		{name: "truncated", content: checkOK[:5], declared: int64(len(checkOK)), kind: neverbounce.FailureTransport},
		{name: "declares 0 but sends a body", content: checkOK, declared: 0, kind: neverbounce.FailureTransport},
		{name: "invalid JSON", content: `{"status":"success",}`, declared: exact, kind: neverbounce.FailureMalformedResponse},
		{name: "trailing data", content: checkOK + "{}", declared: exact, kind: neverbounce.FailureMalformedResponse},
		{name: "invalid UTF-8", content: "{\"status\":\"success\",\"result\":\"\xff\"}", declared: exact, kind: neverbounce.FailureMalformedResponse},
		{name: "empty body", content: "", declared: exact, kind: neverbounce.FailureMalformedResponse},
		{name: "not an object", content: `[]`, declared: exact, kind: neverbounce.FailureUnexpectedResponse},
		{
			name: "envelope failure", content: `{"status":"general_failure"}`, declared: exact,
			kind: neverbounce.FailureRejected, api: neverbounce.APIStatusGeneralFailure,
		},
		{
			name: "wrong field type", content: `{"status":"success","result":42}`, declared: exact,
			kind: neverbounce.FailureMalformedResponse, field: "result",
		},
		{
			name: "invalid token", content: `{"status":"success","result":"Not Valid"}`, declared: exact,
			kind: neverbounce.FailureUnexpectedResponse, field: "result",
		},
		{
			name: "create without job_id", op: 1, content: `{"status":"success"}`, declared: exact,
			kind: neverbounce.FailureMalformedResponse, field: "job_id",
		},
		{
			name: "status for another job", op: 4, content: `{"status":"success","id":43,"job_status":"complete"}`,
			declared: exact, kind: neverbounce.FailureUnexpectedResponse, field: "id",
		},
		{
			name: "results with too many rows", op: 5, declared: exact,
			kind: neverbounce.FailureUnexpectedResponse, field: "results",
			content: `{"status":"success","total_results":11,"total_pages":1,"results":[` +
				strings.TrimSuffix(strings.Repeat(`{"verification":{"result":"valid"}},`, 11), ",") + `]}`,
		},
		{name: "download success", op: 6, header: octet, content: csvFile, declared: exact},
		{name: "download non-200", op: 6, status: 429, header: octet, content: csvFile, declared: exact, kind: neverbounce.FailureRateLimited},
		{
			name: "download JSON envelope", op: 6, content: `{"status":"general_failure"}`, declared: exact,
			kind: neverbounce.FailureRejected, api: neverbounce.APIStatusGeneralFailure,
		},
		{
			name: "download wrong content type", op: 6, header: http.Header{cfContentType: {"text/html"}}, content: csvFile,
			declared: exact, kind: neverbounce.FailureUnexpectedResponse,
		},
		{
			name: "download oversize", op: 6, header: octet, content: csvFile, declared: -1, kind: neverbounce.FailureResponseTooLarge,
			opts: []neverbounce.Option{neverbounce.WithMaxDownloadBytes(3)}, written: 3,
		},
		{
			name: "download encoded", op: 6, header: http.Header{cfContentType: {mediaOctet}, "Content-Encoding": {"br"}},
			content: csvFile, declared: exact, kind: neverbounce.FailureUnexpectedResponse,
		},
	}
}

// TestHostileBodyClosedExactlyOnce proves that the response body is closed
// exactly once on every path a call can take.
func TestHostileBodyClosedExactlyOnce(t *testing.T) {
	for _, tt := range cfClosedOnceCases() {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			declared := tt.declared
			if declared == -2 {
				declared = int64(len(tt.content))
			}
			body := testutil.NewBody(tt.content)
			ct := &testutil.CountingTransport{Next: testutil.TransportWith(status, tt.header, declared, body)}
			c := newTransportClient(t, ct, tt.opts...)
			o := cfWireOps()[tt.op]
			err := o.call(t.Context(), c)
			assert.Equal(t, 1, body.Closes(), "the body is closed exactly once")
			assert.Equal(t, 1, ct.Attempts())
			if tt.kind == "" {
				require.NoError(t, err)
				return
			}
			want := o.want(wantCall{Kind: tt.kind, Status: status})
			want.API, want.Field, want.Written = tt.api, tt.field, tt.written
			assertCallError(t, err, want)
		})
	}
}

// cfCloseErrBody fails its Close.
type cfCloseErrBody struct {
	io.Reader

	closes atomic.Int32
}

func (b *cfCloseErrBody) Close() error {
	b.closes.Add(1)
	return errCFPlanted
}

func TestHostileCloseErrorIgnored(t *testing.T) {
	for _, o := range cfWireOps() {
		t.Run(o.name, func(t *testing.T) {
			body := &cfCloseErrBody{Reader: strings.NewReader(o.ok.Body)}
			rt := testutil.TransportWith(http.StatusOK, o.ok.Header, int64(len(o.ok.Body)), body)
			c := newTransportClient(t, rt)
			require.NoError(t, o.call(t.Context(), c), "a close error after a complete read changes nothing")
			assert.Equal(t, int32(1), body.closes.Load())
		})
	}
}

func TestHostileNilBodyAndHeader(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, http.Header{}, 0, nil))
		_, err := c.CheckEmail(t.Context(), checkRequest())
		// An absent body is an empty one.
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureMalformedResponse, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
		})
	})
	t.Run("nil header", func(t *testing.T) {
		// TransportWith would keep its default header, so build the response.
		c := newTransportClient(t, testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			testutil.Drain(r)
			return &http.Response{
				StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(checkOK)),
				ContentLength: int64(len(checkOK)), Request: r,
			}, nil
		}))
		res, err := c.CheckEmail(t.Context(), checkRequest())
		require.NoError(t, err)
		assert.Equal(t, neverbounce.ResultValid, res.Result)
	})
}

// cfHostileHeaders returns a set of hostile response headers, around a
// JSON Content-Type, that the SDK must never surface.
func cfHostileHeaders() http.Header {
	return http.Header{
		cfContentType: {mediaJSON, "text/" + cfMarker},
		"Location":    {"https://" + cfMarker + ".example.com/?key=" + testKey},
		"Retry-After": {cfMarker},
		"Set-Cookie":  {"session=" + cfMarker},
		"X-Debug":     {testKey + " " + testEmail},
	}
}

func TestHostileHeadersNeverSurface(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := io.NopCloser(strings.NewReader(checkOK))
			c := newTransportClient(t, testutil.TransportWith(status, cfHostileHeaders(), int64(len(checkOK)), body),
				neverbounce.WithProviderDiagnostics())
			_, err := c.CheckEmail(t.Context(), checkRequest())
			if status == http.StatusOK {
				require.NoError(t, err, "unknown and duplicate headers do not matter to a JSON call")
				return
			}
			assertCallError(t, err, wantCall{
				Kind: statusKind(status), Op: neverbounce.OpCheckEmail,
				Status: status, API: neverbounce.APIStatusSuccess,
			}, cfMarker)
			assert.Empty(t, callError(t, err).Diagnostics().Message)
		})
	}
}

// TestHostileNoProgressBodyFailsAsStalled proves that a body whose reads
// never make progress fails promptly as a transport failure, well before the
// call's timeout, and is closed.
func TestHostileNoProgressBodyFailsAsStalled(t *testing.T) {
	for _, declared := range []int64{-1, 100} {
		t.Run(strconv.FormatInt(declared, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Every read returns (0, nil): the body never makes progress, but
				// it honors Close.
				body := testutil.NewBody("")
				body.Stall = true
				c := newTransportClient(t, testutil.TransportWith(http.StatusOK, nil, declared, body))
				elapsed, err := cfTimed(func() error {
					_, err := c.CheckEmail(t.Context(), checkRequest())
					return err
				})
				assertCallError(t, err, wantCall{
					Kind: neverbounce.FailureTransport, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
				})
				assert.Equal(t, cfCauseStalled, errors.Unwrap(callError(t, err)).Error())
				assert.Zero(t, elapsed, "no wait for the timeout")
				assert.Equal(t, 1, body.Closes())
			})
		})
	}
}
