package neverbounce_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// jbPaidOps returns the operations that may bill or change state, each in its
// most consequential form: CheckEmail, CreateJob with AutoParse and AutoStart,
// ParseJob with AutoStart, and a full StartJob.
func jbPaidOps() []opCase {
	consequential := map[string]func(ctx context.Context, c *neverbounce.Client) error{
		"CreateJob": func(ctx context.Context, c *neverbounce.Client) error {
			req := jbSuppliedRequest()
			req.AutoParse, req.AutoStart = true, true
			return noValueWith(c.CreateJob(ctx, req))
		},
		"ParseJob": func(ctx context.Context, c *neverbounce.Client) error {
			return noValueWith(c.ParseJob(ctx, neverbounce.ParseJobRequest{JobID: opJobID, AutoStart: true}))
		},
		"StartJob": func(ctx context.Context, c *neverbounce.Client) error {
			return noValueWith(c.StartJob(ctx, neverbounce.StartJobRequest{JobID: opJobID}))
		},
	}
	var paid []opCase
	for _, o := range operations() {
		if call, ok := consequential[o.name]; ok {
			o.call = call
		} else if o.op != neverbounce.OpCheckEmail {
			continue
		}
		paid = append(paid, o)
	}
	return paid
}

// jbFreeOps returns the read-only operations that make one call each:
// JobStatus, JobResults and DownloadJob.
func jbFreeOps() []opCase {
	return slices.DeleteFunc(operations(), func(o opCase) bool {
		return o.op != neverbounce.OpJobStatus && o.op != neverbounce.OpJobResults && o.op != neverbounce.OpDownloadJob
	})
}

// jbFollowUps are answers a script holds after the failing step, so that any
// follow-up call (a retry, a status check, a resubmit) would succeed and be
// recorded rather than fail on an empty script.
func jbFollowUps() []testutil.Step {
	return []testutil.Step{
		{Body: `{"status":"success","job_id":42,"id":42,"job_status":"running","result":"valid","queue_id":"Q",` +
			`"total_results":0,"total_pages":0,"results":[]}`},
		{Body: `{"status":"success","job_id":42,"id":42,"job_status":"complete","result":"valid"}`},
	}
}

// jbAssertOneAttempt asserts that s recorded exactly one request, to path,
// and nothing else.
func jbAssertOneAttempt(t *testing.T, s *testutil.Script, path string) {
	t.Helper()
	assert.Equal(t, []string{path}, s.Paths(), "exactly one POST to the operation's path, and no follow-up call")
	for _, r := range s.Requests() {
		assert.Equal(t, http.MethodPost, r.Method)
	}
}

// TestCallCountAfterFailures proves that every failure an attempted call can
// meet ends the call after one request: nothing is retried, resubmitted,
// started or checked on the caller's behalf. Each failure reports the same
// CallError whatever the operation, DownloadJob included: a download answered
// as application/json is read as an envelope, like every JSON answer.
func TestCallCountAfterFailures(t *testing.T) {
	ops := append(jbPaidOps(), jbFreeOps()...)
	for _, op := range ops {
		for _, f := range jbCallFailures() {
			t.Run(op.name+" "+f.name, func(t *testing.T) {
				c, s := newScriptClient(t, append([]testutil.Step{f.step}, jbFollowUps()...)...)
				err := op.call(t.Context(), c)
				assertCallError(t, err, f.wantFor(op.op, op.jobID, op.page), jbFailureSecrets()...)
				jbAssertOneAttempt(t, s, op.path)
			})
		}
	}
}

// TestCallCountAfterTimeout lets each operation's own timeout expire while
// the provider holds the request: the call reports a timeout after one
// request, and an hour later still nothing else was sent.
func TestCallCountAfterTimeout(t *testing.T) {
	for _, op := range append(jbPaidOps(), jbFreeOps()...) {
		t.Run(op.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testutil.NewScript(append([]testutil.Step{{Respond: testutil.BlockUntilDone}}, jbFollowUps()...)...)
				c := newTransportClient(t, s, neverbounce.WithTimeout(20*time.Second),
					neverbounce.WithDownloadTimeout(20*time.Second))
				start := time.Now()
				err := op.call(t.Context(), c)
				assert.Equal(t, 20*time.Second, time.Since(start))
				assertCallError(t, err, op.want(wantCall{Kind: neverbounce.FailureTimeout}))

				<-time.After(time.Hour)
				synctest.Wait()
				jbAssertOneAttempt(t, s, op.path)
			})
		})
	}
}

// TestCallCountAfterCancel cancels the caller's context while the provider
// holds the request.
func TestCallCountAfterCancel(t *testing.T) {
	for _, op := range append(jbPaidOps(), jbFreeOps()...) {
		t.Run(op.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				hold := func(r *http.Request) (*http.Response, error) {
					cancel()
					return testutil.BlockUntilDone(r)
				}
				c, s := newScriptClient(t, append([]testutil.Step{{Respond: hold}}, jbFollowUps()...)...)
				err := op.call(ctx, c)
				assertCallError(t, err, op.want(wantCall{Kind: neverbounce.FailureCanceled}))

				<-time.After(time.Hour)
				synctest.Wait()
				jbAssertOneAttempt(t, s, op.path)
			})
		})
	}
}

// TestCallCountOnSuccess checks the success path too: one request each.
func TestCallCountOnSuccess(t *testing.T) {
	for _, op := range append(jbPaidOps(), jbFreeOps()...) {
		t.Run(op.name, func(t *testing.T) {
			c, s := newScriptClient(t, append([]testutil.Step{op.ok}, jbFollowUps()...)...)
			require.NoError(t, op.call(t.Context(), c))
			jbAssertOneAttempt(t, s, op.path)
		})
	}
}
