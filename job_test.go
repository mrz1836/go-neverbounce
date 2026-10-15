package neverbounce_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Shared values of the bulk-job tests.
const (
	// jbLeak is a leak marker placed inside rejected values: no error may ever
	// contain it.
	jbLeak = "jbleakmarker5e1d" // gitleaks:allow (synthetic test marker)

	jbColEmail       = "email"
	jbFieldRemoteURL = "RemoteURL"

	// jbCreateOK is a successful jobs/create answer for job 4242.
	jbCreateOK = `{"status":"success","job_id":4242}`

	// jbSuppliedHead opens the input of a supplied create body.
	jbSuppliedHead = `,"input_location":"supplied","input":[`

	// jbBoolsOff is the boolean members of a create body whose booleans are
	// all false: each is always sent.
	jbBoolsOff = `,"auto_parse":false,"auto_start":false,"run_sample":false,"allow_manual_review":false`

	// jbUnicode mixes two- and four-byte UTF-8, sent verbatim.
	jbUnicode = "caf\xc3\xa9 na\xc3\xafve \xe2\x98\x95 \xf0\x9f\x93\xac"

	// jbLineSeps are U+2028 and U+2029: valid JSON string content, sent raw.
	jbLineSeps = "\xe2\x80\xa8 \xe2\x80\xa9"

	jbRemoteURL   = "https://list-user:p%40ss@example.com/exports/list.csv?sig=" + jbLeak + "&exp=1"
	jbCallbackURL = "https://hooks:" + jbLeak + "@example.net/neverbounce?x=1"
)

// jbSuppliedRequest returns a minimal valid supplied create request.
func jbSuppliedRequest() neverbounce.CreateJobRequest {
	return neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList(testEmail)}
}

// jbBools returns the boolean members of a create body.
func jbBools(parse, start, sample, review bool) string {
	return `,"auto_parse":` + strconv.FormatBool(parse) + `,"auto_start":` + strconv.FormatBool(start) +
		`,"run_sample":` + strconv.FormatBool(sample) + `,"allow_manual_review":` + strconv.FormatBool(review)
}

// jbSizedList returns a one-column supplied list whose create body, with
// every boolean false and nothing else set, is exactly size bytes long.
func jbSizedList(tb testing.TB, size int) neverbounce.SuppliedList {
	tb.Helper()
	const rowOverhead = len(`{"c":""}`)
	full := strings.Repeat("v", 4096)
	remaining := size - len(keyMember+jbSuppliedHead) - len("]"+jbBoolsOff+"}")
	var rows [][]string
	for {
		sep := min(len(rows), 1)
		last := remaining - sep - rowOverhead
		if last <= len(full) {
			require.GreaterOrEqual(tb, last, 0)
			rows = append(rows, []string{full[:last]})
			break
		}
		// Leave room for at least one more, possibly empty, row.
		n := min(len(full), last-1-rowOverhead)
		rows = append(rows, []string{full[:n]})
		remaining -= sep + rowOverhead + n
	}
	return neverbounce.SuppliedList{Columns: []string{"c"}, Rows: rows}
}

func TestJobIDString(t *testing.T) {
	assert.Equal(t, "289022", neverbounce.JobID(289022).String())
	assert.Equal(t, "9223372036854775807", neverbounce.JobID(9223372036854775807).String())
	assert.Equal(t, "0", neverbounce.JobID(0).String())
}

func TestCreateJobWireBodies(t *testing.T) {
	headers := map[string]string{
		"X-Token":       `tok"en\` + jbLeak,
		"Authorization": "Bearer " + jbLeak,
		"x-a":           "caf\xc3\xa9",
		"X-Empty":       "",
	}
	tests := []struct {
		name string
		req  neverbounce.CreateJobRequest
		want string
	}{
		{
			name: "email list with every boolean explicitly false",
			req:  neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList(testEmail, "blake.example@example.org")},
			want: jbSuppliedHead + `{"email":"` + testEmail + `"},{"email":"blake.example@example.org"}]` + jbBoolsOff + `}`,
		},
		{
			name: "columns and values needing escapes",
			req: neverbounce.CreateJobRequest{Supplied: neverbounce.SuppliedList{
				Columns: []string{"id", `say "col"`, `back\slash`},
				Rows: [][]string{
					{"1001", `she said "hi"`, `C:\lists\a.csv`},
					{"", "   ", jbUnicode},
					{"</script>&<b>", jbLineSeps, "'single' /slash/ ~tilde"},
				},
			}},
			want: jbSuppliedHead +
				`{"id":"1001","say \"col\"":"she said \"hi\"","back\\slash":"C:\\lists\\a.csv"},` +
				`{"id":"","say \"col\"":"   ","back\\slash":"` + jbUnicode + `"},` +
				`{"id":"</script>&<b>","say \"col\"":"` + jbLineSeps + `","back\\slash":"'single' /slash/ ~tilde"}]` +
				jbBoolsOff + `}`,
		},
		{
			name: "remote url with userinfo and query",
			req:  neverbounce.CreateJobRequest{RemoteURL: jbRemoteURL},
			want: `,"input_location":"remote_url","input":"` + jbRemoteURL + `"` + jbBoolsOff + `}`,
		},
		{
			name: "remote url with the ftp scheme",
			req:  neverbounce.CreateJobRequest{RemoteURL: "ftp://anon@example.org/pub/list.txt"},
			want: `,"input_location":"remote_url","input":"ftp://anon@example.org/pub/list.txt"` + jbBoolsOff + `}`,
		},
		{
			name: "filename escaped after the input",
			req:  neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList(testEmail), Filename: `signups "final" \ 2026.csv`},
			want: jbSuppliedHead + `{"email":"` + testEmail + `"}],"filename":"signups \"final\" \\ 2026.csv"` +
				jbBoolsOff + `}`,
		},
		{
			name: "run sample only",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv", RunSample: true},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBools(false, false, true, false) + `}`,
		},
		{
			name: "allow manual review only",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv", AllowManualReview: true},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBools(false, false, false, true) + `}`,
		},
		{
			name: "leverage historical data nil is not sent",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv"},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBoolsOff + `}`,
		},
		{
			name: "leverage historical data true",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv", LeverageHistoricalData: new(true)},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBoolsOff +
				`,"request_meta_data":{"leverage_historical_data":true}}`,
		},
		{
			name: "leverage historical data false",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv", LeverageHistoricalData: new(false)},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBoolsOff +
				`,"request_meta_data":{"leverage_historical_data":false}}`,
		},
		{
			name: "callback url without headers",
			req:  neverbounce.CreateJobRequest{RemoteURL: "https://example.com/a.csv", CallbackURL: jbCallbackURL},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBoolsOff +
				`,"callback_url":"` + jbCallbackURL + `"}`,
		},
		{
			name: "callback headers sorted by name with values escaped",
			req: neverbounce.CreateJobRequest{
				RemoteURL: "https://example.com/a.csv", CallbackURL: jbCallbackURL, CallbackHeaders: headers,
			},
			want: `,"input_location":"remote_url","input":"https://example.com/a.csv"` + jbBoolsOff +
				`,"callback_url":"` + jbCallbackURL + `","callback_headers":{"Authorization":"Bearer ` + jbLeak +
				`","X-Empty":"","X-Token":"tok\"en\\` + jbLeak + `","x-a":"caf` + "\xc3\xa9" + `"}}`,
		},
		{
			name: "every member in order",
			req: neverbounce.CreateJobRequest{
				Supplied:  neverbounce.SuppliedList{Columns: []string{"id", jbColEmail}, Rows: [][]string{{"1", testEmail}}},
				Filename:  "every-member.csv",
				AutoParse: true, AutoStart: true, RunSample: true, AllowManualReview: true,
				LeverageHistoricalData: new(false),
				CallbackURL:            "http://example.net/cb",
				CallbackHeaders:        map[string]string{"X-B": "2", "X-A": "1"},
			},
			want: jbSuppliedHead + `{"id":"1","email":"` + testEmail + `"}],"filename":"every-member.csv"` +
				jbBools(true, true, true, true) + `,"request_meta_data":{"leverage_historical_data":false}` +
				`,"callback_url":"http://example.net/cb","callback_headers":{"X-A":"1","X-B":"2"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Body: jbCreateOK})
			res, err := c.CreateJob(t.Context(), tt.req)
			require.NoError(t, err)
			assert.Equal(t, &neverbounce.CreateJobResult{JobID: 4242}, res)
			r := testutil.OnlyRequest(t, s)
			assertWireRequest(t, r, pathCreate, mediaJSON)
			assertBody(t, keyMember+tt.want, r.Body)
		})
	}
}

// TestCreateJobAutoStartMatrix proves that auto_start, which bills a job
// without review, is always sent explicitly: the zero value sends false.
func TestCreateJobAutoStartMatrix(t *testing.T) {
	for _, parse := range []bool{false, true} {
		for _, start := range []bool{false, true} {
			t.Run("auto_parse "+strconv.FormatBool(parse)+" auto_start "+strconv.FormatBool(start), func(t *testing.T) {
				c, s := newScriptClient(t, testutil.Step{Body: jbCreateOK})
				_, err := c.CreateJob(t.Context(), neverbounce.CreateJobRequest{
					Supplied: neverbounce.EmailList(testEmail), AutoParse: parse, AutoStart: start,
				})
				require.NoError(t, err)
				assertBody(t, keyMember+jbSuppliedHead+`{"email":"`+testEmail+`"}]`+
					jbBools(parse, start, false, false)+`}`, testutil.OnlyRequest(t, s).Body)
			})
		}
	}
}

// TestCreateJobSnapshotsRequestBeforeSend changes the request from inside
// the round trip: the bytes on the wire were fixed before any I/O.
func TestCreateJobSnapshotsRequestBeforeSend(t *testing.T) {
	req := neverbounce.CreateJobRequest{
		Supplied:        neverbounce.SuppliedList{Columns: []string{jbColEmail}, Rows: [][]string{{testEmail}}},
		CallbackURL:     "https://example.net/cb",
		CallbackHeaders: map[string]string{"X-Token": "before"},
	}
	var sent []byte
	rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		req.Supplied.Rows[0][0] = "changed@example.org"
		req.Supplied.Columns[0] = "changed"
		req.CallbackHeaders["X-Token"] = "after"
		sent = testutil.Record(r).Body
		return testutil.Response(r, http.StatusOK, testutil.NewBody(jbCreateOK), int64(len(jbCreateOK))), nil
	})
	c := newTransportClient(t, rt)
	_, err := c.CreateJob(t.Context(), req)
	require.NoError(t, err)
	assertBody(t, keyMember+jbSuppliedHead+`{"email":"`+testEmail+`"}]`+jbBoolsOff+
		`,"callback_url":"https://example.net/cb","callback_headers":{"X-Token":"before"}}`, sent)
}

func TestCreateJobRequestSizeLimit(t *testing.T) {
	t.Run("a body of exactly MaxJobRequestBytes is sent once", func(t *testing.T) {
		list := jbSizedList(t, neverbounce.MaxJobRequestBytes)
		c, s := newScriptClient(t, testutil.Step{Body: jbCreateOK})
		res, err := c.CreateJob(t.Context(), neverbounce.CreateJobRequest{Supplied: list})
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobID(4242), res.JobID)
		r := testutil.OnlyRequest(t, s)
		require.Len(t, r.Body, neverbounce.MaxJobRequestBytes)
		assert.Equal(t, int64(neverbounce.MaxJobRequestBytes), r.ContentLength)
		assert.True(t, strings.HasPrefix(string(r.Body), keyMember+jbSuppliedHead+`{"c":"vvv`))
		assert.True(t, strings.HasSuffix(string(r.Body), `"}]`+jbBoolsOff+`}`))
	})

	over := []struct {
		name string
		req  func(t *testing.T) neverbounce.CreateJobRequest
	}{
		{"one byte over in the rows", func(t *testing.T) neverbounce.CreateJobRequest {
			return neverbounce.CreateJobRequest{Supplied: jbSizedList(t, neverbounce.MaxJobRequestBytes+1)}
		}},
		{"a filename pushing the body over", func(t *testing.T) neverbounce.CreateJobRequest {
			return neverbounce.CreateJobRequest{Supplied: jbSizedList(t, neverbounce.MaxJobRequestBytes), Filename: "x"}
		}},
		{"a callback pushing the body over", func(t *testing.T) neverbounce.CreateJobRequest {
			return neverbounce.CreateJobRequest{
				Supplied: jbSizedList(t, neverbounce.MaxJobRequestBytes), CallbackURL: "https://example.net/cb",
			}
		}},
	}
	for _, tt := range over {
		t.Run(tt.name, func(t *testing.T) {
			ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, jbCreateOK)}
			c := newTransportClient(t, ct)
			res, err := c.CreateJob(t.Context(), tt.req(t))
			assert.Nil(t, res)
			assertValidation(t, err, "request", "too_large")
			assert.Equal(t, "neverbounce: invalid request: request: too_large", err.Error())
			assert.Zero(t, ct.Attempts())
		})
	}
}

// jbFarOverList returns a list of a million 4 KiB rows, about 4 GB encoded,
// whose last row holds a control character: an encoder that walked every row
// would report that row instead of the size.
func jbFarOverList() neverbounce.SuppliedList {
	row := []string{strings.Repeat("v", 4096)}
	rows := make([][]string, 1_000_000)
	for i := range rows {
		rows[i] = row
	}
	rows[len(rows)-1] = []string{"bad\x01" + jbLeak}
	return neverbounce.SuppliedList{Columns: []string{"c"}, Rows: rows}
}

func TestCreateJobFarOverLimitStopsEarly(t *testing.T) {
	req := neverbounce.CreateJobRequest{Supplied: jbFarOverList()}
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, jbCreateOK)}
	c := newTransportClient(t, ct)

	_, err := c.CreateJob(t.Context(), req)
	assertValidation(t, err, "request", "too_large")
	assertErrorClean(t, err, jbLeak, testKey)
	assert.Zero(t, ct.Attempts())

	t.Run("allocation stays near the limit, not the list size", func(t *testing.T) {
		testutil.SkipUnderRace(t)
		grown := testutil.HeapGrowth(func() {
			_, _ = c.CreateJob(t.Context(), req) // the outcome is asserted above
		})
		t.Logf("one rejected call allocated %d bytes for a list of about 4.1e9 encoded bytes", grown)
		assert.Less(t, grown, uint64(4*neverbounce.MaxJobRequestBytes))
	})
}

// jbInvalidCreate is one CreateJob request that local validation refuses.
type jbInvalidCreate struct {
	name   string
	req    neverbounce.CreateJobRequest
	field  string
	reason string
}

// jbWithColumns returns a supplied request with columns and one row of
// matching length.
func jbWithColumns(cols ...string) neverbounce.CreateJobRequest {
	return neverbounce.CreateJobRequest{Supplied: neverbounce.SuppliedList{
		Columns: cols, Rows: [][]string{make([]string, len(cols))},
	}}
}

// jbWithRows returns a supplied request with the columns id and email and rows.
func jbWithRows(rows ...[]string) neverbounce.CreateJobRequest {
	return neverbounce.CreateJobRequest{Supplied: neverbounce.SuppliedList{Columns: []string{"id", jbColEmail}, Rows: rows}}
}

// jbWithRemote returns a remote-url request for raw.
func jbWithRemote(raw string) neverbounce.CreateJobRequest {
	return neverbounce.CreateJobRequest{RemoteURL: raw}
}

// jbWithCallback returns a valid request with a callback URL and headers.
func jbWithCallback(rawURL string, headers map[string]string) neverbounce.CreateJobRequest {
	r := jbSuppliedRequest()
	r.CallbackURL, r.CallbackHeaders = rawURL, headers
	return r
}

// jbWithFilename returns a valid request with the filename name.
func jbWithFilename(name string) neverbounce.CreateJobRequest {
	r := jbSuppliedRequest()
	r.Filename = name
	return r
}

// jbNumbered returns n distinct strings prefix0, prefix1, ...
func jbNumbered(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + strconv.Itoa(i)
	}
	return out
}

// jbHeaders returns n distinct valid callback headers.
func jbHeaders(n int) map[string]string {
	h := make(map[string]string, n)
	for _, name := range jbNumbered("X-H", n) {
		h[name] = jbLeak
	}
	return h
}

// jbInputCases are the create requests refused for their input selection or
// their supplied list.
func jbInputCases() []jbInvalidCreate {
	long := func(n int) string { return jbLeak + strings.Repeat("x", n-len(jbLeak)) }
	return []jbInvalidCreate{
		{"supplied and remote url", neverbounce.CreateJobRequest{
			Supplied: neverbounce.EmailList(jbLeak), RemoteURL: "https://example.com/" + jbLeak,
		}, jbFieldRemoteURL, "conflict"},
		{"rows without columns and a remote url", neverbounce.CreateJobRequest{
			Supplied: neverbounce.SuppliedList{Rows: [][]string{{jbLeak}}}, RemoteURL: "https://example.com/a",
		}, jbFieldRemoteURL, "conflict"},
		{"no input", neverbounce.CreateJobRequest{Filename: jbLeak}, "Supplied", "required"},
		{"rows without columns", neverbounce.CreateJobRequest{
			Supplied: neverbounce.SuppliedList{Rows: [][]string{{jbLeak}}},
		}, "Supplied.Columns", "required"},
		{"129 columns", jbWithColumns(jbNumbered(jbLeak, 129)...), "Supplied.Columns", "too_many"},
		{"column of 257 bytes", jbWithColumns("id", long(257)), "Supplied.Columns[1]", "too_long"},
		{"empty column", jbWithColumns("id", ""), "Supplied.Columns[1]", "required"},
		{"blank column", jbWithColumns("   "), "Supplied.Columns[0]", "blank"},
		{"column with a control character", jbWithColumns(jbLeak + "\tx"), "Supplied.Columns[0]", "control_character"},
		{"column with a C1 control character", jbWithColumns(jbLeak + "\xc2\x85"), "Supplied.Columns[0]", "control_character"},
		{"column with invalid UTF-8", jbWithColumns(jbLeak + "\xff"), "Supplied.Columns[0]", "invalid_utf8"},
		{
			"duplicate column ignoring ASCII case", jbWithColumns("id", "Email"+jbLeak, "EMAIL"+strings.ToUpper(jbLeak)),
			"Supplied.Columns[2]", "duplicate",
		},
		{"exact duplicate column", jbWithColumns(jbLeak, jbLeak), "Supplied.Columns[1]", "duplicate"},
		{"no rows", neverbounce.CreateJobRequest{
			Supplied: neverbounce.SuppliedList{Columns: []string{jbLeak}},
		}, "Supplied.Rows", "required"},
		{"empty email list", neverbounce.CreateJobRequest{Supplied: neverbounce.EmailList()}, "Supplied.Rows", "required"},
		{
			"short row", jbWithRows([]string{"1", testEmail}, []string{"2", jbLeak}, []string{jbLeak}),
			"Supplied.Rows[2]", "length_mismatch",
		},
		{
			"long row", jbWithRows([]string{"1", testEmail}, []string{"2", jbLeak, jbLeak}),
			"Supplied.Rows[1]", "length_mismatch",
		},
		{"empty row", jbWithRows([]string{}), "Supplied.Rows[0]", "length_mismatch"},
		{
			"value with a line break", jbWithRows([]string{"1", testEmail}, []string{"2", jbLeak + "\n"}),
			"Supplied.Rows[1][1]", "control_character",
		},
		{"value with NUL", jbWithRows([]string{jbLeak + "\x00", testEmail}), "Supplied.Rows[0][0]", "control_character"},
		{
			"value with DEL", jbWithRows([]string{"1", testEmail}, []string{"2", jbLeak + "\x7f"}),
			"Supplied.Rows[1][1]", "control_character",
		},
		{
			"value with invalid UTF-8", jbWithRows([]string{"1", testEmail}, []string{jbLeak + "\xc3", "x"}),
			"Supplied.Rows[1][0]", "invalid_utf8",
		},
		{"value of 4097 bytes", jbWithRows([]string{"1", testEmail}, []string{"2", testEmail},
			[]string{"3", long(4097)}), "Supplied.Rows[2][1]", "too_long"},
	}
}

// jbExtraCases are the create requests refused for a URL, the filename or
// the callback.
func jbExtraCases() []jbInvalidCreate {
	long := func(prefix string, n int) string { return prefix + strings.Repeat("a", n-len(prefix)) }
	header := func(name, value string) map[string]string { return map[string]string{name: value} }
	cases := make([]jbInvalidCreate, 0, 64)
	cases = append(cases, []jbInvalidCreate{
		{"relative remote url", jbWithRemote("/exports/" + jbLeak + ".csv"), jbFieldRemoteURL, reasonInvalidURL},
		{"opaque remote url", jbWithRemote("mailto:" + jbLeak + "@example.com"), jbFieldRemoteURL, reasonInvalidURL},
		{"remote url without a host", jbWithRemote("https:///" + jbLeak), jbFieldRemoteURL, reasonInvalidURL},
		{"remote url without a scheme", jbWithRemote("example.com/" + jbLeak), jbFieldRemoteURL, reasonInvalidURL},
		{"remote url that does not parse", jbWithRemote("https://[::1/" + jbLeak), jbFieldRemoteURL, reasonInvalidURL},
		{"remote url with a space", jbWithRemote("https://example.com/" + jbLeak + " a.csv"), jbFieldRemoteURL, reasonInvalidURL},
		{
			"remote url with a no-break space", jbWithRemote("https://example.com/" + jbLeak + "\xc2\xa0"),
			jbFieldRemoteURL, reasonInvalidURL,
		},
		{"remote url of 8193 bytes", jbWithRemote(long("https://example.com/"+jbLeak+"/", 8193)), jbFieldRemoteURL, "too_long"},
		{
			"remote url with a control character", jbWithRemote("https://example.com/" + jbLeak + "\x00"),
			jbFieldRemoteURL, "control_character",
		},
		{"remote url with invalid UTF-8", jbWithRemote("https://example.com/" + jbLeak + "\xff"), jbFieldRemoteURL, "invalid_utf8"},
		{"blank filename", jbWithFilename("   "), "Filename", "blank"},
		{"filename of 256 bytes", jbWithFilename(long(jbLeak, 256)), "Filename", "too_long"},
		{"filename with a control character", jbWithFilename(jbLeak + "\r"), "Filename", "control_character"},
		{"filename with invalid UTF-8", jbWithFilename(jbLeak + "\xfe"), "Filename", "invalid_utf8"},
		{"ftp callback url", jbWithCallback("ftp://example.net/"+jbLeak, nil), "CallbackURL", "unsupported_scheme"},
		{"mailto callback url", jbWithCallback("mailto:"+jbLeak+"@example.net", nil), "CallbackURL", reasonInvalidURL},
		{"relative callback url", jbWithCallback("/hooks/"+jbLeak, nil), "CallbackURL", reasonInvalidURL},
		{"callback url without a host", jbWithCallback("https:///"+jbLeak, nil), "CallbackURL", reasonInvalidURL},
		{"callback url with a space", jbWithCallback("https://example.net/ "+jbLeak, nil), "CallbackURL", reasonInvalidURL},
		{
			"callback url of 2049 bytes", jbWithCallback(long("https://example.net/"+jbLeak+"/", 2049), nil),
			"CallbackURL", "too_long",
		},
		{
			"callback url with a control character", jbWithCallback("https://example.net/"+jbLeak+"\n", nil),
			"CallbackURL", "control_character",
		},
		{
			"callback headers without a callback url", jbWithCallback("", header("X-Token", jbLeak)),
			"CallbackHeaders", "requires_callback_url",
		},
		{"33 callback headers", jbWithCallback(jbCallbackURL, jbHeaders(33)), "CallbackHeaders", "too_many"},
		{
			"header name of 129 bytes", jbWithCallback(jbCallbackURL, header(long("X-"+jbLeak, 129), "v")),
			"CallbackHeaders", "invalid_header_name",
		},
		{"duplicate header names ignoring case", jbWithCallback(jbCallbackURL, map[string]string{
			"X-Token-" + jbLeak: "a", "x-token-" + jbLeak: jbLeak,
		}), "CallbackHeaders", "duplicate"},
		{"X-Token and x-token", jbWithCallback(jbCallbackURL, map[string]string{
			"X-Token": "first-" + jbLeak, "x-token": "second-" + jbLeak,
		}), "CallbackHeaders", "duplicate"},
		{
			"header value of 1025 bytes", jbWithCallback(jbCallbackURL, header("X-Token", long(jbLeak, 1025))),
			"CallbackHeaders", "invalid_header_value",
		},
		{
			"header value with invalid UTF-8", jbWithCallback(jbCallbackURL, header("X-Token", jbLeak+"\xff")),
			"CallbackHeaders", "invalid_header_value",
		},
	}...)
	for _, bad := range []string{
		"", " ", "X " + jbLeak, "X:" + jbLeak, "X-T\xc3\xb6k-" + jbLeak, "(" + jbLeak + ")", "a/" + jbLeak,
		"a@" + jbLeak, `a"` + jbLeak, "a," + jbLeak, "a;" + jbLeak, "a=" + jbLeak, "{" + jbLeak + "}",
		`a\` + jbLeak, "a?" + jbLeak, "[" + jbLeak + "]", jbLeak + "\x7f", jbLeak + "\t",
	} {
		cases = append(cases, jbInvalidCreate{
			"header name " + strconv.Quote(bad),
			jbWithCallback(jbCallbackURL, header(bad, "v")), "CallbackHeaders", "invalid_header_name",
		})
	}
	for _, ctl := range []string{"\r", "\n", "\x00", "\x7f", "\t", "\x1b"} {
		cases = append(cases, jbInvalidCreate{
			"header value with " + strconv.Quote(ctl),
			jbWithCallback(jbCallbackURL, header("X-Token", jbLeak+ctl+"v")), "CallbackHeaders", "invalid_header_value",
		})
	}
	return cases
}

func TestCreateJobRejectsInvalidRequests(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, jbCreateOK)}
	c := newTransportClient(t, ct)
	for _, tt := range append(jbInputCases(), jbExtraCases()...) {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.CreateJob(t.Context(), tt.req)
			assert.Nil(t, res)
			assertValidation(t, err, tt.field, tt.reason)
			assert.Equal(t, "neverbounce: invalid request: "+tt.field+": "+tt.reason, err.Error())
			assertErrorClean(t, err, jbLeak, strings.ToUpper(jbLeak), testKey)
		})
	}
	assert.Zero(t, ct.Attempts(), "no invalid request may reach the network")
}

// TestCreateJobValidationOrder checks the order every method shares: the
// Client, the context, the request, its size, then a done context, and only
// then the network.
// TestCreateJobValidationOrder checks that the size of the encoded body is part
// of validation: a list over MaxJobRequestBytes is refused as such even with a
// done context. The order every method shares (Client, then ctx, then the
// request, then the context's state) is in client_test.go.
func TestCreateJobValidationOrder(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, jbCreateOK)}
	c := newTransportClient(t, ct)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.CreateJob(canceled, neverbounce.CreateJobRequest{Supplied: jbSizedList(t, neverbounce.MaxJobRequestBytes+1)})
	assertValidation(t, err, "request", "too_large")
	assert.Zero(t, ct.Attempts())
}

func TestCreateJobAcceptsBoundaryValues(t *testing.T) {
	repeat := func(prefix string, n int) string { return prefix + strings.Repeat("a", n-len(prefix)) }
	tests := []struct {
		name string
		req  neverbounce.CreateJobRequest
	}{
		{"128 columns", jbWithColumns(jbNumbered("c", 128)...)},
		{"column of 256 bytes", jbWithColumns(repeat("c", 256))},
		{"sharp s and SS are not ASCII duplicates", jbWithColumns("stra\xc3\x9fe", "STRASSE")},
		{"accented letters are not ASCII duplicates", jbWithColumns("\xc3\xa9", "\xc3\x89")},
		{"value of 4096 bytes", jbWithRows([]string{"1", repeat("v", 4096)})},
		{"empty and whitespace values", jbWithRows([]string{"", "   "}, []string{"\xc2\xa0", ""})},
		{"remote url of 8192 bytes", jbWithRemote(repeat("https://example.com/", 8192))},
		{"remote url with any scheme", jbWithRemote("s3://bucket.example.com/list.csv")},
		{"filename of 255 bytes", jbWithFilename(repeat("f", 255))},
		{"filename with spaces around", jbWithFilename(" a.csv ")},
		{"callback url of 2048 bytes", jbWithCallback(repeat("https://example.net/", 2048), nil)},
		{"callback url with an uppercase scheme", jbWithCallback("HTTPS://Example.net/cb", nil)},
		{"callback url over plain http", jbWithCallback("http://user:pw@example.net:8443/cb", nil)},
		{"32 callback headers", jbWithCallback(jbCallbackURL, jbHeaders(32))},
		{"every token character in a header name", jbWithCallback(jbCallbackURL, map[string]string{
			"!#$%&'*+-.^_`|~09azAZ": "v",
		})},
		{"header name of 128 bytes", jbWithCallback(jbCallbackURL, map[string]string{repeat("X", 128): "v"})},
		{"header value of 1024 bytes and an empty one", jbWithCallback(jbCallbackURL, map[string]string{
			"X-A": repeat("v", 1024), "X-B": "",
		})},
		{"header value with non-ASCII text", jbWithCallback(jbCallbackURL, map[string]string{"X-A": jbUnicode})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Body: jbCreateOK})
			res, err := c.CreateJob(t.Context(), tt.req)
			require.NoError(t, err)
			assert.Equal(t, neverbounce.JobID(4242), res.JobID)
			assert.Equal(t, []string{pathCreate}, s.Paths())
		})
	}
}

func TestEmailListCopiesInput(t *testing.T) {
	emails := []string{testEmail, "blake.example@example.org", testEmail}
	list := neverbounce.EmailList(emails...)
	emails[0] = "changed@example.net"
	emails[1] = "other@example.net"
	assert.Equal(t, neverbounce.SuppliedList{
		Columns: []string{jbColEmail},
		Rows:    [][]string{{testEmail}, {"blake.example@example.org"}, {testEmail}},
	}, list, "duplicates are kept, in order")

	list.Rows[0][0] = "row@example.net"
	assert.Equal(t, testEmail, list.Rows[2][0], "each row is its own slice")

	empty := neverbounce.EmailList()
	assert.Equal(t, []string{jbColEmail}, empty.Columns)
	assert.Empty(t, empty.Rows)
}

func TestCreateJobJobIDIsRequired(t *testing.T) {
	tests := []struct {
		name   string
		answer string
	}{
		{"missing", `{"status":"success"}`},
		{"json null", `{"status":"success","job_id":null}`},
		{"zero", `{"status":"success","job_id":0}`},
		{"negative zero", `{"status":"success","job_id":-0}`},
		{"negative", `{"status":"success","job_id":-7}`},
		{"quoted", `{"status":"success","job_id":"4242"}`},
		{"fraction", `{"status":"success","job_id":4242.5}`},
		{"integral fraction", `{"status":"success","job_id":4242.0}`},
		{"exponent", `{"status":"success","job_id":4.242e3}`},
		{"overflow", `{"status":"success","job_id":9223372036854775808}`},
		{"boolean", `{"status":"success","job_id":true}`},
		{"object", `{"status":"success","job_id":{"id":4242}}`},
		{"other case only", `{"status":"success","Job_ID":4242,"jobId":4242}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Body: tt.answer})
			res, err := c.CreateJob(t.Context(), jbSuppliedRequest())
			assert.Nil(t, res)
			// An unreadable create has no job handle: JobID stays 0.
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureMalformedResponse, Op: neverbounce.OpCreateJob, Status: http.StatusOK, Field: "job_id",
			})
			assert.Equal(t, 1, s.Calls())
		})
	}

	t.Run("largest int64", func(t *testing.T) {
		c, _ := newScriptClient(t, testutil.Step{Body: `{"status":"success","job_id":9223372036854775807}`})
		res, err := c.CreateJob(t.Context(), jbSuppliedRequest())
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobID(9223372036854775807), res.JobID)
	})
}

func TestCreateJobHTTP413IsRejected(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		apiStatus neverbounce.APIStatus
	}{
		{"empty body", ``, ""},
		{"html body", `<html><body>413 Request Entity Too Large</body></html>`, ""},
		{"envelope body", `{"status":"general_failure","message":"too large for ` + testKey + `"}`, neverbounce.APIStatusGeneralFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Status: http.StatusRequestEntityTooLarge, Body: tt.body})
			res, err := c.CreateJob(t.Context(), jbSuppliedRequest())
			assert.Nil(t, res)
			assertCallError(t, err, wantCall{
				Kind: neverbounce.FailureRejected, Op: neverbounce.OpCreateJob, Status: http.StatusRequestEntityTooLarge,
				API: tt.apiStatus,
			})
			assert.Equal(t, []string{pathCreate}, s.Paths())
		})
	}
}

func TestCreateJobRequestStringShape(t *testing.T) {
	const tail = ", autoParse: false, autoStart: false, runSample: false, allowManualReview: false, " +
		"leverageHistoricalData: unset, callbackURL: unset, callbackHeaders: 0}"
	supplied := neverbounce.SuppliedList{
		Columns: []string{"id", jbColEmail},
		Rows:    [][]string{{"1", testEmail}, {"2", jbLeak + "@example.org"}, {"3", ""}},
	}
	tests := []struct {
		name string
		req  neverbounce.CreateJobRequest
		want string
	}{
		{
			name: "supplied with every option",
			req: neverbounce.CreateJobRequest{
				Supplied: supplied, Filename: jbLeak + ".csv", AutoParse: true, LeverageHistoricalData: new(false),
				CallbackURL:     jbCallbackURL,
				CallbackHeaders: map[string]string{"X-" + jbLeak: jbLeak, "Authorization": "Bearer " + jbLeak},
			},
			want: "neverbounce.CreateJobRequest{input: supplied, columns: 2, rows: 3, filename: set, autoParse: true, " +
				"autoStart: false, runSample: false, allowManualReview: false, leverageHistoricalData: set, " +
				"callbackURL: set, callbackHeaders: 2}",
		},
		{
			name: "remote url",
			req:  neverbounce.CreateJobRequest{RemoteURL: jbRemoteURL, AutoStart: true, RunSample: true, AllowManualReview: true},
			want: "neverbounce.CreateJobRequest{input: remote_url, columns: 0, rows: 0, filename: unset, autoParse: false, " +
				"autoStart: true, runSample: true, allowManualReview: true, leverageHistoricalData: unset, " +
				"callbackURL: unset, callbackHeaders: 0}",
		},
		{
			name: "both inputs",
			req:  neverbounce.CreateJobRequest{Supplied: supplied, RemoteURL: jbRemoteURL},
			want: "neverbounce.CreateJobRequest{input: supplied+remote_url, columns: 2, rows: 3, filename: unset" + tail,
		},
		{
			name: "rows without columns count as supplied",
			req:  neverbounce.CreateJobRequest{Supplied: neverbounce.SuppliedList{Rows: [][]string{{jbLeak}}}},
			want: "neverbounce.CreateJobRequest{input: supplied, columns: 0, rows: 1, filename: unset" + tail,
		},
		{
			name: "none",
			req:  neverbounce.CreateJobRequest{},
			want: "neverbounce.CreateJobRequest{input: none, columns: 0, rows: 0, filename: unset" + tail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.req.String())
			assert.Equal(t, tt.want, tt.req.GoString())
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				assert.Equal(t, tt.want, fmt.Sprintf(verb, tt.req), verb)
				assert.Equal(t, tt.want, fmt.Sprintf(verb, &tt.req), verb+" on a pointer")
			}
			assert.Equal(t, strconv.Quote(tt.want), fmt.Sprintf("%q", tt.req))
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"} {
				testutil.AssertRedacted(t, fmt.Sprintf(verb, tt.req), []string{jbLeak, testEmail}, verb)
			}
		})
	}
}

// TestCreateJobRequestNeverPrintsDataWithAnyVerb formats a request with every
// fmt verb: none may reach its rows, URLs, filename or callback headers,
// which may hold contact data or third-party credentials. The one exception
// is %p on a value rather than a pointer: fmt reports it as a bad verb and
// dumps the value without consulting any method, which no type can prevent.
func TestCreateJobRequestNeverPrintsDataWithAnyVerb(t *testing.T) {
	req := neverbounce.CreateJobRequest{
		Supplied:        neverbounce.SuppliedList{Columns: []string{jbColEmail}, Rows: [][]string{{testEmail}}},
		RemoteURL:       jbRemoteURL,
		Filename:        "file-" + jbLeak,
		CallbackURL:     jbCallbackURL,
		CallbackHeaders: map[string]string{"X-Name-" + jbLeak: "value-" + jbLeak},
	}
	secrets := []string{testEmail, jbLeak}
	testutil.AssertNeverPrints(t, &req, secrets...)
	for _, verb := range testutil.FormatVerbs() {
		if verb == "%p" {
			continue
		}
		testutil.AssertRedacted(t, fmt.Sprintf(verb, req), secrets, "verb %s on the value", verb)
		testutil.AssertRedacted(t, fmt.Sprintf(verb, struct{ R neverbounce.CreateJobRequest }{req}), secrets,
			"verb %s on an exported field", verb)
	}
}
