package neverbounce_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// errJbTransport is the transport failure the in-memory scripts return. Its
// text is a leak marker: it must never reach a CallError.
var errJbTransport = errors.New("jbtransportmarker: connection refused") // gitleaks:allow (synthetic test marker)

// jbControlOK is a successful jobs/parse or jobs/start answer.
const jbControlOK = `{"status":"success","queue_id":"NB-PQ-EXAMPLE-1","execution_time":9}`

// jbControlOp is the case of jobs/parse or jobs/start, with the request's one
// boolean, and a call that takes the job and that boolean and returns what
// both answers hold.
type jbControlOp struct {
	opCase

	flag    string
	control func(ctx context.Context, c *neverbounce.Client, id neverbounce.JobID, flag bool) (string, *int64, error)
}

// jbControlOps returns ParseJob and StartJob as jbControlOps.
func jbControlOps() []jbControlOp {
	return []jbControlOp{
		{
			opCase: opCaseNamed("ParseJob"), flag: "auto_start",
			control: func(ctx context.Context, c *neverbounce.Client, id neverbounce.JobID, flag bool) (string, *int64, error) {
				res, err := c.ParseJob(ctx, neverbounce.ParseJobRequest{JobID: id, AutoStart: flag})
				if err != nil {
					if res != nil {
						return "", nil, errJbResultWithError
					}
					return "", nil, err
				}
				return res.QueueID, res.ExecutionTime, nil
			},
		},
		{
			opCase: opCaseNamed("StartJob"), flag: "run_sample",
			control: func(ctx context.Context, c *neverbounce.Client, id neverbounce.JobID, flag bool) (string, *int64, error) {
				res, err := c.StartJob(ctx, neverbounce.StartJobRequest{JobID: id, RunSample: flag})
				if err != nil {
					if res != nil {
						return "", nil, errJbResultWithError
					}
					return "", nil, err
				}
				return res.QueueID, res.ExecutionTime, nil
			},
		},
	}
}

// errJbResultWithError reports a method that returned a result alongside an
// error.
var errJbResultWithError = errors.New("jobs test: a result came back with an error")

func TestJobControlWireBodies(t *testing.T) {
	for _, op := range jbControlOps() {
		for _, flag := range []bool{false, true} {
			name := op.name + " " + op.flag + " false"
			want := keyMember + `,"job_id":289022,"` + op.flag + `":false}`
			if flag {
				name = op.name + " " + op.flag + " true"
				want = keyMember + `,"job_id":289022,"` + op.flag + `":true}`
			}
			t.Run(name, func(t *testing.T) {
				c, s := newScriptClient(t, testutil.Step{Body: jbControlOK})
				queue, exec, err := op.control(t.Context(), c, 289022, flag)
				require.NoError(t, err)
				assert.Equal(t, "NB-PQ-EXAMPLE-1", queue)
				assert.Equal(t, new(int64(9)), exec)
				r := testutil.OnlyRequest(t, s)
				assertWireRequest(t, r, op.path, mediaJSON)
				assertBody(t, want, r.Body)
			})
		}
	}
}

func TestJobControlResultTypes(t *testing.T) {
	c, _ := newScriptClient(t, testutil.Step{Body: jbControlOK}, testutil.Step{Body: `{"status":"success"}`})
	parsed, err := c.ParseJob(t.Context(), neverbounce.ParseJobRequest{JobID: 1})
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.ParseJobResult{QueueID: "NB-PQ-EXAMPLE-1", ExecutionTime: new(int64(9))}, parsed)
	started, err := c.StartJob(t.Context(), neverbounce.StartJobRequest{JobID: 1, RunSample: true})
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.StartJobResult{}, started, "queue_id is metadata: an answer without it still succeeds")
}

func TestJobControlQueueIDIsMetadata(t *testing.T) {
	visible128 := "Q" + strings.Repeat("~", 127)
	tests := []struct {
		name  string
		extra string
		want  string
	}{
		{"absent", ``, ""},
		{"json null", `,"queue_id":null`, ""},
		{"number", `,"queue_id":12345`, ""},
		{"boolean", `,"queue_id":true`, ""},
		{"object", `,"queue_id":{"id":"NB"}`, ""},
		{"empty", `,"queue_id":""`, ""},
		{"128 visible bytes", `,"queue_id":"` + visible128 + `"`, visible128},
		{"129 bytes", `,"queue_id":"` + visible128 + `Z"`, ""},
		{"non-ASCII", `,"queue_id":"queu` + "\xc3\xa9" + `"`, ""},
		{"inner space", `,"queue_id":"NB PQ"`, ""},
		{"escaped visible text", testutil.Esc(`,"queue_id":"NB~u002dPQ~/1"`), "NB-PQ/1"},
		{"escaped control character", testutil.Esc(`,"queue_id":"NB~u0001"`), ""},
		{"escaped line break", testutil.Esc(`,"queue_id":"NB~n"`), ""},
		{"lone surrogate", testutil.Esc(`,"queue_id":"NB~ud800"`), ""},
		{"other case only", `,"Queue_ID":"NB-PQ"`, ""},
	}
	for _, op := range jbControlOps() {
		for _, tt := range tests {
			t.Run(op.name+" "+tt.name, func(t *testing.T) {
				c, _ := newScriptClient(t, testutil.Step{Body: `{"status":"success"` + tt.extra + `}`})
				queue, exec, err := op.control(t.Context(), c, 42, false)
				require.NoError(t, err, "metadata never fails a call that may have started a job")
				assert.Equal(t, tt.want, queue)
				assert.Nil(t, exec)
			})
		}
	}
}

// jbCallFailure is one way an attempted call fails, with the kind, HTTP
// status, envelope status and field the CallError reports.
type jbCallFailure struct {
	name   string
	step   testutil.Step
	kind   neverbounce.FailureKind
	status int
	api    neverbounce.APIStatus
	field  string
}

// wantFor returns the CallError f produces when it ends a call of op about
// job id and page.
func (f jbCallFailure) wantFor(op neverbounce.Operation, id neverbounce.JobID, page int) wantCall {
	return wantCall{Kind: f.kind, Op: op, Status: f.status, API: f.api, JobID: id, Page: page, Field: f.field}
}

// jbCallFailures returns the failures an attempted JSON call can meet without
// a clock: transport, HTTP status, body and envelope failures.
func jbCallFailures() []jbCallFailure {
	return []jbCallFailure{
		{"transport error", testutil.Step{Err: errJbTransport}, neverbounce.FailureTransport, 0, "", ""},
		{
			"malformed 200 body",
			testutil.Step{Body: `{"status":"success",`},
			neverbounce.FailureMalformedResponse, 200, "", "",
		},
		{
			"200 body without status",
			testutil.Step{Body: `{"job_id":1}`},
			neverbounce.FailureMalformedResponse, 200, "", "status",
		},
		{"200 array body", testutil.Step{Body: `[]`}, neverbounce.FailureUnexpectedResponse, 200, "", ""},
		{
			"HTTP 500",
			testutil.Step{Status: http.StatusInternalServerError, Body: `oops`},
			neverbounce.FailureUpstream, 500, "", "",
		},
		{"HTTP 502", testutil.Step{Status: http.StatusBadGateway}, neverbounce.FailureUpstream, 502, "", ""},
		{"HTTP 503", testutil.Step{Status: http.StatusServiceUnavailable}, neverbounce.FailureUnavailable, 503, "", ""},
		{"HTTP 413", testutil.Step{Status: http.StatusRequestEntityTooLarge}, neverbounce.FailureRejected, 413, "", ""},
		{"HTTP 429", testutil.Step{Status: http.StatusTooManyRequests}, neverbounce.FailureRateLimited, 429, "", ""},
		{"HTTP 302", testutil.Step{
			Status: http.StatusFound, Header: http.Header{"Location": {"https://example.org/elsewhere"}},
		}, neverbounce.FailureUnexpectedResponse, 302, "", ""},
		{
			"general_failure envelope",
			testutil.Step{Body: `{"status":"general_failure"}`},
			neverbounce.FailureRejected, 200, neverbounce.APIStatusGeneralFailure, "",
		},
		{
			"throttle_triggered envelope",
			testutil.Step{Body: `{"status":"throttle_triggered"}`},
			neverbounce.FailureRateLimited, 200, neverbounce.APIStatusThrottleTriggered, "",
		},
		{
			"auth_failure envelope",
			testutil.Step{Body: `{"status":"auth_failure"}`},
			neverbounce.FailureUnauthorized, 200, neverbounce.APIStatusAuthFailure, "",
		},
	}
}

// jbFailureSecrets returns the texts jbCallFailures plant, which no CallError
// may print: the transport error's marker and the redirect's target.
func jbFailureSecrets() []string {
	return []string{"jbtransportmarker", "example.org/elsewhere"}
}
