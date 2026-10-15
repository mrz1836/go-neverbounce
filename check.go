package neverbounce

import (
	"context"
	"strconv"
	"time"
)

// Single-check bounds.
const (
	maxEmailBytes        = 1 << 10
	minCheckTimeout      = time.Second
	maxCheckTimeout      = 300 * time.Second
	checkTimeoutHeadroom = 10 * time.Second
)

// CheckEmailRequest is one single/check call. It is read only while CheckEmail
// encodes it, before any I/O, and is never kept or modified.
type CheckEmailRequest struct {
	// Email is the address to verify, sent verbatim: never trimmed,
	// normalized or checked against an RFC. It must be 1 to 1024 bytes of
	// valid UTF-8 without control characters, and not only whitespace. A
	// malformed address is a legitimate input: the provider answers it (and
	// bills it) like any other.
	Email string

	// AddressInfo asks for the provider's parse of the address
	// (CheckEmailResult.AddressInfo). It is always sent, false included.
	AddressInfo bool

	// CreditsInfo asks for the account's credit counters
	// (CheckEmailResult.CreditsInfo). It is always sent, false included.
	CreditsInfo bool

	// Timeout is the provider's server-side verification timeout, sent as
	// "timeout" in whole seconds. 0 means not sent (the provider's default,
	// which is undocumented). When it expires the provider answers
	// ResultUnknown, and the whole request can take longer than it. It must
	// be whole seconds from 1s to 300s, and at least 10 seconds shorter than
	// the Client's WithTimeout, so that the SDK does not give up on a check the
	// provider will still bill.
	Timeout time.Duration

	// LeverageHistoricalData, when non-nil, is sent as
	// request_meta_data.leverage_historical_data; false asks for real-time
	// verification only. nil is not sent (the provider's default is true).
	// It is documented in the v4.1 changelog for single checks and in the
	// v4.2 jobs/create schema, but not in the v4.2 single/check schema.
	LeverageHistoricalData *bool
}

// CheckEmailResult is the provider's answer to one single/check call. It
// holds contact data (the address and its parse): treat it as personal data.
//
// It survives a JSON round trip through encoding/json unchanged, under the
// provider's snake_case names. Unmarshaling a raw provider body into it skips
// every check the SDK makes.
type CheckEmailResult struct {
	// Result is the verdict. ResultUnknown is a successful answer.
	Result Result `json:"result"`

	// Flags are the provider's observations, in its order. nil when none.
	Flags Flags `json:"flags,omitempty"`

	// SuggestedCorrection is the provider's soft suggestion for a likely typo,
	// such as a misspelled domain, or "". The SDK never applies it or checks
	// the suggestion.
	SuggestedCorrection string `json:"suggested_correction,omitempty"`

	// AddressInfo is the provider's parse of the address, when reported (ask
	// for it with CheckEmailRequest.AddressInfo), and nil otherwise.
	AddressInfo *AddressInfo `json:"address_info,omitempty"`

	// CreditsInfo is the account's credit counters, when reported (ask for
	// them with CheckEmailRequest.CreditsInfo), and nil otherwise.
	CreditsInfo *CreditsInfo `json:"credits_info,omitempty"`

	// RetryToken is the provider's retry_token, kept as opaque text when it is
	// a string of at most 1 KiB. It appears only in a provider guide example:
	// the SDK gives it no meaning and never sends it back.
	RetryToken string `json:"retry_token,omitempty"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// Members of a single/check answer after the envelope, by index into
// checkNames. The first verCount follow the verification-member order.
const (
	checkCreditsInfo = envFirst + verCount + iota
	checkRetryToken
)

// checkNames returns the member names a single/check answer is read against.
func checkNames() []string {
	v := verificationNames()
	return envelopeNames(v[verResult], v[verFlags], v[verSuggestedCorrection], v[verAddressInfo],
		"credits_info", "retry_token")
}

// CheckEmail verifies one address: POST https://api.neverbounce.com/v4.2/single/check.
//
// PAID: every call costs one credit, duplicates and syntactically bad
// addresses included. It makes exactly one attempt and never retries. After a
// timeout, cancellation, transport failure or unreadable response the check
// may already be billed: do not resend it blindly. ResultUnknown is a
// successful answer, never an error.
//
// The provider's usage guidelines forbid verifying an existing list one
// address at a time: use CreateJob for lists.
//
// Errors are a *ValidationError (nothing sent) or a *CallError.
func (c *Client) CheckEmail(ctx context.Context, req CheckEmailRequest) (*CheckEmailResult, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, err
	}
	b, err := req.encode(c)
	if err != nil {
		return nil, err
	}
	obj, cerr := c.call(ctx, OpCheckEmail, b, checkNames())
	if cerr != nil {
		return nil, cerr
	}
	res, derr := decodeCheckEmail(&obj)
	if derr != nil {
		return nil, derr.callError(OpCheckEmail)
	}
	return res, nil
}

// encode validates the request and returns its JSON body, with the key member
// first: {"key":…,"email":…,"address_info":…,"credits_info":…[,"timeout":N]
// [,"request_meta_data":{"leverage_historical_data":…}]}.
func (r *CheckEmailRequest) encode(c *Client) ([]byte, error) {
	if err := checkText("Email", r.Email, maxEmailBytes, true); err != nil {
		return nil, err
	}
	if err := checkServerTimeout(r.Timeout, c.timeout); err != nil {
		return nil, err
	}
	b := c.newRequestBody(len(r.Email) + 160)
	b = b.str("email", r.Email).boolean("address_info", r.AddressInfo).boolean("credits_info", r.CreditsInfo)
	if r.Timeout != 0 {
		b = b.integer("timeout", int64(r.Timeout/time.Second))
	}
	return appendRequestMetaData(b, r.LeverageHistoricalData).finish(maxSmallRequestBytes)
}

// appendRequestMetaData appends request_meta_data.leverage_historical_data
// when v is non-nil.
func appendRequestMetaData(b body, v *bool) body {
	if v == nil {
		return b
	}
	b = append(b.name("request_meta_data"), `{"leverage_historical_data":`...)
	b = strconv.AppendBool(b, *v)
	return append(b, '}')
}

// checkServerTimeout validates CheckEmailRequest.Timeout against the Client's
// own timeout.
func checkServerTimeout(d, clientTimeout time.Duration) error {
	switch {
	case d == 0:
		return nil
	case d%time.Second != 0:
		return invalidRequest("Timeout", reasonNotWholeSeconds)
	case d < minCheckTimeout || d > maxCheckTimeout:
		return invalidRequest("Timeout", reasonOutOfRange)
	case d+checkTimeoutHeadroom > clientTimeout:
		return invalidRequest("Timeout", reasonExceedsClientTimeout)
	}
	return nil
}

// decodeCheckEmail decodes a single/check success envelope.
func decodeCheckEmail(o *object) (*CheckEmailResult, *decodeError) {
	v, derr := decodeVerification([verCount][]byte(o.fields[envFirst:envFirst+verCount]), path{})
	if derr != nil {
		return nil, derr
	}
	credits, derr := decodeCreditsInfo(o.fields[checkCreditsInfo])
	if derr != nil {
		return nil, derr
	}
	return &CheckEmailResult{
		Result:              v.result,
		Flags:               v.flags,
		SuggestedCorrection: v.suggestedCorrection,
		AddressInfo:         v.addressInfo,
		CreditsInfo:         credits,
		RetryToken:          retryToken(o.fields[checkRetryToken]),
		ExecutionTime:       executionTime(o.fields[envExecutionTime]),
	}, nil
}
