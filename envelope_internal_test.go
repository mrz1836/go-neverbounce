package neverbounce

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// sjLoopback is the test base URL of every internal client: even the real
// sender, were it ever used, could only reach loopback.
const sjLoopback = "https://127.0.0.1"

// sjRespond makes every call c makes answer in memory with status and body,
// declared with its exact length, without an http.Client: the response goes
// straight to the SDK's reading, strict check and decoding.
func sjRespond(c *Client, status int, body []byte) {
	c.send = func(context.Context, Operation, []byte) (*http.Response, error) {
		return &http.Response{
			StatusCode:    status,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}, nil
	}
}

// sjClient returns a client built with opts whose calls answer in memory with
// status and body (see sjRespond).
func sjClient(tb testing.TB, status int, body []byte, opts ...Option) *Client {
	tb.Helper()
	c := newInternalClient(tb, append([]Option{WithTestBaseURL(sjLoopback)}, opts...)...)
	sjRespond(c, status, body)
	return c
}

func TestEnvelopeNames(t *testing.T) {
	assert.Equal(t, []string{statusMember, "message", "execution_time"}, envelopeNames())
	names := envelopeNames("job_id", "queue_id")
	assert.Equal(t, statusMember, names[envStatus])
	assert.Equal(t, "message", names[envMessage])
	assert.Equal(t, "execution_time", names[envExecutionTime])
	assert.Equal(t, "job_id", names[envFirst])
	assert.Equal(t, "queue_id", names[envFirst+1])
	assert.Len(t, names, envFirst+2)
}

func TestEnvelopeAPIStatusKind(t *testing.T) {
	tests := []struct {
		status APIStatus
		kind   FailureKind
		cause  error
	}{
		{APIStatusGeneralFailure, FailureRejected, errAPIStatus},
		{APIStatusAuthFailure, FailureUnauthorized, errAPIStatus},
		{APIStatusBadReferrer, FailureUnauthorized, errAPIStatus},
		{APIStatusTempUnavail, FailureUnavailable, errAPIStatus},
		{APIStatusThrottleTriggered, FailureRateLimited, errAPIStatus},
		{"maintenance", FailureUnexpectedResponse, errUnknownAPIStatus},
		{"SUCCESS", FailureUnexpectedResponse, errUnknownAPIStatus},
		{"", FailureUnexpectedResponse, errUnknownAPIStatus},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			kind, cause := apiStatusKind(tt.status)
			assert.Equal(t, tt.kind, kind)
			require.ErrorIs(t, cause, tt.cause)
		})
	}
	kind, cause := apiStatusKind(APIStatusSuccess)
	assert.Empty(t, kind)
	require.NoError(t, cause)
}

func TestEnvelopeValidAPIStatus(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"success", true},
		{"general_failure", true},
		{"maintenance", true},
		{"a", true},
		{"_", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"SUCCESS", false},
		{"Success", false},
		{"temp_unavail2", false},
		{"auth-failure", false},
		{"auth failure", false},
		{"é", false},
		{"a`", false},
		{"a{", false},
		{"a@", false},
		{"a[", false},
	}
	for _, tt := range tests {
		t.Run(strconv.Quote(tt.s), func(t *testing.T) {
			assert.Equal(t, tt.want, validAPIStatus(tt.s))
		})
	}
}

func TestEnvelopeStatusKind(t *testing.T) {
	tests := []struct {
		codes []int
		kind  FailureKind
	}{
		{[]int{-1, 0, 1, 99, 100, 101, 103, 199}, FailureUnexpectedResponse},
		{[]int{201, 202, 204, 206, 226, 299}, FailureUnexpectedResponse},
		{[]int{300, 301, 302, 303, 304, 307, 308, 399}, FailureUnexpectedResponse},
		{[]int{401, 403}, FailureUnauthorized},
		{[]int{429}, FailureRateLimited},
		{[]int{503}, FailureUnavailable},
		{[]int{400, 402, 404, 405, 408, 409, 413, 418, 422, 428, 430, 451, 499}, FailureRejected},
		{[]int{500, 501, 502, 504, 505, 511, 599}, FailureUpstream},
		{[]int{600, 601, 999, 1000}, FailureUnexpectedResponse},
	}
	for _, tt := range tests {
		for _, code := range tt.codes {
			assert.Equal(t, tt.kind, statusKind(code), "HTTP %d", code)
		}
	}
}

func TestEnvelopeFailureOnHTTP200(t *testing.T) {
	long := strings.Repeat("a", 65)
	tests := []struct {
		name   string
		status string // raw JSON value, or sjAbsent
		kind   FailureKind
		api    APIStatus
		field  string
		cause  error
	}{
		{name: "success", status: `"success"`},
		{name: "escaped success", status: testutil.Esc(`"~u0073uccess"`)},
		{name: sjAbsent, status: sjAbsent, kind: FailureMalformedResponse, field: statusMember, cause: errMissingField},
		{name: jsonNull, status: jsonNull, kind: FailureMalformedResponse, field: statusMember, cause: errMissingField},
		{name: "number", status: `200`, kind: FailureMalformedResponse, field: statusMember, cause: errWrongType},
		{name: "boolean", status: sjTrue, kind: FailureMalformedResponse, field: statusMember, cause: errWrongType},
		{name: "object", status: `{"code":"success"}`, kind: FailureMalformedResponse, field: statusMember, cause: errWrongType},
		{name: "array", status: `["success"]`, kind: FailureMalformedResponse, field: statusMember, cause: errWrongType},
		{name: "general_failure", status: `"general_failure"`, kind: FailureRejected, api: APIStatusGeneralFailure, cause: errAPIStatus},
		{name: "auth_failure", status: `"auth_failure"`, kind: FailureUnauthorized, api: APIStatusAuthFailure, cause: errAPIStatus},
		{name: "bad_referrer", status: `"bad_referrer"`, kind: FailureUnauthorized, api: APIStatusBadReferrer, cause: errAPIStatus},
		{name: "temp_unavail", status: `"temp_unavail"`, kind: FailureUnavailable, api: APIStatusTempUnavail, cause: errAPIStatus},
		{name: "throttle_triggered", status: `"throttle_triggered"`, kind: FailureRateLimited, api: APIStatusThrottleTriggered, cause: errAPIStatus},
		{name: "escaped documented status", status: testutil.Esc(`"auth~u005ffailure"`), kind: FailureUnauthorized, api: APIStatusAuthFailure, cause: errAPIStatus},
		{name: "unknown lowercase", status: `"maintenance"`, kind: FailureUnexpectedResponse, api: "maintenance", cause: errUnknownAPIStatus},
		{name: "uppercase success", status: `"SUCCESS"`, kind: FailureUnexpectedResponse, cause: errUnknownAPIStatus},
		{name: "empty status", status: `""`, kind: FailureUnexpectedResponse, cause: errUnknownAPIStatus},
		{name: "token with digits", status: `"temp_unavail2"`, kind: FailureUnexpectedResponse, cause: errUnknownAPIStatus},
		{name: "64 letters kept", status: `"` + long[:64] + `"`, kind: FailureUnexpectedResponse, api: APIStatus(long[:64]), cause: errUnknownAPIStatus},
		{name: "65 letters dropped", status: `"` + long + `"`, kind: FailureUnexpectedResponse, cause: errUnknownAPIStatus},
		{name: "padded success", status: `" success"`, kind: FailureUnexpectedResponse, cause: errUnknownAPIStatus},
	}
	c := newInternalClient(t, WithProviderDiagnostics())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := c.envelopeFailure(OpJobStatus, sjRaw(tt.status), []byte(`"provider text"`))
			if tt.kind == "" {
				assert.Nil(t, ce)
				return
			}
			require.NotNil(t, ce)
			assert.Equal(t, tt.kind, ce.Kind)
			assert.Equal(t, OpJobStatus, ce.Op)
			assert.Equal(t, http.StatusOK, ce.StatusCode)
			assert.Equal(t, tt.api, ce.APIStatus)
			assert.Equal(t, tt.field, ce.Field)
			require.ErrorIs(t, ce, tt.cause)
			if tt.field == "" {
				assert.Equal(t, "provider text", ce.Diagnostics().Message, "diagnostics come with an API status")
			}
		})
	}
}

// TestEnvelopeAPIStatusNeverHoldsTheKey proves that an envelope status that
// has the APIStatus shape but holds the API key, as an echoing server could
// send it, is never kept: APIStatus appears in Error().
func TestEnvelopeAPIStatusNeverHoldsTheKey(t *testing.T) {
	const key = "secret_sjlowercasekey" // gitleaks:allow (synthetic test marker)
	c, err := NewClient(key, WithTestBaseURL(sjLoopback))
	require.NoError(t, err)
	require.True(t, validAPIStatus(key), "the key has the APIStatus shape")
	tests := []struct {
		status string
		want   APIStatus
	}{
		{key, ""},
		{"x_" + key, ""},
		{key + "_x", ""},
		{key + key, ""},
		{key[:len(key)-1], APIStatus(key[:len(key)-1])},
		{"maintenance", "maintenance"},
		{"success", APIStatusSuccess},
		{"SUCCESS", ""},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			assert.Equal(t, tt.want, c.apiStatus(tt.status))
			if tt.status != string(APIStatusSuccess) {
				ce := c.envelopeFailure(OpCheckEmail, []byte(`"`+tt.status+`"`), nil)
				require.NotNil(t, ce)
				assert.Equal(t, FailureUnexpectedResponse, ce.Kind)
				assert.Equal(t, tt.want, ce.APIStatus)
				assert.NotContains(t, ce.Error(), key)
			}
			ce := c.statusFailure(OpCheckEmail, &exchange{status: 401, body: []byte(`{"status":"` + tt.status + `"}`)})
			assert.Equal(t, FailureUnauthorized, ce.Kind)
			assert.Equal(t, tt.want, ce.APIStatus)
			assert.NotContains(t, ce.Error(), key)
		})
	}
	escaped := []byte(testutil.Esc(`"~u0073ecret_sjlowercasekey"`))
	assert.Empty(t, c.envelopeFailure(OpJobStatus, escaped, nil).APIStatus, "escaped spellings of the key are caught too")
	assert.Equal(t, APIStatus(key), newInternalClient(t).apiStatus(key), "another client's key is just a status")
}

func TestEnvelopeDiagnosticsFrom(t *testing.T) {
	plain := newInternalClient(t)
	diag := newInternalClient(t, WithProviderDiagnostics())
	escapedKey := testutil.Esc(`~u0073`) + internalKey[1:]
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `"Missing required parameter"`, want: "Missing required parameter"},
		{name: "key scrubbed", raw: `"key ` + internalKey + ` and ` + internalKey + `!"`, want: "key [redacted] and [redacted]!"},
		{name: "escaped key scrubbed", raw: `"key ` + escapedKey + `"`, want: "key [redacted]"},
		{name: "key run together", raw: `"` + internalKey + internalKey + `"`, want: "[redacted][redacted]"},
		{name: "key prefix kept", raw: `"` + internalKey[:10] + `"`, want: internalKey[:10]},
		{name: "escapes decoded", raw: testutil.Esc(`"a~nb~"c~ud800"`), want: "a\nb\"c" + string(utf8.RuneError)},
		{name: "exactly 1 KiB", raw: `"` + strings.Repeat("m", 1024) + `"`, want: strings.Repeat("m", 1024)},
		{name: "cut to 1 KiB", raw: `"` + strings.Repeat("m", 1500) + `"`, want: strings.Repeat("m", 1024)},
		{name: "cut on a rune boundary", raw: `"` + strings.Repeat("m", 1023) + `é"`, want: strings.Repeat("m", 1023)},
		{name: "scrubbed before the cut", raw: `"` + strings.Repeat("m", 1020) + internalKey + `"`, want: strings.Repeat("m", 1020) + "[red"},
		{name: "empty message", raw: `""`, want: ""},
		{name: "number", raw: `42`, want: ""},
		{name: "object", raw: `{"text":"x"}`, want: ""},
		{name: jsonNull, raw: jsonNull, want: ""},
		{name: sjAbsent, raw: sjAbsent, want: ""},
		{name: "lone quote", raw: `"`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, ProviderDiagnostics{}, plain.diagnosticsFrom(sjRaw(tt.raw)), "off without the option")
			got := diag.diagnosticsFrom(sjRaw(tt.raw))
			assert.Equal(t, tt.want, got.Message)
			assert.LessOrEqual(t, len(got.Message), maxDiagnosticMessageBytes)
			assert.True(t, utf8.ValidString(got.Message))
			assert.NotContains(t, got.Message, internalKey)
		})
	}
}

func TestEnvelopeFailureEnvelope(t *testing.T) {
	tests := []struct {
		name            string
		body            []byte
		status, message string
	}{
		{name: "nil body", body: nil},
		{name: "empty body", body: []byte{}},
		{name: "envelope", body: []byte(`{"status":"auth_failure","message":"m","x":1}`), status: `"auth_failure"`, message: `"m"`},
		{name: "status only", body: []byte(`{"status":7}`), status: `7`},
		{name: "case-different keys ignored", body: []byte(`{"Status":"a","MESSAGE":"b"}`)},
		{name: "invalid JSON", body: []byte(`{"status":"auth_failure"`)},
		{name: "trailing data", body: []byte(`{"status":"auth_failure"} {}`)},
		{name: "duplicate key", body: []byte(testutil.Esc(`{"status":"a","~u0073tatus":"b"}`))},
		{name: "invalid UTF-8", body: []byte("{\"status\":\"a\",\"message\":\"\xff\"}")},
		{name: "not an object", body: []byte(`[{"status":"a"}]`)},
		{name: "HTML", body: []byte(`<html><body>502 Bad Gateway</body></html>`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message := failureEnvelope(tt.body)
			assert.Equal(t, tt.status, string(status))
			assert.Equal(t, tt.message, string(message))
		})
	}
}

func TestEnvelopeStatusFailure(t *testing.T) {
	plain := newInternalClient(t)
	diag := newInternalClient(t, WithProviderDiagnostics())
	tests := []struct {
		name    string
		code    int
		body    string
		kind    FailureKind
		api     APIStatus
		message string
	}{
		{
			name: "401 with an envelope", code: 401, body: `{"status":"auth_failure","message":"bad key ` + internalKey + `"}`,
			kind: FailureUnauthorized, api: APIStatusAuthFailure, message: "bad key [redacted]",
		},
		{name: "500 with a success envelope", code: 500, body: `{"status":"success"}`, kind: FailureUpstream, api: APIStatusSuccess},
		{name: "429 with an unknown status", code: 429, body: `{"status":"slow_down"}`, kind: FailureRateLimited, api: "slow_down"},
		{name: "503 with an invalid status shape", code: 503, body: `{"status":"Temp Unavail","message":"x"}`, kind: FailureUnavailable, message: "x"},
		{name: "413 with a numeric status", code: 413, body: `{"status":413,"message":7}`, kind: FailureRejected},
		{name: "400 with a null status", code: 400, body: `{"status":null}`, kind: FailureRejected},
		{name: "302 with an envelope", code: 302, body: `{"status":"bad_referrer"}`, kind: FailureUnexpectedResponse, api: APIStatusBadReferrer},
		{name: "502 not strict JSON", code: 502, body: `{"status":"general_failure","message":"m"} x`, kind: FailureUpstream},
		{name: "404 duplicate status", code: 404, body: `{"status":"general_failure","status":"general_failure"}`, kind: FailureRejected},
		{name: "101 without a body", code: 101, kind: FailureUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			if tt.body != "" {
				body = []byte(tt.body)
			}
			ex := &exchange{status: tt.code, body: body}
			for _, c := range []*Client{plain, diag} {
				ce := c.statusFailure(OpParseJob, ex)
				require.NotNil(t, ce)
				assert.Equal(t, tt.kind, ce.Kind)
				assert.Equal(t, OpParseJob, ce.Op)
				assert.Equal(t, tt.code, ce.StatusCode)
				assert.Equal(t, tt.api, ce.APIStatus)
				assert.Empty(t, ce.Field)
				require.ErrorIs(t, ce, errStatus)
			}
			assert.Empty(t, plain.statusFailure(OpParseJob, ex).Diagnostics().Message)
			assert.Equal(t, tt.message, diag.statusFailure(OpParseJob, ex).Diagnostics().Message)
		})
	}
}

// TestEnvelopeCallOrder proves the order call classifies an answer in: the
// HTTP status first, then the strict check, then the envelope status.
func TestEnvelopeCallOrder(t *testing.T) {
	tests := []struct {
		name  string
		code  int
		body  string
		kind  FailureKind
		cause error
	}{
		{name: "status before strictness", code: 503, body: `{"status":"success"} trailing`, kind: FailureUnavailable, cause: errStatus},
		{name: "strictness before the envelope", code: 200, body: `{"status":"auth_failure","status":"auth_failure"}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "not an object before the envelope", code: 200, body: `[{"status":"auth_failure"}]`, kind: FailureUnexpectedResponse, cause: errNotObject},
		{
			name: "too many values before the envelope", code: 200, body: string(sjFill(`{"status":"auth_failure","a":[`, `[]`, maxContainers)),
			kind: FailureResponseTooLarge, cause: errTooManyValues,
		},
		{name: "envelope", code: 200, body: `{"status":"auth_failure"}`, kind: FailureUnauthorized, cause: errAPIStatus},
		{name: "invalid UTF-8 before the envelope", code: 200, body: "{\"status\":\"auth_failure\",\"m\":\"\xff\"}", kind: FailureMalformedResponse, cause: errInvalidUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := sjClient(t, tt.code, []byte(tt.body))
			obj, ce := c.call(t.Context(), OpStartJob, []byte(`{}`), envelopeNames("queue_id"))
			require.NotNil(t, ce)
			assert.Equal(t, object{}, obj)
			assert.Equal(t, tt.kind, ce.Kind)
			assert.Equal(t, tt.code, ce.StatusCode)
			assert.Empty(t, ce.Field)
			require.ErrorIs(t, ce, tt.cause)
		})
	}

	// The caller's context comes before any answer, which is never read.
	c := sjClient(t, http.StatusOK, []byte(`{"status":"success"}`))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	obj, ce := c.call(ctx, OpStartJob, []byte(`{}`), envelopeNames("queue_id"))
	require.NotNil(t, ce)
	assert.Equal(t, object{}, obj)
	assert.Equal(t, FailureCanceled, ce.Kind)
	assert.Zero(t, ce.StatusCode)

	c = sjClient(t, http.StatusOK, []byte(`{"status":"success","queue_id":"Q","Queue_ID":"X"}`))
	obj, ce = c.call(t.Context(), OpStartJob, []byte(`{}`), envelopeNames("queue_id"))
	require.Nil(t, ce)
	assert.Equal(t, `"success"`, string(obj.fields[envStatus]))
	assert.Equal(t, `"Q"`, string(obj.fields[envFirst]))
	assert.Nil(t, obj.fields[envMessage])
}
