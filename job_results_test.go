package neverbounce_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// jbResultsJob is the job the results tests read.
const jbResultsJob neverbounce.JobID = 1001

// jbFieldResults is the results member, the Field of a page-level failure.
const jbFieldResults = "results"

// jbMinimalRow is the smallest valid results row.
const jbMinimalRow = `{"verification":{"result":"valid"}}`

// jbResultsAnswer returns a jobs/results success answer with the given totals
// and rows.
func jbResultsAnswer(totalResults, totalPages int, rows ...string) string {
	return `{"status":"success","total_results":` + strconv.Itoa(totalResults) + `,"total_pages":` +
		strconv.Itoa(totalPages) + `,"query":{"page":0},"results":[` + strings.Join(rows, ",") + `]}`
}

// jbRepeatRows returns n copies of row.
func jbRepeatRows(row string, n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = row
	}
	return rows
}

// jbGetFirstPage reads page 1 of jbResultsJob at the default page size: the
// call most results tests make through callOnce.
func jbGetFirstPage(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobResultsPage, error) {
	return c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: jbResultsJob})
}

// jbGetPage returns the call that reads the page req asks for, for the results
// tests that make another request through callOnce.
func jbGetPage(req neverbounce.JobResultsRequest,
) func(context.Context, *neverbounce.Client) (*neverbounce.JobResultsPage, error) {
	return func(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobResultsPage, error) {
		return c.JobResults(ctx, req)
	}
}

// jbResultsFailure returns the CallError of a jobs/results answer for page 1
// of jbResultsJob, received with HTTP 200, that fails to decode with kind at
// field.
func jbResultsFailure(kind neverbounce.FailureKind, field string) wantCall {
	return wantCall{
		Kind: kind, Op: neverbounce.OpJobResults, Status: http.StatusOK, JobID: jbResultsJob, Page: 1, Field: field,
	}
}

func TestJobResultsWireBodies(t *testing.T) {
	tests := []struct {
		name string
		req  neverbounce.JobResultsRequest
		want string
	}{
		{"defaults not sent", neverbounce.JobResultsRequest{JobID: 7}, `,"job_id":7}`},
		{"page only", neverbounce.JobResultsRequest{JobID: 7, Page: 2}, `,"job_id":7,"page":2}`},
		{"items per page only", neverbounce.JobResultsRequest{JobID: 7, ItemsPerPage: 500}, `,"job_id":7,"items_per_page":500}`},
		{"both", neverbounce.JobResultsRequest{JobID: 7, Page: 3, ItemsPerPage: 1000}, `,"job_id":7,"page":3,"items_per_page":1000}`},
		{
			"largest page",
			neverbounce.JobResultsRequest{JobID: 7, Page: math.MaxInt32, ItemsPerPage: 1},
			`,"job_id":7,"page":2147483647,"items_per_page":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Body: jbResultsAnswer(0, 0)})
			_, err := c.JobResults(t.Context(), tt.req)
			require.NoError(t, err)
			r := testutil.OnlyRequest(t, s)
			assertWireRequest(t, r, pathResults, mediaJSON)
			assertBody(t, keyMember+tt.want, r.Body)
		})
	}
}

// TestJobResultsPageIsTheRequestedPage proves that Page is the page the SDK
// asked for, never the provider's query echo, whose documented example says
// page 0.
func TestJobResultsPageIsTheRequestedPage(t *testing.T) {
	for _, tt := range []struct{ requested, want int }{{0, 1}, {1, 1}, {2, 2}, {3, 3}, {math.MaxInt32, math.MaxInt32}} {
		t.Run(strconv.Itoa(tt.requested), func(t *testing.T) {
			body := `{"status":"success","total_results":30,"total_pages":3,"page":0,` +
				`"query":{"job_id":1001,"page":0,"items_per_page":10},"results":[]}`
			req := neverbounce.JobResultsRequest{JobID: jbResultsJob, Page: tt.requested}
			p, err := callOnce(t, pathResults, body, jbGetPage(req))
			require.NoError(t, err)
			assert.Equal(t, tt.want, p.Page)
		})
	}
}

func TestJobResultsDecodesFixture(t *testing.T) {
	p, err := callOnce(t, pathResults, string(testutil.Fixture(t, "synthetic/results_page.json")), jbGetFirstPage)
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.JobResultsPage{
		Page:         1,
		TotalResults: 3,
		TotalPages:   1,
		Results: []neverbounce.ResultRow{
			{
				Data: json.RawMessage(`{"id":"id","email":"email"}`),
				Verification: neverbounce.RowVerification{
					Result: neverbounce.ResultInvalid, AddressInfo: &neverbounce.AddressInfo{OriginalEmail: jbColEmail},
				},
			},
			{
				Data: json.RawMessage(`{"id":"1001","email":"avery.example@example.com"}`),
				Verification: neverbounce.RowVerification{
					Result: neverbounce.ResultValid,
					Flags:  neverbounce.Flags{neverbounce.FlagHasDNS, neverbounce.FlagHasDNSMX, neverbounce.FlagSMTPConnectable},
				},
			},
			{
				Data: json.RawMessage(`{"id":1002,"email":"blake.example@example.org"}`),
				Verification: neverbounce.RowVerification{
					Result: neverbounce.ResultCatchall,
					Flags:  neverbounce.Flags{neverbounce.FlagHasDNS, neverbounce.FlagAcceptsAll},
				},
			},
		},
		ExecutionTime: new(int64(18)),
	}, p)
	assert.False(t, p.More())

	header, ok := p.Results[0].Column(jbColEmail)
	assert.True(t, ok)
	assert.Equal(t, jbColEmail, header, "the header row comes back as a record")
	id, ok := p.Results[2].Column("id")
	assert.True(t, ok)
	assert.Equal(t, "1002", id, "a number comes back as its literal text")

	// A JSON round trip keeps the page.
	assertRoundTrip(t, p, `{"page":1,"total_results":3,"total_pages":1,"results":[`+
		`{"data":{"id":"id","email":"email"},"verification":{"result":"invalid","address_info":{"original_email":"email"}}},`+
		`{"data":{"id":"1001","email":"`+testEmail+`"},`+
		`"verification":{"result":"valid","flags":["has_dns","has_dns_mx","smtp_connectable"]}},`+
		`{"data":{"id":1002,"email":"blake.example@example.org"},`+
		`"verification":{"result":"catchall","flags":["has_dns","accepts_all"]}}],"execution_time":18}`)
}

func TestJobResultsKeepsProviderOrderAndDuplicates(t *testing.T) {
	row := func(id string) string {
		return `{"data":{"id":"` + id + `","email":"` + testEmail + `"},"verification":{"result":"valid"}}`
	}
	p, err := callOnce(t, pathResults, jbResultsAnswer(5, 1, row("3"), row("1"), row("1"), row("2"), row("1")),
		jbGetPage(neverbounce.JobResultsRequest{JobID: jbResultsJob, ItemsPerPage: 5}))
	require.NoError(t, err)
	ids := make([]string, 0, len(p.Results))
	for _, r := range p.Results {
		id, ok := r.Column("id")
		require.True(t, ok)
		ids = append(ids, id)
	}
	assert.Equal(t, []string{"3", "1", "1", "2", "1"}, ids, "nothing dropped, reordered or deduplicated")
}

func TestJobResultsEmptyPage(t *testing.T) {
	req := neverbounce.JobResultsRequest{JobID: jbResultsJob, Page: 2}
	p, err := callOnce(t, pathResults, jbResultsAnswer(0, 0), jbGetPage(req))
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.JobResultsPage{Page: 2}, p)
	assert.Nil(t, p.Results, "an empty page is nil")
}

func TestJobResultsNormalizesData(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"whitespace removed", "{ \"id\" : \"1\" ,\n\t\"n\" : [ 1 , 2 ] }", `{"id":"1","n":[1,2]}`},
		{"HTML characters escaped", `{"note":"<b>&</b>"}`, testutil.Esc(`{"note":"~u003cb~u003e~u0026~u003c/b~u003e"}`)},
		{"line separators escaped", `{"note":"` + jbLineSeps + `"}`, testutil.Esc(`{"note":"~u2028 ~u2029"}`)},
		{"escapes kept as sent", testutil.Esc(`{"~u0069d":"a~"b~/c~n"}`), testutil.Esc(`{"~u0069d":"a~"b~/c~n"}`)},
		{"non-ASCII kept", `{"name":"` + jbUnicode + `"}`, `{"name":"` + jbUnicode + `"}`},
		{"numbers kept as sent", `{"a":1.50,"b":-0,"c":1E+2}`, `{"a":1.50,"b":-0,"c":1E+2}`},
		{"nested values", `{"a":{"b":[true,false,null]}}`, `{"a":{"b":[true,false,null]}}`},
		{"array data", `[ "1001" , "` + testEmail + `" ]`, `["1001","` + testEmail + `"]`},
		{"empty object", `{ }`, `{}`},
		{"empty array", `[ ]`, `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oracle, err := json.Marshal(json.RawMessage(tt.raw))
			require.NoError(t, err)
			require.Equal(t, tt.want, string(oracle), "the expectation is what encoding/json produces")
			p, err := callOnce(t, pathResults, jbResultsAnswer(1, 1, `{"data":`+tt.raw+`,"verification":{"result":"valid"}}`),
				jbGetFirstPage)
			require.NoError(t, err)
			require.Len(t, p.Results, 1)
			assertBody(t, tt.want, p.Results[0].Data)
		})
	}

	t.Run("lone surrogate escapes become the replacement character", func(t *testing.T) {
		p, err := callOnce(t, pathResults, jbResultsAnswer(1, 1,
			testutil.Esc(`{"data":{"a":"~ud800x","b":"~ud83d~udcec"},"verification":{"result":"valid"}}`)), jbGetFirstPage)
		require.NoError(t, err)
		assertBody(t, testutil.Esc(`{"a":"~ufffdx","b":"~ud83d~udcec"}`), p.Results[0].Data)
	})

	for _, absent := range []string{`{"verification":{"result":"valid"}}`, `{"data":null,"verification":{"result":"valid"}}`} {
		t.Run("absent or null data "+absent[:8], func(t *testing.T) {
			p, err := callOnce(t, pathResults, jbResultsAnswer(1, 1, absent), jbGetFirstPage)
			require.NoError(t, err)
			assert.Nil(t, p.Results[0].Data)
		})
	}

	t.Run("data of exactly 64 KiB", func(t *testing.T) {
		raw := `{"a":"` + strings.Repeat("x", 64<<10-8) + `"}`
		body := jbResultsAnswer(1, 1, `{"data":`+raw+`,"verification":{"result":"valid"}}`)
		p, err := callOnce(t, pathResults, body, jbGetFirstPage)
		require.NoError(t, err)
		assert.Len(t, p.Results[0].Data, 64<<10)
	})
	for _, tt := range []struct{ name, raw string }{
		{"data over 64 KiB", `{"a":"` + strings.Repeat("x", 64<<10-7) + `"}`},
		{"data over 64 KiB once escaped", `{"a":"` + strings.Repeat("<", 11<<10) + `"}`},
		{"data padded far beyond 64 KiB", `{"a":` + strings.Repeat(" ", 256<<10) + `1}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callOnce(t, pathResults, jbResultsAnswer(1, 1, `{"data":`+tt.raw+`,"verification":{"result":"valid"}}`),
				jbGetFirstPage)
			assertCallError(t, err, jbResultsFailure(neverbounce.FailureResponseTooLarge, "results[0].data"))
		})
	}
}

func TestResultRowColumn(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		col   string
		want  string
		found bool
	}{
		{"string value", `{"id":"1001","email":"` + testEmail + `"}`, jbColEmail, testEmail, true},
		{"number literal", `{"id":1002}`, "id", "1002", true},
		{"negative number literal", `{"n":-3.5e2}`, "n", "-3.5e2", true},
		{"escaped value decoded", testutil.Esc(`{"note":"a~"b~u003c~u00e9"}`), "note", "a\"b<\xc3\xa9", true},
		{"escaped key matched", testutil.Esc(`{"~u0069d":"x"}`), "id", "x", true},
		{"empty value", `{"id":""}`, "id", "", true},
		{"empty name", `{"":"v"}`, "", "v", true},
		{"spaced object", `{ "a" : 1 , "id" : "x" }`, "id", "x", true},
		{"missing name", `{"id":"1"}`, jbColEmail, "", false},
		{"exact case only", `{"ID":"x","Id":"y"}`, "id", "", false},
		{"exact case among variants", `{"ID":"x","id":"y"}`, "id", "y", true},
		{"nested object", `{"id":{"v":"1"}}`, "id", "", false},
		{"array value", `{"id":["1"]}`, "id", "", false},
		{"boolean", `{"id":true}`, "id", "", false},
		{"json null", `{"id":null}`, "id", "", false},
		{"array data", `["id","1001"]`, "id", "", false},
		{"string data", `"id"`, "id", "", false},
		{"no data", ``, "id", "", false},
		{"member without a value", `{"id":}`, "id", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var row neverbounce.ResultRow
			if tt.data != "" {
				row.Data = json.RawMessage(tt.data)
			}
			got, ok := row.Column(tt.col)
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestJobResultsRowLimits(t *testing.T) {
	tests := []struct {
		name string
		ipp  int
		rows int
		ok   bool
	}{
		{"as many rows as items per page", 2, 2, true},
		{"one row more than items per page", 2, 3, false},
		{"one row with items per page 1", 1, 1, true},
		{"two rows with items per page 1", 1, 2, false},
		{"1000 rows with items per page unset", 0, 1000, true},
		{"1001 rows with items per page unset", 0, 1001, false},
		{"1001 rows with items per page 1000", 1000, 1001, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := callOnce(t, pathResults, jbResultsAnswer(5000, 5, jbRepeatRows(jbMinimalRow, tt.rows)...),
				jbGetPage(neverbounce.JobResultsRequest{JobID: jbResultsJob, ItemsPerPage: tt.ipp}))
			if tt.ok {
				require.NoError(t, err)
				assert.Len(t, p.Results, tt.rows)
				return
			}
			assertCallError(t, err, jbResultsFailure(neverbounce.FailureUnexpectedResponse, jbFieldResults))
		})
	}
}

func TestJobResultsMalformedAnswers(t *testing.T) {
	head := `{"status":"success","total_results":1,"total_pages":1`
	row := func(r string) string { return head + `,"results":[` + jbMinimalRow + `,` + r + `]}` }
	verification := func(v string) string { return row(`{"data":{},"verification":` + v + `}`) }
	tests := []struct {
		name  string
		body  string
		kind  neverbounce.FailureKind
		field string
	}{
		{"total_results missing", `{"status":"success","total_pages":1,"results":[]}`, jbMalformed, "total_results"},
		{"total_results null", `{"status":"success","total_results":null,"total_pages":1,"results":[]}`, jbMalformed, "total_results"},
		{"total_results quoted", `{"status":"success","total_results":"1","total_pages":1,"results":[]}`, jbMalformed, "total_results"},
		{"total_results negative", `{"status":"success","total_results":-1,"total_pages":1,"results":[]}`, jbMalformed, "total_results"},
		{"total_results fraction", `{"status":"success","total_results":1.5,"total_pages":1,"results":[]}`, jbMalformed, "total_results"},
		{"total_pages missing", `{"status":"success","total_results":1,"results":[]}`, jbMalformed, "total_pages"},
		{"total_pages null", `{"status":"success","total_results":1,"total_pages":null,"results":[]}`, jbMalformed, "total_pages"},
		{
			"total_pages overflow", `{"status":"success","total_results":1,"total_pages":9223372036854775808,"results":[]}`,
			jbMalformed, "total_pages",
		},
		{"results missing", head + `}`, jbMalformed, jbFieldResults},
		{"results null", head + `,"results":null}`, jbMalformed, jbFieldResults},
		{"results object", head + `,"results":{"0":{}}}`, jbMalformed, jbFieldResults},
		{"results string", head + `,"results":"[]"}`, jbMalformed, jbFieldResults},
		{"other case only", head + `,"Results":[]}`, jbMalformed, jbFieldResults},
		{"row number", row(`5`), jbMalformed, "results[1]"},
		{"row string", row(`"x"`), jbMalformed, "results[1]"},
		{"row array", row(`[]`), jbMalformed, "results[1]"},
		{"row null", row(`null`), jbMalformed, "results[1]"},
		{"data string", row(`{"data":"1001","verification":{"result":"valid"}}`), jbMalformed, "results[1].data"},
		{"data number", row(`{"data":1001,"verification":{"result":"valid"}}`), jbMalformed, "results[1].data"},
		{"data boolean", row(`{"data":true,"verification":{"result":"valid"}}`), jbMalformed, "results[1].data"},
		{"verification missing", row(`{"data":{}}`), jbMalformed, "results[1].verification"},
		{"verification null", verification(`null`), jbMalformed, "results[1].verification"},
		{"verification array", verification(`[]`), jbMalformed, "results[1].verification"},
		{"verification string", verification(`"valid"`), jbMalformed, "results[1].verification"},
		{"result missing", verification(`{"flags":[]}`), jbMalformed, "results[1].verification.result"},
		{"result null", verification(`{"result":null}`), jbMalformed, "results[1].verification.result"},
		{"result number", verification(`{"result":0}`), jbMalformed, "results[1].verification.result"},
		{"result other case only", verification(`{"Result":"valid"}`), jbMalformed, "results[1].verification.result"},
		{"result uppercase", verification(`{"result":"VALID"}`), jbUnexpected, "results[1].verification.result"},
		{"flags object", verification(`{"result":"valid","flags":{}}`), jbMalformed, "results[1].verification.flags"},
		{
			"flag number", verification(`{"result":"valid","flags":["has_dns",5]}`), jbMalformed,
			"results[1].verification.flags[1]",
		},
		{
			"flag uppercase", verification(`{"result":"valid","flags":["Has_DNS"]}`), jbUnexpected,
			"results[1].verification.flags[0]",
		},
		{"65 flags", verification(`{"result":"valid","flags":[` +
			strings.TrimSuffix(strings.Repeat(`"has_dns",`, 65), ",") + `]}`), jbTooLarge, "results[1].verification.flags"},
		{
			"suggested_correction number", verification(`{"result":"valid","suggested_correction":1}`), jbMalformed,
			"results[1].verification.suggested_correction",
		},
		{"suggested_correction over 4 KiB", verification(`{"result":"valid","suggested_correction":"` +
			strings.Repeat("s", 4097) + `"}`), jbTooLarge, "results[1].verification.suggested_correction"},
		{
			"address_info array", verification(`{"result":"valid","address_info":[]}`), jbMalformed,
			"results[1].verification.address_info",
		},
		{
			"address_info member number", verification(`{"result":"valid","address_info":{"host":5}}`), jbMalformed,
			"results[1].verification.address_info.host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callOnce(t, pathResults, tt.body, jbGetFirstPage)
			assertCallError(t, err, jbResultsFailure(tt.kind, tt.field))
		})
	}
}

// Failure kinds the results tables repeat.
const (
	jbMalformed  = neverbounce.FailureMalformedResponse
	jbUnexpected = neverbounce.FailureUnexpectedResponse
	jbTooLarge   = neverbounce.FailureResponseTooLarge
)

func TestJobResultsVerificationDecoded(t *testing.T) {
	p, err := callOnce(t, pathResults, jbResultsAnswer(1, 1,
		`{"data":["x"],"verification":{"result":"disposable","flags":["disposable_email","acedemic_host",`+
			`"disposable_email"],"suggested_correction":"avery@example.org","address_info":{"original_email":"a",`+
			`"normalized_email":"b","addr":"c","alias":"d","host":"e","fqdn":"f","domain":"g","subdomain":"h",`+
			`"tld":"i"},"credits_info":{"paid_credits_used":1},"Result":"valid"}}`), jbGetFirstPage)
	require.NoError(t, err)
	assert.Equal(t, neverbounce.RowVerification{
		Result:              neverbounce.ResultDisposable,
		Flags:               neverbounce.Flags{neverbounce.FlagDisposableEmail, "acedemic_host", neverbounce.FlagDisposableEmail},
		SuggestedCorrection: "avery@example.org",
		AddressInfo: &neverbounce.AddressInfo{
			OriginalEmail: "a", NormalizedEmail: "b", Addr: "c", Alias: "d", Host: "e", FQDN: "f",
			Domain: "g", Subdomain: "h", TLD: "i",
		},
	}, p.Results[0].Verification)

	for _, extra := range []string{`,"flags":[]`, `,"flags":null`, ``} {
		p, err := callOnce(t, pathResults, jbResultsAnswer(1, 1,
			`{"verification":{"result":"unknown"`+extra+`,"address_info":null,"suggested_correction":null}}`), jbGetFirstPage)
		require.NoError(t, err)
		assert.Equal(t, neverbounce.RowVerification{Result: neverbounce.ResultUnknown}, p.Results[0].Verification, extra)
	}
}

func TestJobResultsPageMore(t *testing.T) {
	tests := []struct {
		page       int
		totalPages int64
		more       bool
	}{
		{1, 3, true},
		{2, 3, true},
		{3, 3, false},
		{4, 3, false},
		{1, 1, false},
		{1, 0, false},
	}
	for _, tt := range tests {
		p := &neverbounce.JobResultsPage{Page: tt.page, TotalPages: tt.totalPages}
		assert.Equal(t, tt.more, p.More(), "page %d of %d", tt.page, tt.totalPages)
	}
	var none *neverbounce.JobResultsPage
	assert.False(t, none.More(), "a nil page has no successor")
}

// TestJobResultsValidation checks JobResults' own rules. JobID and the checks
// every method shares are in client_test.go.
func TestJobResultsValidation(t *testing.T) {
	overInt32 := math.MaxInt32
	overInt32++
	tests := []struct {
		name   string
		req    neverbounce.JobResultsRequest
		field  string
		reason string
	}{
		{"negative page", neverbounce.JobResultsRequest{JobID: 1, Page: -1}, "Page", "out_of_range"},
		{"page beyond int32", neverbounce.JobResultsRequest{JobID: 1, Page: overInt32}, "Page", "out_of_range"},
		{"negative items per page", neverbounce.JobResultsRequest{JobID: 1, ItemsPerPage: -1}, "ItemsPerPage", "out_of_range"},
		{"1001 items per page", neverbounce.JobResultsRequest{JobID: 1, ItemsPerPage: 1001}, "ItemsPerPage", "out_of_range"},
	}
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, jbResultsAnswer(0, 0))}
	c := newTransportClient(t, ct)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := c.JobResults(t.Context(), tt.req)
			assert.Nil(t, p)
			assertValidation(t, err, tt.field, tt.reason)
		})
	}
	assert.Zero(t, ct.Attempts())
}
