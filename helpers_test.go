package neverbounce_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Synthetic test values. They double as leak markers: no error, formatted
// value or reflection walk may ever contain them. They are not real secrets.
const (
	testKey   = "secret_keymarker7f3a2c91" // gitleaks:allow (synthetic test marker)
	testEmail = "avery.example@example.com"
)

// loopbackOrigin is a test base URL for in-memory transports, which never dial.
const loopbackOrigin = "https://127.0.0.1"

// keyMember is the first member of every request body the test key produces.
const keyMember = `{"key":"` + testKey + `"`

// checkOK is the smallest successful single/check answer.
const checkOK = `{"status":"success","result":"valid"}`

// csvFile is a small download: a header row and one address.
const csvFile = "email\r\n" + testEmail + "\r\n"

// Media types the SDK sends, accepts and refuses.
const (
	mediaJSON  = "application/json"
	mediaOctet = "application/octet-stream"
	mediaCSV   = "text/csv"
)

// The path of each operation.
const (
	pathCheck    = "/v4.2/single/check"
	pathCreate   = "/v4.2/jobs/create"
	pathParse    = "/v4.2/jobs/parse"
	pathStart    = "/v4.2/jobs/start"
	pathStatus   = "/v4.2/jobs/status"
	pathResults  = "/v4.2/jobs/results"
	pathDownload = "/v4.2/jobs/download"
)

// Validation reasons, as ValidationError.Reason reports them.
const (
	reasonRequired         = "required"
	reasonOutOfRange       = "out_of_range"
	reasonNotInitialized   = "not_initialized"
	reasonInvalidFormat    = "invalid_format"
	reasonInvalidURL       = "invalid_url"
	reasonControlCharacter = "control_character"
)

// errUnrelated is an error no CallError may ever match.
var errUnrelated = errors.New("unrelated test error")

// errBrokenCall reports an operation case whose call broke its own contract: a
// download that returned a count other than the bytes its writer received,
// or other bytes than its answer held, or a walk that yielded after its error.
var errBrokenCall = errors.New("the call broke its contract")

// opJobID is the job every operation case concerns.
const opJobID neverbounce.JobID = 42

// opPage is the jobs/results page the operation cases read.
const opPage = 2

// opCase is one public method of Client that calls the provider, called with a
// minimal valid request and reduced to its error. Every suite that runs all
// the methods shares it; a suite that needs more, such as a consequential
// request, a marker payload or an exact wire body, wraps it.
type opCase struct {
	name   string                // the method's name
	op     neverbounce.Operation // the operation it calls
	path   string                // that operation's path
	jobID  neverbounce.JobID     // the JobID of its CallErrors: 0 for CheckEmail and CreateJob
	page   int                   // the Page of its CallErrors: the first page it reads, jobs/results only
	helper bool                  // WaitForJob and JobResultPages, which loop over an operation
	ok     testutil.Step         // an answer to its first call that ends it successfully
	call   func(ctx context.Context, c *neverbounce.Client) error
}

// want completes w, the CallError of a failed call of o, with o's Op, JobID
// and Page.
func (o opCase) want(w wantCall) wantCall {
	w.Op, w.JobID, w.Page = o.op, o.jobID, o.page
	return w
}

// takesJob reports whether o's request names a job.
func (o opCase) takesJob() bool {
	return o.op != neverbounce.OpCheckEmail && o.op != neverbounce.OpCreateJob
}

// opCases returns the operation cases for opJobID; see opCasesFor.
func opCases() []opCase { return opCasesFor(opJobID) }

// opCasesFor returns the operation cases for job id: the seven operations in
// the documentation's order, then WaitForJob and JobResultPages. A test of
// JobID validation passes an id the SDK refuses.
func opCasesFor(id neverbounce.JobID) []opCase {
	jsonStep := func(body string) testutil.Step {
		return testutil.Step{Header: http.Header{"Content-Type": {mediaJSON}}, Body: body}
	}
	jobText := strconv.FormatInt(int64(id), 10)
	status := jsonStep(`{"status":"success","id":` + jobText + `,"job_status":"complete"}`)
	results := jsonStep(`{"status":"success","total_results":0,"total_pages":0,"results":[]}`)
	queued := jsonStep(`{"status":"success","queue_id":"NB-PQ-0001"}`)
	return []opCase{
		{
			name: "CheckEmail", op: neverbounce.OpCheckEmail, path: pathCheck, ok: jsonStep(checkOK),
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.CheckEmail(ctx, checkRequest()))
			},
		},
		{
			name: "CreateJob", op: neverbounce.OpCreateJob, path: pathCreate,
			ok: jsonStep(`{"status":"success","job_id":` + jobText + `}`),
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.CreateJob(ctx, neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList(testEmail)}))
			},
		},
		{
			name: "ParseJob", op: neverbounce.OpParseJob, path: pathParse, jobID: id, ok: queued,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.ParseJob(ctx, neverbounce.ParseJobRequest{JobID: id}))
			},
		},
		{
			name: "StartJob", op: neverbounce.OpStartJob, path: pathStart, jobID: id, ok: queued,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.StartJob(ctx, neverbounce.StartJobRequest{JobID: id, RunSample: true}))
			},
		},
		{
			name: "JobStatus", op: neverbounce.OpJobStatus, path: pathStatus, jobID: id, ok: status,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.JobStatus(ctx, neverbounce.JobStatusRequest{JobID: id}))
			},
		},
		{
			name: "JobResults", op: neverbounce.OpJobResults, path: pathResults, jobID: id, page: opPage, ok: results,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return noValueWith(c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: id, Page: opPage, ItemsPerPage: 10}))
			},
		},
		{
			name: "DownloadJob", op: neverbounce.OpDownloadJob, path: pathDownload, jobID: id,
			ok: testutil.Step{Header: http.Header{"Content-Type": {mediaOctet}}, Body: csvFile},
			call: func(ctx context.Context, c *neverbounce.Client) error {
				var w bytes.Buffer
				req := neverbounce.DownloadJobRequest{JobID: id, LineFeed: neverbounce.LineFeedCRLF}
				n, err := c.DownloadJob(ctx, req, &w)
				if n != int64(w.Len()) || (err == nil && w.String() != csvFile) {
					return errBrokenCall
				}
				return err
			},
		},
		{
			name: "WaitForJob", op: neverbounce.OpJobStatus, path: pathStatus, jobID: id, helper: true, ok: status,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				// WaitForJob returns the last status it read alongside its error,
				// so only a refused request must come without one.
				st, err := c.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: id, MaxWait: time.Hour})
				if _, refused := errors.AsType[*neverbounce.ValidationError](err); refused && st != nil {
					return errBrokenCall
				}
				return err
			},
		},
		{
			name: "JobResultPages", op: neverbounce.OpJobResults, path: pathResults, jobID: id, page: 1, helper: true,
			ok: results,
			call: func(ctx context.Context, c *neverbounce.Client) error {
				return walkErr(c.JobResultPages(ctx, neverbounce.JobResultPagesRequest{
					JobID: id, ItemsPerPage: 10, MaxPages: 5,
				}))
			},
		},
	}
}

// opCaseNamed returns the case of the method called name.
func opCaseNamed(name string) opCase {
	i := slices.IndexFunc(opCases(), func(o opCase) bool { return o.name == name })
	return opCases()[i]
}

// operations returns the cases of the seven operations, without the helpers.
func operations() []opCase {
	return slices.DeleteFunc(opCases(), func(o opCase) bool { return o.helper })
}

// jsonOperations returns the cases of the six JSON operations: every
// operation but DownloadJob.
func jsonOperations() []opCase {
	return slices.DeleteFunc(operations(), func(o opCase) bool { return o.op == neverbounce.OpDownloadJob })
}

// noValueWith returns err, or errBrokenCall when a value came with it: a
// method that fails returns nil.
func noValueWith[T any](v *T, err error) error {
	if err != nil && v != nil {
		return errBrokenCall
	}
	return err
}

// walkErr ranges over a walk and returns the error it ends with, or nil. A
// walk yields an error once, as its last value: anything after it is
// errBrokenCall.
func walkErr(seq iter.Seq2[*neverbounce.JobResultsPage, error]) error {
	var last error
	for _, err := range seq {
		if last != nil {
			return errBrokenCall
		}
		last = err
	}
	return last
}

// newClient returns a client built with the test key and opts, failing tb when
// NewClient fails.
func newClient(tb testing.TB, opts ...neverbounce.Option) *neverbounce.Client {
	tb.Helper()
	c, err := neverbounce.NewClient(testKey, opts...)
	require.NoError(tb, err)
	return c
}

// newServerClient starts a recording HTTP/1.1 TLS server running h and returns
// a client pointed at it, with opts applied after the server options.
func newServerClient(tb testing.TB, h http.HandlerFunc, opts ...neverbounce.Option) (*neverbounce.Client, *testutil.Server) {
	tb.Helper()
	return newServerClientProto(tb, testutil.HTTP1, h, opts...)
}

// newServerClientProto is newServerClient for a server speaking proto.
func newServerClientProto(tb testing.TB, proto testutil.Protocol, h http.HandlerFunc,
	opts ...neverbounce.Option,
) (*neverbounce.Client, *testutil.Server) {
	tb.Helper()
	srv := testutil.NewServerProto(tb, proto, h)
	all := append([]neverbounce.Option{
		neverbounce.WithTestBaseURL(srv.URL()),
		neverbounce.WithHTTPClient(srv.Client()),
	}, opts...)
	return newClient(tb, all...), srv
}

// newTransportClient returns a client whose calls go through rt, in memory,
// with opts applied after the transport options.
func newTransportClient(tb testing.TB, rt http.RoundTripper, opts ...neverbounce.Option) *neverbounce.Client {
	tb.Helper()
	all := append([]neverbounce.Option{
		neverbounce.WithTestBaseURL(loopbackOrigin),
		neverbounce.WithHTTPClient(&http.Client{Transport: rt}),
	}, opts...)
	return newClient(tb, all...)
}

// newScriptClient returns a client whose calls go, in memory, through a new
// Script answering with steps.
func newScriptClient(tb testing.TB, steps ...testutil.Step) (*neverbounce.Client, *testutil.Script) {
	tb.Helper()
	s := testutil.NewScript(steps...)
	return newTransportClient(tb, s), s
}

// callOnce makes one call answered in memory with body on HTTP 200, asserts
// that it made exactly one attempt, to path, and returned nothing alongside
// an error, and returns its outcome.
func callOnce[T any](tb testing.TB, path, body string,
	call func(context.Context, *neverbounce.Client) (*T, error),
) (*T, error) {
	tb.Helper()
	c, s := newScriptClient(tb, testutil.Step{Body: body})
	v, err := call(tb.Context(), c)
	assert.Equal(tb, []string{path}, s.Paths(), "exactly one attempt, to the operation's path")
	if err != nil {
		assert.Nil(tb, v, "nothing alongside an error")
	}
	return v, err
}

// respond returns a handler that answers with status and body as JSON.
func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// callError returns the *neverbounce.CallError in err's chain, failing tb when
// there is none.
func callError(tb testing.TB, err error) *neverbounce.CallError {
	tb.Helper()
	return testutil.RequireErrorAs[*neverbounce.CallError](tb, err)
}

// validationError returns the *neverbounce.ValidationError in err's chain,
// failing tb when there is none.
func validationError(tb testing.TB, err error) *neverbounce.ValidationError {
	tb.Helper()
	return testutil.RequireErrorAs[*neverbounce.ValidationError](tb, err)
}

// assertValidation asserts that err is a request *ValidationError for field
// with reason.
func assertValidation(tb testing.TB, err error, field, reason string) {
	tb.Helper()
	require.ErrorIs(tb, err, neverbounce.ErrInvalidRequest)
	ve := validationError(tb, err)
	assert.Equal(tb, field, ve.Field)
	assert.Equal(tb, reason, ve.Reason)
}

// assertErrorClean fails tb when a secret appears however a caller might print
// or walk err: any fmt verb on each error of its Unwrap chain, or on err
// wrapped or joined, or any string reflection reaches. The chain must also hold
// no *url.Error, whose text quotes the URL.
func assertErrorClean(tb testing.TB, err error, secrets ...string) {
	tb.Helper()
	for e := err; e != nil; e = errors.Unwrap(e) {
		testutil.AssertNeverPrints(tb, e, secrets...)
	}
	testutil.AssertNeverPrints(tb, fmt.Errorf("wrapped: %w", err), secrets...)
	testutil.AssertNeverPrints(tb, errors.Join(err, context.Canceled), secrets...)
	testutil.AssertRedacted(tb, strings.Join(testutil.ReflectStrings(err), "\n"), secrets, "reflection walk")
	_, isURLErr := errors.AsType[*url.Error](err)
	assert.False(tb, isURLErr, "the chain must never hold a *url.Error")
}

// wantCall is the *CallError a test expects: every exported field, zero
// values included.
type wantCall struct {
	Kind    neverbounce.FailureKind
	Op      neverbounce.Operation
	Status  int
	API     neverbounce.APIStatus
	JobID   neverbounce.JobID
	Page    int
	Field   string
	Written int64
}

// assertCallError asserts that err is the *CallError want describes, field
// for field. The Error text the caller sees must be the documented one, err
// must match exactly the sentinels of its kind, its cause must be a fixed one,
// and neither testKey, testEmail nor any of secrets may appear however err is
// printed or walked. A test that needs more of the CallError, such as its
// Diagnostics, takes it with callError.
func assertCallError(tb testing.TB, err error, want wantCall, secrets ...string) {
	tb.Helper()
	ce := callError(tb, err)
	assert.Equal(tb, want, wantCall{
		Kind: ce.Kind, Op: ce.Op, Status: ce.StatusCode, API: ce.APIStatus,
		JobID: ce.JobID, Page: ce.Page, Field: ce.Field, Written: ce.BytesWritten,
	})
	assert.Equal(tb, errorText(want), err.Error(), "the error the caller sees")
	assertOnlySentinel(tb, err, want.Kind)
	assertFixedCause(tb, err)
	assertErrorClean(tb, err, append([]string{testKey, testEmail}, secrets...)...)
}

// errorText renders the documented Error grammar of a CallError,
// "neverbounce: <op>: <kind>[ (HTTP <n>, <api status>)][: <field>]", where the
// parentheses hold whichever of the two is known, independently of the
// library.
func errorText(w wantCall) string {
	s := "neverbounce: " + string(w.Op) + ": " + string(w.Kind)
	var meta []string
	if w.Status != 0 {
		meta = append(meta, "HTTP "+strconv.Itoa(w.Status))
	}
	if w.API != "" {
		meta = append(meta, string(w.API))
	}
	if len(meta) > 0 {
		s += " (" + strings.Join(meta, ", ") + ")"
	}
	if w.Field != "" {
		s += ": " + w.Field
	}
	return s
}

// allSentinels returns every error a CallError could be asked about with
// errors.Is: the package's sentinels, both context errors, and an unrelated
// error.
func allSentinels() []error {
	return []error{
		neverbounce.ErrInvalidConfig, neverbounce.ErrInvalidRequest, neverbounce.ErrUnauthorized,
		neverbounce.ErrRateLimited, neverbounce.ErrUnavailable, neverbounce.ErrRejected, neverbounce.ErrUpstream,
		neverbounce.ErrUnexpectedResponse, neverbounce.ErrMalformedResponse, neverbounce.ErrResponseTooLarge,
		neverbounce.ErrTransport, neverbounce.ErrTimeout, neverbounce.ErrWriteFailed, neverbounce.ErrJobFailed,
		neverbounce.ErrUnknownJobState, neverbounce.ErrBudgetExhausted,
		context.Canceled, context.DeadlineExceeded, errUnrelated,
	}
}

// wantMatches returns the targets a CallError of kind must match with
// errors.Is: its own sentinel, plus context.DeadlineExceeded for a timeout.
func wantMatches(kind neverbounce.FailureKind) []error {
	want := []error{kindSentinels()[kind]}
	if kind == neverbounce.FailureTimeout {
		want = append(want, context.DeadlineExceeded)
	}
	return want
}

// assertOnlySentinel asserts that err, a CallError of kind or anything
// wrapping one, matches exactly the targets documented for kind and no other
// sentinel or context error.
func assertOnlySentinel(tb testing.TB, err error, kind neverbounce.FailureKind) {
	tb.Helper()
	want := wantMatches(kind)
	for _, target := range allSentinels() {
		assert.Equal(tb, slices.Contains(want, target), errors.Is(err, target), "kind %s vs %v", kind, target)
	}
}

// assertFixedCause asserts that the cause of err's *CallError is a context
// error or a fixed error of the package that wraps nothing: never a writer's,
// reader's or transport's own error.
func assertFixedCause(tb testing.TB, err error) {
	tb.Helper()
	cause := callError(tb, err).Unwrap()
	require.Error(tb, cause, "a CallError the SDK returns keeps a cause")
	if slices.Contains([]error{context.Canceled, context.DeadlineExceeded}, cause) {
		return
	}
	assert.True(tb, strings.HasPrefix(cause.Error(), "neverbounce: "), "cause %q is a fixed package error", cause)
	require.NoError(tb, errors.Unwrap(cause), "a fixed cause wraps nothing")
}

// statusKind is the documented failure kind of an HTTP status other than
// 200, written out independently of the library.
func statusKind(code int) neverbounce.FailureKind {
	switch {
	case code == 401 || code == 403:
		return neverbounce.FailureUnauthorized
	case code == 429:
		return neverbounce.FailureRateLimited
	case code == 503:
		return neverbounce.FailureUnavailable
	case code >= 400 && code <= 499:
		return neverbounce.FailureRejected
	case code >= 500 && code <= 599:
		return neverbounce.FailureUpstream
	default:
		return neverbounce.FailureUnexpectedResponse
	}
}

// kindSentinels is the documented mapping from each failure kind to the
// sentinel, or context error, that it matches with errors.Is, written out
// independently of the library's. FailureTimeout also matches
// context.DeadlineExceeded.
func kindSentinels() map[neverbounce.FailureKind]error {
	return map[neverbounce.FailureKind]error{
		neverbounce.FailureUnauthorized:       neverbounce.ErrUnauthorized,
		neverbounce.FailureRateLimited:        neverbounce.ErrRateLimited,
		neverbounce.FailureUnavailable:        neverbounce.ErrUnavailable,
		neverbounce.FailureRejected:           neverbounce.ErrRejected,
		neverbounce.FailureUpstream:           neverbounce.ErrUpstream,
		neverbounce.FailureUnexpectedResponse: neverbounce.ErrUnexpectedResponse,
		neverbounce.FailureMalformedResponse:  neverbounce.ErrMalformedResponse,
		neverbounce.FailureResponseTooLarge:   neverbounce.ErrResponseTooLarge,
		neverbounce.FailureTransport:          neverbounce.ErrTransport,
		neverbounce.FailureTimeout:            neverbounce.ErrTimeout,
		neverbounce.FailureCanceled:           context.Canceled,
		neverbounce.FailureDeadlineExceeded:   context.DeadlineExceeded,
		neverbounce.FailureWriteFailed:        neverbounce.ErrWriteFailed,
		neverbounce.FailureJobFailed:          neverbounce.ErrJobFailed,
		neverbounce.FailureUnknownJobState:    neverbounce.ErrUnknownJobState,
		neverbounce.FailureBudgetExhausted:    neverbounce.ErrBudgetExhausted,
	}
}

// assertBody asserts that got is exactly the JSON text want, byte for byte:
// member order, escaping and the key's position are part of the contract, so
// a semantic JSON comparison would be too lenient.
func assertBody(tb testing.TB, want string, got []byte) {
	tb.Helper()
	assert.Equal(tb, []byte(want), got, "JSON text\n got: %s\nwant: %s", got, want)
}

// assertRoundTrip marshals v, checks the JSON is exactly want, unmarshals it
// into a new T, and checks the copy is reflect.DeepEqual to v. It returns the
// copy.
func assertRoundTrip[T any](tb testing.TB, v *T, want string) *T {
	tb.Helper()
	data, err := json.Marshal(v)
	require.NoError(tb, err)
	assertBody(tb, want, data)
	back := new(T)
	require.NoError(tb, json.Unmarshal(data, back))
	assert.True(tb, reflect.DeepEqual(v, back), "round trip changed the value")
	assert.Equal(tb, v, back)
	return back
}

// bannedHeaders are the request headers the SDK must never send: credentials,
// cookies, the idempotency keys with which the standard library may replay a
// POST, and Range, which would ask for part of a download.
func bannedHeaders() []string {
	return []string{"Authorization", "Proxy-Authorization", "Cookie", "Idempotency-Key", "X-Idempotency-Key", "Range"}
}

// assertWireRequest asserts the wire policy every operation shares: one POST
// to path with the SDK's fixed headers, Accept being accept, none of the
// banned headers, a body that is neither rewindable nor http.NoBody, and an
// exact Content-Length.
func assertWireRequest(tb testing.TB, r testutil.Request, path, accept string) {
	tb.Helper()
	assert.Equal(tb, http.MethodPost, r.Method)
	assert.Equal(tb, path, r.Path)
	assert.Equal(tb, []string{mediaJSON}, r.Header.Values("Content-Type"))
	assert.Equal(tb, []string{accept}, r.Header.Values("Accept"))
	assert.Equal(tb, []string{"go-neverbounce"}, r.Header.Values("User-Agent"))
	for _, h := range bannedHeaders() {
		assert.Empty(tb, r.Header.Values(h), "header %s must never be sent", h)
	}
	assert.False(tb, r.GetBody, "GetBody must be nil, so nothing can be resent")
	assert.False(tb, r.NoBody, "the body is never http.NoBody")
	assert.Equal(tb, int64(len(r.Body)), r.ContentLength)
}

// checkRequest returns a minimal valid single-check request.
func checkRequest() neverbounce.CheckEmailRequest {
	return neverbounce.CheckEmailRequest{Email: testEmail}
}
