package neverbounce_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
)

// errPvJoined is joined with a CallError to prove errors.Is sees through a
// join.
var errPvJoined = errors.New("pv joined error")

// pvKinds returns every documented failure kind, in a fixed order.
func pvKinds() []neverbounce.FailureKind {
	return slices.Sorted(maps.Keys(kindSentinels()))
}

func TestErrorsSentinelMessages(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range allSentinels()[:16] {
		msg := s.Error()
		assert.True(t, strings.HasPrefix(msg, "neverbounce: "), msg)
		assert.False(t, seen[msg], "duplicate message %q", msg)
		seen[msg] = true
	}
}

func TestValidationErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  *neverbounce.ValidationError
		want string
	}{
		{
			name: "request field and reason",
			err: &neverbounce.ValidationError{
				Err: neverbounce.ErrInvalidRequest, Field: "Email", Reason: reasonControlCharacter,
			},
			want: "neverbounce: invalid request: Email: control_character",
		},
		{
			name: "config field and reason",
			err:  &neverbounce.ValidationError{Err: neverbounce.ErrInvalidConfig, Field: "apiKey", Reason: reasonRequired},
			want: "neverbounce: invalid configuration: apiKey: required",
		},
		{
			name: "nested path",
			err: &neverbounce.ValidationError{
				Err: neverbounce.ErrInvalidRequest, Field: "Supplied.Rows[17][1]", Reason: "too_long",
			},
			want: "neverbounce: invalid request: Supplied.Rows[17][1]: too_long",
		},
		{
			name: "no field",
			err:  &neverbounce.ValidationError{Err: neverbounce.ErrInvalidRequest, Reason: "too_large"},
			want: "neverbounce: invalid request: too_large",
		},
		{
			name: "no reason",
			err:  &neverbounce.ValidationError{Err: neverbounce.ErrInvalidRequest, Field: "Email"},
			want: "neverbounce: invalid request: Email",
		},
		{
			name: "no field and no reason",
			err:  &neverbounce.ValidationError{Err: neverbounce.ErrInvalidConfig},
			want: "neverbounce: invalid configuration",
		},
		{
			name: "no sentinel",
			err:  &neverbounce.ValidationError{Field: pvFieldTimeout, Reason: reasonOutOfRange},
			want: "neverbounce: validation failed: Timeout: out_of_range",
		},
		{
			name: "zero value",
			err:  &neverbounce.ValidationError{},
			want: "neverbounce: validation failed",
		},
		{
			name: "nil receiver",
			err:  nil,
			want: "neverbounce: <nil ValidationError>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}

func TestValidationErrorUnwrap(t *testing.T) {
	var none *neverbounce.ValidationError
	require.NoError(t, none.Unwrap())

	ve := &neverbounce.ValidationError{Err: neverbounce.ErrInvalidConfig, Field: pvFieldTimeout, Reason: reasonOutOfRange}
	assert.Same(t, neverbounce.ErrInvalidConfig, ve.Unwrap())
	require.ErrorIs(t, ve, neverbounce.ErrInvalidConfig)
	require.NotErrorIs(t, ve, neverbounce.ErrInvalidRequest)

	wrapped := fmt.Errorf("context: %w", ve)
	got, ok := errors.AsType[*neverbounce.ValidationError](wrapped)
	require.True(t, ok)
	assert.Same(t, ve, got)
	require.ErrorIs(t, wrapped, neverbounce.ErrInvalidConfig)

	require.NoError(t, (&neverbounce.ValidationError{}).Unwrap())
	assert.NotErrorIs(t, &neverbounce.ValidationError{}, neverbounce.ErrInvalidRequest)
}

func TestValidationErrorFromTheSDK(t *testing.T) {
	_, err := neverbounce.NewClient("")
	require.ErrorIs(t, err, neverbounce.ErrInvalidConfig)
	require.NotErrorIs(t, err, neverbounce.ErrInvalidRequest)
	ve := validationError(t, err)
	assert.Equal(t, "neverbounce: invalid configuration: apiKey: required", ve.Error())

	_, err = newClient(t).CheckEmail(t.Context(), neverbounce.CheckEmailRequest{})
	require.ErrorIs(t, err, neverbounce.ErrInvalidRequest)
	require.NotErrorIs(t, err, neverbounce.ErrInvalidConfig)
	assert.Equal(t, "neverbounce: invalid request: Email: required", err.Error())
	_, isCall := errors.AsType[*neverbounce.CallError](err)
	assert.False(t, isCall, "a validation failure is never a CallError")
}

func TestCallErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  *neverbounce.CallError
		want string
	}{
		{
			name: "op and kind",
			err:  &neverbounce.CallError{Op: neverbounce.OpJobStatus, Kind: neverbounce.FailureTransport},
			want: "neverbounce: jobs/status: transport",
		},
		{
			name: "no op",
			err:  &neverbounce.CallError{Kind: neverbounce.FailureTimeout},
			want: "neverbounce: timeout",
		},
		{
			name: "no kind",
			err:  &neverbounce.CallError{Op: neverbounce.OpCheckEmail},
			want: "neverbounce: single/check: call_failed",
		},
		{
			name: "zero value",
			err:  &neverbounce.CallError{},
			want: "neverbounce: call_failed",
		},
		{
			name: "status code only",
			err:  &neverbounce.CallError{Op: neverbounce.OpCreateJob, Kind: neverbounce.FailureRejected, StatusCode: 413},
			want: "neverbounce: jobs/create: rejected (HTTP 413)",
		},
		{
			name: "API status only",
			err: &neverbounce.CallError{
				Op: neverbounce.OpParseJob, Kind: neverbounce.FailureRejected, APIStatus: neverbounce.APIStatusGeneralFailure,
			},
			want: "neverbounce: jobs/parse: rejected (general_failure)",
		},
		{
			name: "status code and API status",
			err: &neverbounce.CallError{
				Op: neverbounce.OpJobStatus, Kind: neverbounce.FailureRateLimited, StatusCode: 200,
				APIStatus: neverbounce.APIStatusThrottleTriggered,
			},
			want: "neverbounce: jobs/status: rate_limited (HTTP 200, throttle_triggered)",
		},
		{
			name: "field",
			err: &neverbounce.CallError{
				Op: neverbounce.OpJobResults, Kind: neverbounce.FailureUnexpectedResponse, StatusCode: 200,
				Field: "results[3].verification.result",
			},
			want: "neverbounce: jobs/results: unexpected_response (HTTP 200): results[3].verification.result",
		},
		{
			name: "field without status",
			err:  &neverbounce.CallError{Op: neverbounce.OpStartJob, Kind: neverbounce.FailureMalformedResponse, Field: "status"},
			want: "neverbounce: jobs/start: malformed_response: status",
		},
		{
			name: "everything",
			err: &neverbounce.CallError{
				Op: neverbounce.OpDownloadJob, Kind: neverbounce.FailureUnauthorized, StatusCode: 200,
				APIStatus: neverbounce.APIStatusAuthFailure, Field: "status", JobID: 42, Page: 3, BytesWritten: 9,
			},
			want: "neverbounce: jobs/download: unauthorized (HTTP 200, auth_failure): status",
		},
		{
			name: "no kind with everything else",
			err:  &neverbounce.CallError{StatusCode: 503, APIStatus: pvStatusUnavail, Field: "x"},
			want: "neverbounce: call_failed (HTTP 503, temp_unavail): x",
		},
		{
			name: "negative status code is still printed",
			err:  &neverbounce.CallError{Kind: neverbounce.FailureUnexpectedResponse, StatusCode: -1},
			want: "neverbounce: unexpected_response (HTTP -1)",
		},
		{
			name: "nil receiver",
			err:  nil,
			want: "neverbounce: <nil CallError>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}

func TestCallErrorIsMatchesExactlyItsSentinel(t *testing.T) {
	kinds := pvKinds()
	require.Len(t, kinds, 16, "every documented kind has a sentinel")
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			ce := &neverbounce.CallError{Kind: kind, Op: neverbounce.OpCheckEmail}
			assertOnlySentinel(t, ce, kind)
			assertOnlySentinel(t, fmt.Errorf("wrapped: %w", ce), kind)
			assertOnlySentinel(t, errors.Join(errPvJoined, ce), kind)
			for _, target := range allSentinels() {
				assert.Equal(t, slices.Contains(wantMatches(kind), target), ce.Is(target), "Is(%v)", target)
			}
			assert.False(t, ce.Is(nil), "nil is never matched")
		})
	}
}

func TestCallErrorIsContextKinds(t *testing.T) {
	timeout := &neverbounce.CallError{Kind: neverbounce.FailureTimeout}
	require.ErrorIs(t, timeout, neverbounce.ErrTimeout)
	require.ErrorIs(t, timeout, context.DeadlineExceeded, "an SDK timeout also matches the context error")
	require.NotErrorIs(t, timeout, context.Canceled)

	deadline := &neverbounce.CallError{Kind: neverbounce.FailureDeadlineExceeded}
	require.ErrorIs(t, deadline, context.DeadlineExceeded)
	require.NotErrorIs(t, deadline, neverbounce.ErrTimeout, "the caller's own deadline is not an SDK timeout")
	require.NotErrorIs(t, deadline, context.Canceled)

	canceled := &neverbounce.CallError{Kind: neverbounce.FailureCanceled}
	require.ErrorIs(t, canceled, context.Canceled)
	require.NotErrorIs(t, canceled, context.DeadlineExceeded)
	require.NotErrorIs(t, canceled, neverbounce.ErrTimeout)
}

func TestCallErrorUnknownKindMatchesNothing(t *testing.T) {
	for _, kind := range []neverbounce.FailureKind{"", "call_failed", "TimeOut", "TIMEOUT", "made_up", "timeout "} {
		ce := &neverbounce.CallError{Kind: kind, Op: neverbounce.OpJobStatus, StatusCode: 500}
		for _, target := range allSentinels() {
			require.NotErrorIs(t, ce, target, "kind %q matched %v", kind, target)
		}
	}
	var none *neverbounce.CallError
	assert.False(t, none.Is(neverbounce.ErrTimeout))
	assert.False(t, none.Is(context.Canceled))
	assert.False(t, none.Is(context.DeadlineExceeded))
}

func TestCallErrorBuiltFromExportedFields(t *testing.T) {
	// A caller's fake, as the README shows: exported fields only.
	var fake error = &neverbounce.CallError{
		Kind: neverbounce.FailureRateLimited, Op: neverbounce.OpJobStatus, StatusCode: 200,
		APIStatus: neverbounce.APIStatusThrottleTriggered, JobID: 42,
	}
	assertOnlySentinel(t, fake, neverbounce.FailureRateLimited)
	require.NoError(t, errors.Unwrap(fake), "no cause without the SDK")
	ce := callError(t, fmt.Errorf("poll: %w", fake))
	require.NoError(t, ce.Unwrap())
	assert.Equal(t, neverbounce.ProviderDiagnostics{}, ce.Diagnostics())
	assert.Equal(t, neverbounce.JobID(42), ce.JobID)
	assert.Equal(t, "neverbounce: jobs/status: rate_limited (HTTP 200, throttle_triggered)", fake.Error())

	for _, kind := range pvKinds() {
		assertOnlySentinel(t, &neverbounce.CallError{Kind: kind}, kind)
	}
}

func TestCallErrorFromTheSDK(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newClient(t).JobStatus(canceled, neverbounce.JobStatusRequest{JobID: 7})
	assertCallError(t, err, wantCall{Kind: neverbounce.FailureCanceled, Op: neverbounce.OpJobStatus, JobID: 7})
	ce := callError(t, err)
	assert.Same(t, context.Canceled, ce.Unwrap(), "the cause is the context error itself")
	assert.Equal(t, neverbounce.ProviderDiagnostics{}, ce.Diagnostics())
}

func TestCallErrorGoString(t *testing.T) {
	ce := &neverbounce.CallError{
		Kind: neverbounce.FailureRateLimited, Op: neverbounce.OpJobStatus, StatusCode: 200,
		APIStatus: neverbounce.APIStatusThrottleTriggered, JobID: 42, Page: 3, Field: "status", BytesWritten: 9,
	}
	want := `&neverbounce.CallError{Kind:"rate_limited", Op:"jobs/status", StatusCode:200, ` +
		`APIStatus:"throttle_triggered", JobID:42, Page:3, Field:"status", BytesWritten:9}`
	assert.Equal(t, want, ce.GoString())
	assert.Equal(t, want, fmt.Sprintf("%#v", ce), "%#v uses GoString")
	assert.NotContains(t, ce.GoString(), "private", "only exported fields")

	zero := `&neverbounce.CallError{Kind:"", Op:"", StatusCode:0, APIStatus:"", JobID:0, Page:0, Field:"", BytesWritten:0}`
	assert.Equal(t, zero, (&neverbounce.CallError{}).GoString())

	var none *neverbounce.CallError
	assert.Equal(t, "(*neverbounce.CallError)(nil)", none.GoString())
	assert.Equal(t, "(*neverbounce.CallError)(nil)", fmt.Sprintf("%#v", none))

	sdk := callError(t, pvCanceledCheck(t))
	assert.Equal(t, `&neverbounce.CallError{Kind:"canceled", Op:"single/check", StatusCode:0, APIStatus:"", JobID:0, `+
		`Page:0, Field:"", BytesWritten:0}`, fmt.Sprintf("%#v", sdk))
}

// pvCanceledCheck returns the error of a CheckEmail whose context is already
// canceled.
func pvCanceledCheck(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newClient(t).CheckEmail(ctx, checkRequest())
	return err
}

func TestCallErrorDiagnosticsNilReceiver(t *testing.T) {
	var none *neverbounce.CallError
	assert.Equal(t, neverbounce.ProviderDiagnostics{}, none.Diagnostics())
	require.NoError(t, none.Unwrap())
	assert.Equal(t, neverbounce.ProviderDiagnostics{}, (&neverbounce.CallError{}).Diagnostics())
}

func TestErrorValuesAreSafeTokens(t *testing.T) {
	kindPattern := regexp.MustCompile(`^[a-z_]{1,32}$`)
	kinds := []neverbounce.FailureKind{
		neverbounce.FailureUnauthorized, neverbounce.FailureRateLimited, neverbounce.FailureUnavailable,
		neverbounce.FailureRejected, neverbounce.FailureUpstream, neverbounce.FailureUnexpectedResponse,
		neverbounce.FailureMalformedResponse, neverbounce.FailureResponseTooLarge, neverbounce.FailureTransport,
		neverbounce.FailureTimeout, neverbounce.FailureCanceled, neverbounce.FailureDeadlineExceeded,
		neverbounce.FailureWriteFailed, neverbounce.FailureJobFailed, neverbounce.FailureUnknownJobState,
		neverbounce.FailureBudgetExhausted,
	}
	assert.ElementsMatch(t, pvKinds(), kinds, "the documented mapping covers every kind constant")
	for _, k := range kinds {
		assert.Regexp(t, kindPattern, string(k))
	}

	ops := map[neverbounce.Operation]string{
		neverbounce.OpCheckEmail:  "single/check",
		neverbounce.OpCreateJob:   "jobs/create",
		neverbounce.OpParseJob:    "jobs/parse",
		neverbounce.OpStartJob:    "jobs/start",
		neverbounce.OpJobStatus:   "jobs/status",
		neverbounce.OpJobResults:  "jobs/results",
		neverbounce.OpDownloadJob: "jobs/download",
	}
	require.Len(t, ops, 7)
	for op, want := range ops {
		assert.Equal(t, want, string(op))
		assert.Regexp(t, `^[a-z]+/[a-z]+$`, string(op))
	}

	for _, s := range []neverbounce.APIStatus{
		neverbounce.APIStatusSuccess, neverbounce.APIStatusGeneralFailure, neverbounce.APIStatusAuthFailure,
		neverbounce.APIStatusTempUnavail, neverbounce.APIStatusThrottleTriggered, neverbounce.APIStatusBadReferrer,
	} {
		assert.Regexp(t, `^[a-z_]{1,64}$`, string(s))
	}
}
