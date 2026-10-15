package neverbounce

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// errCFRead is the error a failing reader returns.
var errCFRead = errors.New("cf: read failed")

// How long, and how often, to poll for an asynchronous close.
const (
	cfWait = 5 * time.Second
	cfTick = time.Millisecond
)

func TestTransportRequestHeader(t *testing.T) {
	for _, op := range []Operation{OpCheckEmail, OpCreateJob, OpParseJob, OpStartJob, OpJobStatus, OpJobResults, OpDownloadJob} {
		t.Run(string(op), func(t *testing.T) {
			accept := "application/json"
			if op == OpDownloadJob {
				accept = "*/*"
			}
			h := requestHeader(op)
			assert.Equal(t, http.Header{
				"Content-Type": {"application/json"},
				"Accept":       {accept},
				"User-Agent":   {"go-neverbounce"},
			}, h)
			for k, v := range h {
				assert.Equal(t, 1, cap(v), "a value must leave no room for an append to overwrite the next: %s", k)
			}

			// Each call gets fresh memory: changing one header map changes no other.
			other := requestHeader(op)
			h["Accept"] = append(h["Accept"], "text/plain")
			h["Content-Type"][0] = "changed"
			h.Set("Authorization", internalKey)
			assert.Equal(t, []string{"go-neverbounce"}, h["User-Agent"])
			assert.Equal(t, []string{accept}, other["Accept"])
			assert.Equal(t, []string{"application/json"}, other["Content-Type"])
			assert.Empty(t, other.Values("Authorization"))
		})
	}
}

func TestTransportEncoded(t *testing.T) {
	tests := []struct {
		values []string
		want   bool
	}{
		{values: nil, want: false},
		{values: []string{""}, want: false},
		{values: []string{"identity"}, want: false},
		{values: []string{"IdEnTiTy"}, want: false},
		{values: []string{" \tidentity\t "}, want: false},
		{values: []string{",identity,,"}, want: false},
		{values: []string{"identity", "", "identity"}, want: false},
		{values: []string{"gzip"}, want: true},
		{values: []string{"identity", "gzip"}, want: true},
		{values: []string{"identity,gzip"}, want: true},
		{values: []string{"identity;q=1"}, want: true},
		{values: []string{"x"}, want: true},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.values != nil {
			h["Content-Encoding"] = tt.values
		}
		assert.Equal(t, tt.want, encoded(h), "%q", tt.values)
	}
}

// cfFailingBody returns a body that serves content, then fails with errCFRead.
func cfFailingBody(content string) *testutil.Body {
	body := testutil.NewBody(content)
	body.End = errCFRead
	return body
}

func TestTransportReadBody(t *testing.T) {
	big := strings.Repeat("b", maxPrealloc+10)
	tests := []struct {
		name     string
		r        io.Reader
		declared int64
		max      int64
		want     string
		err      error
	}{
		{name: "unknown length", r: strings.NewReader("hello"), declared: -1, max: 10, want: "hello"},
		{name: "exact declared", r: strings.NewReader("hello"), declared: 5, max: 10, want: "hello"},
		{name: "declared longer than sent", r: strings.NewReader("hel"), declared: 5, max: 10, want: "hel"},
		{name: "declared shorter than sent", r: strings.NewReader("hello"), declared: 2, max: 10, want: "hello"},
		{name: "declared zero, sent nothing", r: strings.NewReader(""), declared: 0, max: 10, want: ""},
		{name: "over the limit by one", r: strings.NewReader("hello!"), declared: -1, max: 5, want: "hello!"},
		{name: "far over the limit", r: strings.NewReader(strings.Repeat("x", 100)), declared: -1, max: 5, want: "xxxxxx"},
		{name: "declared short, far over the limit", r: strings.NewReader(strings.Repeat("x", 100)), declared: 2, max: 5, want: "xxxxxx"},
		{name: "large declared length", r: strings.NewReader(big), declared: int64(len(big)), max: int64(len(big)), want: big},
		{name: "read error", r: cfFailingBody("he"), declared: -1, max: 10, want: "he", err: errCFRead},
		{name: "read error with declared length", r: cfFailingBody("he"), declared: 5, max: 10, want: "he", err: errCFRead},
		{name: "read error past the declared length", r: cfFailingBody("hello"), declared: 2, max: 10, want: "hello", err: errCFRead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readBody(tt.r, tt.declared, tt.max)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestTransportExcerpt(t *testing.T) {
	full := strings.Repeat("e", maxErrorBodyBytes)
	tests := []struct {
		name     string
		body     *testutil.Body
		declared int64
		want     []byte
		read     int64
	}{
		{name: "small", body: testutil.NewBody("{}"), declared: 2, want: []byte("{}"), read: 2},
		{name: "exactly the bound", body: testutil.NewBody(full), declared: -1, want: []byte(full), read: maxErrorBodyBytes},
		{name: "declared over the bound", body: testutil.NewBody(full + "e"), declared: maxErrorBodyBytes + 1, want: nil, read: 0},
		{name: "found over the bound", body: testutil.NewBody(full + "e"), declared: -1, want: nil, read: maxErrorBodyBytes + 1},
		{name: "empty", body: testutil.NewBody(""), declared: 0, want: []byte{}, read: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, excerpt(tt.body, tt.declared))
			assert.Equal(t, tt.read, tt.body.BytesRead())
		})
	}
	assert.Nil(t, excerpt(cfFailingBody("{}"), -1), "a failed read yields nothing")
}

func TestTransportClassifyPrecedence(t *testing.T) {
	live := context.Background()
	// A network error whose Timeout reports false.
	noTimeout := &net.OpError{Op: "read", Net: "tcp", Err: errCFRead}
	canceled, cancel := context.WithCancel(live)
	cancel()
	expired, cancelExpired := context.WithDeadline(live, time.Time{})
	defer cancelExpired()

	tests := []struct {
		name    string
		caller  context.Context //nolint:containedctx // each case runs with its own contexts
		callCtx context.Context //nolint:containedctx // each case runs with its own contexts
		err     error
		kind    FailureKind
		cause   error
	}{
		{name: "caller canceled beats everything", caller: canceled, callCtx: expired, err: context.DeadlineExceeded, kind: FailureCanceled, cause: context.Canceled},
		{name: "caller deadline beats the call's", caller: expired, callCtx: expired, err: errCFRead, kind: FailureDeadlineExceeded, cause: context.DeadlineExceeded},
		{name: "call deadline", caller: live, callCtx: expired, err: errCFRead, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "deadline error alone", caller: live, callCtx: live, err: context.DeadlineExceeded, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "network timeout", caller: live, callCtx: live, err: testutil.TimeoutError{}, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "network error without timeout", caller: live, callCtx: live, err: noTimeout, kind: FailureTransport, cause: errTransportOther},
		{name: "transport failure", caller: live, callCtx: live, err: io.ErrUnexpectedEOF, kind: FailureTransport, cause: errTransportClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := classify(tt.caller, tt.callCtx, OpJobStatus, tt.err, 202)
			assert.Equal(t, tt.kind, ce.Kind)
			assert.Equal(t, OpJobStatus, ce.Op)
			assert.Equal(t, 202, ce.StatusCode)
			assert.Equal(t, tt.cause, ce.Unwrap())
		})
	}
}

func TestTransportSenderRefusesAnUnbuildableRequest(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, "{}")}
	send := newSender(&http.Client{Transport: ct}, "https://127.0.0.1", `{"key":"`+internalKey+`"`)
	body := make([]byte, len(`{"key":"`+internalKey+`"`), 64)
	resp, err := send(t.Context(), Operation("single/check\x7f"), append(body, '}')) //nolint:bodyclose // the response is nil
	assert.Nil(t, resp)
	require.ErrorIs(t, err, errBuildRequest)
	assert.Zero(t, ct.Attempts())

	resp, release, cerr := open(t.Context(), t.Context(), send, Operation("\x00"), append(body, '}')) //nolint:bodyclose // nothing to close
	assert.Nil(t, resp)
	assert.Nil(t, release)
	require.NotNil(t, cerr)
	assert.Equal(t, FailureTransport, cerr.Kind)
	assert.Same(t, errBuildRequest, cerr.Unwrap())
	assert.Zero(t, ct.Attempts())
}

func TestTransportSenderWritesTheKeyMember(t *testing.T) {
	s := testutil.NewScript(testutil.Step{Body: "{}"})
	keyMember := `{"key":"` + internalKey + `"`
	send := newSender(&http.Client{Transport: s}, "https://[::1]:8443", keyMember)
	body := append(make([]byte, len(keyMember)), `,"job_id":42}`...)
	resp, err := send(t.Context(), OpJobStatus, body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	r := testutil.OnlyRequest(t, s)
	assert.Equal(t, []byte(keyMember+`,"job_id":42}`), r.Body)
	assert.Equal(t, "https://[::1]:8443/v4.2/jobs/status", r.URL)
	assert.False(t, r.GetBody)
	assert.Equal(t, int64(len(body)), r.ContentLength)
}

func TestTransportOpenClosesOnce(t *testing.T) {
	newSend := func(body *testutil.Body) sender {
		return func(context.Context, Operation, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		}
	}
	t.Run("release", func(t *testing.T) {
		body := testutil.NewBody("{}")
		resp, release, cerr := open(t.Context(), t.Context(), newSend(body), OpCheckEmail, nil) //nolint:bodyclose // closed by release
		require.Nil(t, cerr)
		require.NotNil(t, resp)
		assert.Zero(t, body.Closes())
		release()
		release()
		assert.Equal(t, 1, body.Closes())
	})
	t.Run("call context ends first", func(t *testing.T) {
		body := testutil.NewBody("{}")
		callCtx, cancel := context.WithCancel(t.Context())
		_, release, cerr := open(t.Context(), callCtx, newSend(body), OpCheckEmail, nil) //nolint:bodyclose // closed by release
		require.Nil(t, cerr)
		cancel()
		require.Eventually(t, func() bool { return body.Closes() == 1 }, cfWait, cfTick, "the call context's end closes the body")
		release()
		assert.Equal(t, 1, body.Closes(), "release never closes it again")
	})
}

// cfTrustingTransport returns the transport the SDK builds for itself, made to
// trust srv's certificate, and nothing else changed.
func cfTrustingTransport(tb testing.TB, srv *testutil.Server) *http.Transport {
	tb.Helper()
	tr := newTransport()
	srvTr, ok := srv.Client().Transport.(*http.Transport)
	require.True(tb, ok)
	tr.TLSClientConfig = srvTr.TLSClientConfig.Clone()
	tb.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestTransportDefaultTransportDecodesGzip(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			srv := testutil.NewServerProto(t, proto, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "gzip" {
					w.WriteHeader(http.StatusNotAcceptable)
					return
				}
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				_, _ = zw.Write([]byte(`{"status":"success","result":"valid"}`))
				_ = zw.Close()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(buf.Bytes())
			})
			c := newInternalClient(t, WithTestBaseURL(srv.URL()),
				WithHTTPClient(&http.Client{Transport: cfTrustingTransport(t, srv)}))
			res, err := c.CheckEmail(t.Context(), CheckEmailRequest{Email: "avery.example@example.com"})
			require.NoError(t, err)
			assert.Equal(t, ResultValid, res.Result)
		})
	}
}

func TestTransportDefaultTransportBoundsResponseHeaders(t *testing.T) {
	srv := testutil.NewServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("p", maxResponseHeaderBytes))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","result":"valid"}`)
	})
	c := newInternalClient(t, WithTestBaseURL(srv.URL()),
		WithHTTPClient(&http.Client{Transport: cfTrustingTransport(t, srv)}))
	_, err := c.CheckEmail(t.Context(), CheckEmailRequest{Email: "avery.example@example.com"})
	ce := asCallError(t, err)
	assert.Equal(t, FailureTransport, ce.Kind)
	assert.Zero(t, ce.StatusCode)
	assert.Same(t, errTransportOther, ce.Unwrap())
}
