package neverbounce

import "context"

// ParseJobRequest is one jobs/parse call.
type ParseJobRequest struct {
	// JobID is the job to parse, created without AutoParse.
	JobID JobID

	// AutoStart starts the full verification right after parsing, which
	// DEDUCTS CREDITS for the provider's billable count. It is always sent,
	// false included.
	AutoStart bool
}

// ParseJobResult is the provider's answer to jobs/parse.
type ParseJobResult struct {
	// QueueID is the provider's queue_id, kept as opaque metadata when it is a
	// string of 1 to 128 visible ASCII bytes, and "" otherwise.
	QueueID string `json:"queue_id,omitempty"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// StartJobRequest is one jobs/start call.
type StartJobRequest struct {
	// JobID is the parsed job to start. After a sample reaches
	// JobStateWaitingAnalyzed, the full start uses the same JobID.
	JobID JobID

	// RunSample runs a free sample verification instead. When false, which is
	// always sent explicitly, this is a FULL start: it deducts credits and the
	// job cannot be stopped or restarted.
	RunSample bool
}

// StartJobResult is the provider's answer to jobs/start.
type StartJobResult struct {
	// QueueID is the provider's queue_id, kept as opaque metadata when it is a
	// string of 1 to 128 visible ASCII bytes, and "" otherwise.
	QueueID string `json:"queue_id,omitempty"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// Members of a jobs/parse or jobs/start answer after the envelope.
const controlQueueID = envFirst

// ParseJob parses a job created without AutoParse: POST
// https://api.neverbounce.com/v4.2/jobs/parse.
//
// STATE-CHANGING, and PAID when AutoStart is true. A list cannot be parsed
// twice. It makes exactly one attempt and never retries; after an ambiguous
// failure, read JobStatus before calling it again.
//
// Errors are a *ValidationError (nothing sent) or a *CallError whose JobID is
// req.JobID.
func (c *Client) ParseJob(ctx context.Context, req ParseJobRequest) (*ParseJobResult, error) {
	queue, exec, err := c.control(ctx, OpParseJob, req.JobID, "auto_start", req.AutoStart)
	if err != nil {
		return nil, err
	}
	return &ParseJobResult{QueueID: queue, ExecutionTime: exec}, nil
}

// StartJob starts a parsed job: POST https://api.neverbounce.com/v4.2/jobs/start.
//
// PAID AND IRREVERSIBLE unless RunSample is true: a full start deducts credits
// for the provider's billable count (see JobStatus Totals.Billable), and the
// job cannot be stopped or restarted. A sample start is free. It makes exactly
// one attempt and never retries. After any error, a timeout included, the job
// may already be queued and billed: read JobStatus before ever starting it
// again.
//
// Errors are a *ValidationError (nothing sent) or a *CallError whose JobID is
// req.JobID.
func (c *Client) StartJob(ctx context.Context, req StartJobRequest) (*StartJobResult, error) {
	queue, exec, err := c.control(ctx, OpStartJob, req.JobID, "run_sample", req.RunSample)
	if err != nil {
		return nil, err
	}
	return &StartJobResult{QueueID: queue, ExecutionTime: exec}, nil
}

// control makes a jobs/parse or jobs/start call, whose request is the job ID
// and one explicit boolean, and whose answer is a queue ID.
func (c *Client) control(ctx context.Context, op Operation, id JobID, flag string, value bool) (string, *int64, error) {
	if err := c.prepare(ctx); err != nil {
		return "", nil, err
	}
	if err := checkJobID(id); err != nil {
		return "", nil, err
	}
	b, err := c.newRequestBody(64).integer("job_id", int64(id)).boolean(flag, value).finish(maxSmallRequestBytes)
	if err != nil {
		return "", nil, err
	}
	obj, cerr := c.call(ctx, op, b, envelopeNames("queue_id"))
	if cerr != nil {
		return "", nil, cerr.forJob(id, 0)
	}
	return queueID(obj.fields[controlQueueID]), executionTime(obj.fields[envExecutionTime]), nil
}
