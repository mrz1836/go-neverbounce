package neverbounce_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// hpJobID is the job every helper test watches or walks.
const hpJobID neverbounce.JobID = 42

// hpBoundaryRuns is how often a boundary case is repeated in fresh bubbles.
// A race between the poll timer and the MaxWait deadline would show up as a
// different outcome in some of the runs.
const hpBoundaryRuns = 64

// errHPTransport is a synthetic transport failure.
var errHPTransport = errors.New("hp synthetic transport failure")

// hpPollName is the filename the scripted status answer n carries, so a test
// can tell which answer WaitForJob returned.
func hpPollName(n int) string { return "hp-poll-" + strconv.Itoa(n) }

// hpStatusBody returns a jobs/status success body for hpJobID in state,
// numbered n, with extra members appended.
func hpStatusBody(n int, state, extra string) string {
	return `{"status":"success","id":42,"job_status":"` + state + `","filename":"` + hpPollName(n) + `"` +
		extra + `,"execution_time":3}`
}

// hpStatus returns the scripted status answer n for state.
func hpStatus(n int, state neverbounce.JobState) testutil.Step {
	return testutil.Step{Body: hpStatusBody(n, string(state), "")}
}

// hpStatuses returns count answers in state, numbered from first.
func hpStatuses(first, count int, state neverbounce.JobState) []testutil.Step {
	steps := make([]testutil.Step, 0, count)
	for i := range count {
		steps = append(steps, hpStatus(first+i, state))
	}
	return steps
}

// hpSteps concatenates step lists.
func hpSteps(lists ...[]testutil.Step) []testutil.Step {
	var out []testutil.Step
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// hpBlock is a scripted step that never answers: it waits for the request's
// context to end.
func hpBlock() testutil.Step { return testutil.Step{Respond: testutil.BlockUntilDone} }

// hpStallingBody returns a response body that, once read, blocks until it is
// closed, which the SDK does when the call's context ends. It must be made
// inside the bubble.
func hpStallingBody() *testutil.Body {
	b := testutil.NewBody("")
	b.Block = true
	return b
}

// hpStallAfterHeaders is a scripted step that answers 200 with a body that
// stalls until the call's context ends.
func hpStallAfterHeaders() testutil.Step {
	return testutil.Step{Respond: func(r *http.Request) (*http.Response, error) {
		return testutil.Response(r, http.StatusOK, hpStallingBody(), -1), nil
	}}
}

// hpEffective is the interval WaitForJob uses for interval.
func hpEffective(interval time.Duration) time.Duration {
	if interval == 0 {
		return neverbounce.DefaultWaitInterval
	}
	return interval
}

// hpRun is the outcome of one WaitForJob call in a bubble.
type hpRun struct {
	status  *neverbounce.JobStatus
	err     error
	elapsed time.Duration
	calls   int
}

// hpWait calls WaitForJob inside the current bubble and measures the fake
// clock's elapsed time. It then advances the clock by an hour and asserts that
// nothing was requested after the call returned, and that every request was a
// jobs/status poll for hpJobID.
func hpWait(ctx context.Context, t *testing.T, c *neverbounce.Client, s *testutil.Script,
	req neverbounce.WaitForJobRequest,
) hpRun {
	t.Helper()
	start := time.Now()
	st, err := c.WaitForJob(ctx, req)
	run := hpRun{status: st, err: err, elapsed: time.Since(start), calls: s.Calls()}
	<-time.After(time.Hour)
	assert.Equal(t, run.calls, s.Calls(), "no request after WaitForJob returned")
	hpAssertStatusPolls(t, s, keyMember+`,"job_id":42}`)
	return run
}

// hpAssertStatusPolls asserts that every request s recorded is a jobs/status
// poll that keeps the wire policy and carries exactly body.
func hpAssertStatusPolls(t *testing.T, s *testutil.Script, body string) {
	t.Helper()
	for _, r := range s.Requests() {
		assertWireRequest(t, r, pathStatus, mediaJSON)
		assertBody(t, body, r.Body)
	}
}

// hpAssertLast asserts that status is the scripted answer n, or nil for n 0.
func hpAssertLast(t *testing.T, status *neverbounce.JobStatus, n int) {
	t.Helper()
	if n == 0 {
		assert.Nil(t, status, "no status was read")
		return
	}
	require.NotNil(t, status)
	assert.Equal(t, hpJobID, status.ID)
	assert.Equal(t, hpPollName(n), status.Filename, "the last status read")
}

// hpAssertWaitStopped asserts that err is the *CallError of a WaitForJob for
// hpJobID that stopped with kind, no HTTP response being at fault.
func hpAssertWaitStopped(t *testing.T, err error, kind neverbounce.FailureKind) {
	t.Helper()
	assertCallError(t, err, wantCall{Kind: kind, Op: neverbounce.OpJobStatus, JobID: hpJobID})
}

// hpCancelAfter returns a context factory whose context is canceled d after
// it is made. It must be called inside the bubble.
func hpCancelAfter(d time.Duration) func(context.Context) (context.Context, context.CancelFunc) {
	return func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		time.AfterFunc(d, cancel)
		return ctx, cancel
	}
}

// hpCanceled is a context factory whose context is already canceled.
func hpCanceled(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	return ctx, cancel
}

// hpDeadline returns a context factory whose context expires d after it is
// made (already, when d <= 0).
func hpDeadline(d time.Duration) func(context.Context) (context.Context, context.CancelFunc) {
	return func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, d)
	}
}

func TestWaitForJobDefaultInterval(t *testing.T) {
	assert.Equal(t, 10*time.Second, neverbounce.DefaultWaitInterval)
}

func TestWaitForJobReturnsAtOnceForSettledStates(t *testing.T) {
	tests := []struct {
		state       neverbounce.JobState
		needsAction bool
	}{
		{neverbounce.JobStateComplete, false},
		{neverbounce.JobStateWaiting, true},
		{neverbounce.JobStateWaitingAnalyzed, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, hpStatus(1, tt.state), hpStatus(2, neverbounce.JobStateComplete))
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{JobID: hpJobID, MaxWait: time.Hour})
				require.NoError(t, run.err)
				assert.Equal(t, 1, run.calls, "exactly one poll")
				assert.Zero(t, run.elapsed, "no pause before returning")
				hpAssertLast(t, run.status, 1)
				assert.Equal(t, tt.state, run.status.State)
				assert.Equal(t, tt.needsAction, run.status.State.NeedsAction())
				require.NotNil(t, run.status.ExecutionTime)
				assert.Equal(t, int64(3), *run.status.ExecutionTime)
			})
		})
	}
}

func TestWaitForJobPollsThroughEachInProgressState(t *testing.T) {
	states := []neverbounce.JobState{
		neverbounce.JobStateUploading, neverbounce.JobStateParsing, neverbounce.JobStateQueued,
		neverbounce.JobStateRunning, neverbounce.JobStateUnderReview,
	}
	intervals := []time.Duration{0, time.Second, 7 * time.Second, 10 * time.Minute}
	for _, state := range states {
		for _, interval := range intervals {
			t.Run(string(state)+"/interval "+interval.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c, s := newScriptClient(t, hpSteps(
						hpStatuses(1, 3, state),
						[]testutil.Step{hpStatus(4, neverbounce.JobStateComplete), hpStatus(5, neverbounce.JobStateComplete)},
					)...)
					run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
						JobID: hpJobID, Interval: interval, MaxWait: 72 * time.Hour,
					})
					require.NoError(t, run.err)
					assert.Equal(t, 4, run.calls, "three in-progress polls, then complete")
					assert.Equal(t, 3*hpEffective(interval), run.elapsed, "exactly three intervals")
					hpAssertLast(t, run.status, 4)
					assert.Equal(t, neverbounce.JobStateComplete, run.status.State)
				})
			})
		}
	}
}

func TestWaitForJobKeepsPollingUntilSettled(t *testing.T) {
	tests := []struct {
		name      string
		steps     []testutil.Step
		wantState neverbounce.JobState
	}{
		{
			name: "every in-progress state in turn",
			steps: []testutil.Step{
				hpStatus(1, neverbounce.JobStateUploading), hpStatus(2, neverbounce.JobStateParsing),
				hpStatus(3, neverbounce.JobStateQueued), hpStatus(4, neverbounce.JobStateRunning),
				hpStatus(5, neverbounce.JobStateUnderReview), hpStatus(6, neverbounce.JobStateWaitingAnalyzed),
			},
			wantState: neverbounce.JobStateWaitingAnalyzed,
		},
		{
			name: "percent_complete 100 is not completion",
			steps: []testutil.Step{
				{Body: hpStatusBody(1, "running", `,"percent_complete":100`)},
				{Body: hpStatusBody(2, "running", `,"percent_complete":100.0`)},
				{Body: hpStatusBody(3, "running", `,"percent_complete":100,"finished_at":"2026-10-15 14:52:27"`)},
				{Body: hpStatusBody(4, "running", `,"percent_complete":1e2`)},
				{Body: hpStatusBody(5, "parsing", `,"percent_complete":100`)},
				hpStatus(6, neverbounce.JobStateComplete),
			},
			wantState: neverbounce.JobStateComplete,
		},
		{
			name: "back to waiting after parsing",
			steps: []testutil.Step{
				hpStatus(1, neverbounce.JobStateUploading), hpStatus(2, neverbounce.JobStateParsing),
				hpStatus(3, neverbounce.JobStateParsing), hpStatus(4, neverbounce.JobStateParsing),
				hpStatus(5, neverbounce.JobStateParsing), hpStatus(6, neverbounce.JobStateWaiting),
			},
			wantState: neverbounce.JobStateWaiting,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, append(tt.steps, hpStatus(99, neverbounce.JobStateComplete))...)
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: 5 * time.Second, MaxWait: time.Hour,
				})
				require.NoError(t, run.err)
				assert.Equal(t, len(tt.steps), run.calls)
				assert.Equal(t, time.Duration(len(tt.steps)-1)*5*time.Second, run.elapsed)
				hpAssertLast(t, run.status, len(tt.steps))
				assert.Equal(t, tt.wantState, run.status.State)
			})
		})
	}
}

func TestWaitForJobFailedJob(t *testing.T) {
	tests := []struct {
		name       string
		steps      []testutil.Step
		wantReason neverbounce.JobFailureReason
	}{
		{
			name:       "failed at once with a reason",
			steps:      []testutil.Step{{Body: hpStatusBody(1, "failed", `,"failure_reason":"file_corrupt"`)}},
			wantReason: neverbounce.JobFailureFileCorrupt,
		},
		{
			name:  "failed with a null reason",
			steps: []testutil.Step{{Body: hpStatusBody(1, "failed", `,"failure_reason":null`)}},
		},
		{
			name:       "failed with an undocumented reason",
			steps:      []testutil.Step{{Body: hpStatusBody(1, "failed", `,"failure_reason":"quota_reached"`)}},
			wantReason: "quota_reached",
		},
		{
			name: "failed after parsing",
			steps: []testutil.Step{
				hpStatus(1, neverbounce.JobStateParsing), hpStatus(2, neverbounce.JobStateParsing),
				{Body: hpStatusBody(3, "failed", `,"failure_reason":"empty_file"`)},
			},
			wantReason: neverbounce.JobFailureEmptyFile,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, append(tt.steps, hpStatus(99, neverbounce.JobStateComplete))...)
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: 10 * time.Second, MaxWait: time.Hour,
				})
				hpAssertWaitStopped(t, run.err, neverbounce.FailureJobFailed)
				assert.Equal(t, len(tt.steps), run.calls, "no poll after failed")
				assert.Equal(t, time.Duration(len(tt.steps)-1)*10*time.Second, run.elapsed)
				hpAssertLast(t, run.status, len(tt.steps))
				assert.Equal(t, neverbounce.JobStateFailed, run.status.State)
				assert.Equal(t, tt.wantReason, run.status.FailureReason)
			})
		})
	}
}

func TestWaitForJobUnknownState(t *testing.T) {
	tests := []struct {
		name  string
		steps []testutil.Step
	}{
		{"archived at once", []testutil.Step{{Body: hpStatusBody(1, "archived", "")}}},
		{
			"archived after running",
			[]testutil.Step{hpStatus(1, neverbounce.JobStateRunning), {Body: hpStatusBody(2, "archived", "")}},
		},
		{"v5 spelling", []testutil.Step{{Body: hpStatusBody(1, "in_progress_v5", "")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, append(tt.steps, hpStatus(99, neverbounce.JobStateComplete))...)
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: 10 * time.Second, MaxWait: time.Hour,
				})
				hpAssertWaitStopped(t, run.err, neverbounce.FailureUnknownJobState)
				assert.Equal(t, len(tt.steps), run.calls, "an undocumented state is surfaced, not polled through")
				hpAssertLast(t, run.status, len(tt.steps))
				assert.False(t, run.status.State.Known())
			})
		})
	}
}

// TestWaitForJobBudgetPollCount checks the poll count for budgets that are
// not a whole number of intervals: floor(MaxWait/Interval)+1 polls, one at
// the start and one after each whole interval, then budget_exhausted with the
// last status as soon as the next poll could not start within MaxWait.
func TestWaitForJobBudgetPollCount(t *testing.T) {
	tests := []struct {
		maxWait  time.Duration
		interval time.Duration
	}{
		{25 * time.Second, 10 * time.Second},
		{9 * time.Second, 10 * time.Second},
		{time.Nanosecond, time.Second},
		{35 * time.Second, 0},
		{59500 * time.Millisecond, time.Second},
		{time.Hour, 7 * time.Minute},
		{10*time.Minute + time.Nanosecond, 10 * time.Minute},
		{72 * time.Hour, 9*time.Minute + 59*time.Second},
	}
	for _, tt := range tests {
		eff := hpEffective(tt.interval)
		want := int(tt.maxWait/eff) + 1
		t.Run("MaxWait "+tt.maxWait.String()+" interval "+tt.interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, hpStatuses(1, want+3, neverbounce.JobStateRunning)...)
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: tt.interval, MaxWait: tt.maxWait,
				})
				hpAssertWaitStopped(t, run.err, neverbounce.FailureBudgetExhausted)
				assert.Equal(t, want, run.calls, "floor(MaxWait/Interval)+1 polls")
				assert.Equal(t, time.Duration(want-1)*eff, run.elapsed, "returns as soon as no poll fits")
				assert.LessOrEqual(t, run.elapsed, tt.maxWait)
				hpAssertLast(t, run.status, want)
			})
		})
	}
}

// TestWaitForJobBudgetBoundaryIsDeterministic covers a MaxWait that is a whole
// number of intervals, so that the next poll would start exactly when MaxWait
// ends. Such a poll cannot complete within the budget (a real call takes time,
// and it would run on a context that is expiring), so it must never start:
// floor(MaxWait/Interval) polls, the same in every run.
func TestWaitForJobBudgetBoundaryIsDeterministic(t *testing.T) {
	tests := []struct {
		maxWait  time.Duration
		interval time.Duration
	}{
		{30 * time.Second, 10 * time.Second},
		{30 * time.Second, 0},
		{time.Second, time.Second},
		{20 * time.Minute, 10 * time.Minute},
	}
	for _, tt := range tests {
		eff := hpEffective(tt.interval)
		want := int(tt.maxWait / eff)
		t.Run("MaxWait "+tt.maxWait.String()+" interval "+tt.interval.String(), func(t *testing.T) {
			outcomes := map[int]int{}
			for range hpBoundaryRuns {
				synctest.Test(t, func(t *testing.T) {
					c, s := newScriptClient(t, hpStatuses(1, want+3, neverbounce.JobStateRunning)...)
					run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
						JobID: hpJobID, Interval: tt.interval, MaxWait: tt.maxWait,
					})
					hpAssertWaitStopped(t, run.err, neverbounce.FailureBudgetExhausted)
					assert.LessOrEqual(t, run.elapsed, tt.maxWait)
					outcomes[run.calls]++
					if run.status != nil {
						assert.Equal(t, hpPollName(run.calls), run.status.Filename, "the last status read")
					}
				})
			}
			assert.Equal(t, map[int]int{want: hpBoundaryRuns}, outcomes,
				"poll count -> runs: no poll may start when MaxWait ends")
		})
	}
}

func TestWaitForJobBudgetCutsOffAPollInFlight(t *testing.T) {
	tests := []struct {
		name     string
		steps    []testutil.Step
		wantLast int
	}{
		{"first poll never answers", []testutil.Step{hpBlock()}, 0},
		{"first poll body stalls", []testutil.Step{hpStallAfterHeaders()}, 0},
		{
			"third poll never answers",
			hpSteps(hpStatuses(1, 2, neverbounce.JobStateRunning), []testutil.Step{hpBlock()}), 2,
		},
		{
			"third poll body stalls",
			hpSteps(hpStatuses(1, 2, neverbounce.JobStateQueued), []testutil.Step{hpStallAfterHeaders()}), 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, append(tt.steps, hpStatus(99, neverbounce.JobStateComplete))...)
				run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: 10 * time.Second, MaxWait: 25 * time.Second,
				})
				hpAssertWaitStopped(t, run.err, neverbounce.FailureBudgetExhausted)
				require.NotErrorIs(t, run.err, context.DeadlineExceeded, "MaxWait is not the caller's deadline")
				assert.Equal(t, len(tt.steps), run.calls)
				assert.Equal(t, 25*time.Second, run.elapsed, "cut off exactly when MaxWait ends")
				hpAssertLast(t, run.status, tt.wantLast)
			})
		})
	}
}

func TestWaitForJobCallerContext(t *testing.T) {
	running := hpStatuses(1, 6, neverbounce.JobStateRunning)
	tests := []struct {
		name        string
		ctx         func(context.Context) (context.Context, context.CancelFunc)
		steps       []testutil.Step
		wantKind    neverbounce.FailureKind
		wantStatus  int
		wantCalls   int
		wantLast    int
		wantElapsed time.Duration
	}{
		{
			name: "canceled while sleeping", ctx: hpCancelAfter(15 * time.Second), steps: running,
			wantKind: neverbounce.FailureCanceled, wantCalls: 2, wantLast: 2, wantElapsed: 15 * time.Second,
		},
		{
			name: "canceled during a call", ctx: hpCancelAfter(15 * time.Second),
			steps:    hpSteps(hpStatuses(1, 1, neverbounce.JobStateRunning), []testutil.Step{hpBlock()}),
			wantKind: neverbounce.FailureCanceled, wantCalls: 2, wantLast: 1, wantElapsed: 15 * time.Second,
		},
		{
			name: "canceled while the body stalls", ctx: hpCancelAfter(3 * time.Second),
			steps:    []testutil.Step{hpStallAfterHeaders()},
			wantKind: neverbounce.FailureCanceled, wantStatus: http.StatusOK, wantCalls: 1, wantElapsed: 3 * time.Second,
		},
		{
			name: "deadline while the body stalls", ctx: hpDeadline(12 * time.Second),
			steps:    hpSteps(hpStatuses(1, 1, neverbounce.JobStateRunning), []testutil.Step{hpStallAfterHeaders()}),
			wantKind: neverbounce.FailureDeadlineExceeded, wantStatus: http.StatusOK, wantCalls: 2, wantLast: 1,
			wantElapsed: 12 * time.Second,
		},
		{
			name: "canceled before the first poll", ctx: hpCanceled, steps: running,
			wantKind: neverbounce.FailureCanceled,
		},
		{
			name: "deadline while sleeping", ctx: hpDeadline(25 * time.Second), steps: running,
			wantKind: neverbounce.FailureDeadlineExceeded, wantCalls: 3, wantLast: 3, wantElapsed: 25 * time.Second,
		},
		{
			name: "deadline during a call", ctx: hpDeadline(15 * time.Second),
			steps:    hpSteps(hpStatuses(1, 1, neverbounce.JobStateRunning), []testutil.Step{hpBlock()}),
			wantKind: neverbounce.FailureDeadlineExceeded, wantCalls: 2, wantLast: 1, wantElapsed: 15 * time.Second,
		},
		{
			name: "deadline during the first call", ctx: hpDeadline(5 * time.Second), steps: []testutil.Step{hpBlock()},
			wantKind: neverbounce.FailureDeadlineExceeded, wantCalls: 1, wantElapsed: 5 * time.Second,
		},
		{
			name: "deadline already passed", ctx: hpDeadline(-time.Second), steps: running,
			wantKind: neverbounce.FailureDeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, tt.steps...)
				ctx, cancel := tt.ctx(t.Context())
				defer cancel()
				run := hpWait(ctx, t, c, s, neverbounce.WaitForJobRequest{
					JobID: hpJobID, Interval: 10 * time.Second, MaxWait: time.Hour,
				})
				assertCallError(t, run.err, wantCall{
					Kind: tt.wantKind, Op: neverbounce.OpJobStatus, Status: tt.wantStatus, JobID: hpJobID,
				})
				require.NotErrorIs(t, run.err, neverbounce.ErrBudgetExhausted)
				require.NotErrorIs(t, run.err, neverbounce.ErrTimeout, "the caller's deadline is not the SDK's timeout")
				assert.Equal(t, tt.wantCalls, run.calls)
				assert.Equal(t, tt.wantElapsed, run.elapsed)
				hpAssertLast(t, run.status, tt.wantLast)
			})
		})
	}
}

// hpSameCallError asserts that got is the very error want describes: equal
// exported fields, text and cause.
func hpSameCallError(t *testing.T, want, got error) {
	t.Helper()
	wantCE, gotCE := callError(t, want), callError(t, got)
	assert.Equal(t, wantCE.GoString(), gotCE.GoString())
	assert.Equal(t, want.Error(), got.Error())
	assert.Equal(t, errors.Unwrap(want), errors.Unwrap(got), "the same fixed cause")
	assert.Equal(t, wantCE.Diagnostics(), gotCE.Diagnostics())
}

func TestWaitForJobStopsAtAFailingPoll(t *testing.T) {
	failures := []struct {
		name      string
		step      testutil.Step
		kind      neverbounce.FailureKind
		status    int
		apiStatus neverbounce.APIStatus
		field     string
	}{
		{
			name: "throttle_triggered envelope", kind: neverbounce.FailureRateLimited, status: http.StatusOK,
			apiStatus: neverbounce.APIStatusThrottleTriggered,
			step:      testutil.Step{Body: `{"status":"throttle_triggered","message":"slow down"}`},
		},
		{
			name: "general_failure envelope", kind: neverbounce.FailureRejected, status: http.StatusOK,
			apiStatus: neverbounce.APIStatusGeneralFailure,
			step:      testutil.Step{Body: `{"status":"general_failure"}`},
		},
		{
			name: "HTTP 429", kind: neverbounce.FailureRateLimited, status: http.StatusTooManyRequests,
			apiStatus: neverbounce.APIStatusThrottleTriggered,
			step:      testutil.Step{Status: http.StatusTooManyRequests, Body: `{"status":"throttle_triggered"}`},
		},
		{
			name: "HTTP 502", kind: neverbounce.FailureUpstream, status: http.StatusBadGateway,
			step: testutil.Step{Status: http.StatusBadGateway, Body: `<html>bad gateway</html>`},
		},
		{
			name: "HTTP 503", kind: neverbounce.FailureUnavailable, status: http.StatusServiceUnavailable,
			step: testutil.Step{Status: http.StatusServiceUnavailable},
		},
		{
			name: "malformed JSON", kind: neverbounce.FailureMalformedResponse, status: http.StatusOK,
			step: testutil.Step{Body: `{"status":"success","id":42,"job_status":"running"`},
		},
		{
			name: "missing job_status", kind: neverbounce.FailureMalformedResponse, status: http.StatusOK,
			field: "job_status", step: testutil.Step{Body: `{"status":"success","id":42}`},
		},
		{
			name: "another job's status", kind: neverbounce.FailureUnexpectedResponse, status: http.StatusOK,
			field: "id", step: testutil.Step{Body: `{"status":"success","id":43,"job_status":"complete"}`},
		},
		{
			name: "invalid job_status token", kind: neverbounce.FailureUnexpectedResponse, status: http.StatusOK,
			field: "job_status", step: testutil.Step{Body: `{"status":"success","id":42,"job_status":"Running"}`},
		},
		{name: "transport failure", kind: neverbounce.FailureTransport, step: testutil.Step{Err: errHPTransport}},
	}
	for _, f := range failures {
		for _, before := range []int{0, 2} {
			t.Run(f.name+"/after "+strconv.Itoa(before)+" polls", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					direct, _ := newScriptClient(t, f.step)
					_, want := direct.JobStatus(t.Context(), neverbounce.JobStatusRequest{JobID: hpJobID})
					require.Error(t, want)

					c, s := newScriptClient(t, hpSteps(
						hpStatuses(1, before, neverbounce.JobStateRunning),
						[]testutil.Step{f.step},
						hpStatuses(before+2, 3, neverbounce.JobStateRunning),
					)...)
					run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
						JobID: hpJobID, Interval: 10 * time.Second, MaxWait: time.Hour,
					})
					assertCallError(t, run.err, wantCall{
						Kind: f.kind, Op: neverbounce.OpJobStatus, Status: f.status, API: f.apiStatus, JobID: hpJobID,
						Field: f.field,
					})
					hpSameCallError(t, want, run.err)
					assert.Equal(t, before+1, run.calls, "no retry after a failing poll")
					assert.Equal(t, time.Duration(before)*10*time.Second, run.elapsed)
					hpAssertLast(t, run.status, before)
				})
			})
		}
	}
}

func TestWaitForJobStopsAtTheSDKTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testutil.NewScript(hpStatus(1, neverbounce.JobStateRunning), hpBlock(), hpStatus(3, neverbounce.JobStateComplete))
		c := newTransportClient(t, s, neverbounce.WithTimeout(5*time.Second))
		run := hpWait(t.Context(), t, c, s, neverbounce.WaitForJobRequest{
			JobID: hpJobID, Interval: 10 * time.Second, MaxWait: time.Hour,
		})
		hpAssertWaitStopped(t, run.err, neverbounce.FailureTimeout)
		assert.Equal(t, 2, run.calls, "no retry after a timeout")
		assert.Equal(t, 15*time.Second, run.elapsed, "one interval, then the SDK's 5s timeout")
		hpAssertLast(t, run.status, 1)
	})
}

// TestWaitForJobInvalidSendsNothing checks WaitForJob's own rules and their
// order. JobID and the checks every method shares are in client_test.go.
func TestWaitForJobInvalidSendsNothing(t *testing.T) {
	c, s := newScriptClient(t, hpStatus(1, neverbounce.JobStateComplete))
	with := func(edit func(*neverbounce.WaitForJobRequest)) neverbounce.WaitForJobRequest {
		r := neverbounce.WaitForJobRequest{JobID: hpJobID, MaxWait: time.Minute}
		edit(&r)
		return r
	}
	tests := []struct {
		name   string
		req    neverbounce.WaitForJobRequest
		field  string
		reason string
	}{
		{
			"interval 999ms", with(func(r *neverbounce.WaitForJobRequest) { r.Interval = 999 * time.Millisecond }),
			"Interval", reasonOutOfRange,
		},
		{"interval 1ns", with(func(r *neverbounce.WaitForJobRequest) { r.Interval = 1 }), "Interval", reasonOutOfRange},
		{
			"interval 10m+1ns", with(func(r *neverbounce.WaitForJobRequest) { r.Interval = 10*time.Minute + 1 }),
			"Interval", reasonOutOfRange,
		},
		{
			"interval negative", with(func(r *neverbounce.WaitForJobRequest) { r.Interval = -time.Second }),
			"Interval", reasonOutOfRange,
		},
		{"interval -1ns", with(func(r *neverbounce.WaitForJobRequest) { r.Interval = -1 }), "Interval", reasonOutOfRange},
		{"max wait zero", with(func(r *neverbounce.WaitForJobRequest) { r.MaxWait = 0 }), "MaxWait", reasonRequired},
		{"max wait negative", with(func(r *neverbounce.WaitForJobRequest) { r.MaxWait = -1 }), "MaxWait", reasonOutOfRange},
		{
			"max wait 72h+1ns", with(func(r *neverbounce.WaitForJobRequest) { r.MaxWait = 72*time.Hour + 1 }),
			"MaxWait", reasonOutOfRange,
		},
		{"job id before interval", neverbounce.WaitForJobRequest{Interval: -1, MaxWait: -1}, "JobID", reasonRequired},
		{
			"interval before max wait",
			neverbounce.WaitForJobRequest{JobID: hpJobID, Interval: -1},
			"Interval", reasonOutOfRange,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := c.WaitForJob(t.Context(), tt.req)
			assert.Nil(t, st)
			assertValidation(t, err, tt.field, tt.reason)
			assert.Equal(t, "neverbounce: invalid request: "+tt.field+": "+tt.reason, err.Error())
			_, isCallErr := errors.AsType[*neverbounce.CallError](err)
			assert.False(t, isCallErr, "nothing was attempted")
		})
	}
	assert.Zero(t, s.Calls(), "validation sends nothing")
}

func TestWaitForJobAcceptsBoundaryValues(t *testing.T) {
	tests := []struct {
		name        string
		req         neverbounce.WaitForJobRequest
		steps       []testutil.Step
		wantElapsed time.Duration
	}{
		{
			name:        "interval 1s",
			req:         neverbounce.WaitForJobRequest{JobID: hpJobID, Interval: time.Second, MaxWait: time.Minute},
			steps:       []testutil.Step{hpStatus(1, neverbounce.JobStateRunning), hpStatus(2, neverbounce.JobStateComplete)},
			wantElapsed: time.Second,
		},
		{
			name:        "interval 10m",
			req:         neverbounce.WaitForJobRequest{JobID: hpJobID, Interval: 10 * time.Minute, MaxWait: time.Hour},
			steps:       []testutil.Step{hpStatus(1, neverbounce.JobStateRunning), hpStatus(2, neverbounce.JobStateComplete)},
			wantElapsed: 10 * time.Minute,
		},
		{
			name:        "max wait 72h",
			req:         neverbounce.WaitForJobRequest{JobID: hpJobID, Interval: 10 * time.Minute, MaxWait: 72 * time.Hour},
			steps:       []testutil.Step{hpStatus(1, neverbounce.JobStateUnderReview), hpStatus(2, neverbounce.JobStateComplete)},
			wantElapsed: 10 * time.Minute,
		},
		{
			name:  "max wait 1ns",
			req:   neverbounce.WaitForJobRequest{JobID: hpJobID, MaxWait: 1},
			steps: []testutil.Step{hpStatus(1, neverbounce.JobStateComplete)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := newScriptClient(t, tt.steps...)
				run := hpWait(t.Context(), t, c, s, tt.req)
				require.NoError(t, run.err)
				assert.Equal(t, len(tt.steps), run.calls)
				assert.Equal(t, tt.wantElapsed, run.elapsed)
				hpAssertLast(t, run.status, len(tt.steps))
			})
		})
	}
}

func TestWaitForJobOnlyPollsJobStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = 9007199254740993 // above 2^53: exact only as an integer
		answer := func(state string) testutil.Step {
			return testutil.Step{Body: `{"status":"success","id":` + strconv.FormatInt(id, 10) + `,"job_status":"` + state + `"}`}
		}
		c, s := newScriptClient(t, answer("uploading"), answer("parsing"), answer("running"), answer("complete"))
		st, err := c.WaitForJob(t.Context(), neverbounce.WaitForJobRequest{
			JobID: id, Interval: 2 * time.Second, MaxWait: time.Minute,
		})
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobID(id), st.ID)
		assert.Equal(t, neverbounce.JobStateComplete, st.State)
		require.Equal(t, 4, s.Calls())
		hpAssertStatusPolls(t, s, keyMember+`,"job_id":9007199254740993}`)
	})
}

// TestWaitForJobKeepsAnAnswerThatArrivesAtMaxWait proves that only the
// deadline failure MaxWait itself causes becomes budget_exhausted: an HTTP 401
// whose headers arrive while its body is still being read when MaxWait ends
// keeps its own kind, as JobStatus alone would report it.
func TestWaitForJobKeepsAnAnswerThatArrivesAtMaxWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			testutil.Drain(r)
			calls++
			if calls == 1 {
				return testutil.Response(r, http.StatusOK, io.NopCloser(strings.NewReader(
					`{"status":"success","id":42,"job_status":"running"}`)), -1), nil
			}
			// The 401's headers arrive at once; its body never ends, so the
			// excerpt read is still running when MaxWait ends.
			return testutil.Response(r, http.StatusUnauthorized, hpStallingBody(), -1), nil
		})
		c := newTransportClient(t, rt)
		st, err := c.WaitForJob(t.Context(), neverbounce.WaitForJobRequest{
			JobID: 42, Interval: time.Second, MaxWait: 5 * time.Second,
		})
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureUnauthorized, Op: neverbounce.OpJobStatus, Status: http.StatusUnauthorized,
			JobID: hpJobID,
		})
		require.NotErrorIs(t, err, neverbounce.ErrBudgetExhausted)
		require.NotNil(t, st)
		assert.Equal(t, neverbounce.JobStateRunning, st.State)
		assert.Equal(t, 2, calls)
	})
}
