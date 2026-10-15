package neverbounce

import (
	"context"
	"errors"
	"time"
)

// WaitForJob bounds.
const (
	minWaitInterval = time.Second
	maxWaitInterval = 10 * time.Minute
	maxWaitLimit    = 72 * time.Hour
)

// WaitForJobRequest is one WaitForJob call.
type WaitForJobRequest struct {
	// JobID is the job to watch.
	JobID JobID

	// Interval is the pause between the end of one poll and the start of the
	// next: 0 means DefaultWaitInterval, otherwise 1 second to 10 minutes.
	// The provider suggests polling every 5 to 10 seconds for lists under
	// 50,000 addresses, and less often for larger lists.
	Interval time.Duration

	// MaxWait is the required elapsed-time budget, more than 0 and at most 72
	// hours. Manual review can take a business day: size it accordingly.
	MaxWait time.Duration
}

// WaitForJob polls jobs/status for an existing job until it no longer needs
// the provider's attention, and returns the last status it read.
//
// Free and read-only: it only ever calls jobs/status, first immediately and
// then Interval after each poll ends, and it never creates, parses, starts,
// resubmits or deletes a job, nor infers completion from PercentComplete. It
// starts no background goroutine of its own. It returns:
//   - (status, nil) for JobStateComplete, JobStateWaiting (call StartJob) and
//     JobStateWaitingAnalyzed (a sample is done; a full StartJob is needed);
//   - (status, *CallError{Kind: job_failed}) for JobStateFailed, with the
//     provider's reason in status.FailureReason;
//   - (status, *CallError{Kind: unknown_job_state}) for an undocumented state,
//     which it surfaces rather than polls through;
//   - (last, *CallError{Kind: budget_exhausted}) when MaxWait ends: no poll
//     starts at or after it, and one still running then is cut off, so it
//     makes at most ceil(MaxWait/Interval) polls;
//   - (last, a canceled or deadline_exceeded *CallError) when ctx ends;
//   - (last, err) when a poll fails before MaxWait: it never retries, so
//     polling policy stays the caller's.
//
// last is the last status read, or nil when there was none. Canceling ctx
// stops the polling, never the job or its billing. Errors that are not a
// *ValidationError carry Op jobs/status and JobID req.JobID.
func (c *Client) WaitForJob(ctx context.Context, req WaitForJobRequest) (*JobStatus, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, err
	}
	interval, err := req.validate()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	waitCtx, cancel := context.WithDeadlineCause(ctx, start.Add(req.MaxWait), errWaitBudget)
	defer cancel()
	var last *JobStatus
	for {
		st, err := c.JobStatus(waitCtx, JobStatusRequest{JobID: req.JobID})
		if err != nil {
			return last, req.pollFailed(waitCtx, err)
		}
		last = st
		if done, err := req.settled(st.State); done {
			return st, err
		}
		if err := req.pause(waitCtx, start, interval); err != nil {
			return last, err
		}
	}
}

// settled reports whether the wait ends at state, and with which error.
func (r *WaitForJobRequest) settled(state JobState) (bool, error) {
	switch {
	case state == JobStateFailed:
		return true, r.stopped(FailureJobFailed, errJobFailed)
	case state.Terminal() || state.NeedsAction():
		return true, nil
	case !state.Known():
		return true, r.stopped(FailureUnknownJobState, errUnknownJobState)
	}
	return false, nil
}

// pollFailed returns the error for a poll that failed within waitCtx:
// budget_exhausted when MaxWait cut it off, and the poll's own error
// otherwise. Only the deadline failure MaxWait itself caused is relabeled: an
// answer that arrived as MaxWait ended, such as an HTTP 401, keeps its own
// kind, and so does the caller's own deadline.
func (r *WaitForJobRequest) pollFailed(waitCtx context.Context, err error) error {
	ce, ok := errors.AsType[*CallError](err)
	if ok && ce.Kind == FailureDeadlineExceeded && errors.Is(context.Cause(waitCtx), errWaitBudget) {
		return r.stopped(FailureBudgetExhausted, errWaitBudget)
	}
	return err
}

// pause waits interval before the next poll, or returns the error that ends
// the wait instead: budget_exhausted when the next poll could not start
// before MaxWait, counted from start, ends, or the caller's context failure.
// A poll due exactly when MaxWait ends is never started: its context expires
// at that instant.
func (r *WaitForJobRequest) pause(waitCtx context.Context, start time.Time, interval time.Duration) error {
	if time.Since(start)+interval >= r.MaxWait {
		return r.stopped(FailureBudgetExhausted, errWaitBudget)
	}
	select {
	case <-waitCtx.Done():
		if errors.Is(context.Cause(waitCtx), errWaitBudget) {
			return r.stopped(FailureBudgetExhausted, errWaitBudget)
		}
		return contextFailure(OpJobStatus, waitCtx.Err(), 0).forJob(r.JobID, 0)
	case <-time.After(interval):
		return nil
	}
}

// validate checks the request and returns the effective interval.
func (r *WaitForJobRequest) validate() (time.Duration, error) {
	if err := checkJobID(r.JobID); err != nil {
		return 0, err
	}
	interval := r.Interval
	switch {
	case interval == 0:
		interval = DefaultWaitInterval
	case interval < minWaitInterval || interval > maxWaitInterval:
		return 0, invalidRequest("Interval", reasonOutOfRange)
	}
	switch {
	case r.MaxWait == 0:
		return 0, invalidRequest("MaxWait", reasonRequired)
	case r.MaxWait < 0 || r.MaxWait > maxWaitLimit:
		return 0, invalidRequest("MaxWait", reasonOutOfRange)
	}
	return interval, nil
}

// stopped returns the CallError for a wait that ended without a failed call.
func (r *WaitForJobRequest) stopped(kind FailureKind, cause error) *CallError {
	return newCallError(OpJobStatus, kind, 0, cause).forJob(r.JobID, 0)
}
