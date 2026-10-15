package neverbounce

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// sjFuzzMax bounds every fuzz input: 64 KiB.
const sjFuzzMax = 64 << 10

// sjSeeds returns the synthetic fixtures, the strict corpus and tricky
// operation bodies that every fuzz target starts from.
func sjSeeds(f *testing.F) [][]byte {
	f.Helper()
	seeds := sjFixtures(f)
	for _, s := range append(sjValidCorpus(), sjInvalidCorpus()...) {
		if len(s) <= sjFuzzMax {
			seeds = append(seeds, []byte(s))
		}
	}
	for _, s := range []string{
		`{"status":"success"}`, `{"status":"success","result":"valid"}`, `{"status":null}`, `{"status":7}`,
		`{"status":"SUCCESS"}`, `{"status":"maintenance","message":"down"}`, `{"Status":"success"}`,
		`{"status":"general_failure","message":"bad key ` + internalKey + `"}`,
		testutil.Esc(`{"status":"auth_failure","message":"~u0073ecret_internalmarker42 ~ud800"}`),
		`{"status":"throttle_triggered","message":` + testutil.JSONQuote(strings.Repeat("é", 700)) + `}`,
		`{"status":"success","result":"Valid"}`, `{"status":"success","result":"valid","flags":[null]}`,
		`{"status":"success","result":"valid","flags":` + sjFlags(65) + `}`,
		`{"status":"success","result":"valid","credits_info":{"paid_credits_used":-1}}`,
		`{"status":"success","result":"valid","address_info":{"host":1}}`,
		`{"status":"success","result":"valid","retry_token":"r","execution_time":-3}`,
		`{"status":"success","id":1001,"job_status":"complete","percent_complete":-0,"bounce_estimate":1e2}`,
		`{"status":"success","id":1001,"job_status":"failed","failure_reason":"","total":{"records":"4"}}`,
		`{"status":"success","id":1002,"job_status":"running"}`, `{"status":"success","id":"1001","job_status":"x"}`,
		`{"status":"success","id":1001,"job_status":"complete","percent_complete":100.1}`,
		`{"status":"success","total_results":1,"total_pages":1,"results":[{"data":"x","verification":{"result":"valid"}}]}`,
		`{"status":"success","total_results":0,"total_pages":0,"results":[]}`,
		`{"status":"success","total_results":1,"total_pages":1,"results":[{"verification":{}}]}`,
		`{"status":"success","total_results":1,"total_pages":1,"results":[null]}`,
		`{"status":"success","total_results":1,"total_pages":1,"results":[{"data":{"a":"<>&` + "\xe2\x80\xa8" +
			`"},"verification":{"result":"valid"}}]}`,
		testutil.Esc(`{"status":"success","total_results":1,"total_pages":1,"results":[{"data":["~ud800"],"verification":{"result":"valid"}}]}`),
	} {
		seeds = append(seeds, []byte(s))
	}
	return seeds
}

// sjKinds is the set of kinds an answer that was received can fail with.
func sjKinds() map[FailureKind]bool {
	return map[FailureKind]bool{
		FailureUnauthorized: true, FailureRateLimited: true, FailureUnavailable: true, FailureRejected: true,
		FailureUpstream: true, FailureUnexpectedResponse: true, FailureMalformedResponse: true,
		FailureResponseTooLarge: true,
	}
}

// sjPatterns are the shapes a received failure is checked against.
type sjPatterns struct {
	apiStatus *regexp.Regexp // CallError.APIStatus when not empty
	token     *regexp.Regexp // every decoded token
	field     *regexp.Regexp // CallError.Field when not empty
	text      *regexp.Regexp // CallError.Error()
}

func sjNewPatterns() sjPatterns {
	return sjPatterns{
		apiStatus: regexp.MustCompile(`^[a-z_]{1,64}$`),
		token:     regexp.MustCompile(`^[a-z0-9_]{1,64}$`),
		field:     regexp.MustCompile(`^[a-z_]+(\[\d+\])?(\.[a-z_]+(\[\d+\])?)*$`),
		text: regexp.MustCompile(`^neverbounce: (single/check|jobs/[a-z]+): [a-z_]+` +
			`( \(HTTP \d+(, [a-z_]{1,64})?\))?(: [a-z_]+(\[\d+\])?(\.[a-z_]+(\[\d+\])?)*)?$`),
	}
}

// sjCheckReceived asserts the invariants of any failure of a call that
// received an answer with HTTP status code.
func sjCheckReceived(t *testing.T, p sjPatterns, ce *CallError, code int, diag bool) {
	t.Helper()
	require.NotNil(t, ce)
	assert.True(t, sjKinds()[ce.Kind], "kind %q", ce.Kind)
	assert.Equal(t, code, ce.StatusCode)
	if ce.APIStatus != "" {
		assert.Regexp(t, p.apiStatus, string(ce.APIStatus))
	}
	if ce.Field != "" {
		assert.Regexp(t, p.field, ce.Field)
	}
	assert.Regexp(t, p.text, ce.Error())
	msg := ce.Diagnostics().Message
	if !diag {
		assert.Empty(t, msg)
	}
	assert.LessOrEqual(t, len(msg), maxDiagnosticMessageBytes)
	assert.True(t, utf8.ValidString(msg))
	assert.NotContains(t, msg, internalKey)
}

// sjRoundTrip asserts that v survives encoding/json unchanged.
func sjRoundTrip[T any](t *testing.T, v *T) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var back T
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, *v, back, "a decoded result round-trips through JSON")
}

func FuzzCheckStrict(f *testing.F) {
	for _, seed := range sjSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > sjFuzzMax {
			return
		}
		kind, cause := checkStrict(data)
		assert.Equal(t, kind == "", cause == nil)
		sjSweep(data)
		if sjMaxDepth(data) > sjValidDepth {
			assert.Equal(t, FailureMalformedResponse, kind)
			assert.True(t, sjSyntaxCause(cause), "nesting past json.Valid's limit is invalid")
			return
		}

		valid, oracleDup := testutil.TokenWalk(data)
		require.Equal(t, !valid, sjSyntaxCause(cause), "checkStrict and the oracle agree on validity")
		if !valid {
			assert.Equal(t, FailureMalformedResponse, kind)
			return
		}
		require.Equal(t, oracleDup, duplicateKey(data), "duplicateKey agrees with the oracle")
		wantContainers, wantValues := sjOracleCount(data)
		containers, values := countValues(data)
		require.Equal(t, wantContainers, containers, "containers")
		require.Equal(t, wantValues, values, "values")
		if data[skipSpace(data, 0)] != '{' {
			require.ErrorIs(t, cause, errNotObject)
			return
		}
		require.Equal(t, oracleDup, errors.Is(cause, errDuplicateKey))
		if cause != nil {
			return
		}
		var decoded map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Len(t, sjMembers(data), len(decoded), "members sees every member once")
	})
}

func FuzzEnvelope(f *testing.F) {
	for _, seed := range sjSeeds(f) {
		f.Add(seed, uint16(200))
		f.Add(seed, uint16(429))
	}
	p := sjNewPatterns()
	plain := sjClient(f, http.StatusOK, nil)
	diag := sjClient(f, http.StatusOK, nil, WithProviderDiagnostics())
	ops := []Operation{OpCheckEmail, OpCreateJob, OpParseJob, OpStartJob, OpJobStatus, OpJobResults}

	f.Fuzz(func(t *testing.T, body []byte, alt uint16) {
		if len(body) > sjFuzzMax {
			return
		}
		op := ops[int(alt)%len(ops)]
		for _, code := range []int{http.StatusOK, 100 + int(alt)%600} {
			sjEnvelopeOnce(t, p, plain, op, code, body, false)
			sjEnvelopeOnce(t, p, diag, op, code, body, true)
		}
		// The envelope helpers never panic, whatever bytes they are given.
		half := len(body) / 2
		for _, c := range []*Client{plain, diag} {
			_ = c.envelopeFailure(op, body[:half], body[half:])
			_ = c.envelopeFailure(op, body, body)
			_ = c.statusFailure(op, &exchange{status: 100 + int(alt)%600, body: body})
			_ = c.diagnosticsFrom(body)
		}
	})
}

// sjEnvelopeOnce makes c's call for op answer with code and body, and checks
// what it returns.
func sjEnvelopeOnce(t *testing.T, p sjPatterns, c *Client, op Operation, code int, body []byte, diag bool) {
	t.Helper()
	sjRespond(c, code, body)
	obj, ce := c.call(t.Context(), op, []byte(`{}`), envelopeNames())
	if ce == nil {
		require.Equal(t, http.StatusOK, code, "only HTTP 200 succeeds")
		assert.Equal(t, "success", decodeKey(obj.fields[envStatus]), "only success passes the envelope")
		return
	}
	sjCheckReceived(t, p, ce, code, diag)
	assert.Equal(t, op, ce.Op)
	if code != http.StatusOK {
		assert.Equal(t, statusKind(code), ce.Kind, "the HTTP status alone decides the kind")
		assert.Empty(t, ce.Field)
	} else if ce.Field != "" {
		assert.Equal(t, statusMember, ce.Field)
	}
}

func FuzzDecodeCheckEmail(f *testing.F) {
	for _, seed := range sjSeeds(f) {
		f.Add(seed)
	}
	p := sjNewPatterns()
	c := sjClient(f, http.StatusOK, nil)
	req := CheckEmailRequest{Email: sjEmail}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > sjFuzzMax {
			return
		}
		sjRespond(c, http.StatusOK, body)
		res, err := c.CheckEmail(t.Context(), req)
		if err != nil {
			assert.Nil(t, res)
			ce := asCallError(t, err)
			sjCheckReceived(t, p, ce, http.StatusOK, false)
			assert.Equal(t, OpCheckEmail, ce.Op)
			assert.Zero(t, ce.JobID)
			return
		}
		require.NotNil(t, res)
		assert.Regexp(t, p.token, string(res.Result))
		assert.LessOrEqual(t, len(res.Flags), maxFlags)
		for _, fl := range res.Flags {
			assert.Regexp(t, p.token, string(fl))
		}
		texts := []string{res.SuggestedCorrection}
		if a := res.AddressInfo; a != nil {
			texts = append(texts, a.OriginalEmail, a.NormalizedEmail, a.Addr, a.Alias, a.Host, a.FQDN, a.Domain, a.Subdomain, a.TLD)
		}
		for _, s := range texts {
			assert.LessOrEqual(t, len(s), maxTextBytes)
			assert.True(t, utf8.ValidString(s))
		}
		if ci := res.CreditsInfo; ci != nil {
			for _, n := range []*int64{ci.PaidCreditsUsed, ci.FreeCreditsUsed, ci.PaidCreditsRemaining, ci.FreeCreditsRemaining} {
				assert.True(t, n == nil || *n >= 0, "credit counts are never negative")
			}
		}
		assert.LessOrEqual(t, len(res.RetryToken), maxRetryTokenBytes)
		assert.True(t, res.ExecutionTime == nil || *res.ExecutionTime >= 0)
		sjRoundTrip(t, res)
	})
}

func FuzzDecodeJobStatus(f *testing.F) {
	for _, seed := range sjSeeds(f) {
		f.Add(seed, int64(1001))
	}
	p := sjNewPatterns()
	c := sjClient(f, http.StatusOK, nil)

	f.Fuzz(func(t *testing.T, body []byte, want int64) {
		if len(body) > sjFuzzMax {
			return
		}
		if want <= 0 {
			want = 1001
		}
		sjRespond(c, http.StatusOK, body)
		st, err := c.JobStatus(t.Context(), JobStatusRequest{JobID: JobID(want)})
		if err != nil {
			assert.Nil(t, st)
			ce := asCallError(t, err)
			sjCheckReceived(t, p, ce, http.StatusOK, false)
			assert.Equal(t, OpJobStatus, ce.Op)
			assert.Equal(t, JobID(want), ce.JobID)
			return
		}
		require.NotNil(t, st)
		assert.Equal(t, JobID(want), st.ID, "only the job requested is ever returned")
		assert.Regexp(t, p.token, string(st.State))
		if st.FailureReason != "" {
			assert.Regexp(t, p.token, string(st.FailureReason))
		}
		for _, s := range []string{st.Filename, st.CreatedAt, st.StartedAt, st.FinishedAt} {
			assert.LessOrEqual(t, len(s), maxTextBytes)
			assert.True(t, utf8.ValidString(s))
		}
		tot := st.Totals
		for _, n := range []*int64{
			tot.Records, tot.Billable, tot.Processed, tot.Valid, tot.Invalid, tot.Catchall,
			tot.Disposable, tot.Unknown, tot.Duplicates, tot.BadSyntax,
		} {
			assert.True(t, n == nil || *n >= 0, "counts are never negative")
		}
		for _, pct := range []*float64{st.BounceEstimate, st.PercentComplete} {
			if pct != nil {
				assert.False(t, math.IsNaN(*pct))
				assert.GreaterOrEqual(t, *pct, 0.0)
				assert.LessOrEqual(t, *pct, 100.0)
				assert.False(t, math.Signbit(*pct), "-0 decodes as +0")
			}
		}
		assert.True(t, st.ExecutionTime == nil || *st.ExecutionTime >= 0)
		sjRoundTrip(t, st)
	})
}

func FuzzDecodeResultsPage(f *testing.F) {
	for _, seed := range sjSeeds(f) {
		f.Add(seed, uint16(1), uint16(0))
		f.Add(seed, uint16(0), uint16(1))
	}
	p := sjNewPatterns()
	c := sjClient(f, http.StatusOK, nil)

	f.Fuzz(func(t *testing.T, body []byte, page, perPage uint16) {
		if len(body) > sjFuzzMax {
			return
		}
		req := JobResultsRequest{JobID: 7, Page: int(page), ItemsPerPage: int(perPage) % (MaxItemsPerPage + 1)}
		sjRespond(c, http.StatusOK, body)
		res, err := c.JobResults(t.Context(), req)
		if err != nil {
			assert.Nil(t, res)
			ce := asCallError(t, err)
			sjCheckReceived(t, p, ce, http.StatusOK, false)
			assert.Equal(t, OpJobResults, ce.Op)
			assert.Equal(t, JobID(7), ce.JobID)
			assert.Equal(t, max(req.Page, 1), ce.Page)
			return
		}
		require.NotNil(t, res)
		assert.Equal(t, max(req.Page, 1), res.Page)
		assert.GreaterOrEqual(t, res.TotalResults, int64(0))
		assert.GreaterOrEqual(t, res.TotalPages, int64(0))
		assert.LessOrEqual(t, len(res.Results), req.rowLimit())
		for _, row := range res.Results {
			sjCheckRow(t, p, row)
		}
		assert.True(t, res.ExecutionTime == nil || *res.ExecutionTime >= 0)
		sjRoundTrip(t, res)
	})
}

// sjCheckRow asserts the invariants of one decoded results row.
func sjCheckRow(t *testing.T, p sjPatterns, row ResultRow) {
	t.Helper()
	v := row.Verification
	assert.Regexp(t, p.token, string(v.Result))
	assert.LessOrEqual(t, len(v.Flags), maxFlags)
	for _, fl := range v.Flags {
		assert.Regexp(t, p.token, string(fl))
	}
	assert.LessOrEqual(t, len(v.SuggestedCorrection), maxTextBytes)
	if row.Data == nil {
		return
	}
	assert.True(t, json.Valid(row.Data), "data is valid JSON")
	assert.LessOrEqual(t, len(row.Data), maxDataBytes)
	assert.True(t, row.Data[0] == '{' || row.Data[0] == '[', "data is an object or an array")
	again, err := json.Marshal(row.Data)
	require.NoError(t, err)
	assert.Equal(t, []byte(row.Data), again, "data is already normalized")
	assert.Equal(t, []byte(row.Data), replaceLoneSurrogates(row.Data), "data holds no lone surrogate escape")
}
