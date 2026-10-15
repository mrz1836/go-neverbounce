package neverbounce

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinel errors. Match them with errors.Is.
//
// ErrInvalidConfig and ErrInvalidRequest are local: an error matching either
// means no request was sent. Every other sentinel describes a call that was
// attempted, or a helper that stopped short; after an attempted call the
// provider may already have processed it, billed it or started a job.
var (
	// ErrInvalidConfig reports an invalid NewClient argument or option, or a
	// Client that was not created by NewClient. No request was sent.
	ErrInvalidConfig = errors.New("neverbounce: invalid configuration")

	// ErrInvalidRequest reports a request that fails local validation, or a nil
	// context. No request was sent.
	ErrInvalidRequest = errors.New("neverbounce: invalid request")

	// ErrUnauthorized reports a refused API key: the envelope status
	// auth_failure or bad_referrer, or HTTP 401 or 403.
	ErrUnauthorized = errors.New("neverbounce: unauthorized")

	// ErrRateLimited reports the envelope status throttle_triggered or HTTP
	// 429. The SDK never retries.
	ErrRateLimited = errors.New("neverbounce: rate limited")

	// ErrUnavailable reports the envelope status temp_unavail or HTTP 503: a
	// partial service interruption on the provider's side.
	ErrUnavailable = errors.New("neverbounce: temporarily unavailable")

	// ErrRejected reports a refused request: the envelope status
	// general_failure, or an HTTP 4xx status other than 401, 403 and 429 (413,
	// for a jobs/create body over the provider's limit, included).
	ErrRejected = errors.New("neverbounce: rejected by provider")

	// ErrUpstream reports an HTTP 5xx status other than 503.
	ErrUpstream = errors.New("neverbounce: provider server error")

	// ErrUnexpectedResponse reports a response this SDK cannot interpret: an
	// HTTP status that is not 200 and not 4xx or 5xx (a redirect, which is
	// never followed, included), a body that is not a JSON object, an
	// undocumented envelope status, a value that breaks the documented shape
	// (an invalid token, a job ID other than the one requested, a results page
	// with more rows than requested or that contradicts an earlier one), an
	// undecoded Content-Encoding, or a download that is not a file: a content
	// type other than CSV, a success envelope instead of the file, or a JSON
	// answer over 64 KiB. The provider may already have processed the request.
	ErrUnexpectedResponse = errors.New("neverbounce: unexpected response")

	// ErrMalformedResponse reports an HTTP 200 body that is not valid UTF-8
	// or valid JSON, has data after the top-level value, repeats a key, lacks
	// a required field, or holds a field of the wrong JSON type or out of
	// range. The provider may already have processed the request.
	ErrMalformedResponse = errors.New("neverbounce: malformed response")

	// ErrResponseTooLarge reports a response larger than a configured limit
	// (WithMaxResponseBytes, WithMaxDownloadBytes), than the fixed 1 MiB cap
	// on every JSON answer except jobs/results, or than an SDK safety bound:
	// 100,000 JSON objects and arrays or 1,000,000 values in one body, or one
	// field over its bound. The provider may already have processed the
	// request.
	ErrResponseTooLarge = errors.New("neverbounce: response too large")

	// ErrTransport reports a failure in transit, such as DNS, connection, TLS
	// or proxy errors, a reset, a truncated body, or a stalled body (100 reads
	// in a row that return nothing). The request may or may not have reached
	// the provider.
	ErrTransport = errors.New("neverbounce: transport failure")

	// ErrTimeout reports that the SDK's own timeout (WithTimeout,
	// WithDownloadTimeout) or an injected http.Client's Timeout expired, or
	// that the network reported a timeout. It also matches
	// context.DeadlineExceeded, so check it first to tell it apart from the
	// caller's own deadline. The provider may already have processed the
	// request.
	ErrTimeout = errors.New("neverbounce: timeout")

	// ErrWriteFailed reports that DownloadJob's writer returned an error or
	// wrote fewer bytes than it was given. The writer's own error is not
	// kept: wrap the writer to capture it.
	ErrWriteFailed = errors.New("neverbounce: download write failed")

	// ErrJobFailed reports that WaitForJob observed the job_status "failed".
	// The returned *JobStatus carries the provider's FailureReason.
	ErrJobFailed = errors.New("neverbounce: job failed")

	// ErrUnknownJobState reports that WaitForJob observed an undocumented
	// job_status. The SDK surfaces it rather than polling through it.
	ErrUnknownJobState = errors.New("neverbounce: unknown job state")

	// ErrBudgetExhausted reports that a helper used up its budget: WaitForJob's
	// MaxWait, or JobResultPages' MaxPages while more pages remain.
	ErrBudgetExhausted = errors.New("neverbounce: budget exhausted")
)

// Static causes kept inside a CallError. They replace standard-library
// errors, whose messages can quote URLs, header bytes or response content.
var (
	errBuildRequest     = errors.New("neverbounce: cannot build the HTTP request")
	errStatus           = errors.New("neverbounce: unexpected HTTP status")
	errResponseTooLarge = errors.New("neverbounce: response body exceeds the size limit")
	errContentEncoding  = errors.New("neverbounce: response has an undecoded content encoding")
	errInvalidUTF8      = errors.New("neverbounce: response is not valid UTF-8")
	errInvalidJSON      = errors.New("neverbounce: response is not valid JSON")
	errTrailingData     = errors.New("neverbounce: response has data after the top-level value")
	errDuplicateKey     = errors.New("neverbounce: response repeats an object key")
	errNotObject        = errors.New("neverbounce: response is not a JSON object")
	errTooManyValues    = errors.New("neverbounce: response has too many JSON values")
	errMissingField     = errors.New("neverbounce: response lacks a required field")
	errWrongType        = errors.New("neverbounce: response field has the wrong JSON type")
	errOutOfRange       = errors.New("neverbounce: response field is out of range")
	errInvalidToken     = errors.New("neverbounce: response field is not a valid token")
	errFieldTooLarge    = errors.New("neverbounce: response field exceeds its size bound")
	errAPIStatus        = errors.New("neverbounce: response envelope reports a failure status")
	errUnknownAPIStatus = errors.New("neverbounce: response envelope status is undocumented")
	errIDMismatch       = errors.New("neverbounce: response is for another job")
	errContradiction    = errors.New("neverbounce: results page contradicts the pagination")
	errContentType      = errors.New("neverbounce: download content type is not CSV")
	errJSONDownload     = errors.New("neverbounce: download answered with a JSON envelope")
	errWriter           = errors.New("neverbounce: download writer failed or wrote short")
	errJobFailed        = errors.New("neverbounce: job_status is failed")
	errUnknownJobState  = errors.New("neverbounce: job_status is undocumented")
	errWaitBudget       = errors.New("neverbounce: MaxWait used up before the job settled")
	errPageBudget       = errors.New("neverbounce: MaxPages used up before the last page")

	// Transport classes. The original error is discarded, because standard
	// library errors can quote URLs, proxy credentials or server bytes.
	errTransportDNS     = errors.New("neverbounce: transport: DNS lookup failed")
	errTransportProxy   = errors.New("neverbounce: transport: proxy connection failed")
	errTransportDial    = errors.New("neverbounce: transport: connection failed")
	errTransportTLS     = errors.New("neverbounce: transport: TLS failed")
	errTransportReset   = errors.New("neverbounce: transport: connection reset")
	errTransportClosed  = errors.New("neverbounce: transport: connection closed early")
	errTransportStalled = errors.New("neverbounce: transport: body read made no progress")
	errTransportOther   = errors.New("neverbounce: transport: request failed")
)

// FailureKind classifies a failed call. Each kind matches one target with
// errors.Is (a sentinel of this package, context.Canceled or
// context.DeadlineExceeded), except FailureTimeout, which matches both
// ErrTimeout and context.DeadlineExceeded. The values are short, fixed and
// free of personal data, so they are safe to use as metric labels.
type FailureKind string

// Failure kinds.
const (
	// FailureUnauthorized matches ErrUnauthorized.
	FailureUnauthorized FailureKind = "unauthorized"

	// FailureRateLimited matches ErrRateLimited.
	FailureRateLimited FailureKind = "rate_limited"

	// FailureUnavailable matches ErrUnavailable.
	FailureUnavailable FailureKind = "unavailable"

	// FailureRejected matches ErrRejected.
	FailureRejected FailureKind = "rejected"

	// FailureUpstream matches ErrUpstream.
	FailureUpstream FailureKind = "upstream"

	// FailureUnexpectedResponse matches ErrUnexpectedResponse.
	FailureUnexpectedResponse FailureKind = "unexpected_response"

	// FailureMalformedResponse matches ErrMalformedResponse.
	FailureMalformedResponse FailureKind = "malformed_response"

	// FailureResponseTooLarge matches ErrResponseTooLarge.
	FailureResponseTooLarge FailureKind = "response_too_large"

	// FailureTransport matches ErrTransport.
	FailureTransport FailureKind = "transport"

	// FailureTimeout matches ErrTimeout, and also context.DeadlineExceeded.
	FailureTimeout FailureKind = "timeout"

	// FailureCanceled means the caller's context was canceled. It matches
	// context.Canceled.
	FailureCanceled FailureKind = "canceled"

	// FailureDeadlineExceeded means the caller's context deadline expired. It
	// matches context.DeadlineExceeded, but not ErrTimeout.
	FailureDeadlineExceeded FailureKind = "deadline_exceeded"

	// FailureWriteFailed matches ErrWriteFailed.
	FailureWriteFailed FailureKind = "write_failed"

	// FailureJobFailed matches ErrJobFailed.
	FailureJobFailed FailureKind = "job_failed"

	// FailureUnknownJobState matches ErrUnknownJobState.
	FailureUnknownJobState FailureKind = "unknown_job_state"

	// FailureBudgetExhausted matches ErrBudgetExhausted.
	FailureBudgetExhausted FailureKind = "budget_exhausted"
)

// Operation names the provider operation a CallError concerns. Its value is
// the operation's path under https://api.neverbounce.com/v4.2/.
type Operation string

// Operations.
const (
	// OpCheckEmail is single/check (CheckEmail).
	OpCheckEmail Operation = "single/check"

	// OpCreateJob is jobs/create (CreateJob).
	OpCreateJob Operation = "jobs/create"

	// OpParseJob is jobs/parse (ParseJob).
	OpParseJob Operation = "jobs/parse"

	// OpStartJob is jobs/start (StartJob).
	OpStartJob Operation = "jobs/start"

	// OpJobStatus is jobs/status (JobStatus and WaitForJob).
	OpJobStatus Operation = "jobs/status"

	// OpJobResults is jobs/results (JobResults and JobResultPages).
	OpJobResults Operation = "jobs/results"

	// OpDownloadJob is jobs/download (DownloadJob).
	OpDownloadJob Operation = "jobs/download"
)

// APIStatus is the "status" member of a provider response envelope. A
// CallError keeps an undocumented value too, but only when it is 1 to 64
// bytes of lowercase ASCII letters and underscores, and never when it
// contains the API key.
type APIStatus string

// Documented envelope statuses.
const (
	// APIStatusSuccess means the request succeeded.
	APIStatusSuccess APIStatus = "success"

	// APIStatusGeneralFailure means the request failed; the provider's message
	// says why (see WithProviderDiagnostics).
	APIStatusGeneralFailure APIStatus = "general_failure"

	// APIStatusAuthFailure means the API key was refused.
	APIStatusAuthFailure APIStatus = "auth_failure"

	// APIStatusTempUnavail means a partial service interruption.
	APIStatusTempUnavail APIStatus = "temp_unavail"

	// APIStatusThrottleTriggered means the request was rate limited.
	APIStatusThrottleTriggered APIStatus = "throttle_triggered"

	// APIStatusBadReferrer means the request's referrer is not trusted.
	APIStatusBadReferrer APIStatus = "bad_referrer"
)

// Known reports whether s is one of the six documented statuses.
func (s APIStatus) Known() bool {
	switch s {
	case APIStatusSuccess, APIStatusGeneralFailure, APIStatusAuthFailure,
		APIStatusTempUnavail, APIStatusThrottleTriggered, APIStatusBadReferrer:
		return true
	}
	return false
}

// ValidationError describes a local validation failure: nothing was sent. Err
// is ErrInvalidConfig or ErrInvalidRequest. Field names the argument, option or
// request field as a Go path, such as "Supplied.Rows[17][1]" or "Timeout", and
// Reason is a short fixed code, such as "control_character". Neither ever
// contains the rejected value.
type ValidationError struct {
	Err    error
	Field  string
	Reason string
}

// Error returns the sentinel's message followed by the field and the reason,
// such as "neverbounce: invalid request: Email: control_character".
func (e *ValidationError) Error() string {
	if e == nil {
		return "neverbounce: <nil ValidationError>"
	}
	var b strings.Builder
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		b.WriteString("neverbounce: validation failed")
	}
	for _, part := range [...]string{e.Field, e.Reason} {
		if part != "" {
			b.WriteString(": ")
			b.WriteString(part)
		}
	}
	return b.String()
}

// Unwrap returns the sentinel, ErrInvalidConfig or ErrInvalidRequest.
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CallError describes a call that was attempted, a call that a canceled or
// expired context stopped before it was sent, or a helper (WaitForJob,
// JobResultPages) that stopped short.
//
// Its Error text is built only from Op, Kind, StatusCode, APIStatus and
// Field: it never holds the API key, email addresses, row data, URLs, header
// values, response bodies or provider messages, so it is safe to log.
// GoString and every fmt verb add only the other exported fields. The cause
// and the provider diagnostics sit behind a function value that fmt and
// reflection-based dumpers print only as an address.
//
// Tests and fakes may build a CallError from its exported fields alone; it
// then matches errors.Is like one the SDK returns.
type CallError struct {
	// Kind classifies the failure.
	Kind FailureKind

	// Op is the operation concerned.
	Op Operation

	// StatusCode is the HTTP status, or 0 when no response was received (and
	// for the helper kinds job_failed, unknown_job_state and budget_exhausted).
	StatusCode int

	// APIStatus is the envelope status, kept when it is 1 to 64 bytes of
	// lowercase ASCII letters and underscores and does not contain the API
	// key, and "" otherwise.
	APIStatus APIStatus

	// JobID is the job concerned when it is known: 0 for single/check, and
	// for a jobs/create whose answer could not be read.
	JobID JobID

	// Page is the jobs/results page requested; for budget_exhausted from
	// JobResultPages, it is the next page, where a new iteration can resume.
	Page int

	// Field is a JSON path, such as "results[3].verification.result", for
	// type and shape failures. It names a location, never a value.
	Field string

	// BytesWritten is the number of bytes DownloadJob wrote to its writer
	// before failing. It is 0 for every other operation.
	BytesWritten int64

	// private holds the sanitized cause and the provider diagnostics.
	private func() callDetail
}

// callDetail is the part of a CallError that is never printed.
type callDetail struct {
	cause       error
	diagnostics ProviderDiagnostics
}

// ProviderDiagnostics holds provider text about a failed call. It is empty
// unless the Client was created with WithProviderDiagnostics.
type ProviderDiagnostics struct {
	// Message is the envelope's "message", cut to at most 1 KiB on a UTF-8
	// boundary, with every occurrence of the API key replaced by
	// "[redacted]". It may echo request data, such as an email address.
	Message string
}

// newCallError returns a CallError without diagnostics whose cause is
// reachable only through Unwrap.
func newCallError(op Operation, kind FailureKind, status int, cause error) *CallError {
	return newCallErrorWithDiagnostics(op, kind, status, cause, ProviderDiagnostics{})
}

// newCallErrorWithDiagnostics returns a CallError whose cause and diagnostics
// are reachable only through Unwrap and Diagnostics.
func newCallErrorWithDiagnostics(op Operation, kind FailureKind, status int, cause error,
	diag ProviderDiagnostics,
) *CallError {
	d := callDetail{cause: cause, diagnostics: diag}
	return &CallError{Kind: kind, Op: op, StatusCode: status, private: func() callDetail { return d }}
}

// Error returns "neverbounce: <op>: <kind>", followed by " (HTTP <status>,
// <api status>)" with whichever of the two is known, and ": <field>" when a
// field is named, such as "neverbounce: jobs/status: rate_limited (HTTP 200,
// throttle_triggered)".
func (e *CallError) Error() string {
	if e == nil {
		return "neverbounce: <nil CallError>"
	}
	var b strings.Builder
	b.WriteString("neverbounce: ")
	if e.Op != "" {
		b.WriteString(string(e.Op))
		b.WriteString(": ")
	}
	if e.Kind != "" {
		b.WriteString(string(e.Kind))
	} else {
		b.WriteString("call_failed")
	}
	var meta []string
	if e.StatusCode != 0 {
		meta = append(meta, "HTTP "+strconv.Itoa(e.StatusCode))
	}
	if e.APIStatus != "" {
		meta = append(meta, string(e.APIStatus))
	}
	if len(meta) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(meta, ", "))
		b.WriteString(")")
	}
	if e.Field != "" {
		b.WriteString(": ")
		b.WriteString(e.Field)
	}
	return b.String()
}

// Unwrap returns the sanitized cause: a context error or a fixed error of this
// package. It is never a *url.Error, a JSON decoding error, the writer's error
// or anything else that could quote a URL, a header or response content. It is
// nil for a CallError built from its exported fields.
func (e *CallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.detail().cause
}

// Is reports whether target is the sentinel for e.Kind. A timeout also matches
// context.DeadlineExceeded, a caller deadline matches context.DeadlineExceeded
// and a cancellation matches context.Canceled.
func (e *CallError) Is(target error) bool {
	if e == nil {
		return false
	}
	if target == context.Canceled {
		return e.Kind == FailureCanceled
	}
	if target == context.DeadlineExceeded {
		return e.Kind == FailureDeadlineExceeded || e.Kind == FailureTimeout
	}
	sentinel := kindSentinel(e.Kind)
	return sentinel != nil && target == sentinel
}

// GoString prints the exported metadata only, so that %#v cannot reach the
// cause or the provider diagnostics.
func (e *CallError) GoString() string {
	if e == nil {
		return "(*neverbounce.CallError)(nil)"
	}
	return fmt.Sprintf("&neverbounce.CallError{Kind:%q, Op:%q, StatusCode:%d, APIStatus:%q, JobID:%d, "+
		"Page:%d, Field:%q, BytesWritten:%d}",
		string(e.Kind), string(e.Op), e.StatusCode, string(e.APIStatus), int64(e.JobID),
		e.Page, e.Field, e.BytesWritten)
}

// Diagnostics returns the provider's message for this failure. It is empty
// unless the Client was created with WithProviderDiagnostics. Provider text
// may echo request data, such as an email address: inspect it, but think
// before logging or storing it.
func (e *CallError) Diagnostics() ProviderDiagnostics {
	if e == nil {
		return ProviderDiagnostics{}
	}
	return e.detail().diagnostics
}

// forJob sets the job, and the jobs/results page, that e concerns, and
// returns e.
func (e *CallError) forJob(id JobID, page int) *CallError {
	e.JobID, e.Page = id, page
	return e
}

func (e *CallError) detail() callDetail {
	if e.private == nil {
		return callDetail{}
	}
	return e.private()
}

// kindSentinel returns the sentinel a kind matches, or nil for the context
// kinds and any kind this package does not define.
func kindSentinel(kind FailureKind) error {
	switch kind {
	case FailureUnauthorized:
		return ErrUnauthorized
	case FailureRateLimited:
		return ErrRateLimited
	case FailureUnavailable:
		return ErrUnavailable
	case FailureRejected:
		return ErrRejected
	case FailureUpstream:
		return ErrUpstream
	case FailureUnexpectedResponse:
		return ErrUnexpectedResponse
	case FailureMalformedResponse:
		return ErrMalformedResponse
	case FailureResponseTooLarge:
		return ErrResponseTooLarge
	case FailureTransport:
		return ErrTransport
	case FailureTimeout:
		return ErrTimeout
	case FailureWriteFailed:
		return ErrWriteFailed
	case FailureJobFailed:
		return ErrJobFailed
	case FailureUnknownJobState:
		return ErrUnknownJobState
	case FailureBudgetExhausted:
		return ErrBudgetExhausted
	case FailureCanceled, FailureDeadlineExceeded:
		return nil
	}
	return nil
}

// contextFailure returns the CallError for a caller context that is done, with
// the context's error as its cause.
func contextFailure(op Operation, err error, status int) *CallError {
	if errors.Is(err, context.DeadlineExceeded) {
		return newCallError(op, FailureDeadlineExceeded, status, context.DeadlineExceeded)
	}
	return newCallError(op, FailureCanceled, status, context.Canceled)
}

// Validation reasons, reported in ValidationError.Reason.
const (
	reasonRequired             = "required"
	reasonNil                  = "nil"
	reasonNotInitialized       = "not_initialized"
	reasonOutOfRange           = "out_of_range"
	reasonInvalidURL           = "invalid_url"
	reasonInvalidFormat        = "invalid_format"
	reasonInvalidUTF8          = "invalid_utf8"
	reasonControlCharacter     = "control_character"
	reasonBlank                = "blank"
	reasonTooLong              = "too_long"
	reasonNotWholeSeconds      = "not_whole_seconds"
	reasonExceedsClientTimeout = "exceeds_client_timeout"
	reasonConflict             = "conflict"
	reasonTooMany              = "too_many"
	reasonDuplicate            = "duplicate"
	reasonLengthMismatch       = "length_mismatch"
	reasonTooLarge             = "too_large"
	reasonUnsupportedScheme    = "unsupported_scheme"
	reasonRequiresCallbackURL  = "requires_callback_url"
	reasonInvalidHeaderName    = "invalid_header_name"
	reasonInvalidHeaderValue   = "invalid_header_value"
	reasonInvalidValue         = "invalid_value"
)

// invalidConfig returns a *ValidationError matching ErrInvalidConfig.
func invalidConfig(field, reason string) error {
	return &ValidationError{Err: ErrInvalidConfig, Field: field, Reason: reason}
}

// invalidRequest returns a *ValidationError matching ErrInvalidRequest.
func invalidRequest(field, reason string) error {
	return &ValidationError{Err: ErrInvalidRequest, Field: field, Reason: reason}
}
