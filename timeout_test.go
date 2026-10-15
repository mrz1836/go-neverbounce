package neverbounce_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// cfStallBody serves its Body's content and then stalls, as a dead
// connection does, until the request's context ends: wait blocks until then
// and returns the context's error. The Body counts the reads and closes. The
// Body's own Block cannot stand in, because only Close ends it, and an
// http.Client Timeout ends the request's context without closing the body.
type cfStallBody struct {
	*testutil.Body

	wait func() error
}

// Read serves the content, then stalls.
func (b cfStallBody) Read(p []byte) (int, error) {
	n, err := b.Body.Read(p)
	if errors.Is(err, io.EOF) {
		return n, b.wait()
	}
	return n, err
}

// cfStalling returns a transport that answers with status, contentType and
// body, which stalls after its content until the request's context ends. A
// status of 0 stalls before any response instead.
func cfStalling(status int, contentType string, body *testutil.Body) testutil.RoundTripFunc {
	if status == 0 {
		return testutil.BlockUntilDone
	}
	return func(r *http.Request) (*http.Response, error) {
		ctx := r.Context()
		stall := cfStallBody{Body: body, wait: func() error {
			<-ctx.Done()
			return ctx.Err()
		}}
		return testutil.TransportWith(status, http.Header{cfContentType: {contentType}}, -1, stall)(r)
	}
}

// cfTimed runs call and returns the time it took, which inside a bubble is
// the fake clock's exact elapsed time.
func cfTimed(call func() error) (time.Duration, error) {
	start := time.Now()
	err := call()
	return time.Since(start), err
}

// cfAssertTimedOut asserts that err is the SDK's timeout, never the caller's
// deadline: the CallError want describes, with kind FailureTimeout, whose
// cause is context.DeadlineExceeded itself.
func cfAssertTimedOut(tb testing.TB, err error, want wantCall) {
	tb.Helper()
	want.Kind = neverbounce.FailureTimeout
	assertCallError(tb, err, want)
	cause := errors.Unwrap(callError(tb, err))
	assert.Equal(tb, context.DeadlineExceeded, cause, "the cause is the context error alone")
}

func TestTimeoutBoundsTheWholeCall(t *testing.T) {
	tests := []struct {
		name          string
		status        int           // 0 stalls before the response headers
		sdkTimeout    time.Duration // 0 keeps DefaultTimeout
		clientTimeout time.Duration // an injected http.Client's Timeout
		callerTimeout time.Duration // the caller's context deadline
		wantKind      neverbounce.FailureKind
		wantStatus    int
		wantElapsed   time.Duration
	}{
		{
			name: "default timeout awaiting headers", wantKind: neverbounce.FailureTimeout, wantElapsed: neverbounce.DefaultTimeout,
		},
		{
			name: "custom timeout awaiting headers", sdkTimeout: 7 * time.Second,
			wantKind: neverbounce.FailureTimeout, wantElapsed: 7 * time.Second,
		},
		{
			name: "default timeout mid-body", status: http.StatusOK, wantKind: neverbounce.FailureTimeout,
			wantStatus: http.StatusOK, wantElapsed: neverbounce.DefaultTimeout,
		},
		{
			name: "custom timeout mid-body", status: http.StatusOK, sdkTimeout: 11 * time.Second,
			wantKind: neverbounce.FailureTimeout, wantStatus: http.StatusOK, wantElapsed: 11 * time.Second,
		},
		{
			name: "maximum timeout", sdkTimeout: 10 * time.Minute,
			wantKind: neverbounce.FailureTimeout, wantElapsed: 10 * time.Minute,
		},
		{
			name: "caller deadline awaiting headers", callerTimeout: 5 * time.Second,
			wantKind: neverbounce.FailureDeadlineExceeded, wantElapsed: 5 * time.Second,
		},
		{
			name: "caller deadline mid-body", status: http.StatusOK, callerTimeout: 5 * time.Second,
			wantKind: neverbounce.FailureDeadlineExceeded, wantStatus: http.StatusOK, wantElapsed: 5 * time.Second,
		},
		{
			name: "SDK timeout shorter than the caller deadline", sdkTimeout: 4 * time.Second, callerTimeout: 6 * time.Second,
			wantKind: neverbounce.FailureTimeout, wantElapsed: 4 * time.Second,
		},
		{
			name: "caller deadline shorter than the SDK timeout", sdkTimeout: 9 * time.Second, callerTimeout: 3 * time.Second,
			wantKind: neverbounce.FailureDeadlineExceeded, wantElapsed: 3 * time.Second,
		},
		{
			name: "injected client timeout awaiting headers", clientTimeout: 3 * time.Second,
			wantKind: neverbounce.FailureTimeout, wantElapsed: 3 * time.Second,
		},
		{
			name: "injected client timeout mid-body", status: http.StatusOK, clientTimeout: 3 * time.Second,
			wantKind: neverbounce.FailureTimeout, wantStatus: http.StatusOK, wantElapsed: 3 * time.Second,
		},
		{
			name: "non-200 excerpt stalls until the timeout", status: http.StatusServiceUnavailable, sdkTimeout: 9 * time.Second,
			wantKind: neverbounce.FailureUnavailable, wantStatus: http.StatusServiceUnavailable, wantElapsed: 9 * time.Second,
		},
		{
			name: "non-200 excerpt stalls until the caller deadline", status: http.StatusTooManyRequests, callerTimeout: 2 * time.Second,
			wantKind: neverbounce.FailureRateLimited, wantStatus: http.StatusTooManyRequests, wantElapsed: 2 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := testutil.NewBody(`{"status"`)
				ct := &testutil.CountingTransport{Next: cfStalling(tt.status, mediaJSON, body)}
				opts := []neverbounce.Option{
					neverbounce.WithTestBaseURL(loopbackOrigin),
					neverbounce.WithHTTPClient(&http.Client{Transport: ct, Timeout: tt.clientTimeout}),
				}
				if tt.sdkTimeout > 0 {
					opts = append(opts, neverbounce.WithTimeout(tt.sdkTimeout))
				}
				c := newClient(t, opts...)
				ctx := t.Context()
				if tt.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.callerTimeout)
					defer cancel()
				}
				elapsed, err := cfTimed(func() error {
					_, err := c.CheckEmail(ctx, checkRequest())
					return err
				})
				// Only FailureTimeout matches ErrTimeout; it and FailureDeadlineExceeded
				// match context.DeadlineExceeded.
				assertCallError(t, err, wantCall{
					Kind: tt.wantKind, Op: neverbounce.OpCheckEmail, Status: tt.wantStatus,
				})
				assert.Equal(t, tt.wantElapsed, elapsed, "the call must end exactly at the deadline")
				assert.Equal(t, 1, ct.Attempts(), "exactly one attempt")
				if tt.status != 0 {
					assert.Equal(t, 1, body.Closes(), "the body is closed exactly once")
				}
			})
		})
	}
}

func TestTimeoutAppliesToEveryJSONOperation(t *testing.T) {
	for _, o := range jsonOperations() {
		for _, status := range []int{0, http.StatusOK} {
			name := o.name + "/awaiting headers"
			if status != 0 {
				name = o.name + "/mid-body"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					body := testutil.NewBody(`{"status":"success",`)
					c := newTransportClient(t, cfStalling(status, mediaJSON, body),
						neverbounce.WithTimeout(7*time.Second), neverbounce.WithDownloadTimeout(time.Hour))
					elapsed, err := cfTimed(func() error { return o.call(t.Context(), c) })
					cfAssertTimedOut(t, err, o.want(wantCall{Kind: neverbounce.FailureTimeout, Status: status}))
					assert.Equal(t, 7*time.Second, elapsed)
				})
			})
		}
	}
}

func TestTimeoutCallerDeadlineIsNotErrTimeout(t *testing.T) {
	for _, o := range cfWireOps() {
		t.Run(o.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newTransportClient(t, testutil.RoundTripFunc(testutil.BlockUntilDone))
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				elapsed, err := cfTimed(func() error { return o.call(ctx, c) })
				// The caller's own deadline is not the SDK's timeout: it matches
				// context.DeadlineExceeded, never ErrTimeout.
				assertCallError(t, err, o.want(wantCall{Kind: neverbounce.FailureDeadlineExceeded}))
				assert.Equal(t, 8*time.Second, elapsed)
			})
		})
	}
}

// TestTimeoutDeadlineCarriedToTransport proves that the request's context
// carries the call's deadline: the SDK timeout for a JSON call, the download
// timeout for a download, or the caller's when it is earlier.
func TestTimeoutDeadlineCarriedToTransport(t *testing.T) {
	tests := []struct {
		name   string
		op     int           // index into cfWireOps
		caller time.Duration // 0 for none
		want   time.Duration
	}{
		{name: "JSON call", op: 0, want: 13 * time.Second},
		{name: "download", op: 6, want: 2 * time.Hour},
		{name: "caller deadline earlier", op: 0, caller: 4 * time.Second, want: 4 * time.Second},
		{name: "caller deadline later", op: 4, caller: time.Minute, want: 13 * time.Second},
		{name: "caller deadline earlier than a download", op: 6, caller: time.Minute, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				o := cfWireOps()[tt.op]
				var left time.Duration
				var hasDeadline bool
				s := testutil.NewScript(testutil.Step{Respond: func(r *http.Request) (*http.Response, error) {
					var deadline time.Time
					deadline, hasDeadline = r.Context().Deadline()
					left = time.Until(deadline)
					return testutil.NewScript(o.ok).RoundTrip(r)
				}})
				c := newTransportClient(t, s, neverbounce.WithTimeout(13*time.Second), neverbounce.WithDownloadTimeout(2*time.Hour))
				ctx := t.Context()
				if tt.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.caller)
					defer cancel()
				}
				require.NoError(t, o.call(ctx, c))
				require.True(t, hasDeadline, "every call carries a deadline")
				assert.Equal(t, tt.want, left)
			})
		})
	}
}

func TestTimeoutCallerCancel(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		after      time.Duration
		wantStatus int
	}{
		{name: "awaiting headers"},
		{name: "awaiting headers after a while", after: 2 * time.Second},
		{name: "mid-body", status: http.StatusOK, wantStatus: http.StatusOK},
		{name: "mid-body after a while", status: http.StatusOK, after: 3 * time.Second, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := testutil.NewBody(`{`)
				ct := &testutil.CountingTransport{Next: cfStalling(tt.status, mediaJSON, body)}
				c := newTransportClient(t, ct)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				type outcome struct {
					elapsed time.Duration
					err     error
				}
				done := make(chan outcome, 1)
				go func() {
					elapsed, err := cfTimed(func() error {
						_, err := c.CheckEmail(ctx, checkRequest())
						return err
					})
					done <- outcome{elapsed: elapsed, err: err}
				}()
				if tt.after > 0 {
					<-time.After(tt.after)
				}
				synctest.Wait()
				require.Empty(t, done, "the call must still be waiting")
				cancel()
				out := <-done

				assertCallError(t, out.err, wantCall{
					Kind: neverbounce.FailureCanceled, Op: neverbounce.OpCheckEmail, Status: tt.wantStatus,
				})
				assert.Equal(t, tt.after, out.elapsed, "the call ends when the caller cancels")
				assert.Equal(t, 1, ct.Attempts())
				if tt.status != 0 {
					assert.Equal(t, 1, body.Closes())
				}
			})
		})
	}
}

// cfDownloadCall downloads job 42 into w and returns the time it took.
func cfDownloadCall(ctx context.Context, c *neverbounce.Client, w io.Writer) (time.Duration, int64, error) {
	var n int64
	elapsed, err := cfTimed(func() error {
		var err error
		n, err = c.DownloadJob(ctx, neverbounce.DownloadJobRequest{JobID: opJobID}, w)
		return err
	})
	return elapsed, n, err
}

func TestTimeoutDownloadUsesItsOwnTimeout(t *testing.T) {
	tests := []struct {
		name        string
		opts        []neverbounce.Option
		data        string // delivered before the stall; empty stalls before the headers
		wantElapsed time.Duration
	}{
		{
			name: "default download timeout awaiting headers", opts: []neverbounce.Option{neverbounce.WithTimeout(time.Second)},
			wantElapsed: neverbounce.DefaultDownloadTimeout,
		},
		{
			name:        "custom download timeout awaiting headers",
			opts:        []neverbounce.Option{neverbounce.WithTimeout(5 * time.Second), neverbounce.WithDownloadTimeout(20 * time.Second)},
			wantElapsed: 20 * time.Second,
		},
		{
			name:        "custom download timeout while sniffing",
			opts:        []neverbounce.Option{neverbounce.WithTimeout(5 * time.Second), neverbounce.WithDownloadTimeout(20 * time.Second)},
			data:        "email\r\n",
			wantElapsed: 20 * time.Second,
		},
		{
			name:        "custom download timeout while streaming",
			opts:        []neverbounce.Option{neverbounce.WithTimeout(5 * time.Second), neverbounce.WithDownloadTimeout(40 * time.Second)},
			data:        strings.Repeat("a@example.com\r\n", 10_000),
			wantElapsed: 40 * time.Second,
		},
		{
			name:        "maximum download timeout",
			opts:        []neverbounce.Option{neverbounce.WithDownloadTimeout(12 * time.Hour)},
			wantElapsed: 12 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := testutil.NewBody(tt.data)
				status := 0
				if tt.data != "" {
					status = http.StatusOK
				}
				c := newTransportClient(t, cfStalling(status, mediaOctet, body), tt.opts...)
				w := &testutil.CountingWriter{}
				elapsed, n, err := cfDownloadCall(t.Context(), c, w)
				var written int64 // nothing is written before the body is classified
				if len(tt.data) > 64<<10 {
					written = int64(len(tt.data)) // everything delivered before the stall was written
				}
				cfAssertTimedOut(t, err, wantCall{
					Op: neverbounce.OpDownloadJob, Status: status, JobID: opJobID, Written: written,
				})
				assert.Equal(t, tt.wantElapsed, elapsed)
				assert.Equal(t, written, n)
				assert.Equal(t, int64(w.Len()), n)
			})
		})
	}
}

// TestTimeoutJSONTimeoutDoesNotApplyToDownload proves that the JSON timeout
// leaves a download alone, and the download timeout leaves JSON calls alone.
func TestTimeoutJSONTimeoutDoesNotApplyToDownload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTransportClient(t, testutil.RoundTripFunc(testutil.BlockUntilDone),
			neverbounce.WithTimeout(5*time.Second), neverbounce.WithDownloadTimeout(20*time.Second))
		elapsed, _, err := cfDownloadCall(t.Context(), c, io.Discard)
		cfAssertTimedOut(t, err, wantCall{Op: neverbounce.OpDownloadJob, JobID: opJobID})
		assert.Equal(t, 20*time.Second, elapsed)

		elapsed, err = cfTimed(func() error {
			_, callErr := c.CheckEmail(t.Context(), checkRequest())
			return callErr
		})
		cfAssertTimedOut(t, err, wantCall{Op: neverbounce.OpCheckEmail})
		assert.Equal(t, 5*time.Second, elapsed)
	})
}

func TestTimeoutDownloadOutlastsJSONTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const want = "email\r\na@example.com\r\nb@example.com\r\nc@example.com\r\n"
		// Four reads of 13 bytes, each 2s after the last, take 8s in all.
		body := testutil.NewBody(want)
		body.Step, body.Gap = 13, 2*time.Second
		header := http.Header{cfContentType: {mediaCSV}}
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, header, int64(len(want)), body),
			neverbounce.WithTimeout(time.Second))
		w := &testutil.CountingWriter{}
		elapsed, n, err := cfDownloadCall(t.Context(), c, w)
		require.NoError(t, err, "WithTimeout never cuts a download")
		assert.Equal(t, 8*time.Second, elapsed)
		assert.Equal(t, int64(len(want)), n)
		assert.Equal(t, want, string(w.Bytes()))
	})
}

func TestTimeoutDownloadCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := testutil.NewBody("email\r\n")
		c := newTransportClient(t, cfStalling(http.StatusOK, mediaOctet, body))
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		elapsed, n, err := cfDownloadCall(ctx, c, io.Discard)
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureDeadlineExceeded, Op: neverbounce.OpDownloadJob,
			Status: http.StatusOK, JobID: opJobID,
		})
		assert.Equal(t, 90*time.Second, elapsed)
		assert.Zero(t, n)
		assert.Equal(t, 1, body.Closes())
	})
}
