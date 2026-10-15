package neverbounce_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Values the download tests share.
const (
	dlJobID = neverbounce.JobID(4242)
	dlSniff = 64 << 10 // bytes classified before the first write
	dlChunk = 32 << 10 // the copy loop's read size
	dlBase  = keyMember + `,"job_id":4242`
	dlCSV   = "id,email,email_status\r\n\"1001\",\"" + testEmail + "\",valid\r\n"
)

// The validation field and reason, and the envelope status, that the
// download tests repeat.
const (
	dlFieldJobID     = "JobID"
	dlInvalidValue   = "invalid_value"
	dlGeneralFailure = "general_failure"
)

// Static errors the download fakes return.
var (
	errDLRead   = errors.New("dl: synthetic read failure " + testKey)
	errDLWriter = errors.New("dl: synthetic writer failure " + testEmail)
)

// dlRequest returns the minimal download request the tests send.
func dlRequest() neverbounce.DownloadJobRequest {
	return neverbounce.DownloadJobRequest{JobID: dlJobID}
}

// dlCT returns a header holding the Content-Type values v.
func dlCT(v ...string) http.Header { return http.Header{"Content-Type": v} }

// dlLength returns the length to declare for content: its own, or -1 when
// declare is false.
func dlLength(content []byte, declare bool) int64 {
	if declare {
		return int64(len(content))
	}
	return -1
}

// dlDownload runs DownloadJob with dlRequest through rt, in memory, and
// asserts that it made exactly one attempt.
func dlDownload(tb testing.TB, rt http.RoundTripper, w io.Writer, opts ...neverbounce.Option) (int64, error) {
	tb.Helper()
	ct := &testutil.CountingTransport{Next: rt}
	c := newTransportClient(tb, ct, opts...)
	n, err := c.DownloadJob(tb.Context(), dlRequest(), w)
	assert.Equal(tb, 1, ct.Attempts(), "exactly one attempt")
	return n, err
}

// dlAssertBytes asserts that got is exactly want, printing a digest rather
// than megabytes on failure.
func dlAssertBytes(tb testing.TB, want, got []byte) {
	tb.Helper()
	switch {
	case len(want) == 0:
		assert.Empty(tb, got)
		return
	case len(want) <= 4096:
		assert.Equal(tb, want, got)
		return
	}
	assert.Len(tb, got, len(want))
	assert.Equal(tb, sha256.Sum256(want), sha256.Sum256(got), "content digest")
}

// dlAssertStreamed asserts a successful download of want.
func dlAssertStreamed(tb testing.TB, want []byte, n int64, err error, w *testutil.CountingWriter) {
	tb.Helper()
	require.NoError(tb, err)
	assert.Equal(tb, int64(len(want)), n, "the returned count is the body length")
	dlAssertBytes(tb, want, w.Bytes())
}

// dlAssertFailure asserts that err is the *CallError want describes for the
// download of dlJobID, with Op and JobID filled in, returned as it is rather
// than wrapped, and that n, the count the call returned, is its BytesWritten.
func dlAssertFailure(tb testing.TB, n int64, err error, want wantCall) {
	tb.Helper()
	want.Op, want.JobID = neverbounce.OpDownloadJob, dlJobID
	assertCallError(tb, err, want)
	assert.Same(tb, callError(tb, err), err, "the CallError itself, not a wrapper")
	assert.Equal(tb, want.Written, n, "the returned count is BytesWritten")
}

// dlRandom returns n reproducible pseudo-random bytes.
func dlRandom(n int) []byte {
	var seed [32]byte
	copy(seed[:], "go-neverbounce download fixture")
	b := make([]byte, n)
	_, _ = rand.NewChaCha8(seed).Read(b)
	return b
}

// dlPattern returns n bytes of a repeating CSV-like pattern, so that a prefix
// at any offset is distinguishable from the bytes after it.
func dlPattern(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteString(",row-")
		b.WriteString(strconv.Itoa(i * 7))
		b.WriteString("@example.com\r\n")
	}
	return b.Bytes()[:n]
}

// dlEnvelope returns an envelope with status s padded to exactly size bytes.
func dlEnvelope(s string, size int) []byte {
	head := `{"status":"` + s + `","pad":"`
	return []byte(head + strings.Repeat("x", size-len(head)-2) + `"}`)
}

// dlFile is a download body that must reach the writer byte for byte.
type dlFile struct {
	name string
	body []byte
}

// dlFiles returns the bodies that must stream unchanged.
func dlFiles() []dlFile {
	return []dlFile{
		{"quoted commas", []byte("id,email,note\r\n1," + testEmail + ",\"Smith, Avery, Jr.\"\r\n")},
		{"newlines inside quotes", []byte("id,note\r\n1,\"line one\nline two\r\nline three\"\r\n")},
		{"crlf", []byte("id,email\r\n1,a@example.com\r\n2,b@example.com\r\n")},
		{"lfcr", []byte("id,email\n\r1,a@example.com\n\r2,b@example.com\n\r")},
		{"lone cr", []byte("id,email\r1,a@example.com\r2,b@example.com\r")},
		{"utf-8 bom", []byte("\xef\xbb\xbfid,email\r\n1,a@example.com\r\n")},
		{"no final newline", []byte("id,email\r\n1,a@example.com")},
		{"nul bytes", []byte("\x00id,email\r\n1,a\x00@example.com\r\n\x00\x00")},
		{"invalid utf-8", []byte("id,name\r\n1,\xff\xfe\xc3\x28\xa0\xa1\xed\xa0\x80\r\n")},
		{"empty", []byte{}},
		{"one byte", []byte("x")},
		{"exactly the sniff window", dlPattern(dlSniff)},
		{"one past the sniff window", dlPattern(dlSniff + 1)},
		{"5 MiB random", dlRandom(5 << 20)},
	}
}

// dlSwitch is one optional boolean of a download request and its wire name.
type dlSwitch struct {
	wire string
	set  func(r *neverbounce.DownloadJobRequest, v *bool)
}

// dlSwitches returns every DownloadSegments and DownloadColumns field in the
// documented wire order.
func dlSwitches() []dlSwitch {
	type r = neverbounce.DownloadJobRequest
	return []dlSwitch{
		{"valids", func(q *r, v *bool) { q.Segments.Valids = v }},
		{"invalids", func(q *r, v *bool) { q.Segments.Invalids = v }},
		{"catchalls", func(q *r, v *bool) { q.Segments.Catchalls = v }},
		{"unknowns", func(q *r, v *bool) { q.Segments.Unknowns = v }},
		{"disposables", func(q *r, v *bool) { q.Segments.Disposables = v }},
		{"include_duplicates", func(q *r, v *bool) { q.Segments.IncludeDuplicates = v }},
		{"only_duplicates", func(q *r, v *bool) { q.Segments.OnlyDuplicates = v }},
		{"only_bad_syntax", func(q *r, v *bool) { q.Segments.OnlyBadSyntax = v }},
		{"bad_syntax", func(q *r, v *bool) { q.Append.BadSyntax = v }},
		{"free_email_host", func(q *r, v *bool) { q.Append.FreeEmailHost = v }},
		{"role_account", func(q *r, v *bool) { q.Append.RoleAccount = v }},
		{"addr", func(q *r, v *bool) { q.Append.Addr = v }},
		{"alias", func(q *r, v *bool) { q.Append.Alias = v }},
		{"host", func(q *r, v *bool) { q.Append.Host = v }},
		{"subdomain", func(q *r, v *bool) { q.Append.Subdomain = v }},
		{"domain", func(q *r, v *bool) { q.Append.Domain = v }},
		{"tld", func(q *r, v *bool) { q.Append.TLD = v }},
		{"fqdn", func(q *r, v *bool) { q.Append.FQDN = v }},
		{"network", func(q *r, v *bool) { q.Append.Network = v }},
		{"has_dns_info", func(q *r, v *bool) { q.Append.HasDNSInfo = v }},
		{"has_mail_server", func(q *r, v *bool) { q.Append.HasMailServer = v }},
		{"mail_server_reachable", func(q *r, v *bool) { q.Append.MailServerReachable = v }},
		{"email_status_int", func(q *r, v *bool) { q.Append.EmailStatusInt = v }},
		{"email_status", func(q *r, v *bool) { q.Append.EmailStatus = v }},
	}
}

// dlSentBody runs req through a scripted transport and returns the one
// request body it sent.
func dlSentBody(tb testing.TB, req neverbounce.DownloadJobRequest) []byte {
	tb.Helper()
	c, s := newScriptClient(tb, testutil.Step{Header: dlCT(mediaOctet), Body: dlCSV})
	_, err := c.DownloadJob(tb.Context(), req, io.Discard)
	require.NoError(tb, err)
	return testutil.OnlyRequest(tb, s).Body
}

func TestDownloadJobWireBodySwitches(t *testing.T) {
	t.Run("nil is not sent", func(t *testing.T) {
		assertBody(t, dlBase+`}`, dlSentBody(t, dlRequest()))
	})
	for _, sw := range dlSwitches() {
		for _, v := range []bool{true, false} {
			t.Run(sw.wire+" "+strconv.FormatBool(v), func(t *testing.T) {
				req := dlRequest()
				sw.set(&req, new(v))
				assertBody(t, dlBase+`,"`+sw.wire+`":`+strconv.FormatBool(v)+`}`, dlSentBody(t, req))
			})
		}
	}
	for _, mode := range []string{"all true", "all false", "alternating"} {
		t.Run(mode, func(t *testing.T) {
			req := dlRequest()
			var want strings.Builder
			want.WriteString(dlBase)
			for i, sw := range dlSwitches() {
				v := mode == "all true" || (mode == "alternating" && i%2 == 0)
				sw.set(&req, new(v))
				want.WriteString(`,"` + sw.wire + `":` + strconv.FormatBool(v))
			}
			want.WriteString(`}`)
			assertBody(t, want.String(), dlSentBody(t, req))
		})
	}
	t.Run("everything set", func(t *testing.T) {
		req := dlRequest()
		var want strings.Builder
		want.WriteString(dlBase)
		for _, sw := range dlSwitches() {
			sw.set(&req, new(true))
			want.WriteString(`,"` + sw.wire + `":true`)
		}
		req.BinaryFormat = neverbounce.BinaryTitleYesNo
		req.LineFeed = neverbounce.LineFeedCRLF
		want.WriteString(`,"binary_operators_type":"BIN_Yes_No","line_feed_type":"LINEFEED_0D0A"}`)
		assertBody(t, want.String(), dlSentBody(t, req))
	})
}

func TestDownloadJobWireBodyEnums(t *testing.T) {
	binary := []struct {
		v    neverbounce.BinaryFormat
		wire string
	}{
		{neverbounce.BinaryOneZero, "BIN_1_0"},
		{neverbounce.BinaryUpperYN, "BIN_Y_N"},
		{neverbounce.BinaryLowerYN, "BIN_y_n"},
		{neverbounce.BinaryLowerYesNo, "BIN_yes_no"},
		{neverbounce.BinaryTitleYesNo, "BIN_Yes_No"},
		{neverbounce.BinaryLowerTrueFalse, "BIN_true_false"},
	}
	for _, tt := range binary {
		t.Run(tt.wire, func(t *testing.T) {
			req := dlRequest()
			req.BinaryFormat = tt.v
			assertBody(t, dlBase+`,"binary_operators_type":"`+tt.wire+`"}`, dlSentBody(t, req))
		})
	}
	lineFeeds := []struct {
		v    neverbounce.LineFeed
		wire string
	}{
		{neverbounce.LineFeedLFCR, "LINEFEED_0A0D"},
		{neverbounce.LineFeedCRLF, "LINEFEED_0D0A"},
		{neverbounce.LineFeedLF, "LINEFEED_0A"},
		{neverbounce.LineFeedCR, "LINEFEED_0D"},
	}
	for _, tt := range lineFeeds {
		t.Run(tt.wire, func(t *testing.T) {
			req := dlRequest()
			req.LineFeed = tt.v
			assertBody(t, dlBase+`,"line_feed_type":"`+tt.wire+`"}`, dlSentBody(t, req))
		})
	}
	t.Run("both after the switches", func(t *testing.T) {
		req := dlRequest()
		req.LineFeed = neverbounce.LineFeedLF
		req.BinaryFormat = neverbounce.BinaryOneZero
		req.Append.EmailStatus = new(false)
		assertBody(t, dlBase+`,"email_status":false,"binary_operators_type":"BIN_1_0","line_feed_type":"LINEFEED_0A"}`,
			dlSentBody(t, req))
	})
}

// TestDownloadJobInvalidSendsNothing checks DownloadJob's own rules and where
// the writer check falls among them. JobID and the checks every method shares
// are in client_test.go.
func TestDownloadJobInvalidSendsNothing(t *testing.T) {
	tests := []struct {
		name   string
		done   bool // call with a canceled context
		req    neverbounce.DownloadJobRequest
		nilW   bool
		field  string
		reason string
	}{
		{"binary format lowercase", false, neverbounce.DownloadJobRequest{JobID: dlJobID, BinaryFormat: "bin_1_0"}, false, "BinaryFormat", dlInvalidValue},
		{"binary format padded", false, neverbounce.DownloadJobRequest{JobID: dlJobID, BinaryFormat: "BIN_1_0 "}, false, "BinaryFormat", dlInvalidValue},
		{"binary format undefined", false, neverbounce.DownloadJobRequest{JobID: dlJobID, BinaryFormat: "BIN_YES_NO"}, false, "BinaryFormat", dlInvalidValue},
		{"line feed lowercase", false, neverbounce.DownloadJobRequest{JobID: dlJobID, LineFeed: "linefeed_0d0a"}, false, "LineFeed", dlInvalidValue},
		{"line feed undefined", false, neverbounce.DownloadJobRequest{JobID: dlJobID, LineFeed: "LINEFEED_0A0A"}, false, "LineFeed", dlInvalidValue},
		{"line feed raw bytes", false, neverbounce.DownloadJobRequest{JobID: dlJobID, LineFeed: "\r\n"}, false, "LineFeed", dlInvalidValue},
		{"job id before enums", false, neverbounce.DownloadJobRequest{BinaryFormat: "x", LineFeed: "y"}, false, dlFieldJobID, reasonRequired},
		{"binary format before line feed", false, neverbounce.DownloadJobRequest{JobID: dlJobID, BinaryFormat: "x", LineFeed: "y"}, false, "BinaryFormat", dlInvalidValue},
		{"nil writer", false, dlRequest(), true, "w", "nil"},
		{"nil writer before a done context", true, dlRequest(), true, "w", "nil"},
	}
	ct := &testutil.CountingTransport{Next: testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody(dlCSV))}
	c := newTransportClient(t, ct)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w io.Writer = &testutil.CountingWriter{}
			if tt.nilW {
				w = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.done {
				cancel()
			}
			n, err := c.DownloadJob(ctx, tt.req, w)
			assertValidation(t, err, tt.field, tt.reason)
			assert.Zero(t, n)
			if cw, ok := w.(*testutil.CountingWriter); ok {
				assert.Zero(t, cw.Writes())
			}
		})
	}
	t.Run("nil context before a nil writer", func(t *testing.T) {
		var nilCtx context.Context
		n, err := c.DownloadJob(nilCtx, neverbounce.DownloadJobRequest{}, nil)
		assertValidation(t, err, "ctx", "nil")
		assert.Zero(t, n)
	})
	assert.Zero(t, ct.Attempts(), "nothing is sent")
}

func TestDownloadJobStreamsBytesUnchanged(t *testing.T) {
	types := []string{mediaOctet, "Text/CSV; Charset=UTF-8; Header=Present"}
	for _, f := range dlFiles() {
		for _, declare := range []bool{true, false} {
			for _, ctype := range types {
				t.Run(f.name+" "+ctype+" declared "+strconv.FormatBool(declare), func(t *testing.T) {
					body := testutil.NewBody(string(f.body))
					var w testutil.CountingWriter
					n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(ctype), dlLength(f.body, declare), body), &w)
					dlAssertStreamed(t, f.body, n, err, &w)
					assert.Equal(t, 1, body.Closes(), "the body is closed exactly once")
				})
			}
		}
	}
}

func TestDownloadJobStreamsOverLoopbackTLS(t *testing.T) {
	files := dlFiles()
	for _, proto := range testutil.Protocols() {
		for _, f := range files {
			for _, declare := range []bool{true, false} {
				t.Run(proto.Name()+" "+f.name+" declared "+strconv.FormatBool(declare), func(t *testing.T) {
					c, srv := newServerClientProto(t, proto, func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "text/csv; charset=utf-8")
						if declare {
							w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
						}
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(f.body)
					})
					var w testutil.CountingWriter
					n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
					dlAssertStreamed(t, f.body, n, err, &w)
					assert.Equal(t, 1, srv.Hits())
				})
			}
		}
	}
}

func TestDownloadJobAcceptsContentTypes(t *testing.T) {
	for _, ctype := range []string{
		mediaOctet, "APPLICATION/OCTET-STREAM", `application/octet-stream; name="results.csv"`,
		mediaCSV, "TEXT/CSV", "text/csv; charset=utf-8", "Text/Csv; Charset=UTF-8; Header=Present", " text/csv ",
		"text/csv;",
	} {
		t.Run(ctype, func(t *testing.T) {
			var w testutil.CountingWriter
			n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(ctype), -1, testutil.NewBody(dlCSV)), &w)
			dlAssertStreamed(t, []byte(dlCSV), n, err, &w)
		})
	}
}

// dlRejected is one response DownloadJob must refuse before writing.
type dlRejected struct {
	name      string
	status    int
	header    http.Header // nil for Content-Type application/json alone
	body      string
	declared  int64 // -1 for unknown; -2 for the body's own length
	kind      neverbounce.FailureKind
	apiStatus neverbounce.APIStatus
	field     string
	diag      string
	unread    bool // the body must not be read at all
}

// dlRunRejected serves tt in memory and asserts its failure with nothing
// written, the body closed once, and, when tt.unread, never read.
func dlRunRejected(t *testing.T, tt dlRejected, opts ...neverbounce.Option) {
	t.Helper()
	declared := tt.declared
	if declared == -2 {
		declared = int64(len(tt.body))
	}
	status := tt.status
	if status == 0 {
		status = http.StatusOK
	}
	body := testutil.NewBody(tt.body)
	spy := &testutil.CloseFlushSpy{}
	n, err := dlDownload(t, testutil.TransportWith(status, tt.header, declared, body), spy, opts...)
	dlAssertFailure(t, n, err, wantCall{Kind: tt.kind, Status: status, API: tt.apiStatus, Field: tt.field})
	assert.Equal(t, tt.diag, callError(t, err).Diagnostics().Message)
	assert.Zero(t, spy.Writes(), "nothing reaches the writer")
	assert.False(t, spy.Touched(), "the writer is never closed, flushed or synced")
	assert.Equal(t, 1, body.Closes(), "the body is closed exactly once")
	if tt.unread {
		assert.Zero(t, body.BytesRead(), "the body is never read")
	}
}

func TestDownloadJobJSONEnvelopeOn200(t *testing.T) {
	msg := `job ` + testEmail + ` is not complete; key ` + testKey
	env := func(s string) string { return `{"status":"` + s + `","message":"` + msg + `"}` }
	redacted := `job ` + testEmail + ` is not complete; key [redacted]`
	tests := []dlRejected{
		{name: dlGeneralFailure, body: env(dlGeneralFailure), kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure, diag: redacted},
		{name: "auth_failure", body: env("auth_failure"), kind: neverbounce.FailureUnauthorized, apiStatus: "auth_failure", diag: redacted},
		{name: "bad_referrer", body: env("bad_referrer"), kind: neverbounce.FailureUnauthorized, apiStatus: "bad_referrer", diag: redacted},
		{name: "temp_unavail", body: env("temp_unavail"), kind: neverbounce.FailureUnavailable, apiStatus: "temp_unavail", diag: redacted},
		{name: "throttle_triggered", body: env("throttle_triggered"), kind: neverbounce.FailureRateLimited, apiStatus: "throttle_triggered", diag: redacted},
		{name: "undocumented kept", body: env("job_not_ready"), kind: neverbounce.FailureUnexpectedResponse, apiStatus: "job_not_ready", diag: redacted},
		{name: "uppercase dropped", body: env("SUCCESS"), kind: neverbounce.FailureUnexpectedResponse, diag: redacted},
		{name: "success carries no file", body: env("success"), kind: neverbounce.FailureUnexpectedResponse, apiStatus: "success"},
		{name: "charset parameter", header: dlCT("Application/JSON; charset=utf-8"), body: env("temp_unavail"), kind: neverbounce.FailureUnavailable, apiStatus: "temp_unavail", diag: redacted},
	}
	for _, tt := range tests {
		for _, declared := range []int64{-2, -1} {
			tc := tt
			if tc.header == nil {
				tc.header = dlCT(mediaJSON)
			}
			tc.declared = declared
			t.Run(tc.name+" declared "+strconv.FormatBool(declared == -2), func(t *testing.T) {
				dlRunRejected(t, tc, neverbounce.WithProviderDiagnostics())
			})
		}
	}
	t.Run("diagnostics are opt-in", func(t *testing.T) {
		tt := dlRejected{header: dlCT(mediaJSON), body: env(dlGeneralFailure), declared: -1, kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure}
		dlRunRejected(t, tt)
	})
}

func TestDownloadJobJSONDownloadFailures(t *testing.T) {
	jsonCT := dlCT(mediaJSON)
	big := string(dlEnvelope(dlGeneralFailure, dlSniff+1))
	tests := []dlRejected{
		{name: "status missing", body: `{"message":"x"}`, declared: -2, kind: neverbounce.FailureMalformedResponse, field: "status"},
		{name: "status null", body: `{"status":null}`, declared: -2, kind: neverbounce.FailureMalformedResponse, field: "status"},
		{name: "status number", body: `{"status":1}`, declared: -2, kind: neverbounce.FailureMalformedResponse, field: "status"},
		{name: "status case-folded", body: `{"Status":"general_failure"}`, declared: -2, kind: neverbounce.FailureMalformedResponse, field: "status"},
		{name: "invalid json", body: `{"status":`, declared: -2, kind: neverbounce.FailureMalformedResponse},
		{name: "trailing data", body: `{"status":"general_failure"} {}`, declared: -2, kind: neverbounce.FailureMalformedResponse},
		{name: "repeated key", body: `{"status":"general_failure","status":"success"}`, declared: -2, kind: neverbounce.FailureMalformedResponse},
		{name: "invalid utf-8", body: "{\"status\":\"general_failure\",\"message\":\"\xff\"}", declared: -2, kind: neverbounce.FailureMalformedResponse},
		{name: "empty", body: "", declared: -2, kind: neverbounce.FailureMalformedResponse},
		{name: "top-level array", body: `[{"status":"general_failure"}]`, declared: -2, kind: neverbounce.FailureUnexpectedResponse},
		{name: "csv labeled as an envelope type", body: dlCSV, declared: -1, kind: neverbounce.FailureMalformedResponse},
		{name: "declared over 64 KiB", body: big, declared: -2, kind: neverbounce.FailureUnexpectedResponse, unread: true},
		{name: "streamed over 64 KiB", body: big, declared: -1, kind: neverbounce.FailureUnexpectedResponse},
		{name: "truncated", body: `{"status":"general_failure"}`, declared: 100, kind: neverbounce.FailureTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.header = jsonCT
			dlRunRejected(t, tt)
		})
	}
	t.Run("exactly 64 KiB is classified", func(t *testing.T) {
		body := dlEnvelope(dlGeneralFailure, dlSniff)
		for _, declared := range []int64{-2, -1} {
			tt := dlRejected{header: jsonCT, body: string(body), declared: declared, kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure}
			dlRunRejected(t, tt)
		}
	})
	t.Run("declared over the download limit", func(t *testing.T) {
		tt := dlRejected{header: jsonCT, body: `{"status":"general_failure"}`, declared: 101, kind: neverbounce.FailureResponseTooLarge, unread: true}
		dlRunRejected(t, tt, neverbounce.WithMaxDownloadBytes(100))
	})
	for _, end := range []error{io.ErrUnexpectedEOF, errDLRead} {
		t.Run("read error "+end.Error(), func(t *testing.T) {
			body := testutil.NewBody(`{"status":"general_`)
			body.End = end
			spy := &testutil.CloseFlushSpy{}
			n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, jsonCT, 28, body), spy)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK})
			assert.Zero(t, spy.Writes())
			assert.Equal(t, 1, body.Closes())
		})
	}
}

func TestDownloadJobDisguisedEnvelope(t *testing.T) {
	redacted := "not ready for [redacted]"
	tests := []dlRejected{
		{name: dlGeneralFailure, body: `{"status":"general_failure","message":"not ready for ` + testKey + `"}`, kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure, diag: redacted},
		{name: "whitespace around", body: " \t\r\n" + `{"status":"auth_failure"}` + "\r\n", kind: neverbounce.FailureUnauthorized, apiStatus: "auth_failure"},
		{name: "throttle_triggered", body: `{"status":"throttle_triggered"}`, kind: neverbounce.FailureRateLimited, apiStatus: "throttle_triggered"},
		{name: "temp_unavail spelled with an escape", body: testutil.Esc(`{"st~u0061tus":"temp_unavail"}`), kind: neverbounce.FailureUnavailable, apiStatus: "temp_unavail"},
		{name: "success", body: `{"status":"success","job_status":"complete"}`, kind: neverbounce.FailureUnexpectedResponse, apiStatus: "success"},
		{name: "undocumented", body: `{"status":"maintenance"}`, kind: neverbounce.FailureUnexpectedResponse, apiStatus: "maintenance"},
		{name: "empty status", body: `{"status":""}`, kind: neverbounce.FailureUnexpectedResponse},
		{name: "exactly 64 KiB", body: string(dlEnvelope(dlGeneralFailure, dlSniff)), kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure},
	}
	for _, tt := range tests {
		for _, ctype := range []string{mediaOctet, "text/csv; charset=utf-8"} {
			for _, declared := range []int64{-2, -1} {
				tc := tt
				tc.header = dlCT(ctype)
				tc.declared = declared
				t.Run(tc.name+" "+ctype+" declared "+strconv.FormatBool(declared == -2), func(t *testing.T) {
					dlRunRejected(t, tc, neverbounce.WithProviderDiagnostics())
				})
			}
		}
	}
}

func TestDownloadJobBraceCSVStreams(t *testing.T) {
	bodies := map[string]string{
		"brace header":                "{id},email\r\n1,a@example.com\r\n",
		"lone brace":                  "{",
		"whitespace then brace":       " \r\n{not json}\r\n",
		"object without status":       `{"message":"no status here"}`,
		"status not a string":         `{"status":1}`,
		"status null":                 `{"status":null}`,
		"status array":                `{"status":["general_failure"]}`,
		"status object":               `{"status":{"code":"general_failure"}}`,
		"status case-folded":          `{"Status":"general_failure"}`,
		"status nested":               `{"error":{"status":"general_failure"}}`,
		"repeated status":             `{"status":"general_failure","status":"general_failure"}`,
		"trailing data":               `{"status":"general_failure"}` + "\r\n1,a@example.com\r\n",
		"two objects":                 `{"status":"general_failure"}{"status":"general_failure"}`,
		"invalid utf-8 inside":        "{\"status\":\"general_failure\",\"x\":\"\xff\"}",
		"bom before envelope":         "\xef\xbb\xbf" + `{"status":"general_failure"}`,
		"array of envelopes":          `[{"status":"general_failure"}]`,
		"envelope past sniff window":  string(dlEnvelope(dlGeneralFailure, dlSniff+1)),
		"envelope after a csv header": "id\r\n" + `{"status":"general_failure"}`,
	}
	for name, content := range bodies {
		for _, ctype := range []string{mediaOctet, mediaCSV} {
			for _, declare := range []bool{true, false} {
				t.Run(name+" "+ctype+" declared "+strconv.FormatBool(declare), func(t *testing.T) {
					b := []byte(content)
					var w testutil.CountingWriter
					rt := testutil.TransportWith(http.StatusOK, dlCT(ctype), dlLength(b, declare), testutil.NewBody(content))
					n, err := dlDownload(t, rt, &w)
					dlAssertStreamed(t, b, n, err, &w)
				})
			}
		}
	}
}

func TestDownloadJobRejectsContentTypes(t *testing.T) {
	headers := map[string]http.Header{
		"missing":                 {},
		"empty value":             dlCT(""),
		"two identical headers":   dlCT(mediaOctet, mediaOctet),
		"two different headers":   dlCT(mediaCSV, mediaJSON),
		"text/html":               dlCT("text/html; charset=utf-8"),
		"application/xml":         dlCT("application/xml"),
		"text/plain":              dlCT("text/plain"),
		"application/csv":         dlCT("application/csv"),
		"vnd.ms-excel":            dlCT("application/vnd.ms-excel"),
		"json-seq":                dlCT("application/json-seq"),
		"unparsable no subtype":   dlCT("text/"),
		"unparsable parameter":    dlCT("application/octet-stream; =x"),
		"unparsable bare param":   dlCT("text/csv; charset"),
		"unparsable unterminated": dlCT(`application/octet-stream; name="results.csv`),
	}
	for name, h := range headers {
		for _, declared := range []int64{-2, -1} {
			t.Run(name+" declared "+strconv.FormatBool(declared == -2), func(t *testing.T) {
				tt := dlRejected{header: h, body: dlCSV, declared: declared, kind: neverbounce.FailureUnexpectedResponse, unread: true}
				dlRunRejected(t, tt)
			})
		}
	}
}

func TestDownloadJobRejectsContentEncoding(t *testing.T) {
	for _, enc := range [][]string{{"gzip"}, {"br"}, {"deflate"}, {"identity, gzip"}, {"identity", "gzip"}, {"x-custom"}} {
		t.Run(strings.Join(enc, "+"), func(t *testing.T) {
			h := dlCT(mediaOctet)
			h["Content-Encoding"] = enc
			tt := dlRejected{header: h, body: dlCSV, declared: -2, kind: neverbounce.FailureUnexpectedResponse, unread: true}
			dlRunRejected(t, tt)
		})
	}
	for _, enc := range [][]string{{"identity"}, {""}, {"identity, "}, {" IDENTITY "}} {
		t.Run("streams with "+strconv.Quote(strings.Join(enc, "+")), func(t *testing.T) {
			h := dlCT(mediaOctet)
			h["Content-Encoding"] = enc
			var w testutil.CountingWriter
			n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, h, -1, testutil.NewBody(dlCSV)), &w)
			dlAssertStreamed(t, []byte(dlCSV), n, err, &w)
		})
	}
	t.Run("status decides first", func(t *testing.T) {
		h := dlCT(mediaJSON)
		h["Content-Encoding"] = []string{"gzip"}
		tt := dlRejected{status: http.StatusServiceUnavailable, header: h, body: "\x1f\x8b", declared: -2, kind: neverbounce.FailureUnavailable}
		dlRunRejected(t, tt)
	})
}

func TestDownloadJobHTTPStatus(t *testing.T) {
	statuses := []struct {
		code int
		kind neverbounce.FailureKind
	}{
		{http.StatusCreated, neverbounce.FailureUnexpectedResponse},
		{http.StatusAccepted, neverbounce.FailureUnexpectedResponse},
		{http.StatusNoContent, neverbounce.FailureUnexpectedResponse},
		{http.StatusPartialContent, neverbounce.FailureUnexpectedResponse},
		{299, neverbounce.FailureUnexpectedResponse},
		{http.StatusMultipleChoices, neverbounce.FailureUnexpectedResponse},
		{http.StatusMovedPermanently, neverbounce.FailureUnexpectedResponse},
		{http.StatusFound, neverbounce.FailureUnexpectedResponse},
		{http.StatusSeeOther, neverbounce.FailureUnexpectedResponse},
		{http.StatusNotModified, neverbounce.FailureUnexpectedResponse},
		{http.StatusTemporaryRedirect, neverbounce.FailureUnexpectedResponse},
		{http.StatusPermanentRedirect, neverbounce.FailureUnexpectedResponse},
		{http.StatusBadRequest, neverbounce.FailureRejected},
		{http.StatusUnauthorized, neverbounce.FailureUnauthorized},
		{http.StatusForbidden, neverbounce.FailureUnauthorized},
		{http.StatusNotFound, neverbounce.FailureRejected},
		{http.StatusConflict, neverbounce.FailureRejected},
		{http.StatusRequestEntityTooLarge, neverbounce.FailureRejected},
		{http.StatusTooManyRequests, neverbounce.FailureRateLimited},
		{499, neverbounce.FailureRejected},
		{http.StatusInternalServerError, neverbounce.FailureUpstream},
		{http.StatusBadGateway, neverbounce.FailureUpstream},
		{http.StatusServiceUnavailable, neverbounce.FailureUnavailable},
		{http.StatusGatewayTimeout, neverbounce.FailureUpstream},
		{599, neverbounce.FailureUpstream},
		{600, neverbounce.FailureUnexpectedResponse},
		{999, neverbounce.FailureUnexpectedResponse},
	}
	excerpt := `{"status":"throttle_triggered","message":"slow down ` + testKey + `"}`
	for _, st := range statuses {
		t.Run(strconv.Itoa(st.code), func(t *testing.T) {
			tt := dlRejected{
				status: st.code, header: dlCT(mediaJSON), body: excerpt, declared: -2, kind: st.kind,
				apiStatus: "throttle_triggered", diag: "slow down [redacted]",
			}
			dlRunRejected(t, tt, neverbounce.WithProviderDiagnostics())
		})
	}
	others := []dlRejected{
		{name: "csv body", status: http.StatusNotFound, header: dlCT(mediaOctet), body: dlCSV, declared: -2, kind: neverbounce.FailureRejected},
		{name: "unreadable excerpt", status: http.StatusBadGateway, header: dlCT(mediaJSON), body: `{"status":"temp_un`, declared: 500, kind: neverbounce.FailureUpstream},
		{name: "declared over 64 KiB", status: http.StatusTooManyRequests, header: dlCT(mediaJSON), body: string(dlEnvelope("temp_unavail", dlSniff+1)), declared: -2, kind: neverbounce.FailureRateLimited, unread: true},
		{name: "streamed over 64 KiB", status: http.StatusTooManyRequests, header: dlCT(mediaJSON), body: string(dlEnvelope("temp_unavail", dlSniff+1)), declared: -1, kind: neverbounce.FailureRateLimited},
		{name: "exactly 64 KiB", status: http.StatusTooManyRequests, header: dlCT(mediaJSON), body: string(dlEnvelope("temp_unavail", dlSniff)), declared: -1, kind: neverbounce.FailureRateLimited, apiStatus: "temp_unavail"},
		{name: "uppercase status dropped", status: http.StatusForbidden, header: dlCT(mediaJSON), body: `{"status":"AUTH_FAILURE"}`, declared: -2, kind: neverbounce.FailureUnauthorized},
	}
	for _, tt := range others {
		t.Run(tt.name, func(t *testing.T) { dlRunRejected(t, tt) })
	}
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name()+" over loopback", func(t *testing.T) {
			c, _ := newServerClientProto(t, proto, respond(http.StatusServiceUnavailable, `{"status":"temp_unavail"}`))
			var w testutil.CountingWriter
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			dlAssertFailure(t, n, err, wantCall{
				Kind: neverbounce.FailureUnavailable, Status: http.StatusServiceUnavailable, API: neverbounce.APIStatusTempUnavail,
			})
			assert.Zero(t, w.Writes())
		})
	}
}

func TestDownloadJobInformationalNeverRead(t *testing.T) {
	for _, code := range []int{http.StatusContinue, http.StatusSwitchingProtocols, http.StatusProcessing, http.StatusEarlyHints, 199} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			tt := dlRejected{status: code, header: dlCT(mediaOctet), body: dlCSV, declared: -1, kind: neverbounce.FailureUnexpectedResponse, unread: true}
			dlRunRejected(t, tt)
		})
	}
}

func TestDownloadJobDeclaredLengthOverLimit(t *testing.T) {
	const limit = 10
	t.Run("one over", func(t *testing.T) {
		tt := dlRejected{header: dlCT(mediaOctet), body: "0123456789A", declared: -2, kind: neverbounce.FailureResponseTooLarge, unread: true}
		dlRunRejected(t, tt, neverbounce.WithMaxDownloadBytes(limit))
	})
	t.Run("far over, short body", func(t *testing.T) {
		tt := dlRejected{header: dlCT(mediaCSV), body: "x", declared: 1 << 40, kind: neverbounce.FailureResponseTooLarge, unread: true}
		dlRunRejected(t, tt, neverbounce.WithMaxDownloadBytes(limit))
	})
	t.Run("exactly the limit", func(t *testing.T) {
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), limit, testutil.NewBody("0123456789")), &w,
			neverbounce.WithMaxDownloadBytes(limit))
		dlAssertStreamed(t, []byte("0123456789"), n, err, &w)
	})
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name()+" over loopback", func(t *testing.T) {
			content := dlPattern(4096)
			c, _ := newServerClientProto(t, proto, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", mediaOctet)
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				_, _ = w.Write(content)
			}, neverbounce.WithMaxDownloadBytes(4095))
			var w testutil.CountingWriter
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK})
			assert.Zero(t, w.Writes())
		})
	}
}

func TestDownloadJobStreamedOverflow(t *testing.T) {
	for _, limit := range []int64{1, 2, 100, dlChunk - 1, dlChunk, dlChunk + 1, dlSniff, dlSniff + 1, 100_000, 3*dlChunk + 7} {
		for _, extra := range []int{1, 2, dlChunk} {
			t.Run(strconv.FormatInt(limit, 10)+"+"+strconv.Itoa(extra), func(t *testing.T) {
				content := dlPattern(int(limit) + extra)
				body := testutil.NewBody(string(content))
				spy := &testutil.CloseFlushSpy{}
				n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body), spy,
					neverbounce.WithMaxDownloadBytes(limit))
				dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK, Written: limit})
				dlAssertBytes(t, content[:limit], spy.Bytes())
				assert.False(t, spy.Touched())
				assert.Equal(t, 1, body.Closes())
			})
		}
	}
	t.Run("endless body", func(t *testing.T) {
		body := &testutil.EndlessBody{}
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaCSV), -1, body), &w,
			neverbounce.WithMaxDownloadBytes(1<<20))
		dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK, Written: 1 << 20})
		assert.Equal(t, 1<<20, w.Len())
		assert.Equal(t, 1, body.Closes())
	})
}

func TestDownloadJobExactLimit(t *testing.T) {
	for _, limit := range []int64{1, 2, dlChunk, dlSniff, dlSniff + 1, 100_000} {
		for _, declare := range []bool{true, false} {
			t.Run(strconv.FormatInt(limit, 10)+" declared "+strconv.FormatBool(declare), func(t *testing.T) {
				content := dlPattern(int(limit))
				var w testutil.CountingWriter
				rt := testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), dlLength(content, declare), testutil.NewBody(string(content)))
				n, err := dlDownload(t, rt, &w, neverbounce.WithMaxDownloadBytes(limit))
				dlAssertStreamed(t, content, n, err, &w)
			})
		}
	}
	t.Run("limit 1 with an empty body", func(t *testing.T) {
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody("")), &w,
			neverbounce.WithMaxDownloadBytes(1))
		dlAssertStreamed(t, []byte{}, n, err, &w)
		assert.Zero(t, w.Writes())
	})
	t.Run("limit 1 with two bytes", func(t *testing.T) {
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody("ab")), &w,
			neverbounce.WithMaxDownloadBytes(1))
		dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK, Written: 1})
		assert.Equal(t, []byte("a"), w.Bytes())
	})
	t.Run("an envelope beats the limit", func(t *testing.T) {
		tt := dlRejected{header: dlCT(mediaOctet), body: `{"status":"general_failure"}`, declared: -1, kind: neverbounce.FailureRejected, apiStatus: dlGeneralFailure}
		dlRunRejected(t, tt, neverbounce.WithMaxDownloadBytes(5))
	})
}

// dlCloseBodies returns, by name, makers of a body over content that ignores
// Close, as no real body does, and of one that fails every read after it,
// like a real one.
func dlCloseBodies(content []byte) map[string]func() *testutil.Body {
	return map[string]func() *testutil.Body{
		"body ignoring close": func() *testutil.Body {
			b := testutil.NewBody(string(content))
			b.IgnoreClose = true
			return b
		},
		"body failing after close": func() *testutil.Body { return testutil.NewBody(string(content)) },
	}
}

func TestDownloadJobTruncatedInMemory(t *testing.T) {
	content := dlPattern(200 << 10)
	tests := []struct {
		name     string
		served   int
		declared int64
		end      error
		written  int64
	}{
		{"clean end within the sniff window", 1000, 2000, nil, 0},
		{"clean end filling the sniff window", dlSniff, dlSniff + 1, nil, 0},
		{"clean end one past the sniff window", dlSniff + 1, dlSniff + 2, nil, dlSniff + 1},
		{"clean end after streaming began", 100 << 10, 200 << 10, nil, 100 << 10},
		{"unexpected eof within the sniff window", 1000, 2000, io.ErrUnexpectedEOF, 0},
		{"unexpected eof after streaming began", 100 << 10, 200 << 10, io.ErrUnexpectedEOF, 100 << 10},
		{"chunked unexpected eof within the sniff window", 1000, -1, io.ErrUnexpectedEOF, 0},
		{"chunked unexpected eof after streaming began", 100 << 10, -1, io.ErrUnexpectedEOF, 100 << 10},
		{"read error after streaming began", 100 << 10, -1, errDLRead, 100 << 10},
		{"reset after streaming began", 100 << 10, -1, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 100 << 10},
		{"net error after streaming began", 100 << 10, -1, &net.OpError{Op: "read", Net: "tcp", Err: errDLRead}, 100 << 10},
		{"longer than declared", 3000, 2000, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.NewBody(string(content[:tt.served]))
			body.End = tt.end
			spy := &testutil.CloseFlushSpy{}
			n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), tt.declared, body), spy)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK, Written: tt.written})
			dlAssertBytes(t, content[:tt.written], spy.Bytes())
			assert.False(t, spy.Touched())
			assert.Equal(t, 1, body.Closes())
		})
	}
}

// dlHijack returns a handler that takes over the HTTP/1.1 connection, writes
// raw, the status line, headers and a partial body, and closes it.
func dlHijack(raw []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.Write(raw)
		_ = rw.Flush()
	}
}

// dlAbort returns a handler that declares a length, writes and flushes
// partial, then aborts the response.
func dlAbort(declared int, partial []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaOctet)
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(partial)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
}

func TestDownloadJobTruncatedOverLoopback(t *testing.T) {
	content := dlPattern(200 << 10)
	head := func(extra string) string {
		return "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n" + extra + "\r\n"
	}
	chunk := func(data []byte) []byte {
		out := append([]byte(strconv.FormatInt(int64(len(data)), 16)+"\r\n"), data...)
		return append(out, '\r', '\n')
	}
	tests := []struct {
		name    string
		raw     []byte
		written int64
	}{
		{"hijack within the sniff window", append([]byte(head("Content-Length: 2000\r\n")), content[:1000]...), 0},
		{"hijack after streaming began", append([]byte(head("Content-Length: 204800\r\n")), content[:100<<10]...), 100 << 10},
		{"chunked eof within the sniff window", append([]byte(head("Transfer-Encoding: chunked\r\n")), chunk(content[:1000])...), 0},
		{"chunked eof after streaming began", append([]byte(head("Transfer-Encoding: chunked\r\n")), chunk(content[:100<<10])...), 100 << 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newServerClient(t, dlHijack(tt.raw))
			spy := &testutil.CloseFlushSpy{}
			n, err := c.DownloadJob(t.Context(), dlRequest(), spy)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK, Written: tt.written})
			dlAssertBytes(t, content[:tt.written], spy.Bytes())
			assert.False(t, spy.Touched())
			assert.Equal(t, 1, srv.Hits(), "never retried")
		})
	}
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name()+" aborted after streaming began", func(t *testing.T) {
			c, srv := newServerClientProto(t, proto, dlAbort(len(content), content[:100<<10]))
			var w testutil.CountingWriter
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			ce := callError(t, err)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK, Written: ce.BytesWritten})
			assert.Equal(t, int64(w.Len()), ce.BytesWritten)
			assert.LessOrEqual(t, ce.BytesWritten, int64(100<<10))
			dlAssertBytes(t, content[:ce.BytesWritten], w.Bytes())
			assert.Equal(t, 1, srv.Hits(), "never retried")
		})
	}
}

// dlLateLiar writes correctly until it holds at least after bytes, then
// reports the impossible count n.
type dlLateLiar struct {
	testutil.CountingWriter

	after int
	n     int
}

// Write keeps p and reports len(p) until after bytes are held, then n.
func (w *dlLateLiar) Write(p []byte) (int, error) {
	held := w.Len()
	_, _ = w.CountingWriter.Write(p)
	if held >= w.after {
		return w.n, nil
	}
	return len(p), nil
}

// dlFullThenFail writes every byte and then reports an error anyway.
type dlFullThenFail struct {
	testutil.CountingWriter
}

// Write keeps p and fails.
func (w *dlFullThenFail) Write(p []byte) (int, error) {
	n, _ := w.CountingWriter.Write(p)
	return n, errDLWriter
}

func TestDownloadJobWriterFailures(t *testing.T) {
	content := dlPattern(200 << 10)
	type sized interface {
		io.Writer
		Bytes() []byte
	}
	tests := []struct {
		name    string
		w       sized
		written int64
		held    int // bytes the writer kept, which may exceed what it reported
	}{
		{"fail after 0", &testutil.FailAfterWriter{N: 0}, 0, 0},
		{"fail after 1", &testutil.FailAfterWriter{N: 1}, 1, 1},
		{"fail after 4096", &testutil.FailAfterWriter{N: 4096}, 4096, 4096},
		{"fail after one chunk", &testutil.FailAfterWriter{N: dlChunk}, dlChunk, dlChunk},
		{"fail one past a chunk", &testutil.FailAfterWriter{N: dlChunk + 1}, dlChunk + 1, dlChunk + 1},
		{"fail after the sniff window", &testutil.FailAfterWriter{N: dlSniff + 1}, dlSniff + 1, dlSniff + 1},
		{"fail after 100000", &testutil.FailAfterWriter{N: 100_000}, 100_000, 100_000},
		{"short 0", &testutil.ShortWriter{Max: 0}, 0, 0},
		{"short 1", &testutil.ShortWriter{Max: 1}, 1, 1},
		{"short 1000", &testutil.ShortWriter{Max: 1000}, 1000, 1000},
		{"short by one", &testutil.ShortWriter{Max: dlChunk - 1}, dlChunk - 1, dlChunk - 1},
		{"count -1", &testutil.InvalidCountWriter{N: -1}, 0, dlChunk},
		{"count len+1", &testutil.InvalidCountWriter{N: dlChunk + 1}, 0, dlChunk},
		{"count huge", &testutil.InvalidCountWriter{N: int(^uint(0) >> 1)}, 0, dlChunk},
		{"count -1 on a later chunk", &dlLateLiar{after: 2 * dlChunk, n: -1}, 2 * dlChunk, 2*dlChunk + 1},
		{"count len+1 on a later chunk", &dlLateLiar{after: dlChunk, n: dlChunk + 1}, dlChunk, 2 * dlChunk},
		{"full write with an error", &dlFullThenFail{}, dlChunk, dlChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.NewBody(string(content))
			n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)), body), tt.w)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureWriteFailed, Status: http.StatusOK, Written: tt.written})
			dlAssertBytes(t, content[:tt.held], tt.w.Bytes())
			assert.Equal(t, 1, body.Closes())
		})
	}
}

func TestDownloadJobNeverTouchesWriter(t *testing.T) {
	content := dlPattern(100 << 10)
	paths := []struct {
		name  string
		rt    func() testutil.RoundTripFunc
		ok    bool
		limit int64
	}{
		{"success", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody(string(content)))
		}, true, 0},
		{"empty success", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), 0, testutil.NewBody(""))
		}, true, 0},
		{"envelope", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody(`{"status":"general_failure"}`))
		}, false, 0},
		{"content type", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT("text/html"), -1, testutil.NewBody(dlCSV))
		}, false, 0},
		{"status", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusInternalServerError, dlCT(mediaOctet), -1, testutil.NewBody(dlCSV))
		}, false, 0},
		{"truncated", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)+1), testutil.NewBody(string(content)))
		}, false, 0},
		{"overflow", func() testutil.RoundTripFunc {
			return testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, testutil.NewBody(string(content)))
		}, false, int64(len(content) - 1)},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			spy := &testutil.CloseFlushSpy{}
			var opts []neverbounce.Option
			if p.limit > 0 {
				opts = append(opts, neverbounce.WithMaxDownloadBytes(p.limit))
			}
			_, err := dlDownload(t, p.rt(), spy, opts...)
			if p.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.False(t, spy.Touched(), "the writer is never closed, flushed or synced")
		})
	}
}

func TestDownloadJobBlockingWriterCancel(t *testing.T) {
	content := dlPattern(200 << 10)
	for name, newBody := range dlCloseBodies(content) {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := newBody()
				ct := &testutil.CountingTransport{Next: testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)), body)}
				c := newTransportClient(t, ct)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				w := testutil.NewBlockingWriter()
				var (
					n    int64
					err  error
					done = make(chan struct{})
				)
				go func() {
					defer close(done)
					n, err = c.DownloadJob(ctx, dlRequest(), w)
				}()
				<-w.Entered()
				cancel()
				synctest.Wait()
				select {
				case <-done:
					require.FailNow(t, "DownloadJob returned while its writer was blocked")
				default:
				}
				assert.Equal(t, 1, body.Closes(), "canceling closes the body at once, even while Write blocks")
				w.Release()
				<-done
				assert.Equal(t, 1, body.Closes(), "closed exactly once")
				dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureCanceled, Status: http.StatusOK, Written: dlChunk})
				assert.Equal(t, dlChunk, w.Len(), "nothing is written after the cancellation is seen")
				assert.Equal(t, 1, ct.Attempts())
			})
		})
	}
}

// dlCancelingWriter cancels the call's context in its first Write, then keeps
// every byte.
type dlCancelingWriter struct {
	testutil.CountingWriter

	cancel context.CancelFunc
}

// Write cancels and keeps p.
func (w *dlCancelingWriter) Write(p []byte) (int, error) {
	w.cancel()
	return w.CountingWriter.Write(p)
}

func TestDownloadJobCancelFromWriter(t *testing.T) {
	content := dlPattern(200 << 10)
	for name, newBody := range dlCloseBodies(content) {
		for _, declare := range []bool{true, false} {
			t.Run(name+" declared "+strconv.FormatBool(declare), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				body := newBody()
				c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), dlLength(content, declare), body))
				w := &dlCancelingWriter{cancel: cancel}
				n, err := c.DownloadJob(ctx, dlRequest(), w)
				dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureCanceled, Status: http.StatusOK, Written: dlChunk})
				dlAssertBytes(t, content[:dlChunk], w.Bytes())
				assert.Equal(t, 1, w.Writes(), "no Write after the cancellation")
				assert.Equal(t, 1, body.Closes())
			})
		}
	}
}

func TestDownloadJobCancelDuringCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		content := dlPattern(100 << 10)
		body := testutil.NewBody(string(content))
		body.Block = true
		c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var w testutil.CountingWriter
		var (
			n    int64
			err  error
			done = make(chan struct{})
		)
		start := time.Now()
		go func() {
			defer close(done)
			n, err = c.DownloadJob(ctx, dlRequest(), &w)
		}()
		synctest.Wait()
		assert.Equal(t, len(content), w.Len(), "everything received is written while the body stalls")
		cancel()
		<-done
		dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureCanceled, Status: http.StatusOK, Written: int64(len(content))})
		assert.Zero(t, time.Since(start))
		assert.Equal(t, 1, body.Closes())
	})
}

func TestDownloadJobTimeouts(t *testing.T) {
	content := dlPattern(100 << 10)
	t.Run("download timeout during the copy", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := testutil.NewBody(string(content))
			body.Block = true
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body),
				neverbounce.WithDownloadTimeout(2*time.Minute))
			var w testutil.CountingWriter
			start := time.Now()
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			assert.Equal(t, 2*time.Minute, time.Since(start))
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTimeout, Status: http.StatusOK, Written: int64(len(content))})
			dlAssertBytes(t, content, w.Bytes())
			assert.Equal(t, 1, body.Closes())
		})
	})
	t.Run("download timeout while sniffing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := testutil.NewBody(string(content[:1000]))
			body.Block = true
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body),
				neverbounce.WithDownloadTimeout(time.Minute))
			var w testutil.CountingWriter
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTimeout, Status: http.StatusOK})
			assert.Zero(t, w.Writes())
		})
	})
	t.Run("caller deadline during the copy", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := testutil.NewBody(string(content))
			body.Block = true
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body))
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			var w testutil.CountingWriter
			start := time.Now()
			n, err := c.DownloadJob(ctx, dlRequest(), &w)
			assert.Equal(t, time.Minute, time.Since(start))
			// The caller's deadline, not the SDK's timeout: it never matches ErrTimeout.
			dlAssertFailure(t, n, err, wantCall{
				Kind: neverbounce.FailureDeadlineExceeded, Status: http.StatusOK, Written: int64(len(content)),
			})
		})
	})
	t.Run("the JSON timeout does not limit a download", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := testutil.NewBody(string(content))
			body.Step, body.Gap = 8<<10, 5*time.Second
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)), body),
				neverbounce.WithTimeout(time.Second), neverbounce.WithDownloadTimeout(time.Hour))
			var w testutil.CountingWriter
			start := time.Now()
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			dlAssertStreamed(t, content, n, err, &w)
			assert.GreaterOrEqual(t, time.Since(start), time.Minute, "the download outlasted WithTimeout many times over")
		})
	})
	t.Run("a slow download ends at the download timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			body := testutil.NewBody(string(content))
			body.Step, body.Gap = 8<<10, 5*time.Second
			c := newTransportClient(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)), body),
				neverbounce.WithTimeout(10*time.Minute), neverbounce.WithDownloadTimeout(42*time.Second))
			var w testutil.CountingWriter
			start := time.Now()
			n, err := c.DownloadJob(t.Context(), dlRequest(), &w)
			assert.Equal(t, 42*time.Second, time.Since(start))
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTimeout, Status: http.StatusOK, Written: int64(w.Len())})
		})
	})
}

func TestDownloadJobRoundTripFailures(t *testing.T) {
	for name, rtErr := range map[string]error{
		"plain error":   errDLRead,
		"dial error":    &net.OpError{Op: "dial", Net: "tcp", Err: errDLRead},
		"reset":         &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET},
		"network error": &net.DNSError{Err: testKey, Name: testEmail},
	} {
		t.Run(name, func(t *testing.T) {
			var w testutil.CountingWriter
			n, err := dlDownload(t, testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				testutil.Drain(r)
				return nil, rtErr
			}), &w)
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport})
			assert.Zero(t, w.Writes())
		})
	}
	t.Run("download timeout before the answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ct := &testutil.CountingTransport{Next: testutil.RoundTripFunc(testutil.BlockUntilDone)}
			c := newTransportClient(t, ct, neverbounce.WithDownloadTimeout(3*time.Minute), neverbounce.WithTimeout(time.Second))
			start := time.Now()
			n, err := c.DownloadJob(t.Context(), dlRequest(), io.Discard)
			assert.Equal(t, 3*time.Minute, time.Since(start), "the download timeout, not WithTimeout, bounds the call")
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTimeout})
			assert.Equal(t, 1, ct.Attempts())
		})
	})
	t.Run("canceled before the answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := newTransportClient(t, testutil.RoundTripFunc(testutil.BlockUntilDone))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var (
				n    int64
				err  error
				done = make(chan struct{})
			)
			go func() {
				defer close(done)
				n, err = c.DownloadJob(ctx, dlRequest(), io.Discard)
			}()
			synctest.Wait()
			cancel()
			<-done
			dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureCanceled})
		})
	})
}

func TestDownloadJobStalledBody(t *testing.T) {
	content := dlPattern(100 << 10)
	t.Run("stall after streaming began", func(t *testing.T) {
		body := testutil.NewBody(string(content))
		body.Stall = true
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body), &w)
		dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK, Written: int64(len(content))})
		dlAssertBytes(t, content, w.Bytes())
		assert.Equal(t, 1, body.Closes())
	})
	t.Run("stall while sniffing", func(t *testing.T) {
		body := testutil.NewBody(string(content[:10]))
		body.Stall = true
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), -1, body), &w)
		dlAssertFailure(t, n, err, wantCall{Kind: neverbounce.FailureTransport, Status: http.StatusOK})
		assert.Zero(t, w.Writes())
	})
	t.Run("empty reads below the bound are progress", func(t *testing.T) {
		body := testutil.NewBody(string(content))
		body.Step, body.Empties = 4096, 99
		var w testutil.CountingWriter
		n, err := dlDownload(t, testutil.TransportWith(http.StatusOK, dlCT(mediaOctet), int64(len(content)), body), &w)
		dlAssertStreamed(t, content, n, err, &w)
	})
}
