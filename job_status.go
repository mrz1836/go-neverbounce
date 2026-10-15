package neverbounce

import "context"

// JobStatusRequest is one jobs/status call.
type JobStatusRequest struct {
	// JobID is the job to read.
	JobID JobID
}

// JobState is a job's "job_status". The set is open: a state the provider
// adds later is kept as sent when it has the token shape Result describes, and
// Known reports false for it (and InProgress, NeedsAction and Terminal all
// report false).
type JobState string

// Documented job states.
const (
	// JobStateUploading means the list is being uploaded.
	JobStateUploading JobState = "uploading"

	// JobStateParsing means the provider is reading the list for addresses and
	// duplicates.
	JobStateParsing JobState = "parsing"

	// JobStateWaiting means the job is parsed and waits to be started: call
	// StartJob (a full start is billed).
	JobStateWaiting JobState = "waiting"

	// JobStateWaitingAnalyzed means a sample has finished and the job waits
	// for further action: a full StartJob verifies it (billed).
	JobStateWaitingAnalyzed JobState = "waiting_analyzed"

	// JobStateQueued means the job waits for capacity, the provider's or the
	// account's concurrent-job limit.
	JobStateQueued JobState = "queued"

	// JobStateRunning means a sample or full verification is running.
	JobStateRunning JobState = "running"

	// JobStateUnderReview means the job is in the provider's manual review,
	// which may take up to one business day.
	JobStateUnderReview JobState = "under_review"

	// JobStateComplete means the results are ready.
	JobStateComplete JobState = "complete"

	// JobStateFailed means the job failed; see JobStatus.FailureReason.
	JobStateFailed JobState = "failed"
)

// Known reports whether s is one of the nine documented states.
func (s JobState) Known() bool { return s.InProgress() || s.NeedsAction() || s.Terminal() }

// InProgress reports whether the provider is still working on the job without
// any action from you: uploading, parsing, queued, running or under_review.
func (s JobState) InProgress() bool {
	switch s {
	case JobStateUploading, JobStateParsing, JobStateQueued, JobStateRunning, JobStateUnderReview:
		return true
	case JobStateWaiting, JobStateWaitingAnalyzed, JobStateComplete, JobStateFailed:
		return false
	}
	return false
}

// NeedsAction reports whether the job waits for you: waiting (a start is
// needed) or waiting_analyzed (a sample is done; a full start is needed).
func (s JobState) NeedsAction() bool { return s == JobStateWaiting || s == JobStateWaitingAnalyzed }

// Terminal reports whether the job has ended: complete or failed.
func (s JobState) Terminal() bool { return s == JobStateComplete || s == JobStateFailed }

// JobFailureReason is a failed job's "failure_reason". The set is open. ""
// means the provider sent null or nothing; on a failed job that means the
// provider gave no reason.
type JobFailureReason string

// Documented failure reasons.
const (
	JobFailureUnknownFileEncoding JobFailureReason = "unknown_file_encoding"
	JobFailureEmptyFile           JobFailureReason = "empty_file"
	JobFailureFileTooLarge        JobFailureReason = "file_too_large"
	JobFailureFileCorrupt         JobFailureReason = "file_corrupt"
)

// Known reports whether r is one of the four documented reasons.
func (r JobFailureReason) Known() bool {
	switch r {
	case JobFailureUnknownFileEncoding, JobFailureEmptyFile, JobFailureFileTooLarge, JobFailureFileCorrupt:
		return true
	}
	return false
}

// JobTotals is a job's "total" counters, which the provider updates
// periodically while the job runs. A nil field was not reported; a pointer to
// 0 was reported as zero.
type JobTotals struct {
	// Records is the number of rows the provider found in the list.
	Records *int64 `json:"records,omitempty"`

	// Billable is the number of credits a full verification of this job
	// consumes: the provider's count of unique, syntactically valid
	// addresses. The SDK never estimates or deduplicates on its own.
	Billable *int64 `json:"billable,omitempty"`

	// Processed is the number of addresses processed so far.
	Processed *int64 `json:"processed,omitempty"`

	// Valid, Invalid, Catchall, Disposable and Unknown count each result.
	Valid      *int64 `json:"valid,omitempty"`
	Invalid    *int64 `json:"invalid,omitempty"`
	Catchall   *int64 `json:"catchall,omitempty"`
	Disposable *int64 `json:"disposable,omitempty"`
	Unknown    *int64 `json:"unknown,omitempty"`

	// Duplicates counts every instance of a duplicated record, not unique
	// duplicates.
	Duplicates *int64 `json:"duplicates,omitempty"`

	// BadSyntax counts records with no data that looks like an address.
	BadSyntax *int64 `json:"bad_syntax,omitempty"`
}

// JobStatus is the provider's jobs/status answer. Only the v4.2 shape, with
// its nested "total", is modeled.
//
// It survives a JSON round trip through encoding/json unchanged, under the
// provider's snake_case names. Unmarshaling a raw provider body into it skips
// every check the SDK makes.
type JobStatus struct {
	// ID is the job's handle. The SDK checks that it is the job requested.
	ID JobID `json:"id"`

	// State is the job's job_status: what can be done with the job now.
	State JobState `json:"job_status"`

	// Filename is the job's name in the dashboard.
	Filename string `json:"filename,omitempty"`

	// CreatedAt, StartedAt and FinishedAt are the provider's timestamps as
	// text, such as "2026-10-15 14:52:27". Their time zone is undocumented, so
	// the SDK never parses them. "" means not reported.
	CreatedAt  string `json:"created_at,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	// FailureReason is the provider's reason for a failed job, or "".
	FailureReason JobFailureReason `json:"failure_reason,omitempty"`

	// Totals are the job's counters.
	Totals JobTotals `json:"total,omitzero"`

	// BounceEstimate is the provider's estimated bounce rate, from 0 to 100,
	// when reported.
	BounceEstimate *float64 `json:"bounce_estimate,omitempty"`

	// PercentComplete is the job's progress, from 0 to 100, when reported. It
	// is progress only: completion is State, never this value.
	PercentComplete *float64 `json:"percent_complete,omitempty"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// Members of a jobs/status answer after the envelope, by index into
// statusNames.
const (
	statusID = envFirst + iota
	statusJobStatus
	statusFilename
	statusCreatedAt
	statusStartedAt
	statusFinishedAt
	statusFailureReason
	statusTotal
	statusBounceEstimate
	statusPercentComplete
)

// statusNames returns the member names a jobs/status answer is read against.
func statusNames() []string {
	return envelopeNames("id", "job_status", "filename", "created_at", "started_at", "finished_at",
		"failure_reason", "total", "bounce_estimate", "percent_complete")
}

// JobStatus reads a job's status: POST https://api.neverbounce.com/v4.2/jobs/status.
//
// Free and read-only. It makes exactly one attempt and never retries. An
// answer for any job other than req.JobID fails as unexpected_response, so a
// caller, and WaitForJob, can never follow another job.
//
// Errors are a *ValidationError (nothing sent) or a *CallError whose JobID is
// req.JobID.
func (c *Client) JobStatus(ctx context.Context, req JobStatusRequest) (*JobStatus, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, err
	}
	if err := checkJobID(req.JobID); err != nil {
		return nil, err
	}
	b, err := c.newRequestBody(32).integer("job_id", int64(req.JobID)).finish(maxSmallRequestBytes)
	if err != nil {
		return nil, err
	}
	obj, cerr := c.call(ctx, OpJobStatus, b, statusNames())
	if cerr != nil {
		return nil, cerr.forJob(req.JobID, 0)
	}
	st, derr := decodeJobStatus(&obj, req.JobID)
	if derr != nil {
		return nil, derr.callError(OpJobStatus).forJob(req.JobID, 0)
	}
	return st, nil
}

// decodeJobStatus decodes a jobs/status success envelope for the job want.
func decodeJobStatus(o *object, want JobID) (*JobStatus, *decodeError) {
	id, derr := decodeJobID(o.fields[statusID], "id")
	if derr != nil {
		return nil, derr
	}
	if id != want {
		return nil, idMismatch("id")
	}
	state, derr := decodeToken(o.fields[statusJobStatus], "job_status", true)
	if derr != nil {
		return nil, derr
	}
	st := &JobStatus{ID: id, State: JobState(state), ExecutionTime: executionTime(o.fields[envExecutionTime])}
	texts := [...]struct {
		i    int
		name string
		dst  *string
	}{
		{statusFilename, "filename", &st.Filename},
		{statusCreatedAt, "created_at", &st.CreatedAt},
		{statusStartedAt, "started_at", &st.StartedAt},
		{statusFinishedAt, "finished_at", &st.FinishedAt},
	}
	for _, t := range texts {
		if *t.dst, derr = decodeText(o.fields[t.i], t.name); derr != nil {
			return nil, derr
		}
	}
	reason, derr := decodeToken(o.fields[statusFailureReason], "failure_reason", false)
	if derr != nil {
		return nil, derr
	}
	st.FailureReason = JobFailureReason(reason)
	if st.Totals, derr = decodeTotals(o.fields[statusTotal]); derr != nil {
		return nil, derr
	}
	if st.BounceEstimate, derr = decodePercent(o.fields[statusBounceEstimate], "bounce_estimate"); derr != nil {
		return nil, derr
	}
	if st.PercentComplete, derr = decodePercent(o.fields[statusPercentComplete], "percent_complete"); derr != nil {
		return nil, derr
	}
	return st, nil
}

// decodeTotals decodes the optional "total" object.
func decodeTotals(raw []byte) (JobTotals, *decodeError) {
	const field = "total"
	var t JobTotals
	switch {
	case isNull(raw):
		return t, nil
	case raw[0] != '{':
		return t, wrongType(field)
	}
	slots := [...]slot[*int64]{
		{"records", &t.Records},
		{"billable", &t.Billable},
		{"processed", &t.Processed},
		{"valid", &t.Valid},
		{"invalid", &t.Invalid},
		{"catchall", &t.Catchall},
		{"disposable", &t.Disposable},
		{"unknown", &t.Unknown},
		{"duplicates", &t.Duplicates},
		{"bad_syntax", &t.BadSyntax},
	}
	if i, derr := decodeSlots(raw, slots[:], decodeCount); derr != nil {
		derr.field = field + "." + slots[i].name
		return JobTotals{}, derr
	}
	return t, nil
}
