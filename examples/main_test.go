package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
)

// testClient returns a client for a local TLS server running h.
func testClient(t *testing.T, h http.HandlerFunc) *neverbounce.Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL), neverbounce.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	return client
}

// answer returns a handler that answers every call with a JSON body.
func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// failingAPI plays the fake, except that it answers call number n (from 1)
// to path with body, a failure envelope, instead.
type failingAPI struct {
	fakeNeverBounce

	path string
	n    int
	body string

	mu    sync.Mutex
	calls int
}

// ServeHTTP answers the failing call itself and passes every other one on.
func (f *failingAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == f.path {
		f.mu.Lock()
		f.calls++
		fail := f.calls == f.n
		f.mu.Unlock()
		if fail {
			answer(f.body)(w, r)
			return
		}
	}
	f.fakeNeverBounce.ServeHTTP(w, r)
}

func TestRunPrintsTheDemo(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, run(t.Context(), &out, &fakeNeverBounce{}))
	assert.Equal(t, `Single check (1 credit on the real API)
  avery.example@example.com: valid
  flags: has_dns, has_dns_mx, smtp_connectable
  host: example.com, normalized: avery.example@example.com
  paid credits: 1 used, 4999 remaining

Bulk job: create, wait, check the billable count, start, wait
  created job 4242 with parsing only: nothing billed yet
  job 4242 is waiting after 2 status reads: 3 records, 3 billable (budget 100)
  started job 4242 (queue NB-EXAMPLE-QUEUE): the real API deducts the billable credits now
  job 4242 is complete after 2 status reads: 2 valid, 1 invalid

Results, matched by your own id column
  page 1 of 2
    skipped a row that is not one of yours (id "id")
    1001 Avery: valid
  page 2 of 2
    1002 Blake: invalid (did you mean blake.example@example.com?)
    1003 Casey: valid

Download: the CSV, streamed unchanged
  139 bytes
  "id,email,email_status\r\n"
  "1001,avery.example@example.com,valid\r\n"
  "1002,blake.example@example.org,invalid\r\n"
  "1003,casey.example@example.net,valid\r\n"
`, out.String())
}

func TestRunDescribesACanceledCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := run(ctx, &out, &fakeNeverBounce{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "Single check (1 credit on the real API)\n"+
		"stopped by the caller; the call may already have been processed and billed\n", out.String())
}

// TestRunDescribesEachFailedStep fails each step of the bulk flow in turn and
// checks that run stops there, describes the failure, and returns it.
func TestRunDescribesEachFailedStep(t *testing.T) {
	tests := []struct {
		name string
		path string
		n    int
		body string
		want error
		last string // the line before describe's
		says string // describe's line
	}{
		{
			"first wait", "/v4.2/jobs/status", 1, `{"status":"auth_failure"}`, neverbounce.ErrUnauthorized,
			"  created job 4242 with parsing only: nothing billed yet", "check the API key",
		},
		{
			"start", "/v4.2/jobs/start", 1, `{"status":"throttle_triggered"}`, neverbounce.ErrRateLimited,
			"  job 4242 is waiting after 2 status reads: 3 records, 3 billable (budget 100)",
			"slow down or try later; the SDK never retries",
		},
		{
			"second wait", "/v4.2/jobs/status", 3, `{"status":"temp_unavail"}`, neverbounce.ErrUnavailable,
			"  started job 4242 (queue NB-EXAMPLE-QUEUE): the real API deducts the billable credits now",
			"slow down or try later; the SDK never retries",
		},
		{
			"results", "/v4.2/jobs/results", 1, `{"status":"success","total_results":1,"total_pages":1}`,
			neverbounce.ErrMalformedResponse, "Results, matched by your own id column",
			"the outcome is unknown; do not resend blindly",
		},
		{
			"download", "/v4.2/jobs/download", 1, `{"status":"general_failure"}`, neverbounce.ErrRejected,
			"Download: the CSV, streamed unchanged", "the outcome is unknown; do not resend blindly",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(t.Context(), &out, &failingAPI{path: tt.path, n: tt.n, body: tt.body})
			require.ErrorIs(t, err, tt.want)
			assert.True(t, strings.HasSuffix(out.String(), "\n"+tt.last+"\n"+tt.says+"\n"),
				"run stops at the failed step and describes it:\n%s", out.String())
		})
	}
}

func TestVerifyListStopsBeforeAnOverBudgetStart(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		body := createAnswer
		if r.URL.Path == "/v4.2/jobs/status" {
			body = `{"status":"success","id":4242,"job_status":"waiting","total":{"billable":101}}`
		}
		answer(body)(w, r)
	})
	var out bytes.Buffer
	_, err := verifyList(t.Context(), &out, client, &fakeNeverBounce{})
	require.ErrorIs(t, err, errOverBudget)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"/v4.2/jobs/create", "/v4.2/jobs/status"}, paths, "never started")
}

func TestStepsReturnTheirCallError(t *testing.T) {
	tests := []struct {
		name string
		body string
		step func(context.Context, *neverbounce.Client) error
		want error
	}{
		{
			name: "check",
			body: `{"status":"auth_failure"}`,
			step: func(ctx context.Context, c *neverbounce.Client) error { return checkOne(ctx, io.Discard, c) },
			want: neverbounce.ErrUnauthorized,
		},
		{
			name: "create",
			body: `{"status":"throttle_triggered"}`,
			step: func(ctx context.Context, c *neverbounce.Client) error {
				_, err := verifyList(ctx, io.Discard, c, &fakeNeverBounce{})
				return err
			},
			want: neverbounce.ErrRateLimited,
		},
		{
			name: "results",
			body: `{"status":"success","total_results":1,"total_pages":1}`,
			step: func(ctx context.Context, c *neverbounce.Client) error { return readResults(ctx, io.Discard, c, 4242) },
			want: neverbounce.ErrMalformedResponse,
		},
		{
			name: "download",
			body: `{"status":"general_failure"}`,
			step: func(ctx context.Context, c *neverbounce.Client) error { return download(ctx, io.Discard, c, 4242) },
			want: neverbounce.ErrRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.step(t.Context(), testClient(t, answer(tt.body))), tt.want)
		})
	}
}

func TestDescribe(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			"invalid config", &neverbounce.ValidationError{Err: neverbounce.ErrInvalidConfig, Field: "apiKey"},
			"fix the request; nothing was sent",
		},
		{
			"invalid request", &neverbounce.ValidationError{Err: neverbounce.ErrInvalidRequest, Field: "Email"},
			"fix the request; nothing was sent",
		},
		{
			"sdk timeout", &neverbounce.CallError{Kind: neverbounce.FailureTimeout},
			"the SDK's timeout expired; the call may already have been billed",
		},
		{
			"caller deadline", &neverbounce.CallError{Kind: neverbounce.FailureDeadlineExceeded},
			"stopped by the caller; the call may already have been processed and billed",
		},
		{
			"canceled", &neverbounce.CallError{Kind: neverbounce.FailureCanceled},
			"stopped by the caller; the call may already have been processed and billed",
		},
		{"unauthorized", &neverbounce.CallError{Kind: neverbounce.FailureUnauthorized}, "check the API key"},
		{
			"rate limited", &neverbounce.CallError{Kind: neverbounce.FailureRateLimited},
			"slow down or try later; the SDK never retries",
		},
		{
			"unavailable", &neverbounce.CallError{Kind: neverbounce.FailureUnavailable},
			"slow down or try later; the SDK never retries",
		},
		{
			"job failed", &neverbounce.CallError{Kind: neverbounce.FailureJobFailed},
			"the job is not where it should be; read its status before acting again",
		},
		{
			"unknown job state", &neverbounce.CallError{Kind: neverbounce.FailureUnknownJobState},
			"the job is not where it should be; read its status before acting again",
		},
		{
			"budget exhausted", &neverbounce.CallError{Kind: neverbounce.FailureBudgetExhausted},
			"the job is not where it should be; read its status before acting again",
		},
		{"over budget", errOverBudget, "the job is not where it should be; read its status before acting again"},
		{
			"malformed response", &neverbounce.CallError{Kind: neverbounce.FailureMalformedResponse},
			"the outcome is unknown; do not resend blindly",
		},
		{
			"transport", &neverbounce.CallError{Kind: neverbounce.FailureTransport},
			"the outcome is unknown; do not resend blindly",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got := describe(&out, tt.err)
			assert.Same(t, tt.err, got, "describe returns the error unchanged")
			assert.Equal(t, tt.want+"\n", out.String())
		})
	}
}

func TestFakeNeverBounceRefusesWhatItDoesNotPlay(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{"not JSON", "/v4.2/single/check", "email=x", http.StatusBadRequest},
		{"unknown path", "/v4.2/account/info", "{}", http.StatusNotFound},
		{"parse", "/v4.2/jobs/parse", `{"job_id":4242}`, http.StatusNotFound},
		{"page past the end", "/v4.2/jobs/results", `{"job_id":4242,"page":3}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, strings.NewReader(tt.body))
			(&fakeNeverBounce{}).ServeHTTP(rec, req)
			assert.Equal(t, tt.status, rec.Code)
		})
	}
}

func TestJoinFlags(t *testing.T) {
	assert.Equal(t, "none", joinFlags(nil))
	assert.Equal(t, "has_dns, future_flag", joinFlags(neverbounce.Flags{neverbounce.FlagHasDNS, "future_flag"}))
}

func TestCount(t *testing.T) {
	assert.Equal(t, "not reported", count(nil))
	assert.Equal(t, "0", count(new(int64(0))))
	assert.Equal(t, "3", count(new(int64(3))))
}
