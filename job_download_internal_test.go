package neverbounce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Values the internal download tests repeat.
const (
	dlData       = "abc" // the body most copy tests stream
	dlEnvMedia   = "application/json"
	dlOctetMedia = "application/octet-stream"
	dlCSVMedia   = "text/csv"
)

// errDLInternal is a synthetic read or write failure.
var errDLInternal = errors.New("dl: internal synthetic failure")

// dlRead is one scripted Read result.
type dlRead struct {
	data string
	err  error
}

// dlReader returns its reads in order, holding back the part of one that does
// not fit p (and its error) for the next Read, and records the length of every
// buffer it is asked to fill. After the script it returns io.EOF.
type dlReader struct {
	reads []dlRead
	asked []int
}

// Read returns the next scripted result.
func (r *dlReader) Read(p []byte) (int, error) {
	r.asked = append(r.asked, len(p))
	if len(r.reads) == 0 {
		return 0, io.EOF
	}
	next := r.reads[0]
	n := copy(p, next.data)
	if n < len(next.data) {
		r.reads[0].data = next.data[n:]
		return n, nil
	}
	r.reads = r.reads[1:]
	return n, next.err
}

// dlEmpty returns n reads of (0, nil).
func dlEmpty(n int) []dlRead { return make([]dlRead, n) }

// dlWriterFunc adapts a function to io.Writer.
type dlWriterFunc func(p []byte) (int, error)

// Write returns f(p).
func (f dlWriterFunc) Write(p []byte) (int, error) { return f(p) }

// dlContexts returns a caller context and a call context derived from it, as
// download builds them: canceled when done is "caller canceled", past their
// deadline for "caller deadline", and with only the call context expired for
// "call timeout".
func dlContexts(t *testing.T, done string) (ctx, callCtx context.Context) {
	t.Helper()
	ctx = t.Context()
	switch done {
	case "caller canceled":
		c, cancel := context.WithCancel(ctx)
		cancel()
		ctx = c
	case "caller deadline":
		c, cancel := context.WithDeadline(ctx, time.Unix(1, 0))
		t.Cleanup(cancel)
		ctx = c
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Hour)
	t.Cleanup(cancel)
	if done == "call timeout" {
		c, cancelCall := context.WithDeadline(ctx, time.Unix(1, 0))
		t.Cleanup(cancelCall)
		callCtx = c
	}
	return ctx, callCtx
}

// dlAssertCall asserts that cerr is nil when kind is "", or a jobs/download
// failure of kind whose cause is exactly cause.
func dlAssertCall(tb testing.TB, cerr *CallError, kind FailureKind, cause error) {
	tb.Helper()
	if kind == "" {
		assert.Nil(tb, cerr)
		return
	}
	require.NotNil(tb, cerr)
	assert.Equal(tb, kind, cerr.Kind)
	assert.Equal(tb, OpDownloadJob, cerr.Op)
	require.ErrorIs(tb, cerr, cause)
	assert.Equal(tb, cause, cerr.Unwrap(), "the cause is the fixed error itself")
}

func TestCopyBounded(t *testing.T) {
	big := strings.Repeat("a", downloadCopyBuffer) + strings.Repeat("b", 10) + "c"
	tests := []struct {
		name    string
		reads   []dlRead
		cl      int64
		limit   int64
		done    string
		written int64
		kind    FailureKind
		cause   error
		asked   []int
	}{
		{name: "declared length", reads: []dlRead{{dlData, nil}}, cl: 3, limit: 10, written: 3, asked: []int{10, 7}},
		{name: "unknown length", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 10, written: 3},
		{name: "data with eof", reads: []dlRead{{dlData, io.EOF}}, cl: 3, limit: 10, written: 3, asked: []int{10}},
		{name: "empty body", cl: -1, limit: 10, asked: []int{10}},
		{name: "empty declared body", cl: 0, limit: 10},
		{name: "short of declared", reads: []dlRead{{dlData, nil}}, cl: 5, limit: 10, written: 3, kind: FailureTransport, cause: errTransportClosed},
		{name: "longer than declared", reads: []dlRead{{dlData, io.EOF}}, cl: 2, limit: 10, written: 3, kind: FailureTransport, cause: errTransportClosed},
		{name: "exactly the limit", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 3, written: 3, asked: []int{3, 1}},
		{name: "exactly the declared limit", reads: []dlRead{{dlData, nil}}, cl: 3, limit: 3, written: 3, asked: []int{3, 1}},
		{name: "one over the limit", reads: []dlRead{{"abcd", nil}}, cl: -1, limit: 3, written: 3, kind: FailureResponseTooLarge, cause: errResponseTooLarge, asked: []int{3, 1}},
		{name: "probe sees data with eof", reads: []dlRead{{dlData, nil}, {"d", io.EOF}}, cl: -1, limit: 3, written: 3, kind: FailureResponseTooLarge, cause: errResponseTooLarge},
		{name: "probe read error", reads: []dlRead{{dlData, nil}, {"", io.ErrUnexpectedEOF}}, cl: -1, limit: 3, written: 3, kind: FailureTransport, cause: errTransportClosed},
		{name: "limit of one", reads: []dlRead{{"ab", nil}}, cl: -1, limit: 1, written: 1, kind: FailureResponseTooLarge, cause: errResponseTooLarge, asked: []int{1, 1}},
		{name: "limit just over the buffer", reads: []dlRead{{big, nil}}, cl: -1, limit: downloadCopyBuffer + 10, written: downloadCopyBuffer + 10, kind: FailureResponseTooLarge, cause: errResponseTooLarge, asked: []int{downloadCopyBuffer, 10, 1}},
		{name: "large limit reads whole buffers", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 1 << 30, written: 3, asked: []int{downloadCopyBuffer, downloadCopyBuffer}},
		{name: "data then read error", reads: []dlRead{{dlData, errDLInternal}}, cl: -1, limit: 10, written: 3, kind: FailureTransport, cause: errTransportOther},
		{name: "unexpected eof", reads: []dlRead{{dlData, nil}, {"", io.ErrUnexpectedEOF}}, cl: 9, limit: 10, written: 3, kind: FailureTransport, cause: errTransportClosed},
		{name: "reset", reads: []dlRead{{"", &net.OpError{Op: "read", Err: syscall.ECONNRESET}}}, cl: -1, limit: 10, kind: FailureTransport, cause: errTransportReset},
		{name: "closed connection", reads: []dlRead{{"ab", net.ErrClosed}}, cl: -1, limit: 10, written: 2, kind: FailureTransport, cause: errTransportClosed},
		{name: "deadline error", reads: []dlRead{{"ab", context.DeadlineExceeded}}, cl: -1, limit: 10, written: 2, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "network timeout", reads: []dlRead{{"", testutil.TimeoutError{}}}, cl: -1, limit: 10, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "stalled", reads: dlEmpty(maxEmptyReads + 5), cl: -1, limit: 10, kind: FailureTransport, cause: errTransportStalled},
		{name: "stalled after data", reads: append([]dlRead{{"ab", nil}}, dlEmpty(maxEmptyReads+5)...), cl: -1, limit: 10, written: 2, kind: FailureTransport, cause: errTransportStalled},
		{name: "empty reads below the bound", reads: append(dlEmpty(maxEmptyReads-1), dlRead{"ab", nil}), cl: 2, limit: 10, written: 2},
		{name: "empty reads reset by progress", reads: append(append(dlEmpty(maxEmptyReads-1), dlRead{"a", nil}), append(dlEmpty(maxEmptyReads-1), dlRead{"b", nil})...), cl: 2, limit: 10, written: 2},
		{name: "caller canceled", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 10, done: "caller canceled", kind: FailureCanceled, cause: context.Canceled, asked: []int{}},
		{name: "caller deadline", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 10, done: "caller deadline", kind: FailureDeadlineExceeded, cause: context.DeadlineExceeded, asked: []int{}},
		{name: "call timeout", reads: []dlRead{{dlData, nil}}, cl: -1, limit: 10, done: "call timeout", kind: FailureTimeout, cause: context.DeadlineExceeded, asked: []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var all strings.Builder
			for _, r := range tt.reads {
				all.WriteString(r.data)
			}
			ctx, callCtx := dlContexts(t, tt.done)
			r := &dlReader{reads: tt.reads}
			var w testutil.CountingWriter
			n, cerr := copyBounded(ctx, callCtx, &w, &progressReader{r: r}, tt.cl, tt.limit)
			assert.Equal(t, tt.written, n)
			dlAssertCall(t, cerr, tt.kind, tt.cause)
			assert.Equal(t, all.String()[:tt.written], string(w.Bytes()))
			if tt.asked != nil {
				assert.Equal(t, tt.asked, append([]int{}, r.asked...))
			}
			if cerr != nil {
				assert.Equal(t, http.StatusOK, cerr.StatusCode)
			}
		})
	}
}

func TestCopyBoundedWriterFailures(t *testing.T) {
	tests := []struct {
		name    string
		w       io.Writer
		written int64
	}{
		{"error", dlWriterFunc(func([]byte) (int, error) { return 0, errDLInternal }), 0},
		{"full count with error", dlWriterFunc(func(p []byte) (int, error) { return len(p), errDLInternal }), 3},
		{"short", dlWriterFunc(func(p []byte) (int, error) { return len(p) - 1, nil }), 2},
		{"negative", dlWriterFunc(func([]byte) (int, error) { return -1, nil }), 0},
		{"too many", dlWriterFunc(func(p []byte) (int, error) { return len(p) + 1, nil }), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &dlReader{reads: []dlRead{{dlData, nil}, {"def", nil}}}
			n, cerr := copyBounded(t.Context(), t.Context(), tt.w, &progressReader{r: r}, -1, 100)
			assert.Equal(t, tt.written, n)
			dlAssertCall(t, cerr, FailureWriteFailed, errWriter)
			assert.Equal(t, []int{100}, r.asked, "no read after a failed write")
		})
	}
}

func TestWriteChunk(t *testing.T) {
	p := []byte("abcdef")
	tests := []struct {
		name   string
		n      int
		err    error
		counts int
		ok     bool
	}{
		{"full", 6, nil, 6, true},
		{"full with error", 6, errDLInternal, 6, false},
		{"short", 5, nil, 5, false},
		{"short with error", 2, errDLInternal, 2, false},
		{"nothing", 0, nil, 0, false},
		{"nothing with error", 0, errDLInternal, 0, false},
		{"negative", -1, nil, 0, false},
		{"negative with error", -1, errDLInternal, 0, false},
		{"most negative", math.MinInt, nil, 0, false},
		{"one too many", 7, nil, 0, false},
		{"one too many with error", 7, errDLInternal, 0, false},
		{"far too many", math.MaxInt, nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []byte
			w := dlWriterFunc(func(b []byte) (int, error) {
				got = b
				return tt.n, tt.err
			})
			counts, ok := writeChunk(w, p)
			assert.Equal(t, tt.counts, counts)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, p, got, "the writer gets the whole chunk")
		})
	}
	t.Run("empty chunk", func(t *testing.T) {
		counts, ok := writeChunk(dlWriterFunc(func([]byte) (int, error) { return 0, nil }), nil)
		assert.Zero(t, counts)
		assert.True(t, ok)
	})
}

func TestReadEnd(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		written int64
		cl      int64
		done    string
		end     bool
		kind    FailureKind
		cause   error
	}{
		{name: "no error", written: 3, cl: -1},
		{name: "no error past declared", written: 5, cl: 3},
		{name: "eof unknown length", err: io.EOF, written: 3, cl: -1, end: true},
		{name: "eof at declared", err: io.EOF, written: 3, cl: 3, end: true},
		{name: "eof empty declared", err: io.EOF, cl: 0, end: true},
		{name: "eof short", err: io.EOF, written: 2, cl: 3, end: true, kind: FailureTransport, cause: errTransportClosed},
		{name: "eof long", err: io.EOF, written: 4, cl: 3, end: true, kind: FailureTransport, cause: errTransportClosed},
		{name: "wrapped eof", err: fmt.Errorf("dl: wrapped: %w", io.EOF), written: 3, cl: 3, end: true},
		{name: "eof with a done context", err: io.EOF, written: 3, cl: 3, done: "caller canceled", end: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, written: 2, cl: -1, end: true, kind: FailureTransport, cause: errTransportClosed},
		{name: "reset", err: syscall.ECONNRESET, cl: -1, end: true, kind: FailureTransport, cause: errTransportReset},
		{name: "broken pipe", err: syscall.EPIPE, cl: -1, end: true, kind: FailureTransport, cause: errTransportReset},
		{name: "other", err: errDLInternal, cl: -1, end: true, kind: FailureTransport, cause: errTransportOther},
		{name: "deadline error", err: context.DeadlineExceeded, cl: -1, end: true, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "network timeout", err: testutil.TimeoutError{}, cl: -1, end: true, kind: FailureTimeout, cause: context.DeadlineExceeded},
		{name: "caller canceled", err: net.ErrClosed, cl: -1, done: "caller canceled", end: true, kind: FailureCanceled, cause: context.Canceled},
		{name: "caller deadline", err: net.ErrClosed, cl: -1, done: "caller deadline", end: true, kind: FailureDeadlineExceeded, cause: context.DeadlineExceeded},
		{name: "call timeout", err: net.ErrClosed, cl: -1, done: "call timeout", end: true, kind: FailureTimeout, cause: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, callCtx := dlContexts(t, tt.done)
			end, cerr := readEnd(ctx, callCtx, tt.err, tt.written, tt.cl)
			assert.Equal(t, tt.end, end)
			dlAssertCall(t, cerr, tt.kind, tt.cause)
		})
	}
}

func TestMediaType(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"absent", nil, ""},
		{"octet-stream", []string{dlOctetMedia}, dlOctetMedia},
		{"uppercase", []string{"APPLICATION/OCTET-STREAM"}, dlOctetMedia},
		{"csv with parameters", []string{"Text/CSV; Charset=UTF-8; Header=Present"}, dlCSVMedia},
		{"surrounding spaces", []string{" text/csv "}, dlCSVMedia},
		{"trailing semicolon", []string{"text/csv;"}, dlCSVMedia},
		{"json", []string{"application/json; charset=utf-8"}, dlEnvMedia},
		{"other type", []string{"text/html"}, "text/html"},
		{"empty value", []string{""}, ""},
		{"two values", []string{dlCSVMedia, dlCSVMedia}, ""},
		{"two different values", []string{dlCSVMedia, dlEnvMedia}, ""},
		{"empty list", []string{}, ""},
		{"no subtype", []string{"text/"}, ""},
		{"slash only", []string{"/"}, ""},
		{"invalid parameter", []string{"application/octet-stream; =x"}, ""},
		{"bare parameter", []string{"text/csv; charset"}, ""},
		{"double semicolon", []string{"text/csv;;"}, ""},
		{"unterminated quote", []string{`text/csv; name="a`}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.values != nil {
				h["Content-Type"] = tt.values
			}
			assert.Equal(t, tt.want, mediaType(h))
		})
	}
	t.Run("non-canonical key is not read", func(t *testing.T) {
		assert.Empty(t, mediaType(http.Header{"content-type": {dlCSVMedia}}))
	})
}

func TestEnvelopeOf(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		ok      bool
		status  string
		message string
	}{
		{"failure with message", `{"status":"general_failure","message":"m"}`, true, `"general_failure"`, `"m"`},
		{"status only", `{"status":"x"}`, true, `"x"`, ""},
		{"empty status", `{"status":""}`, true, `""`, ""},
		{"whitespace around", " \t\r\n{ \"status\" : \"auth_failure\" , \"message\" : null }\r\n", true, `"auth_failure"`, "null"},
		{"escaped key", testutil.Esc(`{"st~u0061tus":"temp_unavail"}`), true, `"temp_unavail"`, ""},
		{"escaped value", testutil.Esc(`{"status":"temp~u005funavail"}`), true, testutil.Esc(`"temp~u005funavail"`), ""},
		{"status after other members", `{"a":[1,{"status":"x"}],"status":"y"}`, true, `"y"`, ""},
		{"message not a string", `{"status":"x","message":42}`, true, `"x"`, "42"},
		{"status number", `{"status":1}`, false, "", ""},
		{"status null", `{"status":null}`, false, "", ""},
		{"status true", `{"status":true}`, false, "", ""},
		{"status array", `{"status":["x"]}`, false, "", ""},
		{"status object", `{"status":{"x":"y"}}`, false, "", ""},
		{"no status", `{"message":"x"}`, false, "", ""},
		{"empty object", `{}`, false, "", ""},
		{"case-folded key", `{"Status":"x"}`, false, "", ""},
		{"nested status only", `{"error":{"status":"x"}}`, false, "", ""},
		{"repeated status", `{"status":"x","status":"x"}`, false, "", ""},
		{"repeated escaped status", testutil.Esc(`{"status":"x","~u0073tatus":"x"}`), false, "", ""},
		{"trailing data", `{"status":"x"} {}`, false, "", ""},
		{"trailing csv", "{\"status\":\"x\"}\r\n1,a@example.com\r\n", false, "", ""},
		{"invalid utf-8", "{\"status\":\"x\xff\"}", false, "", ""},
		{"invalid json", `{"status":"x"`, false, "", ""},
		{"lone brace", `{`, false, "", ""},
		{"array", `[{"status":"x"}]`, false, "", ""},
		{"string", `"status"`, false, "", ""},
		{"bom first", "\xef\xbb\xbf{\"status\":\"x\"}", false, "", ""},
		{"csv", "id,email\r\n1,a@example.com\r\n", false, "", ""},
		{"empty", "", false, "", ""},
		{"whitespace only", " \r\n\t", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message, ok := envelopeOf([]byte(tt.data))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.status, string(status))
			assert.Equal(t, tt.message, string(message))
		})
	}
}

func TestDownloadEnumsValid(t *testing.T) {
	for _, f := range []BinaryFormat{"", BinaryOneZero, BinaryUpperYN, BinaryLowerYN, BinaryLowerYesNo, BinaryTitleYesNo, BinaryLowerTrueFalse} {
		assert.True(t, f.valid(), "%q", string(f))
	}
	for _, f := range []BinaryFormat{"BIN", "bin_1_0", "BIN_1_0 ", " BIN_1_0", "BIN_YES_NO", "BIN_True_False", "1"} {
		assert.False(t, f.valid(), "%q", string(f))
	}
	for _, f := range []LineFeed{"", LineFeedLFCR, LineFeedCRLF, LineFeedLF, LineFeedCR} {
		assert.True(t, f.valid(), "%q", string(f))
	}
	for _, f := range []LineFeed{"LINEFEED", "linefeed_0a", "LINEFEED_0a", "LINEFEED_0D0D", "\n", "\r\n", "CRLF"} {
		assert.False(t, f.valid(), "%q", string(f))
	}
}

func TestDownloadEncodeSizeBound(t *testing.T) {
	const tail = `,"job_id":1}`
	for _, tt := range []struct {
		name   string
		prefix int
		ok     bool
	}{
		{"at the bound", maxSmallRequestBytes - len(tail), true},
		{"one over", maxSmallRequestBytes - len(tail) + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := *newInternalClient(t)
			c.newBody = func(capHint int) []byte { return make([]byte, tt.prefix, tt.prefix+capHint) }
			req := DownloadJobRequest{JobID: 1}
			b, err := req.encode(&c)
			if tt.ok {
				require.NoError(t, err)
				assert.Len(t, b, maxSmallRequestBytes)
				assert.True(t, strings.HasSuffix(string(b), tail))
				return
			}
			requireValidation(t, err, ErrInvalidRequest, "request", reasonTooLarge)
			assert.Nil(t, b)
		})
	}
}

func TestDownloadJobCauses(t *testing.T) {
	octet := http.Header{"Content-Type": {dlOctetMedia}}
	jsonCT := http.Header{"Content-Type": {dlEnvMedia}}
	gzipped := http.Header{"Content-Type": {dlOctetMedia}, "Content-Encoding": {"gzip"}}
	csv := "id,email\r\n1,a@example.com\r\n"
	wide := `{"status":"general_failure","pad":"` + strings.Repeat("x", downloadSniffBytes) + `"}`
	long := strings.Repeat(csv, 4000)
	str := func(s string) io.ReadCloser { return testutil.NewBody(s) }
	stall := func(s string) io.ReadCloser {
		b := testutil.NewBody(s)
		b.Stall = true
		return b
	}
	tests := []struct {
		name   string
		status int
		h      http.Header
		body   io.ReadCloser
		cl     int64
		w      io.Writer
		limit  int64
		kind   FailureKind
		cause  error
	}{
		{"informational", http.StatusSwitchingProtocols, octet, str(csv), -1, nil, 0, FailureUnexpectedResponse, errStatus},
		{"status", http.StatusNotFound, octet, str(csv), -1, nil, 0, FailureRejected, errStatus},
		{"content encoding", http.StatusOK, gzipped, str(csv), -1, nil, 0, FailureUnexpectedResponse, errContentEncoding},
		{"declared too large", http.StatusOK, octet, str(csv), 1 << 40, nil, 0, FailureResponseTooLarge, errResponseTooLarge},
		{"content type", http.StatusOK, http.Header{"Content-Type": {"text/html"}}, str(csv), -1, nil, 0, FailureUnexpectedResponse, errContentType},
		{"json success", http.StatusOK, jsonCT, str(`{"status":"success"}`), -1, nil, 0, FailureUnexpectedResponse, errJSONDownload},
		{"json declared over the window", http.StatusOK, jsonCT, str(wide), int64(len(wide)), nil, 0, FailureUnexpectedResponse, errJSONDownload},
		{"json streamed over the window", http.StatusOK, jsonCT, str(wide), -1, nil, 0, FailureUnexpectedResponse, errJSONDownload},
		{"json truncated", http.StatusOK, jsonCT, str(`{"status":"x"}`), 20, nil, 0, FailureTransport, errTransportClosed},
		{"json read error", http.StatusOK, jsonCT, io.NopCloser(io.MultiReader(strings.NewReader(`{"st`), iotest.ErrReader(errDLInternal))), -1, nil, 0, FailureTransport, errTransportOther},
		{"json invalid", http.StatusOK, jsonCT, str(`{"status"`), -1, nil, 0, FailureMalformedResponse, errInvalidJSON},
		{"json not an object", http.StatusOK, jsonCT, str(`["status"]`), -1, nil, 0, FailureUnexpectedResponse, errNotObject},
		{"json failure status", http.StatusOK, jsonCT, str(`{"status":"general_failure"}`), -1, nil, 0, FailureRejected, errAPIStatus},
		{"json unknown status", http.StatusOK, jsonCT, str(`{"status":"paused"}`), -1, nil, 0, FailureUnexpectedResponse, errUnknownAPIStatus},
		{"json status missing", http.StatusOK, jsonCT, str(`{}`), -1, nil, 0, FailureMalformedResponse, errMissingField},
		{"json status wrong type", http.StatusOK, jsonCT, str(`{"status":false}`), -1, nil, 0, FailureMalformedResponse, errWrongType},
		{"disguised failure", http.StatusOK, octet, str(`{"status":"throttle_triggered"}`), -1, nil, 0, FailureRateLimited, errAPIStatus},
		{"disguised success", http.StatusOK, octet, str(`{"status":"success"}`), -1, nil, 0, FailureUnexpectedResponse, errJSONDownload},
		{"truncated in the window", http.StatusOK, octet, str(csv), int64(len(csv) + 1), nil, 0, FailureTransport, errTransportClosed},
		{"truncated while streaming", http.StatusOK, octet, str(long), int64(len(long) + 1), nil, 0, FailureTransport, errTransportClosed},
		{"stalled in the window", http.StatusOK, octet, stall(csv), -1, nil, 0, FailureTransport, errTransportStalled},
		{"stalled while streaming", http.StatusOK, octet, stall(long), -1, nil, 0, FailureTransport, errTransportStalled},
		{"overflow", http.StatusOK, octet, str(long), -1, nil, 100, FailureResponseTooLarge, errResponseTooLarge},
		{"writer", http.StatusOK, octet, str(csv), -1, &testutil.FailAfterWriter{N: 3}, 0, FailureWriteFailed, errWriter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := testutil.TransportWith(tt.status, tt.h, tt.cl, tt.body)
			opts := []Option{WithTestBaseURL("https://127.0.0.1"), WithHTTPClient(&http.Client{Transport: rt})}
			if tt.limit > 0 {
				opts = append(opts, WithMaxDownloadBytes(tt.limit))
			}
			c := newInternalClient(t, opts...)
			w := tt.w
			if w == nil {
				w = io.Discard
			}
			n, err := c.DownloadJob(t.Context(), DownloadJobRequest{JobID: 7}, w)
			ce := asCallError(t, err)
			dlAssertCall(t, ce, tt.kind, tt.cause)
			assert.Equal(t, JobID(7), ce.JobID)
			assert.Equal(t, n, ce.BytesWritten)
		})
	}
}
