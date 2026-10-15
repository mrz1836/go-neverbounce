package neverbounce_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Leak markers. Each is distinct, so a failure names what leaked. None is a
// real secret or address.
const (
	pvEmail        = "pv.email.marker@example.com"
	pvRemoteUser   = "pvremoteusermarker"
	pvRemotePass   = "pvremotepassmarker" // gitleaks:allow (synthetic test marker)
	pvRemotePath   = "pv-remote-path-marker"
	pvRemoteQuery  = "pvremotequerymarker"
	pvCallbackUser = "pvcallbackusermarker"
	pvCallbackPass = "pvcallbackpassmarker" // gitleaks:allow (synthetic test marker)
	pvCallbackPath = "pv-callback-path-marker"
	pvHeaderName   = "X-Pv-Header-Name-Marker"
	pvHeaderMarker = "pv-header-value-marker" // gitleaks:allow (synthetic test marker)
	pvColumn       = "pv_column_marker"
	pvRowValue     = "pv-row-value-marker"
	pvFilename     = "pv-filename-marker.csv"
	pvMessage      = "pv-provider-message-marker"
	pvLocation     = "pvlocationmarker"
	pvTransport    = "pv-transport-marker"
	pvQuery        = "pvquerymarker"
	pvProxyPass    = "pvproxypassmarker" // gitleaks:allow (synthetic test marker)
	pvBadValue     = "PvBadValueMarker"

	// pvRemoteURL is a remote list URL with credentials and a signed query.
	pvRemoteURL = "https://" + pvRemoteUser + ":" + pvRemotePass + "@lists.example.com/" + pvRemotePath +
		".csv?sig=" + pvRemoteQuery

	// pvCallbackURL is a callback URL with basic-auth credentials.
	pvCallbackURL = "https://" + pvCallbackUser + ":" + pvCallbackPass + "@hooks.example.net/" + pvCallbackPath

	// pvHeaderValue is a callback header value carrying a token.
	pvHeaderValue = "Bearer " + pvHeaderMarker

	// pvDiagMessage is provider text that echoes the request, as provider
	// text may: the key, an address, a row value and the remote URL.
	pvDiagMessage = pvMessage + " key=" + testKey + " email=" + pvEmail + " row=" + pvRowValue + " url=" + pvRemoteURL

	// pvJobID is the job every job operation names.
	pvJobID     neverbounce.JobID = 4242
	pvJobIDText                   = "4242"
)

// errPvEcho is the transport failure whose message echoes the request.
var errPvEcho = errors.New("pv transport echo")

// pvSecrets returns every marker that must never print.
func pvSecrets() []string {
	return []string{
		testKey, testEmail, pvEmail, pvRemoteUser, pvRemotePass, pvRemotePath, pvRemoteQuery, pvCallbackUser,
		pvCallbackPass, pvCallbackPath, pvHeaderName, pvHeaderMarker, pvColumn, pvRowValue, pvFilename, pvMessage,
		pvLocation, pvTransport, pvQuery, pvProxyPass, pvBadValue,
	}
}

// pvDiagWant is pvDiagMessage as Diagnostics returns it: only the key is
// scrubbed; the address and the other request data may remain, as documented.
func pvDiagWant() string { return strings.ReplaceAll(pvDiagMessage, testKey, "[redacted]") }

// pvDiagBody returns an envelope with status whose message is pvDiagMessage.
func pvDiagBody(status string) string {
	return `{"status":` + testutil.JSONQuote(status) + `,"message":` + testutil.JSONQuote(pvDiagMessage) +
		`,"execution_time":3}`
}

// pvSupplied returns a supplied list whose column names and values hold
// markers.
func pvSupplied() neverbounce.SuppliedList {
	return neverbounce.SuppliedList{
		Columns: []string{"id", pvColumn, "email"},
		Rows:    [][]string{{"1001", pvRowValue, pvEmail}, {"1002", pvRowValue, testEmail}},
	}
}

// pvCreateRequest returns a valid supplied-list job request whose every text
// field holds a marker.
func pvCreateRequest() neverbounce.CreateJobRequest {
	return neverbounce.CreateJobRequest{
		Supplied:               pvSupplied(),
		Filename:               pvFilename,
		AutoParse:              true,
		LeverageHistoricalData: new(false),
		CallbackURL:            pvCallbackURL,
		CallbackHeaders:        map[string]string{pvHeaderName: pvHeaderValue},
	}
}

// pvRemoteRequest returns a valid remote-URL job request whose every text
// field holds a marker.
func pvRemoteRequest() neverbounce.CreateJobRequest {
	r := pvCreateRequest()
	r.Supplied = neverbounce.SuppliedList{}
	r.RemoteURL = pvRemoteURL
	return r
}

// pvWalk returns a call that ranges over req's pages and returns the error the
// walk ends with.
func pvWalk(req neverbounce.JobResultPagesRequest) func(context.Context, *neverbounce.Client) error {
	return func(ctx context.Context, c *neverbounce.Client) error {
		return walkErr(c.JobResultPages(ctx, req))
	}
}

// pvDownload returns a call that downloads into w.
func pvDownload(w io.Writer) func(context.Context, *neverbounce.Client) error {
	return func(ctx context.Context, c *neverbounce.Client) error {
		_, err := c.DownloadJob(ctx, neverbounce.DownloadJobRequest{JobID: pvJobID, LineFeed: neverbounce.LineFeedCRLF}, w)
		return err
	}
}

// pvOps returns every operation case for pvJobID, with marker-bearing requests
// where a request carries data: CheckEmail with every option, and CreateJob
// with a supplied list and with a remote URL. The download takes any file.
func pvOps() []opCase {
	create := func(req neverbounce.CreateJobRequest) func(context.Context, *neverbounce.Client) error {
		return func(ctx context.Context, c *neverbounce.Client) error {
			return noValueWith(c.CreateJob(ctx, req))
		}
	}
	var ops []opCase
	for _, o := range opCasesFor(pvJobID) {
		switch o.name {
		case "CheckEmail":
			o.call = func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.CheckEmail(ctx, neverbounce.CheckEmailRequest{
					Email: pvEmail, AddressInfo: true, CreditsInfo: true, LeverageHistoricalData: new(true),
				}))
			}
		case "CreateJob":
			remote := o
			o.name, o.call = "CreateJob supplied", create(pvCreateRequest())
			remote.name, remote.call = "CreateJob remote", create(pvRemoteRequest())
			ops = append(ops, o, remote)
			continue
		case "DownloadJob":
			o.call = pvDownload(&bytes.Buffer{})
		}
		ops = append(ops, o)
	}
	return ops
}

// pvOpNamed returns the operation called name.
func pvOpNamed(name string) opCase {
	for _, op := range pvOps() {
		if op.name == name {
			return op
		}
	}
	panic("no operation " + name)
}

// pvCallFunc makes one call of op through a client built with opts and
// returns its error.
type pvCallFunc func(t *testing.T, op opCase, opts ...neverbounce.Option) error

// pvFlow is a way for a call to fail.
type pvFlow struct {
	name string
	kind neverbounce.FailureKind
	// downloadKind is DownloadJob's kind when it differs from kind.
	downloadKind neverbounce.FailureKind
	// diag reports that the provider's message is kept with
	// WithProviderDiagnostics.
	diag bool
	// page is the CallError's Page when the flow fails past the operation's
	// first page; 0 keeps the operation's.
	page int
	// written is the CallError's BytesWritten: what a download delivered
	// before it failed.
	written int64
	call    pvCallFunc
}

// pvCase is one flow applied to one operation.
type pvCase struct {
	op   opCase
	flow pvFlow
}

// kind returns the failure kind the case must produce.
func (c pvCase) kind() neverbounce.FailureKind {
	if c.op.op == neverbounce.OpDownloadJob && c.flow.downloadKind != "" {
		return c.flow.downloadKind
	}
	return c.flow.kind
}

// page returns the CallError Page the case must produce.
func (c pvCase) page() int {
	if c.flow.page != 0 {
		return c.flow.page
	}
	return c.op.page
}

// pvCall calls op through rt with a client built with opts.
func pvCall(ctx context.Context, t *testing.T, op opCase, rt http.RoundTripper, opts ...neverbounce.Option) error {
	t.Helper()
	return op.call(ctx, newTransportClient(t, rt, opts...))
}

// pvInBubble runs call inside a synctest bubble, so that timeouts elapse on
// the bubble's clock, and returns its error.
func pvInBubble(t *testing.T, call func(t *testing.T) error) error {
	t.Helper()
	var err error
	synctest.Test(t, func(t *testing.T) { err = call(t) })
	return err
}

// pvHeader returns response headers that echo secrets, with contentType when
// it is not empty.
func pvHeader(contentType string) http.Header {
	h := http.Header{
		"X-Pv-Debug":       {testKey + " " + pvEmail + " " + pvRowValue},
		"Set-Cookie":       {"pv=" + pvQuery + "; Path=/"},
		"Www-Authenticate": {`Basic realm="` + pvCallbackPass + `"`},
	}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return h
}

// pvRedirectHeader returns hostile response headers with a Location.
func pvRedirectHeader(location string) http.Header {
	h := pvHeader(mediaJSON)
	h.Set("Location", location)
	return h
}

// pvAnswer returns a call answered in memory with status, header and body,
// declared with contentLength.
func pvAnswer(status int, header http.Header, body string, contentLength int64, flowOpts ...neverbounce.Option) pvCallFunc {
	return func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
		t.Helper()
		rt := testutil.TransportWith(status, header, contentLength, io.NopCloser(strings.NewReader(body)))
		return pvCall(t.Context(), t, op, rt, slices.Concat(flowOpts, opts)...)
	}
}

// pvStatic returns a call answered with status and a JSON body, declared with
// its length.
func pvStatic(status int, body string, flowOpts ...neverbounce.Option) pvCallFunc {
	return pvAnswer(status, pvHeader(mediaJSON), body, int64(len(body)), flowOpts...)
}

// pvScripted returns a call answered by each body in turn, on HTTP 200.
func pvScripted(bodies ...string) pvCallFunc {
	return func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
		t.Helper()
		steps := make([]testutil.Step, len(bodies))
		for i, b := range bodies {
			steps[i] = testutil.Step{Header: pvHeader(mediaJSON), Body: b}
		}
		return pvCall(t.Context(), t, op, testutil.NewScript(steps...), opts...)
	}
}

// pvEcho reads r and returns everything it carries, the body with the key and
// the request data included, with more markers.
func pvEcho(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	return string(body) + " " + r.URL.String() + " " + pvDiagMessage + " " + pvTransport
}

// pvEchoErr returns an error whose text is echo.
func pvEchoErr(echo string) error { return fmt.Errorf("%w: %s", errPvEcho, echo) }

// pvFailWith returns a call whose round trip fails with the error build
// makes from the request.
func pvFailWith(build func(echo string) error) pvCallFunc {
	return func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
		t.Helper()
		rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, build(pvEcho(r)) })
		return pvCall(t.Context(), t, op, rt, opts...)
	}
}

// pvReadFails returns a call answered on HTTP 200 with contentType, whose
// body fails after prefix with the error build makes from the request.
func pvReadFails(contentType, prefix string, build func(echo string) error) pvCallFunc {
	return func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
		t.Helper()
		rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(build(pvEcho(r))))
			resp := testutil.Response(r, http.StatusOK, io.NopCloser(body), -1)
			resp.Header = pvHeader(contentType)
			return resp, nil
		})
		return pvCall(t.Context(), t, op, rt, opts...)
	}
}

// pvStatusFlows are failures decided by the HTTP status.
func pvStatusFlows() []pvFlow {
	failure := pvDiagBody("general_failure")
	html := "<html><body>" + pvDiagMessage + "</body></html>"
	huge := `{"status":"general_failure","message":` + testutil.JSONQuote(pvDiagMessage) + `,"pad":"` +
		strings.Repeat("p", 70<<10) + `"}`
	flows := make([]pvFlow, 0, 20)
	for _, s := range []struct {
		code int
		kind neverbounce.FailureKind
	}{
		{http.StatusUnauthorized, neverbounce.FailureUnauthorized},
		{http.StatusForbidden, neverbounce.FailureUnauthorized},
		{http.StatusTooManyRequests, neverbounce.FailureRateLimited},
		{http.StatusServiceUnavailable, neverbounce.FailureUnavailable},
		{http.StatusBadRequest, neverbounce.FailureRejected},
		{http.StatusRequestEntityTooLarge, neverbounce.FailureRejected},
		{http.StatusInternalServerError, neverbounce.FailureUpstream},
		{http.StatusAccepted, neverbounce.FailureUnexpectedResponse},
		{600, neverbounce.FailureUnexpectedResponse},
	} {
		flows = append(flows, pvFlow{name: "HTTP " + strconv.Itoa(s.code), kind: s.kind, diag: true, call: pvStatic(s.code, failure)})
	}
	return append(flows,
		pvFlow{name: "HTTP 502 HTML", kind: neverbounce.FailureUpstream, call: pvAnswer(http.StatusBadGateway,
			pvHeader("text/html"), html, int64(len(html)))},
		pvFlow{name: "HTTP 404 over the excerpt", kind: neverbounce.FailureRejected, call: pvStatic(http.StatusNotFound, huge)},
		pvFlow{name: "HTTP 404 over the excerpt undeclared", kind: neverbounce.FailureRejected, call: pvAnswer(
			http.StatusNotFound, pvHeader(mediaJSON), huge, -1)},
		pvFlow{name: "HTTP 101 body never read", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(
			http.StatusSwitchingProtocols, pvHeader(""), failure, -1)},
		pvFlow{name: "HTTP 302 hostile Location", kind: neverbounce.FailureUnexpectedResponse, diag: true, call: pvAnswer(
			http.StatusFound, pvRedirectHeader("https://"+pvLocation+".example.com/next?key="+testKey+"&email="+pvEmail),
			failure, int64(len(failure)))},
		pvFlow{name: "HTTP 307 unparsable Location", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(
			http.StatusTemporaryRedirect, pvRedirectHeader("https://%zz"+pvLocation+"/?q="+pvQuery+"&key="+testKey),
			html, -1)},
		pvFlow{name: "HTTP 308 same-origin Location", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(
			http.StatusPermanentRedirect, pvRedirectHeader("/v4.2/"+pvLocation+"?key="+testKey), "", 0)},
		pvFlow{name: "HTTP 301 cross-origin Location", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(
			http.StatusMovedPermanently, pvRedirectHeader("https://"+pvCallbackUser+":"+pvCallbackPass+"@"+pvLocation+
				".example.net/"), html, int64(len(html)))},
	)
}

// pvEnvelopeFlows are HTTP 200 envelopes whose status is not success.
func pvEnvelopeFlows() []pvFlow {
	echoed := "Pv " + pvEmail + " " + testKey
	return []pvFlow{
		{name: "general_failure", kind: neverbounce.FailureRejected, diag: true, call: pvStatic(http.StatusOK, pvDiagBody("general_failure"))},
		{name: pvStatusAuth, kind: neverbounce.FailureUnauthorized, diag: true, call: pvStatic(http.StatusOK, pvDiagBody(pvStatusAuth))},
		{name: "bad_referrer", kind: neverbounce.FailureUnauthorized, diag: true, call: pvStatic(http.StatusOK, pvDiagBody("bad_referrer"))},
		{name: pvStatusUnavail, kind: neverbounce.FailureUnavailable, diag: true, call: pvStatic(http.StatusOK, pvDiagBody(pvStatusUnavail))},
		{name: "throttle_triggered", kind: neverbounce.FailureRateLimited, diag: true, call: pvStatic(http.StatusOK,
			pvDiagBody("throttle_triggered"))},
		{name: "status echoes data", kind: neverbounce.FailureUnexpectedResponse, diag: true, call: pvStatic(http.StatusOK,
			pvDiagBody(echoed))},
		{name: "status SUCCESS", kind: neverbounce.FailureUnexpectedResponse, diag: true, call: pvStatic(http.StatusOK,
			pvDiagBody("SUCCESS"))},
	}
}

// pvBodyFlows are HTTP 200 bodies that cannot be read as an envelope.
func pvBodyFlows() []pvFlow {
	const malformed = neverbounce.FailureMalformedResponse
	msg := testutil.JSONQuote(pvDiagMessage)
	failure := pvDiagBody("general_failure")
	pad := `{"status":"success","pad":` + testutil.JSONQuote(strings.Repeat(pvDiagMessage, 4)) + `}`
	limits := []neverbounce.Option{neverbounce.WithMaxResponseBytes(64), neverbounce.WithMaxDownloadBytes(64)}
	gzip := pvHeader(mediaJSON)
	gzip.Set("Content-Encoding", "gzip")
	return []pvFlow{
		{name: "status missing", kind: malformed, call: pvStatic(http.StatusOK, `{"message":`+msg+`,"result":"valid","job_id":1}`)},
		{name: "status null", kind: malformed, call: pvStatic(http.StatusOK, `{"status":null,"message":`+msg+`}`)},
		{name: "status number", kind: malformed, call: pvStatic(http.StatusOK, `{"status":200,"message":`+msg+`}`)},
		{name: "status object", kind: malformed, call: pvStatic(http.StatusOK, `{"status":{"text":`+msg+`}}`)},
		{name: "status array", kind: malformed, call: pvStatic(http.StatusOK, `{"status":[`+msg+`]}`)},
		{name: "invalid JSON", kind: malformed, call: pvStatic(http.StatusOK, `{"status":"general_failure","message":`+msg)},
		{name: "trailing data", kind: malformed, call: pvStatic(http.StatusOK, failure+" "+msg)},
		{name: "duplicate key", kind: malformed, call: pvStatic(http.StatusOK,
			`{"status":"general_failure","message":`+msg+`,"message":`+msg+`}`)},
		{name: "escaped duplicate key", kind: malformed, call: pvStatic(http.StatusOK,
			testutil.Esc(`{"status":"success","~u0073tatus":"general_failure","message":`)+msg+`}`)},
		{name: "invalid UTF-8", kind: malformed, call: pvStatic(http.StatusOK,
			`{"status":"general_failure","message":"`+pvDiagMessage+"\xff"+`"}`)},
		{name: "top-level array", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK, `[`+msg+`]`)},
		{name: "top-level string", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK, msg)},
		{name: "empty body", kind: malformed, call: pvStatic(http.StatusOK, "")},
		{name: "undecoded gzip", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(http.StatusOK, gzip, failure,
			int64(len(failure)))},
		{name: "declared too large", kind: neverbounce.FailureResponseTooLarge, call: pvStatic(http.StatusOK, pad, limits...)},
		{
			name: "streamed too large", kind: neverbounce.FailureResponseTooLarge,
			// A download reads JSON up to its own 64 KiB bound: it finds a
			// success envelope, which carries no file.
			downloadKind: neverbounce.FailureUnexpectedResponse,
			call:         pvAnswer(http.StatusOK, pvHeader(mediaJSON), pad, -1, limits...),
		},
		{name: "body shorter than declared", kind: neverbounce.FailureTransport, call: pvAnswer(http.StatusOK,
			pvHeader(mediaJSON), failure, int64(len(failure)+100))},
		{name: "read fails mid-body", kind: neverbounce.FailureTransport, call: pvReadFails(mediaJSON,
			`{"status":"general_failure","message":"`+pvDiagMessage, pvEchoErr)},
		{name: "read times out mid-body", kind: neverbounce.FailureTimeout, call: pvReadFails(mediaJSON,
			`{"status":"general_failure","message":"`+pvDiagMessage, func(echo string) error {
				return testutil.TimeoutError{Msg: echo}
			})},
	}
}

// pvTransportFlows are round trips that fail with errors quoting the request.
func pvTransportFlows() []pvFlow {
	const transport = neverbounce.FailureTransport
	return []pvFlow{
		{name: "echoing error", kind: transport, call: pvFailWith(pvEchoErr)},
		{name: "url.Error with key in the query", kind: transport, call: pvFailWith(func(echo string) error {
			return &url.Error{
				Op:  http.MethodPost,
				URL: loopbackOrigin + pathCheck + "?key=" + testKey + "&email=" + pvEmail + "&q=" + pvQuery,
				Err: pvEchoErr(echo),
			}
		})},
		{name: "dial", kind: transport, call: pvFailWith(func(echo string) error {
			return &net.OpError{Op: "dial", Net: "tcp", Err: pvEchoErr(echo)}
		})},
		{name: "proxy with credentials", kind: transport, call: pvFailWith(func(echo string) error {
			return &net.OpError{Op: "proxyconnect", Net: "tcp", Err: &url.Error{
				Op: "Get", URL: "http://pvproxyuser:" + pvProxyPass + "@proxy.example.com:3128", Err: pvEchoErr(echo),
			}}
		})},
		{name: "DNS", kind: transport, call: pvFailWith(func(echo string) error {
			return &net.DNSError{Err: echo, Name: pvLocation + ".example.com", Server: "192.0.2.53:53", IsNotFound: true}
		})},
		{name: "TLS record header", kind: transport, call: pvFailWith(func(echo string) error {
			return tls.RecordHeaderError{Msg: echo}
		})},
		{name: "TLS certificate", kind: transport, call: pvFailWith(func(echo string) error {
			return &tls.CertificateVerificationError{Err: pvEchoErr(echo)}
		})},
		{name: "TLS alert", kind: transport, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, tls.AlertError(42))
		})},
		{name: "connection reset", kind: transport, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, syscall.ECONNRESET)
		})},
		{name: "broken pipe", kind: transport, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, syscall.EPIPE)
		})},
		{name: "unexpected EOF", kind: transport, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, io.ErrUnexpectedEOF)
		})},
		{name: "closed connection", kind: transport, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, net.ErrClosed)
		})},
		{name: "network timeout", kind: neverbounce.FailureTimeout, call: pvFailWith(func(echo string) error {
			return testutil.TimeoutError{Msg: echo}
		})},
		{name: "deadline error", kind: neverbounce.FailureTimeout, call: pvFailWith(func(echo string) error {
			return fmt.Errorf("%w: %s: %w", errPvEcho, echo, context.DeadlineExceeded)
		})},
	}
}

// pvContextFlows are SDK timeouts and the caller's cancellations and
// deadlines, before and during the call.
func pvContextFlows() []pvFlow {
	block := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		_ = pvEcho(r)
		<-r.Context().Done()
		return nil, fmt.Errorf("%w: %s: %w", errPvEcho, pvDiagMessage, r.Context().Err())
	})
	return []pvFlow{
		{name: "SDK timeout", kind: neverbounce.FailureTimeout, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			return pvInBubble(t, func(t *testing.T) error {
				limits := []neverbounce.Option{neverbounce.WithTimeout(time.Second), neverbounce.WithDownloadTimeout(time.Second)}
				return pvCall(t.Context(), t, op, block, slices.Concat(limits, opts)...)
			})
		}},
		{name: "canceled in flight", kind: neverbounce.FailureCanceled, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				cancel()
				return block(r)
			})
			return pvCall(ctx, t, op, rt, opts...)
		}},
		{name: "canceled before sending", kind: neverbounce.FailureCanceled, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return pvCall(ctx, t, op, block, opts...)
		}},
		{name: "deadline in flight", kind: neverbounce.FailureDeadlineExceeded, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			return pvInBubble(t, func(t *testing.T) error {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				return pvCall(ctx, t, op, block, opts...)
			})
		}},
		{name: "deadline before sending", kind: neverbounce.FailureDeadlineExceeded, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
			defer cancel()
			return pvCall(ctx, t, op, block, opts...)
		}},
	}
}

// pvGenericFlows are the flows every operation can meet.
func pvGenericFlows() []pvFlow {
	return slices.Concat(pvStatusFlows(), pvEnvelopeFlows(), pvBodyFlows(), pvTransportFlows(), pvContextFlows())
}

// pvBig is a JSON string of provider text over the 4 KiB text bound.
func pvBig() string { return testutil.JSONQuote(strings.Repeat(pvDiagMessage+" ", 40)) }

// pvCheckFlows are single/check answers that fail to decode while echoing
// data.
func pvCheckFlows() []pvFlow {
	ok := func(members string) pvCallFunc {
		return pvStatic(http.StatusOK, `{"status":"success","result":"valid",`+members+`}`)
	}
	msg := testutil.JSONQuote(pvDiagMessage)
	return []pvFlow{
		{name: "result echoes the address", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","result":`+testutil.JSONQuote(pvEmail)+`}`)},
		{name: "result echoes provider text", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","result":`+msg+`}`)},
		{name: "flag echoes data", kind: neverbounce.FailureUnexpectedResponse, call: ok(`"flags":["has_dns",` + msg + `]`)},
		{name: "flag of the wrong type", kind: neverbounce.FailureMalformedResponse, call: ok(`"flags":[{"x":` + msg + `}]`)},
		{name: "suggested_correction over 4 KiB", kind: neverbounce.FailureResponseTooLarge, call: ok(`"suggested_correction":` + pvBig())},
		{name: "address_info member over 4 KiB", kind: neverbounce.FailureResponseTooLarge, call: ok(
			`"address_info":{"original_email":` + pvBig() + `}`)},
		{name: "address_info member of the wrong type", kind: neverbounce.FailureMalformedResponse, call: ok(
			`"address_info":{"host":[` + testutil.JSONQuote(pvEmail) + `]}`)},
		{name: "credits_info echoes data", kind: neverbounce.FailureMalformedResponse, call: ok(`"credits_info":` + msg)},
		{name: "credits count echoes data", kind: neverbounce.FailureMalformedResponse, call: ok(
			`"credits_info":{"paid_credits_used":` + msg + `}`)},
		{name: "member names echo data", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success",`+msg+`:"valid",`+testutil.JSONQuote(pvEmail)+`:"valid"}`)},
	}
}

// pvCreateFlows are jobs/create answers without a usable job ID.
func pvCreateFlows() []pvFlow {
	msg := testutil.JSONQuote(pvDiagMessage)
	return []pvFlow{
		{name: "job_id echoes data", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","job_id":`+msg+`}`)},
		{name: "job_id zero", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","job_id":0,"message":`+msg+`}`)},
		{name: "job_id negative", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","job_id":-5,"message":`+msg+`}`)},
		{name: "job_id missing", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","message":`+msg+`,"JOB_ID":1}`)},
	}
}

// pvJobStatusFlows are jobs/status answers that fail to decode while echoing
// data.
func pvJobStatusFlows() []pvFlow {
	msg := testutil.JSONQuote(pvDiagMessage)
	status := func(members string) pvCallFunc {
		return pvStatic(http.StatusOK, `{"status":"success","id":`+pvJobIDText+`,`+members+`}`)
	}
	return []pvFlow{
		{name: "another job", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","id":4243,"job_status":"complete","filename":`+testutil.JSONQuote(pvFilename)+`}`)},
		{name: "job_status echoes data", kind: neverbounce.FailureUnexpectedResponse, call: status(`"job_status":` + msg)},
		{name: "filename over 4 KiB", kind: neverbounce.FailureResponseTooLarge, call: status(
			`"job_status":"complete","filename":` + pvBig())},
		{name: "failure_reason echoes data", kind: neverbounce.FailureUnexpectedResponse, call: status(
			`"job_status":"failed","failure_reason":` + msg)},
		{name: "total echoes data", kind: neverbounce.FailureMalformedResponse, call: status(`"job_status":"complete","total":` + msg)},
		{name: "total count echoes data", kind: neverbounce.FailureMalformedResponse, call: status(
			`"job_status":"complete","total":{"records":` + msg + `}`)},
		{name: "percent echoes data", kind: neverbounce.FailureMalformedResponse, call: status(
			`"job_status":"complete","percent_complete":` + msg)},
		{name: "timestamp of the wrong type", kind: neverbounce.FailureMalformedResponse, call: status(
			`"job_status":"complete","created_at":[` + msg + `]`)},
	}
}

// pvWaitFlows are WaitForJob outcomes that stop it short.
func pvWaitFlows() []pvFlow {
	status := func(state string) string {
		return `{"status":"success","id":` + pvJobIDText + `,"job_status":"` + state + `","filename":` +
			testutil.JSONQuote(pvFilename) + `,"message":` + testutil.JSONQuote(pvDiagMessage) + `}`
	}
	// running answers each poll that the job is still running. A TransportWith
	// body serves one answer, so every poll gets a transport of its own.
	running := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := io.NopCloser(strings.NewReader(status("running")))
		return testutil.TransportWith(http.StatusOK, pvHeader(mediaJSON), -1, body)(r)
	})
	return []pvFlow{
		{name: "job failed", kind: neverbounce.FailureJobFailed, call: pvStatic(http.StatusOK,
			strings.Replace(status("failed"), `"message"`, `"failure_reason":"file_corrupt","message"`, 1))},
		{name: "undocumented job state", kind: neverbounce.FailureUnknownJobState, call: pvStatic(http.StatusOK,
			status("pv_paused_state"))},
		{name: "budget used up", kind: neverbounce.FailureBudgetExhausted, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			return pvInBubble(t, func(t *testing.T) error {
				return pvCall(t.Context(), t, op, running, opts...)
			})
		}},
		{name: "canceled while waiting", kind: neverbounce.FailureCanceled, call: func(t *testing.T, op opCase, opts ...neverbounce.Option) error {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				cancel() // the answer still arrives; the pause that follows sees the cancellation
				return running(r)
			})
			return pvCall(ctx, t, op, rt, opts...)
		}},
	}
}

// pvResultsFlows are jobs/results answers that fail to decode while echoing
// data.
func pvResultsFlows() []pvFlow {
	msg := testutil.JSONQuote(pvDiagMessage)
	page := func(results string) pvCallFunc {
		return pvStatic(http.StatusOK, `{"status":"success","total_results":1,"total_pages":1,"results":`+results+`}`)
	}
	row := `{"data":{"email":` + testutil.JSONQuote(pvEmail) + `,"` + pvColumn + `":` + testutil.JSONQuote(pvRowValue) +
		`},"verification":{"result":"valid"}}`
	return []pvFlow{
		{name: "results echo data", kind: neverbounce.FailureMalformedResponse, call: page(msg)},
		{name: "row is not an object", kind: neverbounce.FailureMalformedResponse, call: page(`[` + msg + `]`)},
		{name: "data is a string", kind: neverbounce.FailureMalformedResponse, call: page(
			`[{"data":` + msg + `,"verification":{"result":"valid"}}]`)},
		{name: "verification missing", kind: neverbounce.FailureMalformedResponse, call: page(
			`[{"data":{"email":` + testutil.JSONQuote(pvEmail) + `}}]`)},
		{name: "verdict echoes the address", kind: neverbounce.FailureUnexpectedResponse, call: page(
			`[{"data":{},"verification":{"result":` + testutil.JSONQuote(pvEmail) + `}}]`)},
		{name: "flag echoes data", kind: neverbounce.FailureUnexpectedResponse, call: page(
			`[{"data":{},"verification":{"result":"valid","flags":[` + msg + `]}}]`)},
		{name: "address_info over 4 KiB", kind: neverbounce.FailureResponseTooLarge, call: page(
			`[{"data":{},"verification":{"result":"valid","address_info":{"host":` + pvBig() + `}}}]`)},
		{name: "more rows than asked for", kind: neverbounce.FailureUnexpectedResponse, call: page(
			`[` + strings.Repeat(row+",", 10) + row + `]`)},
		{name: "data over 64 KiB", kind: neverbounce.FailureResponseTooLarge, call: page(
			`[{"data":{"email":` + testutil.JSONQuote(strings.Repeat(pvDiagMessage, 400)) +
				`},"verification":{"result":"valid"}}]`)},
		{name: "total_results echoes data", kind: neverbounce.FailureMalformedResponse, call: pvStatic(http.StatusOK,
			`{"status":"success","total_results":`+msg+`,"total_pages":1,"results":[]}`)},
	}
}

// pvPagesFlows are JobResultPages walks the pages themselves stop.
func pvPagesFlows() []pvFlow {
	row := `{"data":{"email":` + testutil.JSONQuote(pvEmail) + `},"verification":{"result":"valid"}}`
	page := func(totalPages int, results string) string {
		return `{"status":"success","total_results":30,"total_pages":` + strconv.Itoa(totalPages) +
			`,"results":` + results + `,"message":` + testutil.JSONQuote(pvDiagMessage) + `}`
	}
	full := page(9, "["+row+"]")
	return []pvFlow{
		{name: "pages contradict", kind: neverbounce.FailureUnexpectedResponse, page: 2, call: pvScripted(
			page(3, "["+row+"]"), page(4, "["+row+"]"))},
		{name: "empty page before the last", kind: neverbounce.FailureUnexpectedResponse, call: pvScripted(page(3, "[]"))},
		// The walk stops at the page its budget of five keeps it from asking for.
		{name: "budget used up", kind: neverbounce.FailureBudgetExhausted, page: 6, call: pvScripted(
			full, full, full, full, full)},
	}
}

// pvDownloadFlows are download answers that are not a file, or a file that
// cannot be delivered.
func pvDownloadFlows() []pvFlow {
	csv := "id,email\r\n1001," + pvEmail + "\r\n" + pvRowValue + "," + testKey + "\r\n"
	n := int64(len(csv))
	octet := pvHeader(mediaOctet)
	two := pvHeader(mediaCSV)
	two.Add("Content-Type", mediaOctet)
	success := `{"status":"success","message":` + testutil.JSONQuote(pvDiagMessage) + `}`
	bigJSON := `{"status":"general_failure","message":` + testutil.JSONQuote(strings.Repeat(pvDiagMessage, 400)) + `}`
	html := "<p>" + pvDiagMessage + "</p>"
	const limit = 16
	return []pvFlow{
		{name: "HTML", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(http.StatusOK, pvHeader("text/html"),
			html, int64(len(html)))},
		{name: "content type missing", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(http.StatusOK, pvHeader(""), csv, n)},
		{name: "two content types", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(http.StatusOK, two, csv, n)},
		{name: "unparsable content type", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(http.StatusOK,
			pvHeader(mediaCSV+";"+pvQuery), csv, n)},
		{name: "envelope disguised as a file", kind: neverbounce.FailureRejected, diag: true, call: pvAnswer(http.StatusOK,
			octet, pvDiagBody("general_failure"), -1)},
		{name: "success envelope instead of a file", kind: neverbounce.FailureUnexpectedResponse, call: pvAnswer(
			http.StatusOK, octet, success, int64(len(success)))},
		{name: "JSON over 64 KiB", kind: neverbounce.FailureUnexpectedResponse, call: pvStatic(http.StatusOK, bigJSON)},
		{name: "declared over the download limit", kind: neverbounce.FailureResponseTooLarge, call: pvAnswer(http.StatusOK,
			octet, csv, n, neverbounce.WithMaxDownloadBytes(limit))},
		// A file of unknown length is delivered up to the limit before the
		// byte past it is found.
		{name: "streamed over the download limit", kind: neverbounce.FailureResponseTooLarge, written: limit, call: pvAnswer(
			http.StatusOK, octet, csv, -1, neverbounce.WithMaxDownloadBytes(limit))},
		{name: "file shorter than declared", kind: neverbounce.FailureTransport, call: pvAnswer(http.StatusOK, octet, csv, n+64)},
		{name: "read fails mid-file", kind: neverbounce.FailureTransport, call: pvReadFails(mediaCSV, csv, pvEchoErr)},
	}
}

// pvWriterFailedError is the error a failing writer returns. It quotes data.
type pvWriterFailedError struct{}

func (pvWriterFailedError) Error() string { return "pv writer failed: " + pvDiagMessage }

// pvFailingWriter fails every write with an error that quotes data.
type pvFailingWriter struct{}

func (pvFailingWriter) Write([]byte) (int, error) { return 0, pvWriterFailedError{} }

// pvShortWriter writes one byte less than asked, without an error.
type pvShortWriter struct{}

func (pvShortWriter) Write(p []byte) (int, error) { return max(len(p)-1, 0), nil }

// pvWriterCases are downloads whose writer fails.
func pvWriterCases() []pvCase {
	csv := "id,email\r\n1001," + pvEmail + "\r\n"
	call := pvAnswer(http.StatusOK, pvHeader(mediaCSV), csv, int64(len(csv)))
	op := func(name string, w io.Writer) opCase {
		o := pvOpNamed("DownloadJob")
		o.name, o.call = name, pvDownload(w)
		return o
	}
	return []pvCase{
		{op: op("DownloadJob failing writer", pvFailingWriter{}), flow: pvFlow{
			name: "writer error quotes data", kind: neverbounce.FailureWriteFailed, call: call,
		}},
		{op: op("DownloadJob short writer", pvShortWriter{}), flow: pvFlow{
			name: "short write", kind: neverbounce.FailureWriteFailed, written: int64(len(csv) - 1), call: call,
		}},
	}
}

// pvCases returns every flow on every operation it applies to.
func pvCases() []pvCase {
	var out []pvCase
	add := func(names []string, flows []pvFlow) {
		for _, name := range names {
			op := pvOpNamed(name)
			for _, f := range flows {
				out = append(out, pvCase{op: op, flow: f})
			}
		}
	}
	for _, op := range pvOps() {
		add([]string{op.name}, pvGenericFlows())
	}
	add([]string{pvOpCheck}, pvCheckFlows())
	add([]string{"CreateJob supplied", "CreateJob remote"}, pvCreateFlows())
	add([]string{"JobStatus", "WaitForJob"}, pvJobStatusFlows())
	add([]string{"WaitForJob"}, pvWaitFlows())
	add([]string{"JobResults", "JobResultPages"}, pvResultsFlows())
	add([]string{"JobResultPages"}, pvPagesFlows())
	add([]string{"DownloadJob"}, pvDownloadFlows())
	return append(out, pvWriterCases()...)
}

// pvFieldPattern matches a CallError Field: a JSON path of fixed member names
// and indexes, never a value.
const pvFieldPattern = `^([a-z_]+(\[[0-9]+\])?(\.[a-z_]+(\[[0-9]+\])?)*)?$`

// pvCheckCallError asserts, as assertCallError does, that err is the
// *CallError the case must produce, with secrets never printed, and returns
// it. The HTTP status, the API status and the Field vary with each flow, so
// they are taken as found: the Error text must still render them, and the
// API status and the Field must keep their documented shapes.
func pvCheckCallError(t *testing.T, c pvCase, err error, secrets ...string) *neverbounce.CallError {
	t.Helper()
	ce := callError(t, err)
	assert.Regexp(t, pvFieldPattern, ce.Field)
	assert.Regexp(t, `^[a-z_]{0,64}$`, string(ce.APIStatus))
	assertCallError(t, err, wantCall{
		Kind: c.kind(), Op: c.op.op, Status: ce.StatusCode, API: ce.APIStatus, JobID: c.op.jobID, Page: c.page(),
		Field: ce.Field, Written: c.flow.written,
	}, secrets...)
	return ce
}

func TestPrivacyCallErrorsNeverPrintSecrets(t *testing.T) {
	secrets := pvSecrets()
	for _, c := range pvCases() {
		t.Run(c.op.name+"/"+c.flow.name, func(t *testing.T) {
			err := c.flow.call(t, c.op, neverbounce.WithProviderDiagnostics())
			ce := pvCheckCallError(t, c, err, secrets...)
			data, jerr := json.Marshal(err)
			require.NoError(t, jerr)
			testutil.AssertRedacted(t, string(data), secrets, "JSON")
			if c.flow.diag {
				assert.Equal(t, pvDiagWant(), ce.Diagnostics().Message, "only the key is scrubbed")
			} else {
				assert.Empty(t, ce.Diagnostics().Message, "no envelope message to keep")
			}
		})
	}
}

func TestPrivacyDiagnosticsOnlyWithOptIn(t *testing.T) {
	secrets := pvSecrets()
	for _, c := range pvCases() {
		t.Run(c.op.name+"/"+c.flow.name, func(t *testing.T) {
			err := c.flow.call(t, c.op)
			ce := pvCheckCallError(t, c, err, secrets...)
			assert.Equal(t, neverbounce.ProviderDiagnostics{}, ce.Diagnostics(), "no provider text without the option")
		})
	}
}

// pvShapeCases returns every generic flow on CheckEmail and CreateJob, and
// every operation-specific case.
func pvShapeCases() []pvCase {
	generic := map[string]bool{}
	for _, f := range pvGenericFlows() {
		generic[f.name] = true
	}
	var out []pvCase
	for _, c := range pvCases() {
		if !generic[c.flow.name] || c.op.name == pvOpCheck || c.op.name == "CreateJob remote" {
			out = append(out, c)
		}
	}
	return out
}

func TestPrivacyCallErrorShapesNeverPrintSecrets(t *testing.T) {
	secrets := pvSecrets()
	cases := pvShapeCases()
	var prev *neverbounce.CallError
	for _, c := range cases {
		t.Run(c.op.name+"/"+c.flow.name, func(t *testing.T) {
			ce := callError(t, c.flow.call(t, c.op, neverbounce.WithProviderDiagnostics()))
			testutil.AssertShapesNeverPrint(t, ce, secrets...)
			if prev != nil {
				// The private function makes two SDK errors always differ.
				testutil.AssertDiffRedacted(t, prev, ce, secrets...)
			}
			prev = ce
		})
	}
}

func TestPrivacyDiagnosticsScrubKeyBeforeCutting(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "key across the 1 KiB cut",
			message: strings.Repeat("m", 1010) + testKey + " tail",
			want:    strings.Repeat("m", 1010) + "[redacted] tai",
		},
		{
			name:    "key spelled with escapes",
			message: testutil.Esc("key=secret~u005fkeymarker7f3a2c91 email=") + pvEmail,
			want:    "key=[redacted] email=" + pvEmail,
		},
		{
			name:    "key repeated",
			message: testKey + testKey + " " + testKey,
			want:    "[redacted][redacted] [redacted]",
		},
		{
			name:    "multi-byte text cut on a rune boundary",
			message: strings.Repeat("€", 400),
			want:    strings.Repeat("€", 341),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.message
			if !strings.Contains(raw, `\`) {
				raw = strings.TrimSuffix(strings.TrimPrefix(testutil.JSONQuote(raw), `"`), `"`)
			}
			body := `{"status":"general_failure","message":"` + raw + `"}`
			c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body), neverbounce.WithProviderDiagnostics())
			_, err := c.CheckEmail(t.Context(), checkRequest())
			msg := callError(t, err).Diagnostics().Message
			assert.Equal(t, tt.want, msg)
			assert.LessOrEqual(t, len(msg), 1024)
			assert.NotContains(t, msg, testKey[:8], "no prefix of the key survives the cut")
		})
	}
}

// pvInvalid is one call that fails local validation.
type pvInvalid struct {
	name     string
	sentinel error
	run      func(ctx context.Context, c *neverbounce.Client) error
}

// pvCreateInvalid returns a CreateJob run with the marker request changed by
// mutate.
func pvCreateInvalid(base neverbounce.CreateJobRequest,
	mutate func(*neverbounce.CreateJobRequest),
) func(context.Context, *neverbounce.Client) error {
	return func(ctx context.Context, c *neverbounce.Client) error {
		r := base
		r.Supplied = neverbounce.SuppliedList{
			Columns: slices.Clone(base.Supplied.Columns),
			Rows:    slices.Clone(base.Supplied.Rows),
		}
		mutate(&r)
		_, err := c.CreateJob(ctx, r)
		return err
	}
}

// pvTooManyHeaders returns 33 marker-named callback headers.
func pvTooManyHeaders() map[string]string {
	h := make(map[string]string, 33)
	for i := range 33 {
		h[pvHeaderName+strconv.Itoa(i)] = pvHeaderValue
	}
	return h
}

// pvTooManyColumns returns 129 marker column names.
func pvTooManyColumns() []string {
	cols := make([]string, 129)
	for i := range cols {
		cols[i] = pvColumn + strconv.Itoa(i)
	}
	return cols
}

// pvOversizedRows returns rows that encode to more than MaxJobRequestBytes.
func pvOversizedRows() [][]string {
	value := strings.Repeat(pvRowValue, 4096/len(pvRowValue))
	rows := make([][]string, 3100)
	for i := range rows {
		rows[i] = []string{"1", value, value}
	}
	return rows
}

// pvCreateSuppliedInvalid are supplied-list requests that fail validation.
func pvCreateSuppliedInvalid() []pvInvalid {
	sup := pvCreateRequest()
	rem := pvRemoteRequest()
	set := func(f func(*neverbounce.CreateJobRequest)) func(context.Context, *neverbounce.Client) error {
		return pvCreateInvalid(sup, f)
	}
	return []pvInvalid{
		{"both inputs", nil, set(func(r *neverbounce.CreateJobRequest) { r.RemoteURL = pvRemoteURL })},
		{"no input", nil, pvCreateInvalid(rem, func(r *neverbounce.CreateJobRequest) { r.RemoteURL = "" })},
		{"no columns", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns = nil })},
		{"too many columns", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns = pvTooManyColumns() })},
		{"empty column", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns[0] = "" })},
		{"long column", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns[1] = strings.Repeat(pvColumn, 20) })},
		{"column invalid UTF-8", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns[1] = pvColumn + "\xff" })},
		{"column control", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns[1] = pvColumn + "\n" })},
		{"column blank", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Columns[1] = "  " })},
		{"duplicate column", nil, set(func(r *neverbounce.CreateJobRequest) {
			r.Supplied.Columns[0] = strings.ToUpper(pvColumn)
		})},
		{"no rows", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Rows = nil })},
		{"short row", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Rows[1] = []string{pvRowValue, pvEmail} })},
		{"long value", nil, set(func(r *neverbounce.CreateJobRequest) {
			r.Supplied.Rows[1] = []string{"1", strings.Repeat(pvRowValue, 300), pvEmail}
		})},
		{"value invalid UTF-8", nil, set(func(r *neverbounce.CreateJobRequest) {
			r.Supplied.Rows[0] = []string{"1", pvRowValue + "\xff", pvEmail}
		})},
		{"value control", nil, set(func(r *neverbounce.CreateJobRequest) {
			r.Supplied.Rows[1] = []string{"1", pvRowValue, pvEmail + "\r\n"}
		})},
		{"value C1 control", nil, set(func(r *neverbounce.CreateJobRequest) {
			r.Supplied.Rows[1] = []string{"1", pvRowValue + "\xc2\x85", pvEmail}
		})},
		{"over MaxJobRequestBytes", nil, set(func(r *neverbounce.CreateJobRequest) { r.Supplied.Rows = pvOversizedRows() })},
	}
}

// pvCreateExtrasInvalid are remote URLs, filenames and callbacks that fail
// validation.
func pvCreateExtrasInvalid() []pvInvalid {
	sup := pvCreateRequest()
	rem := pvRemoteRequest()
	remote := func(u string) func(context.Context, *neverbounce.Client) error {
		return pvCreateInvalid(rem, func(r *neverbounce.CreateJobRequest) { r.RemoteURL = u })
	}
	set := func(f func(*neverbounce.CreateJobRequest)) func(context.Context, *neverbounce.Client) error {
		return pvCreateInvalid(sup, f)
	}
	callback := func(u string) func(context.Context, *neverbounce.Client) error {
		return set(func(r *neverbounce.CreateJobRequest) { r.CallbackURL = u })
	}
	headers := func(h map[string]string) func(context.Context, *neverbounce.Client) error {
		return set(func(r *neverbounce.CreateJobRequest) { r.CallbackHeaders = h })
	}
	auth := pvRemoteUser + ":" + pvRemotePass + "@"
	cbAuth := pvCallbackUser + ":" + pvCallbackPass + "@"
	return []pvInvalid{
		{"remote URL too long", nil, remote(pvRemoteURL + "&pad=" + strings.Repeat(pvBadValue, 600))},
		{"remote URL invalid UTF-8", nil, remote(pvRemoteURL + "\xff")},
		{"remote URL control", nil, remote(pvRemoteURL + "\x7f")},
		{"remote URL relative", nil, remote("/" + pvRemotePath + "?key=" + testKey)},
		{"remote URL with a space", nil, remote("https://" + auth + "lists.example.com/" + pvRemotePath + " x")},
		{"remote URL opaque", nil, remote("mailto:" + pvEmail)},
		{"remote URL unparsable", nil, remote("https://" + auth + "[::1/" + pvRemotePath)},
		{"filename too long", nil, set(func(r *neverbounce.CreateJobRequest) { r.Filename = strings.Repeat(pvFilename, 20) })},
		{"filename control", nil, set(func(r *neverbounce.CreateJobRequest) { r.Filename = pvFilename + "\t" })},
		{"filename invalid UTF-8", nil, set(func(r *neverbounce.CreateJobRequest) { r.Filename = pvFilename + "\xc3" })},
		{"filename blank", nil, set(func(r *neverbounce.CreateJobRequest) { r.Filename = "   " })},
		{"callback URL too long", nil, callback(pvCallbackURL + "/" + strings.Repeat(pvBadValue, 200))},
		{"callback URL scheme", nil, callback("ftp://" + cbAuth + "hooks.example.net/" + pvCallbackPath)},
		{"callback URL no scheme", nil, callback("//" + cbAuth + "hooks.example.net/" + pvCallbackPath)},
		{"callback URL control", nil, callback(pvCallbackURL + "\n")},
		{"callback headers without URL", nil, set(func(r *neverbounce.CreateJobRequest) { r.CallbackURL = "" })},
		{"too many callback headers", nil, headers(pvTooManyHeaders())},
		{"callback header name", nil, headers(map[string]string{pvHeaderName + " x": pvHeaderValue})},
		{"callback header name empty", nil, headers(map[string]string{"": pvHeaderValue})},
		{"callback header value control", nil, headers(map[string]string{pvHeaderName: pvHeaderValue + "\r\n"})},
		{"callback header value DEL", nil, headers(map[string]string{pvHeaderName: pvHeaderValue + "\x7f"})},
		{"callback header value too long", nil, headers(map[string]string{pvHeaderName: strings.Repeat(pvHeaderMarker, 60)})},
		{"callback header value invalid UTF-8", nil, headers(map[string]string{pvHeaderName: pvHeaderValue + "\xff"})},
		{"duplicate callback header", nil, headers(map[string]string{
			pvHeaderName: pvHeaderValue, strings.ToLower(pvHeaderName): pvHeaderValue,
		})},
	}
}

// pvOtherInvalid are invalid requests to every other operation.
func pvOtherInvalid() []pvInvalid {
	check := func(req neverbounce.CheckEmailRequest) func(context.Context, *neverbounce.Client) error {
		return func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.CheckEmail(ctx, req)
			return err
		}
	}
	download := func(req neverbounce.DownloadJobRequest, w io.Writer) func(context.Context, *neverbounce.Client) error {
		return func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.DownloadJob(ctx, req, w)
			return err
		}
	}
	var buf bytes.Buffer
	return []pvInvalid{
		{"email too long", nil, check(neverbounce.CheckEmailRequest{Email: strings.Repeat(pvEmail, 40)})},
		{"email control", nil, check(neverbounce.CheckEmailRequest{Email: pvEmail + "\n"})},
		{"email invalid UTF-8", nil, check(neverbounce.CheckEmailRequest{Email: pvEmail + "\xff"})},
		{"email timeout", nil, check(neverbounce.CheckEmailRequest{Email: pvEmail, Timeout: 1500 * time.Millisecond})},
		{"email timeout over client", nil, check(neverbounce.CheckEmailRequest{Email: pvEmail, Timeout: 300 * time.Second})},
		{"parse job ID", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.ParseJob(ctx, neverbounce.ParseJobRequest{})
			return err
		}},
		{"start job ID", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.StartJob(ctx, neverbounce.StartJobRequest{JobID: -1})
			return err
		}},
		{"status job ID", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.JobStatus(ctx, neverbounce.JobStatusRequest{})
			return err
		}},
		{"results page", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: pvJobID, Page: -1})
			return err
		}},
		{"results page size", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: pvJobID, ItemsPerPage: 1001})
			return err
		}},
		{"download binary format", nil, download(neverbounce.DownloadJobRequest{
			JobID: pvJobID, BinaryFormat: neverbounce.BinaryFormat(pvBadValue + testKey),
		}, &buf)},
		{"download line feed", nil, download(neverbounce.DownloadJobRequest{
			JobID: pvJobID, LineFeed: neverbounce.LineFeed(pvBadValue + pvEmail),
		}, &buf)},
		{"download nil writer", nil, download(neverbounce.DownloadJobRequest{JobID: pvJobID}, nil)},
		{"wait interval", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: pvJobID, Interval: time.Millisecond, MaxWait: time.Hour})
			return err
		}},
		{"wait budget", nil, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: pvJobID})
			return err
		}},
		{"pages start", nil, pvWalk(neverbounce.JobResultPagesRequest{JobID: pvJobID, StartPage: -1, MaxPages: 1})},
		{"pages budget", nil, pvWalk(neverbounce.JobResultPagesRequest{JobID: pvJobID})},
	}
}

// pvContextInvalid are calls with a nil context, a nil Client or a zero
// Client, for every operation.
func pvContextInvalid() []pvInvalid {
	ops := pvOps()
	out := make([]pvInvalid, 0, 3*len(ops))
	var nilCtx context.Context
	var nilClient *neverbounce.Client
	for _, op := range ops {
		out = append(out,
			pvInvalid{op.name + " nil context", nil, func(_ context.Context, c *neverbounce.Client) error {
				return op.call(nilCtx, c)
			}},
			pvInvalid{op.name + " nil client", neverbounce.ErrInvalidConfig, func(ctx context.Context, _ *neverbounce.Client) error {
				return op.call(ctx, nilClient)
			}},
			pvInvalid{op.name + " zero client", neverbounce.ErrInvalidConfig, func(ctx context.Context, _ *neverbounce.Client) error {
				return op.call(ctx, &neverbounce.Client{})
			}},
		)
	}
	return out
}

// pvConfigInvalid are NewClient calls that fail validation.
func pvConfigInvalid() []pvInvalid {
	build := func(key string, opts ...neverbounce.Option) func(context.Context, *neverbounce.Client) error {
		return func(context.Context, *neverbounce.Client) error {
			_, err := neverbounce.NewClient(key, opts...)
			return err
		}
	}
	base := func(u string) func(context.Context, *neverbounce.Client) error {
		return build(testKey, neverbounce.WithTestBaseURL(u))
	}
	cfg := neverbounce.ErrInvalidConfig
	return []pvInvalid{
		{"key with a space", cfg, build(testKey + " " + pvBadValue)},
		{"key with a quote", cfg, build(testKey + `"` + pvBadValue)},
		{"key with a backslash", cfg, build(testKey + `\` + pvBadValue)},
		{"key with a newline", cfg, build(testKey + "\n")},
		{"key non-ASCII", cfg, build(testKey + "é")},
		{"key too long", cfg, build(strings.Repeat(testKey, 20))},
		{"test URL userinfo", cfg, base("https://" + pvRemoteUser + ":" + pvRemotePass + "@127.0.0.1")},
		{"test URL key in query", cfg, base("https://127.0.0.1/?key=" + testKey)},
		{"test URL fragment", cfg, base("https://127.0.0.1#" + pvQuery)},
		{"test URL path", cfg, base("https://127.0.0.1/" + pvQuery)},
		{"test URL http", cfg, base("http://127.0.0.1/" + pvLocation)},
		{"test URL remote host", cfg, base("https://" + pvLocation + ".example.com")},
		{"test URL localhost", cfg, base("https://localhost:8443")},
		{"test URL control", cfg, base("https://127.0.0.1\n" + pvLocation)},
		{"nil HTTP client", cfg, build(testKey, neverbounce.WithHTTPClient(nil))},
		{"timeout zero", cfg, build(testKey, neverbounce.WithTimeout(0))},
		{"response limit zero", cfg, build(testKey, neverbounce.WithMaxResponseBytes(0))},
		{"download timeout too long", cfg, build(testKey, neverbounce.WithDownloadTimeout(13*time.Hour))},
		{"download limit negative", cfg, build(testKey, neverbounce.WithMaxDownloadBytes(-1))},
	}
}

func TestPrivacyValidationErrorsNeverPrintSecrets(t *testing.T) {
	secrets := pvSecrets()
	ct := &testutil.CountingTransport{Next: testutil.TransportWith(http.StatusOK, pvHeader(mediaJSON), -1,
		io.NopCloser(strings.NewReader(pvDiagBody("general_failure"))))}
	c := newTransportClient(t, ct)
	cases := slices.Concat(pvCreateSuppliedInvalid(), pvCreateExtrasInvalid(), pvOtherInvalid(), pvContextInvalid(),
		pvConfigInvalid())
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t.Context(), c)
			sentinel := tt.sentinel
			if sentinel == nil {
				sentinel = neverbounce.ErrInvalidRequest
			}
			require.ErrorIs(t, err, sentinel)
			ve := validationError(t, err)
			assert.Regexp(t, `^[A-Za-z]+(\[[0-9]+\])*(\.[A-Za-z]+(\[[0-9]+\])*)*$`, ve.Field)
			assert.Regexp(t, `^[a-z0-9_]+$`, ve.Reason)
			testutil.AssertShapesNeverPrint(t, ve, secrets...)
			assertErrorClean(t, err, secrets...)
			data, jerr := json.Marshal(err)
			require.NoError(t, jerr)
			testutil.AssertRedacted(t, string(data), secrets, "JSON")
		})
	}
	assert.Zero(t, ct.Attempts(), "nothing invalid is ever sent")
}

// pvMarkerTransport is an injected transport that holds a marker. It is never
// called.
type pvMarkerTransport struct{ marker string }

func (t pvMarkerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, pvEchoErr(t.marker)
}

func TestPrivacyClientNeverPrintsSecrets(t *testing.T) {
	const otherKey = "secret_otherkeymarker55aa" // gitleaks:allow (synthetic test marker)
	secrets := append(pvSecrets(), otherKey, "127.0.0.1", "47613", "api.neverbounce.com", "neverbounce.com/v4")
	injected := &http.Client{Transport: pvMarkerTransport{marker: pvTransport}, Timeout: time.Minute}
	options := map[string]neverbounce.Option{
		"WithHTTPClient":          neverbounce.WithHTTPClient(injected),
		"WithTimeout":             neverbounce.WithTimeout(45 * time.Second),
		"WithMaxResponseBytes":    neverbounce.WithMaxResponseBytes(1 << 16),
		"WithDownloadTimeout":     neverbounce.WithDownloadTimeout(time.Hour),
		"WithMaxDownloadBytes":    neverbounce.WithMaxDownloadBytes(1 << 20),
		"WithTestBaseURL":         neverbounce.WithTestBaseURL("https://127.0.0.1:47613"),
		"WithProviderDiagnostics": neverbounce.WithProviderDiagnostics(),
		"nil option":              nil,
	}
	clients := map[string]*neverbounce.Client{"defaults": newClient(t)}
	all := make([]neverbounce.Option, 0, len(options))
	for name, opt := range options {
		clients[name] = newClient(t, opt)
		all = append(all, opt)
	}
	clients["every option"] = newClient(t, all...)
	other, err := neverbounce.NewClient(otherKey, neverbounce.WithTimeout(time.Second))
	require.NoError(t, err)

	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			testutil.AssertShapesNeverPrint(t, c, secrets...)
			testutil.AssertRedacted(t, strings.Join(testutil.ReflectStrings(c), "\n"), secrets, "reflection walk")
			testutil.AssertDiffRedacted(t, c, other, secrets...)
			assert.True(t, strings.HasPrefix(c.String(), "neverbounce.Client{"), c.String())
		})
	}
	var none *neverbounce.Client
	assert.Equal(t, "neverbounce.Client(nil)", none.String())
	assert.Equal(t, "neverbounce.Client(nil)", fmt.Sprintf("%v", none))
}

func TestPrivacyClientAfterCalls(t *testing.T) {
	secrets := pvSecrets()
	c, srv := newServerClient(t, respond(http.StatusOK, pvDiagBody("general_failure")), neverbounce.WithProviderDiagnostics())
	rejected := func(op neverbounce.Operation) wantCall {
		return wantCall{
			Kind: neverbounce.FailureRejected, Op: op, Status: http.StatusOK, API: neverbounce.APIStatusGeneralFailure,
		}
	}
	_, err := c.CheckEmail(t.Context(), neverbounce.CheckEmailRequest{Email: pvEmail})
	assertCallError(t, err, rejected(neverbounce.OpCheckEmail), secrets...)
	_, err = c.CreateJob(t.Context(), pvCreateRequest())
	assertCallError(t, err, rejected(neverbounce.OpCreateJob), secrets...)
	testutil.AssertShapesNeverPrint(t, c, secrets...)
	testutil.AssertRedacted(t, strings.Join(testutil.ReflectStrings(c), "\n"), secrets, "reflection walk")

	reqs := srv.Requests()
	require.Len(t, reqs, 2)
	for _, r := range reqs {
		assert.True(t, bytes.HasPrefix(r.Body, []byte(keyMember)), "the key travels in the body")
		testutil.AssertRedacted(t, r.URL, secrets, "never in the URL")
		for name, values := range r.Header {
			testutil.AssertRedacted(t, name+": "+strings.Join(values, ", "), secrets, "never in a header")
		}
	}
	assert.Contains(t, string(reqs[1].Body), pvCallbackPass, "the callback URL reaches the provider, and only it")
}

// pvRequestShapes returns the ways a caller might hold r whose printing calls
// String or GoString. fmt cannot call a method on a value held in an
// unexported struct field, so it prints that shape's fields by reflection:
// keeping a request in an unexported field and printing the struct is the
// caller's responsibility, and those shapes are left out.
func pvRequestShapes(r *neverbounce.CreateJobRequest) []testutil.Shape {
	var out []testutil.Shape
	for _, s := range testutil.Shapes(r) {
		if !strings.Contains(s.Name, "unexported") {
			out = append(out, s)
		}
	}
	return out
}

func TestPrivacyCreateJobRequestPrintsShapeOnly(t *testing.T) {
	secrets := pvSecrets()
	both := pvCreateRequest()
	both.RemoteURL = pvRemoteURL
	tests := []struct {
		name string
		req  neverbounce.CreateJobRequest
		want string
	}{
		{"supplied", pvCreateRequest(), "neverbounce.CreateJobRequest{input: supplied, columns: 3, rows: 2, " +
			"filename: set, autoParse: true, autoStart: false, runSample: false, allowManualReview: false, " +
			"leverageHistoricalData: set, callbackURL: set, callbackHeaders: 1}"},
		{"remote", pvRemoteRequest(), "neverbounce.CreateJobRequest{input: remote_url, columns: 0, rows: 0, " +
			"filename: set, autoParse: true, autoStart: false, runSample: false, allowManualReview: false, " +
			"leverageHistoricalData: set, callbackURL: set, callbackHeaders: 1}"},
		{"both", both, "neverbounce.CreateJobRequest{input: supplied+remote_url, columns: 3, rows: 2, " +
			"filename: set, autoParse: true, autoStart: false, runSample: false, allowManualReview: false, " +
			"leverageHistoricalData: set, callbackURL: set, callbackHeaders: 1}"},
		{"zero", neverbounce.CreateJobRequest{}, "neverbounce.CreateJobRequest{input: none, columns: 0, rows: 0, " +
			"filename: unset, autoParse: false, autoStart: false, runSample: false, allowManualReview: false, " +
			"leverageHistoricalData: unset, callbackURL: unset, callbackHeaders: 0}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.req.String())
			assert.Equal(t, tt.want, tt.req.GoString())
			for _, s := range pvRequestShapes(&tt.req) {
				for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
					testutil.AssertRedacted(t, fmt.Sprintf(verb, s.Value), secrets, "%s %s", s.Name, verb)
				}
			}
		})
	}
}

// TestPrivacyCreateJobRequestNonStringVerbs covers the verbs fmt never routes
// to String or GoString (%d, %t, %b, %o, %e, %c, %U): without a Format method
// they print every field by reflection, rows, URLs and header values
// included. %p is left out: for a value that is not a pointer, fmt reports a
// bad verb and then prints the fields with every method disabled, which no
// type can prevent.
func TestPrivacyCreateJobRequestNonStringVerbs(t *testing.T) {
	secrets := pvSecrets()
	req := pvCreateRequest()
	req.RemoteURL = pvRemoteURL
	for _, s := range pvRequestShapes(&req) {
		for _, verb := range []string{"%d", "%t", "%b", "%o", "%e", "%c", "%U"} {
			testutil.AssertRedacted(t, fmt.Sprintf(verb, s.Value), secrets, "%s %s", s.Name, verb)
		}
	}
}

// TestPrivacyEchoedKeyNeverBecomesAPIStatus proves that a key made only of
// lowercase letters and underscores, which has the APIStatus shape, is never
// kept as APIStatus when a server echoes it as the envelope status.
func TestPrivacyEchoedKeyNeverBecomesAPIStatus(t *testing.T) {
	const key = "secret_lowercaseonlykey" // gitleaks:allow (synthetic test marker)
	tests := []struct {
		status int
		kind   neverbounce.FailureKind
	}{
		{http.StatusOK, neverbounce.FailureUnexpectedResponse},
		{http.StatusBadRequest, neverbounce.FailureRejected},
	}
	for _, tt := range tests {
		c, err := neverbounce.NewClient(key, neverbounce.WithTestBaseURL(loopbackOrigin),
			neverbounce.WithHTTPClient(&http.Client{Transport: testutil.StaticTransport(tt.status, `{"status":"`+key+`"}`)}))
		require.NoError(t, err)
		_, err = c.CheckEmail(t.Context(), checkRequest())
		// The echoed status is dropped, so API stays empty.
		assertCallError(t, err, wantCall{Kind: tt.kind, Op: neverbounce.OpCheckEmail, Status: tt.status}, key)
	}
}
