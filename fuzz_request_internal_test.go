package neverbounce

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// bzMaxFuzzInput bounds every fuzzed input, in bytes.
const bzMaxFuzzInput = 64 << 10

// bzLoopback is the test base URL of the fuzzed clients, whose in-memory
// transports never dial.
const bzLoopback = "https://127.0.0.1"

// bzCheckOK is the smallest successful single/check answer.
const bzCheckOK = `{"status":"success","result":"valid"}`

// Validation fields and reasons the oracles expect, spelled out apart from
// the SDK's own constants.
const (
	bzFieldJobID       = "JobID"
	bzReasonRequired   = "required"
	bzReasonOutOfRange = "out_of_range"
)

// bzCSVBody is a small download body.
const bzCSVBody = "email,email_status\r\navery.example@example.com,valid\r\n"

// bzScriptClient returns a client for key whose calls go to a script that
// answers with steps, in memory.
func bzScriptClient(tb testing.TB, key string, steps ...testutil.Step) (*Client, *testutil.Script) {
	tb.Helper()
	s := testutil.NewScript(steps...)
	c, err := NewClient(key, WithTestBaseURL(bzLoopback), WithHTTPClient(&http.Client{Transport: s}))
	require.NoError(tb, err)
	return c, s
}

// bzMember is one member of a JSON object, in document order.
type bzMember struct {
	name  string
	value json.RawMessage
}

// bzMembers returns the members of the JSON object data in document order,
// failing tb unless data is exactly one object.
func bzMembers(tb testing.TB, data []byte) []bzMember {
	tb.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	require.NoError(tb, err)
	require.Equal(tb, json.Delim('{'), tok)
	var out []bzMember
	for dec.More() {
		tok, err = dec.Token()
		require.NoError(tb, err)
		name, ok := tok.(string)
		require.True(tb, ok, "member name")
		var v json.RawMessage
		require.NoError(tb, dec.Decode(&v))
		out = append(out, bzMember{name: name, value: v})
	}
	tok, err = dec.Token()
	require.NoError(tb, err)
	require.Equal(tb, json.Delim('}'), tok)
	_, err = dec.Token()
	require.ErrorIs(tb, err, io.EOF, "the body is exactly one JSON value")
	return out
}

// bzNames returns the names of members, in order.
func bzNames(members []bzMember) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.name
	}
	return out
}

// bzValue returns the value of the member name, failing tb when it is absent.
func bzValue(tb testing.TB, members []bzMember, name string) json.RawMessage {
	tb.Helper()
	for _, m := range members {
		if m.name == name {
			return m.value
		}
	}
	require.Failf(tb, "missing member", "no member %q", name)
	return nil
}

// bzDecodeString decodes the JSON string raw, failing tb otherwise.
func bzDecodeString(tb testing.TB, raw json.RawMessage) string {
	tb.Helper()
	var s string
	require.NoError(tb, json.Unmarshal(raw, &s))
	return s
}

// bzDecodeBool decodes the JSON boolean raw, failing tb otherwise.
func bzDecodeBool(tb testing.TB, raw json.RawMessage) bool {
	tb.Helper()
	switch string(raw) {
	case "true":
		return true
	case "false":
		return false
	}
	require.Failf(tb, "not a boolean", "member is %s", raw)
	return false
}

// bzRequireBody checks what every request body shares: strict JSON, with no
// repeated key, whose first member is the key, spelled exactly as the client
// splices it in. It returns the members in order.
func bzRequireBody(tb testing.TB, key string, body []byte) []bzMember {
	tb.Helper()
	require.True(tb, json.Valid(body), "the body is valid JSON")
	valid, dup := testutil.TokenWalk(body)
	require.True(tb, valid)
	require.False(tb, dup, "the body repeats a key")
	require.True(tb, bytes.HasPrefix(body, []byte(`{"key":"`+key+`",`)), "the key member comes first")
	members := bzMembers(tb, body)
	require.NotEmpty(tb, members)
	require.Equal(tb, "key", members[0].name)
	require.Equal(tb, key, bzDecodeString(tb, members[0].value))
	return members
}

// bzTextProblem returns the reason s breaks the request text rules: at most
// maxBytes of valid UTF-8 without control characters and, when blankRule is
// set, not only whitespace. It is written apart from the SDK as an oracle.
func bzTextProblem(s string, maxBytes int, blankRule bool) string {
	switch {
	case len(s) > maxBytes:
		return "too_long"
	case !utf8.ValidString(s):
		return "invalid_utf8"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "control_character"
	case blankRule && strings.TrimSpace(s) == "":
		return "blank"
	}
	return ""
}

// bzCheckEmailProblem returns the field and reason CheckEmail must reject req
// with, on a client with the default timeout, or "" when it is valid.
func bzCheckEmailProblem(req *CheckEmailRequest) (field, reason string) {
	if req.Email == "" {
		return "Email", bzReasonRequired
	}
	if r := bzTextProblem(req.Email, 1024, true); r != "" {
		return "Email", r
	}
	switch d := req.Timeout; {
	case d == 0:
	case d%time.Second != 0:
		return "Timeout", "not_whole_seconds"
	case d < time.Second || d > 300*time.Second:
		return "Timeout", bzReasonOutOfRange
	case d > DefaultTimeout-10*time.Second:
		return "Timeout", "exceeds_client_timeout"
	}
	return "", ""
}

func FuzzEncodeCheckEmail(f *testing.F) {
	for _, seed := range []struct {
		email      string
		timeout    int64
		ai, ci     bool
		lhdSet, lh bool
	}{
		{"avery.example@example.com", 0, false, false, false, false},
		{"avery.example@example.com", int64(20 * time.Second), true, true, true, false},
		{` "quoted\back"@example.com `, int64(50 * time.Second), false, true, true, true},
		{"zoë.ñúñez@example.org", int64(51 * time.Second), true, false, false, false},
		{"x</script>&amp;@example.net", int64(time.Second), false, false, false, false},
		{" line sep@example.com", int64(1500 * time.Millisecond), true, true, false, false},
		{"", 0, false, false, false, false},
		{"   ", 0, false, false, false, false},
		{"a\x00b@example.com", 0, false, false, false, false},
		{"a\xc2\x85b@example.com", 0, false, false, false, false},
		{"\xff@example.com", 0, false, false, false, false},
		{"a\x7fb@example.com", 0, false, false, false, false},
		{strings.Repeat("a", 1024), int64(300 * time.Second), false, false, false, false},
		{strings.Repeat("a", 1025), 0, false, false, false, false},
		{"avery.example@example.com", -int64(time.Second), false, false, false, false},
		{"avery.example@example.com", math.MaxInt64, false, false, false, false},
		{"avery.example@example.com", math.MinInt64, false, false, false, false},
	} {
		f.Add(seed.email, seed.timeout, seed.ai, seed.ci, seed.lhdSet, seed.lh)
	}
	f.Fuzz(func(t *testing.T, email string, timeout int64, addressInfo, creditsInfo, lhdSet, lhd bool) {
		if len(email) > bzMaxFuzzInput {
			return
		}
		req := CheckEmailRequest{
			Email: email, AddressInfo: addressInfo, CreditsInfo: creditsInfo, Timeout: time.Duration(timeout),
		}
		if lhdSet {
			req.LeverageHistoricalData = &lhd
		}
		c, s := bzScriptClient(t, internalKey, testutil.Step{Body: bzCheckOK})
		res, err := c.CheckEmail(t.Context(), req)
		if field, reason := bzCheckEmailProblem(&req); field != "" {
			require.Nil(t, res)
			requireValidation(t, err, ErrInvalidRequest, field, reason)
			require.Zero(t, s.Calls(), "nothing is sent after a validation failure")
			return
		}
		require.NoError(t, err)
		require.Equal(t, ResultValid, res.Result)
		r := testutil.OnlyRequest(t, s)
		body := r.Body
		require.Equal(t, int64(len(body)), r.ContentLength)

		members := bzRequireBody(t, internalKey, body)
		want := []string{"key", "email", "address_info", "credits_info"}
		if req.Timeout != 0 {
			want = append(want, "timeout")
		}
		if lhdSet {
			want = append(want, "request_meta_data")
		}
		require.Equal(t, want, bzNames(members))
		require.Equal(t, email, bzDecodeString(t, members[1].value), "the address is sent verbatim")
		require.Equal(t, addressInfo, bzDecodeBool(t, members[2].value))
		require.Equal(t, creditsInfo, bzDecodeBool(t, members[3].value))
		if req.Timeout != 0 {
			require.Equal(t, strconv.FormatInt(timeout/int64(time.Second), 10), string(bzValue(t, members, "timeout")))
		}
		if lhdSet {
			meta := bzMembers(t, bzValue(t, members, "request_meta_data"))
			require.Equal(t, []string{"leverage_historical_data"}, bzNames(meta))
			require.Equal(t, lhd, bzDecodeBool(t, meta[0].value))
		}
	})
}

// bzCallbackURL is the callback URL the CreateJob fuzz target sets.
const bzCallbackURL = "https://hooks.example.com/neverbounce?token=fuzz"

// bzTchars are the RFC 9110 token characters besides letters and digits.
const bzTchars = "!#$%&'*+-.^_`|~"

// bzIsToken reports whether s is an RFC 9110 token of 1 to 128 bytes.
func bzIsToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && strings.IndexByte(bzTchars, c) < 0 {
			return false
		}
	}
	return true
}

// bzHeaderValueOK reports whether s is a valid callback header value: at most
// 1024 bytes of valid UTF-8 without ASCII control characters.
func bzHeaderValueOK(s string) bool {
	if len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// bzCreateJobProblem returns the field and reason CreateJob must reject req
// with, or "" when it is valid. req has a supplied list of 1 to 3 columns and
// at least one row, each as long as Columns, and no remote URL; the checks run
// in the SDK's documented order: columns, filename, callback, then each value
// row by row.
func bzCreateJobProblem(req *CreateJobRequest) (field, reason string) {
	if field, reason = bzColumnsProblem(req.Supplied.Columns); field != "" {
		return field, reason
	}
	if req.Filename != "" {
		if r := bzTextProblem(req.Filename, 255, true); r != "" {
			return "Filename", r
		}
	}
	if r := bzCallbackHeadersProblem(req.CallbackHeaders, req.CallbackURL != ""); r != "" {
		return "CallbackHeaders", r
	}
	for i, row := range req.Supplied.Rows {
		for j, value := range row {
			if r := bzTextProblem(value, 4096, false); r != "" {
				return "Supplied.Rows[" + strconv.Itoa(i) + "][" + strconv.Itoa(j) + "]", r
			}
		}
	}
	return "", ""
}

// bzColumnsProblem returns the field and reason the column names break the
// rules with: each 1 to 256 bytes of text, not blank, and unique ignoring
// ASCII case.
func bzColumnsProblem(cols []string) (field, reason string) {
	for i, name := range cols {
		f := "Supplied.Columns[" + strconv.Itoa(i) + "]"
		if name == "" {
			return f, bzReasonRequired
		}
		if r := bzTextProblem(name, 256, true); r != "" {
			return f, r
		}
		for _, earlier := range cols[:i] {
			if bzASCIILower(earlier) == bzASCIILower(name) {
				return f, "duplicate"
			}
		}
	}
	return "", ""
}

// bzCallbackHeadersProblem returns the reason callback headers break the
// rules, or "".
func bzCallbackHeadersProblem(h map[string]string, haveURL bool) string {
	if len(h) == 0 {
		return ""
	}
	if !haveURL {
		return "requires_callback_url"
	}
	for name, value := range h {
		if !bzIsToken(name) {
			return "invalid_header_name"
		}
		if !bzHeaderValueOK(value) {
			return "invalid_header_value"
		}
	}
	return ""
}

// bzASCIILower lowercases the ASCII letters of s only.
func bzASCIILower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// bzRows splits cells into rows at '\n' and values at '\t', both control
// characters no valid value holds, and pads or cuts each row to n values.
func bzRows(cells string, n int) [][]string {
	lines := strings.Split(cells, "\n")
	rows := make([][]string, len(lines))
	for i, line := range lines {
		row := make([]string, n)
		copy(row, strings.Split(line, "\t"))
		rows[i] = row
	}
	return rows
}

// bzCreateJobOptions are the bits of the CreateJob fuzz target's options.
const (
	bzOptAutoParse = 1 << iota
	bzOptAutoStart
	bzOptRunSample
	bzOptManualReview
	bzOptCallbackURL
	bzOptLHDSet
	bzOptLHD
)

func FuzzEncodeCreateJob(f *testing.F) {
	all := uint8(bzOptAutoParse | bzOptAutoStart | bzOptRunSample | bzOptManualReview | bzOptCallbackURL | bzOptLHDSet | bzOptLHD)
	f.Add(uint8(1), "id", "email", "", "1001\tavery.example@example.com\n1002\tblake.example@example.org", "", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "avery.example@example.com\navery.example@example.com\nemail", "list.csv", "", "", uint8(bzOptAutoParse))
	f.Add(uint8(2), "Name", "e\"mail\\", "zoë", `"quoted, comma"`+"\t \t ", "signups-2026-10-15-a7f3",
		"X-Token", "secret_callbackmarker", all) // gitleaks:allow (synthetic test marker)
	f.Add(uint8(1), "email", "EMAIL", "", "a\tb", "", "", "", uint8(0))
	f.Add(uint8(0), "", "", "", "a", "", "", "", uint8(0))
	f.Add(uint8(0), " ", "", "", "a", "", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "a\x00b", "", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "a\xc2\x85b", "", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "\xff", "", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "a", "\x7f", "", "", uint8(0))
	f.Add(uint8(0), "email", "", "", "a", "", "Bad Name", "v", uint8(bzOptCallbackURL))
	f.Add(uint8(0), "email", "", "", "a", "", "X-Token", "v\r\n", uint8(bzOptCallbackURL))
	f.Add(uint8(0), "email", "", "", "a", "", "X-Token", "v", uint8(0))
	f.Add(uint8(0), "email", "", "", "a", "", "X-Token", "café\xc2\x85", uint8(bzOptCallbackURL))
	f.Add(uint8(0), strings.Repeat("c", 257), "", "", strings.Repeat("v", 4097), strings.Repeat("f", 256), "", "", uint8(0))
	f.Fuzz(func(t *testing.T, ncols uint8, c0, c1, c2, cells, filename, hdrName, hdrValue string, opts uint8) {
		if len(c0)+len(c1)+len(c2)+len(cells)+len(filename)+len(hdrName)+len(hdrValue) > bzMaxFuzzInput {
			return
		}
		cols := []string{c0, c1, c2}[:1+int(ncols)%3]
		req := CreateJobRequest{
			Supplied:          SuppliedList{Columns: cols, Rows: bzRows(cells, len(cols))},
			Filename:          filename,
			AutoParse:         opts&bzOptAutoParse != 0,
			AutoStart:         opts&bzOptAutoStart != 0,
			RunSample:         opts&bzOptRunSample != 0,
			AllowManualReview: opts&bzOptManualReview != 0,
		}
		if opts&bzOptCallbackURL != 0 {
			req.CallbackURL = bzCallbackURL
		}
		if opts&bzOptLHDSet != 0 {
			req.LeverageHistoricalData = new(opts&bzOptLHD != 0)
		}
		if hdrName != "" || hdrValue != "" {
			req.CallbackHeaders = map[string]string{hdrName: hdrValue}
		}

		c, s := bzScriptClient(t, internalKey, testutil.Step{Body: `{"status":"success","job_id":1001}`})
		res, err := c.CreateJob(t.Context(), req)
		if field, reason := bzCreateJobProblem(&req); field != "" {
			require.Nil(t, res)
			requireValidation(t, err, ErrInvalidRequest, field, reason)
			require.Zero(t, s.Calls(), "nothing is sent after a validation failure")
			return
		}
		require.NoError(t, err)
		require.Equal(t, JobID(1001), res.JobID)
		bzRequireCreateBody(t, &req, testutil.OnlyRequest(t, s).Body)
	})
}

// bzRequireCreateBody checks a jobs/create body against the request it
// encodes: members in order, every boolean explicit, and each row an object
// whose members are the columns, in order, holding the row's values.
func bzRequireCreateBody(t *testing.T, req *CreateJobRequest, body []byte) {
	t.Helper()
	members := bzRequireBody(t, internalKey, body)
	want := []string{"key", "input_location", "input"}
	if req.Filename != "" {
		want = append(want, "filename")
	}
	want = append(want, "auto_parse", "auto_start", "run_sample", "allow_manual_review")
	if req.LeverageHistoricalData != nil {
		want = append(want, "request_meta_data")
	}
	if req.CallbackURL != "" {
		want = append(want, "callback_url")
	}
	if len(req.CallbackHeaders) > 0 {
		want = append(want, "callback_headers")
	}
	require.Equal(t, want, bzNames(members))
	require.Equal(t, "supplied", bzDecodeString(t, bzValue(t, members, "input_location")))

	var rows []json.RawMessage
	require.NoError(t, json.Unmarshal(bzValue(t, members, "input"), &rows))
	require.Len(t, rows, len(req.Supplied.Rows))
	for i, raw := range rows {
		row := bzMembers(t, raw)
		require.Equal(t, req.Supplied.Columns, bzNames(row), "row %d keys", i)
		for j, m := range row {
			require.Equal(t, req.Supplied.Rows[i][j], bzDecodeString(t, m.value), "row %d value %d", i, j)
		}
	}

	if req.Filename != "" {
		require.Equal(t, req.Filename, bzDecodeString(t, bzValue(t, members, "filename")))
	}
	assert.Equal(t, req.AutoParse, bzDecodeBool(t, bzValue(t, members, "auto_parse")))
	assert.Equal(t, req.AutoStart, bzDecodeBool(t, bzValue(t, members, "auto_start")))
	assert.Equal(t, req.RunSample, bzDecodeBool(t, bzValue(t, members, "run_sample")))
	assert.Equal(t, req.AllowManualReview, bzDecodeBool(t, bzValue(t, members, "allow_manual_review")))
	if req.LeverageHistoricalData != nil {
		meta := bzMembers(t, bzValue(t, members, "request_meta_data"))
		require.Equal(t, []string{"leverage_historical_data"}, bzNames(meta))
		assert.Equal(t, *req.LeverageHistoricalData, bzDecodeBool(t, meta[0].value))
	}
	if req.CallbackURL != "" {
		assert.Equal(t, req.CallbackURL, bzDecodeString(t, bzValue(t, members, "callback_url")))
	}
	if len(req.CallbackHeaders) > 0 {
		var headers map[string]string
		require.NoError(t, json.Unmarshal(bzValue(t, members, "callback_headers"), &headers))
		assert.Equal(t, req.CallbackHeaders, headers)
	}
}

// bzSwitches are the download switches in the order the SDK sends them:
// segments, then appended columns.
func bzSwitches(r *DownloadJobRequest) []struct {
	name string
	dst  **bool
} {
	s, a := &r.Segments, &r.Append
	return []struct {
		name string
		dst  **bool
	}{
		{"valids", &s.Valids},
		{"invalids", &s.Invalids},
		{"catchalls", &s.Catchalls},
		{"unknowns", &s.Unknowns},
		{"disposables", &s.Disposables},
		{"include_duplicates", &s.IncludeDuplicates},
		{"only_duplicates", &s.OnlyDuplicates},
		{"only_bad_syntax", &s.OnlyBadSyntax},
		{"bad_syntax", &a.BadSyntax},
		{"free_email_host", &a.FreeEmailHost},
		{"role_account", &a.RoleAccount},
		{"addr", &a.Addr},
		{"alias", &a.Alias},
		{"host", &a.Host},
		{"subdomain", &a.Subdomain},
		{"domain", &a.Domain},
		{"tld", &a.TLD},
		{"fqdn", &a.FQDN},
		{"network", &a.Network},
		{"has_dns_info", &a.HasDNSInfo},
		{"has_mail_server", &a.HasMailServer},
		{"mail_server_reachable", &a.MailServerReachable},
		{"email_status_int", &a.EmailStatusInt},
		{"email_status", &a.EmailStatus},
	}
}

// bzDownloadProblem returns the field and reason DownloadJob must reject r
// with, or "" when it is valid.
func bzDownloadProblem(r *DownloadJobRequest) (field, reason string) {
	switch {
	case r.JobID == 0:
		return bzFieldJobID, bzReasonRequired
	case r.JobID < 0:
		return bzFieldJobID, bzReasonOutOfRange
	}
	switch string(r.BinaryFormat) {
	case "", "BIN_1_0", "BIN_Y_N", "BIN_y_n", "BIN_yes_no", "BIN_Yes_No", "BIN_true_false":
	default:
		return "BinaryFormat", "invalid_value"
	}
	switch string(r.LineFeed) {
	case "", "LINEFEED_0A0D", "LINEFEED_0D0A", "LINEFEED_0A", "LINEFEED_0D":
	default:
		return "LineFeed", "invalid_value"
	}
	return "", ""
}

func FuzzEncodeDownload(f *testing.F) {
	f.Add(int64(1001), uint32(0), uint32(0), "", "")
	f.Add(int64(1001), uint32(1<<24-1), uint32(1<<24-1), "BIN_1_0", "LINEFEED_0D0A")
	f.Add(int64(1001), uint32(0xaaaaaa), uint32(0x0f0f0f), "BIN_true_false", "LINEFEED_0A0D")
	f.Add(int64(math.MaxInt64), uint32(1<<23), uint32(0), "BIN_Yes_No", "LINEFEED_0D")
	f.Add(int64(1001), uint32(0), uint32(0), "bin_1_0", "")
	f.Add(int64(1001), uint32(0), uint32(0), "", "LINEFEED_0a")
	f.Add(int64(1001), uint32(0), uint32(0), "BIN_1_0 ", "\n")
	f.Add(int64(0), uint32(0), uint32(0), "", "")
	f.Add(int64(-1), uint32(0), uint32(0), "x", "y")
	f.Add(int64(math.MinInt64), uint32(0xffffffff), uint32(0xffffffff), "", "")
	f.Fuzz(func(t *testing.T, jobID int64, setMask, valueMask uint32, binary, lineFeed string) {
		if len(binary)+len(lineFeed) > bzMaxFuzzInput {
			return
		}
		req := DownloadJobRequest{JobID: JobID(jobID), BinaryFormat: BinaryFormat(binary), LineFeed: LineFeed(lineFeed)}
		switches := bzSwitches(&req)
		for i, sw := range switches {
			if setMask&(1<<i) != 0 {
				*sw.dst = new(valueMask&(1<<i) != 0)
			}
		}
		c, s := bzScriptClient(t, internalKey, testutil.Step{
			Header: http.Header{"Content-Type": {"text/csv; charset=utf-8"}}, Body: bzCSVBody,
		})
		var out bytes.Buffer
		n, err := c.DownloadJob(t.Context(), req, &out)
		if field, reason := bzDownloadProblem(&req); field != "" {
			require.Zero(t, n)
			requireValidation(t, err, ErrInvalidRequest, field, reason)
			require.Zero(t, s.Calls(), "nothing is sent after a validation failure")
			return
		}
		require.NoError(t, err)
		require.Equal(t, int64(len(bzCSVBody)), n)
		require.Equal(t, bzCSVBody, out.String())
		r := testutil.OnlyRequest(t, s)
		require.Equal(t, "*/*", r.Header.Get("Accept"))
		bzRequireDownloadBody(t, &req, setMask, valueMask, r.Body)
	})
}

// bzRequireDownloadBody checks a jobs/download body against the request it
// encodes: the job ID, each set switch in order with its value, then the
// enums when set.
func bzRequireDownloadBody(t *testing.T, req *DownloadJobRequest, setMask, valueMask uint32, body []byte) {
	t.Helper()
	members := bzRequireBody(t, internalKey, body)
	switches := bzSwitches(req)
	want := []string{"key", "job_id"}
	for i, sw := range switches {
		if setMask&(1<<i) != 0 {
			want = append(want, sw.name)
		}
	}
	if req.BinaryFormat != "" {
		want = append(want, "binary_operators_type")
	}
	if req.LineFeed != "" {
		want = append(want, "line_feed_type")
	}
	require.Equal(t, want, bzNames(members))
	require.Equal(t, req.JobID.String(), string(members[1].value))
	for i, sw := range switches {
		if setMask&(1<<i) != 0 {
			require.Equal(t, valueMask&(1<<i) != 0, bzDecodeBool(t, bzValue(t, members, sw.name)), sw.name)
		}
	}
	if req.BinaryFormat != "" {
		require.Equal(t, string(req.BinaryFormat), bzDecodeString(t, bzValue(t, members, "binary_operators_type")))
	}
	if req.LineFeed != "" {
		require.Equal(t, string(req.LineFeed), bzDecodeString(t, bzValue(t, members, "line_feed_type")))
	}
}
