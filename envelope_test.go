package neverbounce_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// sjStatusField is the CallError Field of a missing or mistyped envelope
// status.
const sjStatusField = "status"

// sjStatusBody returns an envelope whose status is the string status,
// followed by the raw members extra (each starting with a comma).
func sjStatusBody(status, extra string) string {
	return `{"status":"` + status + `"` + extra + `}`
}

// sjDocumentedFailures are the documented failure statuses and their kinds,
// for any operation.
func sjDocumentedFailures() []wantCall {
	return []wantCall{
		{Kind: neverbounce.FailureRejected, Status: http.StatusOK, API: neverbounce.APIStatusGeneralFailure},
		{Kind: neverbounce.FailureUnauthorized, Status: http.StatusOK, API: neverbounce.APIStatusAuthFailure},
		{Kind: neverbounce.FailureUnauthorized, Status: http.StatusOK, API: neverbounce.APIStatusBadReferrer},
		{Kind: neverbounce.FailureUnavailable, Status: http.StatusOK, API: neverbounce.APIStatusTempUnavail},
		{Kind: neverbounce.FailureRateLimited, Status: http.StatusOK, API: neverbounce.APIStatusThrottleTriggered},
	}
}

func TestEnvelopeDocumentedFailuresEveryOperation(t *testing.T) {
	for _, op := range jsonOperations() {
		for _, w := range sjDocumentedFailures() {
			t.Run(op.name+"/"+string(w.API), func(t *testing.T) {
				body := sjStatusBody(string(w.API), `,"message":"echo `+testKey+` for `+testEmail+
					`","execution_time":3,"job_id":1,"result":"valid"`)
				c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body))
				err := op.call(t.Context(), c)
				assertCallError(t, err, op.want(w))
				ce := callError(t, err)
				assert.True(t, ce.APIStatus.Known())
				assert.Empty(t, ce.Diagnostics().Message, "diagnostics are opt-in")
			})
		}
	}
}

func TestEnvelopeStatusShapesEveryOperation(t *testing.T) {
	const ok = http.StatusOK
	malformed := wantCall{Kind: neverbounce.FailureMalformedResponse, Status: ok, Field: sjStatusField}
	unexpected := wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: ok}
	letters := strings.Repeat("z", 65)
	tests := []struct {
		name string
		body string
		want wantCall
	}{
		{name: "status missing", body: `{"result":"valid","job_id":1}`, want: malformed},
		{name: "status null", body: `{"status":null,"result":"valid"}`, want: malformed},
		{name: "status numeric", body: `{"status":200}`, want: malformed},
		{name: "status boolean", body: `{"status":true}`, want: malformed},
		{name: "status object", body: `{"status":{"code":"success"}}`, want: malformed},
		{name: "status array", body: `{"status":["success"]}`, want: malformed},
		{name: "status in another case only", body: `{"Status":"success","result":"valid"}`, want: malformed},
		{name: "status nested only", body: `{"data":{"status":"success"}}`, want: malformed},
		{name: "SUCCESS", body: `{"status":"SUCCESS","result":"valid"}`, want: unexpected},
		{name: "Success", body: `{"status":"Success"}`, want: unexpected},
		{name: "empty status", body: `{"status":""}`, want: unexpected},
		{
			name: "unknown lowercase status", body: `{"status":"maintenance"}`,
			want: wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: ok, API: "maintenance"},
		},
		{name: "token with digits", body: `{"status":"temp_unavail2"}`, want: unexpected},
		{name: "token with a hyphen", body: `{"status":"auth-failure"}`, want: unexpected},
		{
			name: "64-letter status kept", body: sjStatusBody(letters[:64], ""),
			want: wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: ok, API: neverbounce.APIStatus(letters[:64])},
		},
		{name: "65-letter status dropped", body: sjStatusBody(letters, ""), want: unexpected},
		{
			name: "escaped key and value", body: testutil.Esc(`{"~u0073tatus":"general_~u0066ailure"}`),
			want: wantCall{Kind: neverbounce.FailureRejected, Status: ok, API: neverbounce.APIStatusGeneralFailure},
		},
	}
	for _, op := range jsonOperations() {
		for _, tt := range tests {
			t.Run(op.name+"/"+tt.name, func(t *testing.T) {
				c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, tt.body))
				assertCallError(t, op.call(t.Context(), c), op.want(tt.want))
			})
		}
	}
}

func TestEnvelopeMalformedBodiesEveryOperation(t *testing.T) {
	const ok = http.StatusOK
	malformed := wantCall{Kind: neverbounce.FailureMalformedResponse, Status: ok}
	notObject := wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: ok}
	tests := []struct {
		name string
		body string
		want wantCall
	}{
		{name: "empty body", body: ``, want: malformed},
		{name: "whitespace only", body: " \r\n\t", want: malformed},
		{name: "invalid UTF-8", body: "{\"status\":\"success\",\"x\":\"\xff\"}", want: malformed},
		{name: "byte order mark", body: "\xef\xbb\xbf{\"status\":\"success\"}", want: malformed},
		{name: "trailing data", body: `{"status":"success"}{}`, want: malformed},
		{name: "truncated", body: `{"status":"succ`, want: malformed},
		{name: "duplicate status", body: `{"status":"success","status":"success"}`, want: malformed},
		{name: "escaped duplicate status", body: testutil.Esc(`{"status":"success","~u0073tatus":"general_failure"}`), want: malformed},
		{name: "duplicate deep in an ignored member", body: `{"status":"success","query":[{"page":0,"page":0}]}`, want: malformed},
		{name: "duplicate failure status", body: `{"status":"auth_failure","status":"auth_failure"}`, want: malformed},
		{name: "top-level array", body: `[{"status":"success"}]`, want: notObject},
		{name: "top-level string", body: `"success"`, want: notObject},
		{name: "top-level null", body: ` null `, want: notObject},
		{
			name: "too many containers", body: `{"status":"success","a":[` + strings.TrimSuffix(strings.Repeat("[],", 100_000), ",") + `]}`,
			want: wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: ok},
		},
	}
	for _, op := range jsonOperations() {
		for _, tt := range tests {
			t.Run(op.name+"/"+tt.name, func(t *testing.T) {
				c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, tt.body), neverbounce.WithProviderDiagnostics())
				err := op.call(t.Context(), c)
				assertCallError(t, err, op.want(tt.want))
				assert.Empty(t, callError(t, err).Diagnostics().Message, "a body that is not strict JSON yields no diagnostics")
			})
		}
	}
}

func TestEnvelopeDiagnostics(t *testing.T) {
	escapedKey := testutil.Esc(`~u0073`) + testKey[1:]
	fullyEscaped := ""
	for _, r := range testKey {
		fullyEscaped += testutil.Esc(`~u00`) + strconv.FormatInt(int64(r), 16)
	}
	tests := []struct {
		name    string
		message string // the raw JSON value of "message"
		want    string
	}{
		{name: "plain key scrubbed", message: `"bad key ` + testKey + ` again ` + testKey + `"`, want: "bad key [redacted] again [redacted]"},
		{name: "escaped key scrubbed", message: `"bad key ` + escapedKey + `"`, want: "bad key [redacted]"},
		{name: "fully escaped key scrubbed", message: `"` + fullyEscaped + `"`, want: "[redacted]"},
		{name: "email kept", message: `"no such mailbox ` + testEmail + `"`, want: "no such mailbox " + testEmail},
		{name: "escapes decoded", message: testutil.Esc(`"line~nnext ~"q~" ~u00e9"`), want: "line\nnext \"q\" é"},
		{name: "exactly 1 KiB kept", message: `"` + strings.Repeat("b", 1024) + `"`, want: strings.Repeat("b", 1024)},
		{name: "cut to 1 KiB", message: `"` + strings.Repeat("b", 4096) + `"`, want: strings.Repeat("b", 1024)},
		{name: "cut on a rune boundary", message: `"` + strings.Repeat("a", 1023) + `étail"`, want: strings.Repeat("a", 1023)},
		{name: "three-byte runes cut whole", message: `"` + strings.Repeat("€", 400) + `"`, want: strings.Repeat("€", 341)},
		{name: "key scrubbed before the cut", message: `"` + strings.Repeat("c", 1020) + testKey + `"`, want: strings.Repeat("c", 1020) + "[red"},
		{name: "empty message", message: `""`, want: ""},
		{name: "number ignored", message: `42`, want: ""},
		{name: "object ignored", message: `{"text":"x"}`, want: ""},
		{name: "array ignored", message: `["x"]`, want: ""},
		{name: "null ignored", message: ` null`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"status":"general_failure","message":` + tt.message + `}`
			for _, diag := range []bool{false, true} {
				var opts []neverbounce.Option
				want := ""
				if diag {
					opts = append(opts, neverbounce.WithProviderDiagnostics())
					want = tt.want
				}
				c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body), opts...)
				_, err := c.CheckEmail(t.Context(), checkRequest())
				msg := callError(t, err).Diagnostics().Message
				assert.Equal(t, want, msg, "diagnostics %v", diag)
				assert.LessOrEqual(t, len(msg), 1024)
				assert.True(t, utf8.ValidString(msg))
				assert.NotContains(t, msg, testKey)
				assert.NotContains(t, msg, "secret_")
				// The message never reaches Error, whose text is fixed.
				assertCallError(t, err, wantCall{
					Kind: neverbounce.FailureRejected, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
					API: neverbounce.APIStatusGeneralFailure,
				})
			}
		})
	}
}

func TestEnvelopeDiagnosticsEveryOperation(t *testing.T) {
	body := `{"status":"temp_unavail","message":"retry later, key ` + testKey + `"}`
	for _, op := range jsonOperations() {
		t.Run(string(op.op), func(t *testing.T) {
			c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body), neverbounce.WithProviderDiagnostics())
			err := op.call(t.Context(), c)
			assertCallError(t, err, op.want(wantCall{
				Kind: neverbounce.FailureUnavailable, Status: http.StatusOK, API: neverbounce.APIStatusTempUnavail,
			}))
			assert.Equal(t, "retry later, key [redacted]", callError(t, err).Diagnostics().Message)
		})
	}
}

func TestEnvelopeMessageIgnoredOnSuccess(t *testing.T) {
	body := `{"status":"success","result":"valid","message":"note ` + testKey + `"}`
	c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body), neverbounce.WithProviderDiagnostics())
	res, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Equal(t, neverbounce.ResultValid, res.Result)
}

func TestEnvelopeNon200StatusDecidesKind(t *testing.T) {
	letters := strings.Repeat("y", 65)
	tests := []struct {
		name    string
		code    int
		body    string
		kind    neverbounce.FailureKind
		api     neverbounce.APIStatus
		message string // with WithProviderDiagnostics
	}{
		{
			name: "401 with a throttle envelope", code: 401, body: `{"status":"throttle_triggered","message":"m ` + testKey + `"}`,
			kind: neverbounce.FailureUnauthorized, api: neverbounce.APIStatusThrottleTriggered, message: "m [redacted]",
		},
		{
			name: "403 with a success envelope", code: 403, body: `{"status":"success","execution_time":1}`,
			kind: neverbounce.FailureUnauthorized, api: neverbounce.APIStatusSuccess,
		},
		{
			name: "429 with a general failure", code: 429, body: `{"status":"general_failure","message":"slow"}`,
			kind: neverbounce.FailureRateLimited, api: neverbounce.APIStatusGeneralFailure, message: "slow",
		},
		{
			name: "503 with an auth failure", code: 503, body: `{"status":"auth_failure"}`,
			kind: neverbounce.FailureUnavailable, api: neverbounce.APIStatusAuthFailure,
		},
		{
			name: "413 with temp_unavail", code: 413, body: `{"status":"temp_unavail"}`,
			kind: neverbounce.FailureRejected, api: neverbounce.APIStatusTempUnavail,
		},
		{
			name: "400 with an unknown lowercase status", code: 400, body: `{"status":"maintenance"}`,
			kind: neverbounce.FailureRejected, api: "maintenance",
		},
		{name: "404 with SUCCESS", code: 404, body: `{"status":"SUCCESS"}`, kind: neverbounce.FailureRejected},
		{
			name: "500 with a numeric status", code: 500, body: `{"status":500,"message":"boom"}`,
			kind: neverbounce.FailureUpstream, message: "boom",
		},
		{name: "502 with a null status", code: 502, body: `{"status":null}`, kind: neverbounce.FailureUpstream},
		{name: "504 with a 65-letter status", code: 504, body: sjStatusBody(letters, ""), kind: neverbounce.FailureUpstream},
		{
			name: "302 with an envelope", code: 302, body: `{"status":"bad_referrer"}`,
			kind: neverbounce.FailureUnexpectedResponse, api: neverbounce.APIStatusBadReferrer,
		},
		{
			name: "201 with an envelope", code: 201, body: `{"execution_time":1,"status":"success"}`,
			kind: neverbounce.FailureUnexpectedResponse, api: neverbounce.APIStatusSuccess,
		},
		{
			name: "600 with an envelope", code: 600, body: `{"status":"general_failure","execution_time":2}`,
			kind: neverbounce.FailureUnexpectedResponse, api: neverbounce.APIStatusGeneralFailure,
		},
		{
			name: "duplicate status keys", code: 401, body: `{"status":"auth_failure","status":"auth_failure","message":"m"}`,
			kind: neverbounce.FailureUnauthorized,
		},
		{
			name: "escaped duplicate", code: 401, body: testutil.Esc(`{"status":"auth_failure","~u0073tatus":"x"}`),
			kind: neverbounce.FailureUnauthorized,
		},
		{name: "trailing data", code: 503, body: `{"status":"temp_unavail","message":"m"} x`, kind: neverbounce.FailureUnavailable},
		{
			name: "invalid UTF-8", code: 429, body: "{\"status\":\"throttle_triggered\",\"message\":\"\xff\"}",
			kind: neverbounce.FailureRateLimited,
		},
		{name: "top-level array", code: 500, body: `[{"status":"general_failure"}]`, kind: neverbounce.FailureUpstream},
		{name: "HTML error page", code: 502, body: `<html><body>Bad Gateway</body></html>`, kind: neverbounce.FailureUpstream},
		{name: "empty body", code: 503, body: ``, kind: neverbounce.FailureUnavailable},
		{name: "status in another case", code: 401, body: `{"Status":"auth_failure"}`, kind: neverbounce.FailureUnauthorized},
		{
			name: "over the 64 KiB excerpt", code: 503, body: sjStatusBody("temp_unavail", `,"pad":"`+strings.Repeat("p", 64<<10)+`"`),
			kind: neverbounce.FailureUnavailable,
		},
	}
	for _, op := range jsonOperations() {
		for _, tt := range tests {
			t.Run(op.name+"/"+tt.name, func(t *testing.T) {
				w := op.want(wantCall{Kind: tt.kind, Status: tt.code, API: tt.api})
				plain := newTransportClient(t, testutil.StaticTransport(tt.code, tt.body))
				err := op.call(t.Context(), plain)
				assertCallError(t, err, w)
				assert.Empty(t, callError(t, err).Diagnostics().Message)

				diag := newTransportClient(t, testutil.StaticTransport(tt.code, tt.body), neverbounce.WithProviderDiagnostics())
				err = op.call(t.Context(), diag)
				assertCallError(t, err, w)
				assert.Equal(t, tt.message, callError(t, err).Diagnostics().Message)
			})
		}
	}
}

// TestEnvelopeKeysMatchCaseExactly proves that only the exact key "status"
// (escaped spellings included) is the envelope status: a case-folded
// spelling is an unknown member, ignored.
func TestEnvelopeKeysMatchCaseExactly(t *testing.T) {
	tests := []struct {
		name string
		body string
		want neverbounce.Result
	}{
		{
			name: "other cases beside the real ones", want: neverbounce.ResultValid,
			body: `{"Status":"general_failure","STATUS":"x","status":"success","Result":"invalid","result":"valid"}`,
		},
		{name: "escaped keys", want: neverbounce.ResultCatchall, body: testutil.Esc(`{"~u0073tatus":"success","~u0072esult":"catchall"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, tt.body))
			res, err := c.CheckEmail(t.Context(), checkRequest())
			require.NoError(t, err)
			assert.Equal(t, tt.want, res.Result)
		})
	}
}

// TestEnvelopeValueCaps proves the whole-body bounds through the public API
// at their exact edges: 1,000,000 values and 100,000 containers decode, and
// one more of either fails as response_too_large.
func TestEnvelopeValueCaps(t *testing.T) {
	// Six values besides the padding: the object, "success", two counts, the
	// empty results and the padding array.
	values := func(n int) string {
		return `{"status":"success","total_results":0,"total_pages":0,"results":[],"pad":[` +
			strings.TrimSuffix(strings.Repeat("0,", n), ",") + `]}`
	}
	// Two containers besides the padding: the object and the padding array.
	containers := func(n int) string {
		return `{"status":"success","result":"valid","pad":[` + strings.TrimSuffix(strings.Repeat("[],", n), ",") + `]}`
	}
	results := opCaseNamed("JobResults")
	check := opCaseNamed("CheckEmail")
	tooLarge := wantCall{Kind: neverbounce.FailureResponseTooLarge, Status: http.StatusOK}

	c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, values(1_000_000-6)))
	page, err := c.JobResults(t.Context(), neverbounce.JobResultsRequest{JobID: opJobID, Page: opPage, ItemsPerPage: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Results)

	c = newTransportClient(t, testutil.StaticTransport(http.StatusOK, values(1_000_000-5)))
	assertCallError(t, results.call(t.Context(), c), results.want(tooLarge))

	c = newTransportClient(t, testutil.StaticTransport(http.StatusOK, containers(100_000-2)))
	res, err := c.CheckEmail(t.Context(), checkRequest())
	require.NoError(t, err)
	assert.Equal(t, neverbounce.ResultValid, res.Result)

	c = newTransportClient(t, testutil.StaticTransport(http.StatusOK, containers(100_000-1)))
	assertCallError(t, check.call(t.Context(), c), check.want(tooLarge))
}

// TestEnvelopeExecutionTimeNeverFailsEveryOperation proves that
// execution_time is metadata on every operation: no value of it, however
// malformed, fails a call. How each value decodes is in
// decode_internal_test.go, and that each result carries it is in
// roundtrip_test.go.
func TestEnvelopeExecutionTimeNeverFailsEveryOperation(t *testing.T) {
	values := []string{
		`0`, `9223372036854775807`, `null`, `-1`, `1.5`, `1e3`, `"12"`, `true`, `{"ms":5}`, `[5]`,
		`9223372036854775808`,
	}
	for _, o := range jsonOperations() {
		for _, v := range values {
			t.Run(o.name+"/"+v, func(t *testing.T) {
				body := `{"execution_time":` + v + `,` + o.ok.Body[1:]
				c, _ := newScriptClient(t, testutil.Step{Body: body})
				require.NoError(t, o.call(t.Context(), c), "metadata never fails a call")
			})
		}
	}
}

func TestEnvelopeResultsPageDefaultsToOne(t *testing.T) {
	c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, `{"status":"throttle_triggered"}`))
	_, err := c.JobResults(t.Context(), neverbounce.JobResultsRequest{JobID: opJobID})
	// Page 0 means the provider's default, page 1.
	assertCallError(t, err, wantCall{
		Kind: neverbounce.FailureRateLimited, Op: neverbounce.OpJobResults, Status: http.StatusOK,
		API: neverbounce.APIStatusThrottleTriggered, JobID: opJobID, Page: 1,
	})
}

// TestEnvelopeStatusEchoingTheKey proves that an envelope status echoing an
// API key that has the APIStatus shape is dropped, on HTTP 200 and on any
// other status, so that Error() never prints the key.
func TestEnvelopeStatusEchoingTheKey(t *testing.T) {
	const key = "secret_sjenvelopekey" // gitleaks:allow (synthetic test marker)
	tests := []struct {
		code int
		kind neverbounce.FailureKind
	}{
		{http.StatusOK, neverbounce.FailureUnexpectedResponse},
		{http.StatusUnauthorized, neverbounce.FailureUnauthorized},
		{http.StatusBadGateway, neverbounce.FailureUpstream},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			c, err := neverbounce.NewClient(key,
				neverbounce.WithTestBaseURL(loopbackOrigin),
				neverbounce.WithHTTPClient(&http.Client{Transport: testutil.StaticTransport(tt.code, sjStatusBody(key, ""))}),
				neverbounce.WithProviderDiagnostics())
			require.NoError(t, err)
			_, err = c.JobStatus(t.Context(), neverbounce.JobStatusRequest{JobID: opJobID})
			// The echoed status is dropped, so API stays empty.
			assertCallError(t, err, wantCall{Kind: tt.kind, Op: neverbounce.OpJobStatus, Status: tt.code, JobID: opJobID}, key)
		})
	}
}
