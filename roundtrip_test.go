package neverbounce_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Characters that encoding/json escapes when it marshals: U+2028 LINE
// SEPARATOR and U+2029 PARAGRAPH SEPARATOR, which some JavaScript parsers
// treat as line ends, and the replacement character a lone surrogate
// escape becomes.
const (
	exLineSep  = string(rune(0x2028))
	exParaSep  = string(rune(0x2029))
	exReplaced = string(rune(0xFFFD))
	exEmoji    = string(rune(0x1F600))
)

// exCheck returns a call making one CheckEmail.
func exCheck(ctx context.Context, c *neverbounce.Client) (*neverbounce.CheckEmailResult, error) {
	return c.CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: testEmail, AddressInfo: true, CreditsInfo: true})
}

// exCreate returns a call making one CreateJob.
func exCreate(ctx context.Context, c *neverbounce.Client) (*neverbounce.CreateJobResult, error) {
	return c.CreateJob(ctx, neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList(testEmail)})
}

// exStatus is a call making one JobStatus for job 42.
func exStatus(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobStatus, error) {
	return c.JobStatus(ctx, neverbounce.JobStatusRequest{JobID: 42})
}

func TestRoundTripCheckEmailResult(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  string
		check func(t *testing.T, res *neverbounce.CheckEmailResult)
	}{
		{
			name: "result only",
			body: `{"status":"success","result":"valid"}`,
			want: `{"result":"valid"}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				assert.Nil(t, res.Flags)
				assert.Nil(t, res.AddressInfo)
				assert.Nil(t, res.CreditsInfo)
				assert.Nil(t, res.ExecutionTime)
			},
		},
		{
			name: "every field",
			body: `{"status":"success","result":"catchall","flags":["has_dns","accepts_all","has_dns"],` +
				`"suggested_correction":"avery<&>@example.com",` +
				`"address_info":{"original_email":"Avery.Example+news@Example.com",` +
				`"normalized_email":"avery.example+news@example.com","addr":"avery.example","alias":"news",` +
				`"host":"example.com","fqdn":"mail.example.com","domain":"example","subdomain":"mail","tld":"com"},` +
				`"credits_info":{"paid_credits_used":1,"free_credits_used":2,"paid_credits_remaining":3,` +
				`"free_credits_remaining":4},"retry_token":"rt-` + exLineSep + `-1","execution_time":377}`,
			want: testutil.Esc(`{"result":"catchall","flags":["has_dns","accepts_all","has_dns"],` +
				`"suggested_correction":"avery~u003c~u0026~u003e@example.com",` +
				`"address_info":{"original_email":"Avery.Example+news@Example.com",` +
				`"normalized_email":"avery.example+news@example.com","addr":"avery.example","alias":"news",` +
				`"host":"example.com","fqdn":"mail.example.com","domain":"example","subdomain":"mail","tld":"com"},` +
				`"credits_info":{"paid_credits_used":1,"free_credits_used":2,"paid_credits_remaining":3,` +
				`"free_credits_remaining":4},"retry_token":"rt-~u2028-1","execution_time":377}`),
		},
		{
			name: "credits reported as zero",
			body: `{"status":"success","result":"unknown","credits_info":{"paid_credits_used":0,` +
				`"free_credits_used":0,"paid_credits_remaining":0,"free_credits_remaining":0},"execution_time":0}`,
			want: `{"result":"unknown","credits_info":{"paid_credits_used":0,"free_credits_used":0,` +
				`"paid_credits_remaining":0,"free_credits_remaining":0},"execution_time":0}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				ci := res.CreditsInfo
				require.NotNil(t, ci)
				for _, n := range []*int64{ci.PaidCreditsUsed, ci.FreeCreditsUsed, ci.PaidCreditsRemaining, ci.FreeCreditsRemaining} {
					require.NotNil(t, n, "a reported zero is &0, never nil")
					assert.Zero(t, *n)
				}
				require.NotNil(t, res.ExecutionTime)
				assert.Zero(t, *res.ExecutionTime)
			},
		},
		{
			name: "credits partly reported",
			body: `{"status":"success","result":"valid","credits_info":{"paid_credits_used":null,` +
				`"paid_credits_remaining":12}}`,
			want: `{"result":"valid","credits_info":{"paid_credits_remaining":12}}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				ci := res.CreditsInfo
				require.NotNil(t, ci)
				assert.Nil(t, ci.PaidCreditsUsed, "null is not reported")
				assert.Nil(t, ci.FreeCreditsUsed, "absent is not reported")
				assert.Nil(t, ci.FreeCreditsRemaining)
				require.NotNil(t, ci.PaidCreditsRemaining)
				assert.Equal(t, int64(12), *ci.PaidCreditsRemaining)
			},
		},
		{
			name: "empty info objects",
			body: `{"status":"success","result":"invalid","flags":[],"address_info":{},"credits_info":{}}`,
			want: `{"result":"invalid","address_info":{},"credits_info":{}}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				assert.Nil(t, res.Flags, "an empty list is nil")
				assert.Equal(t, &neverbounce.AddressInfo{}, res.AddressInfo)
				assert.Equal(t, &neverbounce.CreditsInfo{}, res.CreditsInfo)
			},
		},
		{
			name: "escaped text",
			body: testutil.Esc(`{"status":"success","result":"valid","suggested_correction":"a~ud800b~u0000c~td",` +
				`"address_info":{"host":"~u0065xample.com","alias":"~ud83d~ude00"}}`),
			want: testutil.Esc(`{"result":"valid","suggested_correction":"a`+exReplaced+`b~u0000c~td",`) +
				`"address_info":{"alias":"` + exEmoji + `","host":"example.com"}}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				assert.Equal(t, "a"+exReplaced+"b\x00c\td", res.SuggestedCorrection)
			},
		},
		{
			name: "undocumented verdict and flags",
			body: `{"status":"success","result":"future_verdict","flags":["acedemic_host","future_flag"]}`,
			want: `{"result":"future_verdict","flags":["acedemic_host","future_flag"]}`,
			check: func(t *testing.T, res *neverbounce.CheckEmailResult) {
				assert.False(t, res.Result.Known())
				assert.Equal(t, neverbounce.Flags{"acedemic_host", "future_flag"}, res.Flags)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := callOnce(t, pathCheck, tt.body, exCheck)
			require.NoError(t, err)
			back := assertRoundTrip(t, res, tt.want)
			if tt.check != nil {
				tt.check(t, res)
				tt.check(t, back)
			}
		})
	}
}

func TestRoundTripJobControlResults(t *testing.T) {
	t.Run("create with execution time", func(t *testing.T) {
		res, err := callOnce(t, pathCreate, `{"status":"success","job_id":289022,"execution_time":31}`, exCreate)
		require.NoError(t, err)
		assertRoundTrip(t, res, `{"job_id":289022,"execution_time":31}`)
	})
	t.Run("create without execution time", func(t *testing.T) {
		res, err := callOnce(t, pathCreate, `{"status":"success","job_id":1,"execution_time":-5}`, exCreate)
		require.NoError(t, err)
		back := assertRoundTrip(t, res, `{"job_id":1}`)
		assert.Nil(t, back.ExecutionTime)
	})

	parse := func(ctx context.Context, c *neverbounce.Client) (*neverbounce.ParseJobResult, error) {
		return c.ParseJob(ctx, neverbounce.ParseJobRequest{JobID: 42})
	}
	start := func(ctx context.Context, c *neverbounce.Client) (*neverbounce.StartJobResult, error) {
		return c.StartJob(ctx, neverbounce.StartJobRequest{JobID: 42, RunSample: true})
	}
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"queue and execution time", `{"status":"success","queue_id":"NB-PQ-0001","execution_time":12}`,
			`{"queue_id":"NB-PQ-0001","execution_time":12}`,
		},
		{"metadata absent", `{"status":"success"}`, `{}`},
		{"metadata unusable", `{"status":"success","queue_id":"has space","execution_time":1.5}`, `{}`},
	}
	for _, tt := range tests {
		t.Run("parse "+tt.name, func(t *testing.T) {
			res, err := callOnce(t, pathParse, tt.body, parse)
			require.NoError(t, err)
			assertRoundTrip(t, res, tt.want)
		})
		t.Run("start "+tt.name, func(t *testing.T) {
			res, err := callOnce(t, pathStart, tt.body, start)
			require.NoError(t, err)
			assertRoundTrip(t, res, tt.want)
		})
	}
}

func TestRoundTripJobStatus(t *testing.T) {
	const allTotals = `"records":4,"billable":3,"processed":4,"valid":2,"invalid":1,"catchall":0,` +
		`"disposable":0,"unknown":0,"duplicates":1,"bad_syntax":1`
	tests := []struct {
		name  string
		body  string
		want  string
		check func(t *testing.T, st *neverbounce.JobStatus)
	}{
		{
			name: "zero totals",
			body: `{"status":"success","id":42,"job_status":"parsing"}`,
			want: `{"id":42,"job_status":"parsing"}`,
			check: func(t *testing.T, st *neverbounce.JobStatus) {
				assert.Equal(t, neverbounce.JobTotals{}, st.Totals)
				assert.Nil(t, st.BounceEstimate)
				assert.Nil(t, st.PercentComplete)
			},
		},
		{
			name: "empty total object",
			body: `{"status":"success","id":42,"job_status":"uploading","total":{},"bounce_estimate":null}`,
			want: `{"id":42,"job_status":"uploading"}`,
		},
		{
			name: "totals reported as zero",
			body: `{"status":"success","id":42,"job_status":"waiting","total":{"records":0,"billable":0,` +
				`"processed":0,"valid":0,"invalid":0,"catchall":0,"disposable":0,"unknown":0,"duplicates":0,` +
				`"bad_syntax":0},"bounce_estimate":0,"percent_complete":-0}`,
			want: `{"id":42,"job_status":"waiting","total":{"records":0,"billable":0,"processed":0,"valid":0,` +
				`"invalid":0,"catchall":0,"disposable":0,"unknown":0,"duplicates":0,"bad_syntax":0},` +
				`"bounce_estimate":0,"percent_complete":0}`,
			check: func(t *testing.T, st *neverbounce.JobStatus) {
				require.NotNil(t, st.Totals.Billable, "a reported zero is &0, never nil")
				assert.Zero(t, *st.Totals.Billable)
				require.NotNil(t, st.PercentComplete)
				assert.Zero(t, *st.PercentComplete)
			},
		},
		{
			name: "every field",
			body: `{"status":"success","id":42,"filename":"signups <a&b>.csv","created_at":"2026-10-15 09:00:00",` +
				`"started_at":"2026-10-15 09:00:12","finished_at":"2026-10-15 09:01:40","failure_reason":null,` +
				`"total":{` + allTotals + `},"bounce_estimate":12.5,"percent_complete":1e2,` +
				`"job_status":"complete","execution_time":31}`,
			want: testutil.Esc(`{"id":42,"job_status":"complete","filename":"signups ~u003ca~u0026b~u003e.csv",` +
				`"created_at":"2026-10-15 09:00:00","started_at":"2026-10-15 09:00:12",` +
				`"finished_at":"2026-10-15 09:01:40","total":{` + allTotals + `},` +
				`"bounce_estimate":12.5,"percent_complete":100,"execution_time":31}`),
		},
		{
			name: "failed with a reason",
			body: `{"status":"success","id":42,"job_status":"failed","failure_reason":"file_corrupt",` +
				`"total":{"records":null,"billable":7},"percent_complete":37.5}`,
			want: `{"id":42,"job_status":"failed","failure_reason":"file_corrupt","total":{"billable":7},` +
				`"percent_complete":37.5}`,
			check: func(t *testing.T, st *neverbounce.JobStatus) {
				assert.Nil(t, st.Totals.Records)
				require.NotNil(t, st.Totals.Billable)
				assert.Equal(t, int64(7), *st.Totals.Billable)
			},
		},
		{
			name: "undocumented state and reason",
			body: `{"status":"success","id":42,"job_status":"archived","failure_reason":"future_reason",` +
				`"bounce_estimate":33.333333333333336}`,
			want: `{"id":42,"job_status":"archived","failure_reason":"future_reason",` +
				`"bounce_estimate":33.333333333333336}`,
			check: func(t *testing.T, st *neverbounce.JobStatus) {
				assert.False(t, st.State.Known())
				assert.False(t, st.FailureReason.Known())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := callOnce(t, pathStatus, tt.body, exStatus)
			require.NoError(t, err)
			back := assertRoundTrip(t, st, tt.want)
			if tt.check != nil {
				tt.check(t, st)
				tt.check(t, back)
			}
		})
	}
}

func TestRoundTripJobResultsPage(t *testing.T) {
	// The provider's answer: rows whose data hold characters encoding/json
	// escapes, a lone surrogate escape, a valid surrogate pair, whitespace to
	// compact, an array row, and rows without data.
	body := testutil.Esc(`{"status":"success","total_results":4,"total_pages":3,"query":{"page":0},"results":[`+
		`{"data": {"id": "1001", "note": "<b>A & B</b>", "sep": "a`) + exLineSep + "b" + exParaSep +
		testutil.Esc(`c", "lone": "x~ud800y", "pair": "~ud83d~ude00", "n": 7},`+
			` "verification": {"result": "valid", "flags": ["has_dns"]}},`+
			`{"data": [ "1002" , "a<b" , 3 , true , null , {"k" : "v&w"} ],`+
			` "verification": {"result": "catchall", "suggested_correction": "x&y@example.com"}},`+
			`{"verification": {"result": "unknown", "address_info": {"host": "example.org"}}},`+
			`{"data": null, "verification": {"result": "invalid", "flags": []}}`+
			`],"execution_time":18}`)
	wantObject := testutil.Esc(`{"id":"1001","note":"~u003cb~u003eA ~u0026 B~u003c/b~u003e",` +
		`"sep":"a~u2028b~u2029c","lone":"x~ufffdy","pair":"~ud83d~ude00","n":7}`)
	wantArray := testutil.Esc(`["1002","a~u003cb",3,true,null,{"k":"v~u0026w"}]`)
	want := `{"page":3,"total_results":4,"total_pages":3,"results":[` +
		`{"data":` + wantObject + `,"verification":{"result":"valid","flags":["has_dns"]}},` +
		`{"data":` + wantArray + testutil.Esc(`,"verification":{"result":"catchall",`+
		`"suggested_correction":"x~u0026y@example.com"}},`) +
		`{"verification":{"result":"unknown","address_info":{"host":"example.org"}}},` +
		`{"verification":{"result":"invalid"}}],"execution_time":18}`

	page, err := callOnce(t, pathResults, body,
		func(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobResultsPage, error) {
			return c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: 42, Page: 3, ItemsPerPage: 4})
		})
	require.NoError(t, err)
	require.Len(t, page.Results, 4)
	assert.Equal(t, 3, page.Page, "the page the SDK requested, never the provider's echo")
	assertBody(t, wantObject, page.Results[0].Data)
	assertBody(t, wantArray, page.Results[1].Data)
	assert.Nil(t, page.Results[2].Data, "absent data is nil")
	assert.Nil(t, page.Results[3].Data, "null data is nil")
	assert.Nil(t, page.Results[3].Verification.Flags, "an empty list is nil")

	back := assertRoundTrip(t, page, want)
	for _, rows := range [][]neverbounce.ResultRow{page.Results, back.Results} {
		columns := []struct {
			name, want string
		}{
			{"id", "1001"},
			{"note", "<b>A & B</b>"},
			{"sep", "a" + exLineSep + "b" + exParaSep + "c"},
			{"lone", "x" + exReplaced + "y"},
			{"pair", exEmoji},
			{"n", "7"},
		}
		for _, col := range columns {
			got, ok := rows[0].Column(col.name)
			assert.True(t, ok, col.name)
			assert.Equal(t, col.want, got, col.name)
		}
		_, ok := rows[1].Column("0")
		assert.False(t, ok, "an array row has no columns")
	}
}

func TestRoundTripEmptyResultsPage(t *testing.T) {
	page, err := callOnce(t, pathResults, `{"status":"success","total_results":0,"total_pages":0,"results":[]}`,
		func(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobResultsPage, error) {
			return c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: 42})
		})
	require.NoError(t, err)
	assert.Nil(t, page.Results, "an empty page is nil")
	assert.Equal(t, 1, page.Page, "page 0 means the provider's default, page 1")
	back := assertRoundTrip(t, page, `{"page":1,"total_results":0,"total_pages":0}`)
	assert.Nil(t, back.Results)
	assert.False(t, back.More())
}

// TestRoundTripToleratesUnknownFields unmarshals stored JSON that carries
// members the types do not know, at every depth, such as a newer SDK or the
// provider's raw answer would add. They are ignored.
func TestRoundTripToleratesUnknownFields(t *testing.T) {
	t.Run("check", func(t *testing.T) {
		var got neverbounce.CheckEmailResult
		require.NoError(t, json.Unmarshal([]byte(`{"status":"success","result":"valid","future":{"a":[1,2]},`+
			`"address_info":{"host":"example.com","geo":"x"},"credits_info":{"paid_credits_used":1,"bonus":5}}`), &got))
		assert.Equal(t, neverbounce.CheckEmailResult{
			Result:      neverbounce.ResultValid,
			AddressInfo: &neverbounce.AddressInfo{Host: "example.com"},
			CreditsInfo: &neverbounce.CreditsInfo{PaidCreditsUsed: new(int64(1))},
		}, got)
	})
	t.Run("create", func(t *testing.T) {
		var got neverbounce.CreateJobResult
		require.NoError(t, json.Unmarshal([]byte(`{"status":"success","job_id":7,"input_location":"supplied"}`), &got))
		assert.Equal(t, neverbounce.CreateJobResult{JobID: 7}, got)
	})
	t.Run("parse and start", func(t *testing.T) {
		const stored = `{"queue_id":"NB-PQ-0001","job_id":7,"later":[null]}`
		var parsed neverbounce.ParseJobResult
		require.NoError(t, json.Unmarshal([]byte(stored), &parsed))
		assert.Equal(t, neverbounce.ParseJobResult{QueueID: "NB-PQ-0001"}, parsed)
		var started neverbounce.StartJobResult
		require.NoError(t, json.Unmarshal([]byte(stored), &started))
		assert.Equal(t, neverbounce.StartJobResult{QueueID: "NB-PQ-0001"}, started)
	})
	t.Run("status", func(t *testing.T) {
		var got neverbounce.JobStatus
		require.NoError(t, json.Unmarshal([]byte(`{"id":42,"job_status":"running","query":{"job_id":42},`+
			`"total":{"billable":3,"skipped":9},"type":"legacy"}`), &got))
		assert.Equal(t, neverbounce.JobStatus{
			ID: 42, State: neverbounce.JobStateRunning, Totals: neverbounce.JobTotals{Billable: new(int64(3))},
		}, got)
	})
	t.Run("results page", func(t *testing.T) {
		var got neverbounce.JobResultsPage
		require.NoError(t, json.Unmarshal([]byte(`{"page":2,"total_results":1,"total_pages":2,"query":{"page":0},`+
			`"results":[{"data":{"id":"1"},"meta":true,"verification":{"result":"valid","score":9}}]}`), &got))
		assert.Equal(t, neverbounce.JobResultsPage{
			Page: 2, TotalResults: 1, TotalPages: 2,
			Results: []neverbounce.ResultRow{{
				Data:         json.RawMessage(`{"id":"1"}`),
				Verification: neverbounce.RowVerification{Result: neverbounce.ResultValid},
			}},
		}, got)
	})
}

// TestRoundTripResultIsTextNotStatusInt keeps the JSON verdict, a string,
// apart from the CSV download's email_status_int column, a number whose
// mapping is undocumented: neither the SDK's decoder nor encoding/json turns
// a number into a Result.
func TestRoundTripResultIsTextNotStatusInt(t *testing.T) {
	data, err := json.Marshal(neverbounce.CheckEmailResult{Result: neverbounce.ResultValid})
	require.NoError(t, err)
	assertBody(t, `{"result":"valid"}`, data)

	var stored neverbounce.CheckEmailResult
	err = json.Unmarshal([]byte(`{"result":0}`), &stored)
	ute := testutil.RequireErrorAs[*json.UnmarshalTypeError](t, err)
	assert.Equal(t, "number", ute.Value)

	_, err = callOnce(t, pathCheck, `{"status":"success","result":0}`, exCheck)
	assertCallError(t, err, wantCall{
		Kind: neverbounce.FailureMalformedResponse, Op: neverbounce.OpCheckEmail, Status: http.StatusOK, Field: "result",
	})
}
