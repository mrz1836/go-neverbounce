package neverbounce

import (
	"context"
	"net/http"
	"strings"
)

// Envelope members every operation reads, by index into the names its decoder
// passes to call. An operation's own members follow from envFirst on.
const (
	envStatus = iota
	envMessage
	envExecutionTime
	envFirst
)

// envelopeNames returns the envelope member names followed by an operation's
// own member names, in the order of their indexes.
func envelopeNames(fields ...string) []string {
	return append([]string{envStatus: "status", envMessage: "message", envExecutionTime: "execution_time"}, fields...)
}

// call makes the one attempt of a JSON operation and returns the members of
// its success envelope, read against names (see envelopeNames). Every failure
// is a *CallError: the caller's context, the SDK's timeout and transport
// failures first, then an HTTP status other than 200, then a body that is not
// strict JSON, then an envelope status other than "success".
func (c *Client) call(ctx context.Context, op Operation, body []byte, names []string) (object, *CallError) {
	ex, cerr := c.exchange(ctx, op, body)
	if cerr != nil {
		return object{}, cerr
	}
	if ex.status != http.StatusOK {
		return object{}, c.statusFailure(op, ex)
	}
	if kind, cause := checkStrict(ex.body); cause != nil {
		return object{}, newCallError(op, kind, ex.status, cause)
	}
	obj := readObject(ex.body, names)
	if cerr := c.envelopeFailure(op, obj.fields[envStatus], obj.fields[envMessage]); cerr != nil {
		return object{}, cerr
	}
	return obj, nil
}

// envelopeFailure returns nil for an HTTP 200 envelope whose status is
// "success", and the CallError it reports otherwise. A status that is absent,
// null or not a string is malformed.
func (c *Client) envelopeFailure(op Operation, statusRaw, messageRaw []byte) *CallError {
	switch {
	case isNull(statusRaw):
		return missingField("status").callError(op)
	case statusRaw[0] != '"':
		return wrongType("status").callError(op)
	}
	status := decodeKey(statusRaw)
	if APIStatus(status) == APIStatusSuccess {
		return nil
	}
	kind, cause := apiStatusKind(APIStatus(status))
	ce := newCallErrorWithDiagnostics(op, kind, http.StatusOK, cause, c.diagnosticsFrom(messageRaw))
	ce.APIStatus = c.apiStatus(status)
	return ce
}

// statusFailure returns the CallError for an HTTP status other than 200. The
// status alone decides the kind. The body is read best-effort for the
// envelope status and, when enabled, the provider's message: a body that
// cannot be parsed never changes the kind.
func (c *Client) statusFailure(op Operation, ex *exchange) *CallError {
	statusRaw, messageRaw := failureEnvelope(ex.body)
	ce := newCallErrorWithDiagnostics(op, statusKind(ex.status), ex.status, errStatus, c.diagnosticsFrom(messageRaw))
	if isString(statusRaw) {
		ce.APIStatus = c.apiStatus(decodeKey(statusRaw))
	}
	return ce
}

// failureEnvelope returns the raw status and message members of a failed
// call's body, or nil for either when the body is absent or not strict JSON.
func failureEnvelope(body []byte) (statusRaw, messageRaw []byte) {
	if body == nil {
		return nil, nil
	}
	if kind, _ := checkStrict(body); kind != "" {
		return nil, nil
	}
	obj := readObject(body, envelopeNames())
	return obj.fields[envStatus], obj.fields[envMessage]
}

// diagnosticsFrom returns the provider's message when diagnostics are enabled
// and the member is a JSON string, with the API key scrubbed, cut to 1 KiB.
func (c *Client) diagnosticsFrom(raw []byte) ProviderDiagnostics {
	if !c.diagnostics || !isString(raw) {
		return ProviderDiagnostics{}
	}
	return ProviderDiagnostics{Message: truncateUTF8(c.scrub(decodeKey(raw)), maxDiagnosticMessageBytes)}
}

// apiStatus returns s as a CallError's APIStatus when it has the documented
// shape, and "" otherwise. A status that holds the API key, which an echoing
// server could send, is never kept: APIStatus appears in Error().
func (c *Client) apiStatus(s string) APIStatus {
	if !validAPIStatus(s) || c.scrub(s) != s {
		return ""
	}
	return APIStatus(s)
}

// apiStatusKind classifies an envelope status other than "success".
func apiStatusKind(s APIStatus) (FailureKind, error) {
	switch s {
	case APIStatusAuthFailure, APIStatusBadReferrer:
		return FailureUnauthorized, errAPIStatus
	case APIStatusThrottleTriggered:
		return FailureRateLimited, errAPIStatus
	case APIStatusTempUnavail:
		return FailureUnavailable, errAPIStatus
	case APIStatusGeneralFailure:
		return FailureRejected, errAPIStatus
	case APIStatusSuccess:
		return "", nil
	}
	return FailureUnexpectedResponse, errUnknownAPIStatus
}

// validAPIStatus reports whether s matches ^[a-z_]{1,64}$, the shape a
// CallError's APIStatus is kept in: a token without digits.
func validAPIStatus(s string) bool { return validToken(s) && !strings.ContainsAny(s, "0123456789") }

// statusKind classifies an HTTP status other than 200.
func statusKind(code int) FailureKind {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return FailureUnauthorized
	case code == http.StatusTooManyRequests:
		return FailureRateLimited
	case code == http.StatusServiceUnavailable:
		return FailureUnavailable
	case code >= 400 && code < 500:
		return FailureRejected
	case code >= 500 && code < 600:
		return FailureUpstream
	}
	return FailureUnexpectedResponse
}
