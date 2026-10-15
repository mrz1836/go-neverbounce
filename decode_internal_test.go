package neverbounce

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// sjRaw returns the raw member value v, or nil (absent) for "<absent>".
func sjRaw(v string) []byte {
	if v == sjAbsent {
		return nil
	}
	return []byte(v)
}

// sjAbsent marks a member that is not in the object at all.
const sjAbsent = "<absent>"

func TestDecodeErrorConstructors(t *testing.T) {
	tests := []struct {
		derr  *decodeError
		kind  FailureKind
		cause error
	}{
		{missingField("a"), FailureMalformedResponse, errMissingField},
		{wrongType("a"), FailureMalformedResponse, errWrongType},
		{outOfRange("a"), FailureMalformedResponse, errOutOfRange},
		{badToken("a"), FailureUnexpectedResponse, errInvalidToken},
		{fieldTooLarge("a"), FailureResponseTooLarge, errFieldTooLarge},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind)+"/"+tt.cause.Error(), func(t *testing.T) {
			assertDecodeError(t, tt.derr, tt.kind, "a")
			require.ErrorIs(t, tt.derr.cause, tt.cause)

			ce := tt.derr.callError(OpJobStatus)
			assert.Equal(t, tt.kind, ce.Kind)
			assert.Equal(t, OpJobStatus, ce.Op)
			assert.Equal(t, 200, ce.StatusCode)
			assert.Equal(t, "a", ce.Field)
			assert.Empty(t, ce.APIStatus)
			assert.Zero(t, ce.JobID)
			require.ErrorIs(t, ce, tt.cause, "the cause is reachable through Unwrap")
			require.ErrorIs(t, ce, kindSentinel(tt.kind))
			assert.Equal(t, "neverbounce: jobs/status: "+string(tt.kind)+" (HTTP 200): a", ce.Error())
			assert.Empty(t, ce.Diagnostics().Message)
		})
	}
}

func TestDecodePath(t *testing.T) {
	assert.Equal(t, "address_info", at("address_info", -1, "").String(), "a negative index renders no index")
	assert.Equal(t, "address_info.tld", at("address_info", -1, "").member("tld"))
	assert.Equal(t, "address_info.flags[2]", at("address_info", -1, "").element(sjFlagsField, 2))
	assert.Equal(t, "total.records", at("total", -1, "records").String())
	assert.Equal(t, "results[3].verification", at(sjResultsField, 3, "verification").String())
	assert.Equal(t, "results[0]", at(sjResultsField, 0, "").String())
	assert.Equal(t, "results[12].data", at(sjResultsField, 12, "data").String())
	assert.Equal(t, "results[3].verification.result", at(sjResultsField, 3, "verification").member("result"))
	assert.Equal(t, "results[3].verification.flags[0]", at(sjResultsField, 3, "verification").element(sjFlagsField, 0))
	assert.Equal(t, "results[7].x", at(sjResultsField, 7, "").member("x"))
	assert.Equal(t, "rows[2].cells[5]", at("rows", 2, "").element("cells", 5), "any base and list name")
	assert.Equal(t, "rows[0].cells.text", at("rows", 0, "cells").member("text"))

	var zero path
	assert.Equal(t, sjFlagsField, zero.member(sjFlagsField), "a zero path renders the member alone")
	assert.Equal(t, "flags[64]", zero.element(sjFlagsField, 64))
	assert.Equal(t, "result", zero.member("result"))
}

func TestDecodeReadObject(t *testing.T) {
	names := []string{statusMember, "result", "job_id"}
	tests := []struct {
		name string
		body string
		want [3]string // raw values; "" when absent
	}{
		{name: "every member", body: `{"status":"success","result":"valid","job_id":7}`, want: [3]string{`"success"`, `"valid"`, `7`}},
		{name: "order does not matter", body: `{"job_id":[1],"result":{"a":"b"},"status":null}`, want: [3]string{jsonNull, `{"a":"b"}`, `[1]`}},
		{name: "other cases are ignored", body: `{"Status":"x","STATUS":"y","Result":"invalid","jobId":1,"JOB_ID":2}`},
		{name: "exact case wins over other cases", body: `{"Status":"x","status":"success","RESULT":"z"}`, want: [3]string{`"success"`}},
		{name: "escaped keys match", body: testutil.Esc(`{"~u0073tatus":"success","r~u0065sult":"valid"}`), want: [3]string{`"success"`, `"valid"`}},
		{name: "unknown members are ignored", body: `{"x":1,"status":"success","y":{"status":"z"}}`, want: [3]string{`"success"`}},
		{name: "nested members are not flattened", body: `{"data":{"status":"success","result":"valid"}}`},
		{name: "whitespace around members", body: " {\n \"status\" :\t\"success\" , \"job_id\" : -1 } ", want: [3]string{`"success"`, "", `-1`}},
		{name: "keys with a trailing space differ", body: `{"status ":"a"," status":"b"}`},
		{name: "not an object", body: `["status","success"]`},
		{name: "empty body", body: ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := readObject([]byte(tt.body), names)
			for i := range names {
				if tt.want[i] == "" {
					assert.Nil(t, o.fields[i], names[i])
					continue
				}
				assert.Equal(t, tt.want[i], string(o.fields[i]), names[i])
			}
			for i := len(names); i < maxFields; i++ {
				assert.Nil(t, o.fields[i], "slot %d is unused", i)
			}
		})
	}

	body := []byte(`{"status":"success"}`)
	o := readObject(body, names)
	assert.Same(t, &body[10], &o.fields[0][0], "members alias the body until decoded")
	assert.Len(t, statusNames(), maxFields, "the jobs/status envelope fills every slot")
}

func TestDecodeText(t *testing.T) {
	exact := strings.Repeat("a", maxTextBytes)
	tests := []struct {
		name string
		raw  string
		want string
		kind FailureKind
	}{
		{name: "string", raw: `"2026-10-15 14:52:27"`, want: "2026-10-15 14:52:27"},
		{name: "empty string", raw: `""`, want: ""},
		{name: "escaped", raw: testutil.Esc(`"a~"b~nc~u00e9"`), want: "a\"b\ncé"},
		{name: "lone surrogate", raw: testutil.Esc(`"~ud800"`), want: string(utf8.RuneError)},
		{name: jsonNull, raw: jsonNull, want: ""},
		{name: sjAbsent, raw: sjAbsent, want: ""},
		{name: "exactly 4 KiB", raw: `"` + exact + `"`, want: exact},
		{name: "4 KiB once decoded", raw: `"` + strings.Repeat(testutil.Esc(`~u0061`), maxTextBytes) + `"`, want: exact},
		{name: "4 KiB of two-byte runes", raw: `"` + strings.Repeat("é", maxTextBytes/2) + `"`, want: strings.Repeat("é", maxTextBytes/2)},
		{name: "one byte over 4 KiB", raw: `"` + exact + `b"`, kind: FailureResponseTooLarge},
		{name: "over 4 KiB by a multibyte rune", raw: `"` + strings.Repeat("a", maxTextBytes-1) + `é"`, kind: FailureResponseTooLarge},
		{name: "over 4 KiB once decoded", raw: `"` + strings.Repeat(testutil.Esc(`~u20ac`), 1366) + `"`, kind: FailureResponseTooLarge},
		{name: "number", raw: `12`, kind: FailureMalformedResponse},
		{name: "negative number", raw: `-1`, kind: FailureMalformedResponse},
		{name: sjTrue, raw: sjTrue, kind: FailureMalformedResponse},
		{name: sjFalse, raw: sjFalse, kind: FailureMalformedResponse},
		{name: "object", raw: `{"a":"b"}`, kind: FailureMalformedResponse},
		{name: "array", raw: `["a"]`, kind: FailureMalformedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeText(sjRaw(tt.raw), sjFilenameField)
			if tt.kind != "" {
				assertDecodeError(t, derr, tt.kind, sjFilenameField)
				assert.Empty(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
		})
	}
	_, derr := decodeText([]byte("7"), sjFilenameField)
	require.ErrorIs(t, derr.cause, errWrongType)
	_, derr = decodeText([]byte(`"`+exact+`b"`), sjFilenameField)
	require.ErrorIs(t, derr.cause, errFieldTooLarge)
}

func TestDecodeValidToken(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"valid", true},
		{"a", true},
		{"0", true},
		{"_", true},
		{"has_dns_mx", true},
		{"free_email_host", true},
		{"acedemic_host", true},
		{"z9_", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"Valid", false},
		{"VALID", false},
		{"has-dns", false},
		{"has dns", false},
		{" valid", false},
		{"valid\n", false},
		{"é", false},
		{"a`", false},
		{"a{", false},
		{"a/", false},
		{"a:", false},
		{"a^", false},
		{"a@", false},
		{"a.b", false},
		{"a\x00", false},
	}
	for _, tt := range tests {
		t.Run(strconv.Quote(tt.s), func(t *testing.T) {
			assert.Equal(t, tt.want, validToken(tt.s))
		})
	}
}

func TestDecodeToken(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		required bool
		want     string
		kind     FailureKind
	}{
		{name: "required token", raw: `"valid"`, required: true, want: "valid"},
		{name: "required unknown token kept", raw: `"brand_new_state"`, required: true, want: "brand_new_state"},
		{name: "required escaped token", raw: testutil.Esc(`"~u0076alid"`), required: true, want: "valid"},
		{name: "required 64 bytes", raw: `"` + strings.Repeat("x", 64) + `"`, required: true, want: strings.Repeat("x", 64)},
		{name: "required absent", raw: sjAbsent, required: true, kind: FailureMalformedResponse},
		{name: "required null", raw: jsonNull, required: true, kind: FailureMalformedResponse},
		{name: "required empty string", raw: `""`, required: true, kind: FailureUnexpectedResponse},
		{name: "required number", raw: `1`, required: true, kind: FailureMalformedResponse},
		{name: "required boolean", raw: sjFalse, required: true, kind: FailureMalformedResponse},
		{name: "required array", raw: `["valid"]`, required: true, kind: FailureMalformedResponse},
		{name: "required uppercase", raw: `"VALID"`, required: true, kind: FailureUnexpectedResponse},
		{name: "required 65 bytes", raw: `"` + strings.Repeat("x", 65) + `"`, required: true, kind: FailureUnexpectedResponse},
		{name: "required escaped space", raw: testutil.Esc(`"valid~u0020"`), required: true, kind: FailureUnexpectedResponse},
		{name: "optional token", raw: `"file_corrupt"`, want: "file_corrupt"},
		{name: "optional absent", raw: sjAbsent, want: ""},
		{name: "optional null", raw: jsonNull, want: ""},
		{name: "optional empty string", raw: `""`, want: ""},
		{name: "optional number", raw: `0`, kind: FailureMalformedResponse},
		{name: "optional object", raw: `{}`, kind: FailureMalformedResponse},
		{name: "optional bad token", raw: `"File Corrupt"`, kind: FailureUnexpectedResponse},
		{name: "optional lone surrogate", raw: testutil.Esc(`"~ud800"`), kind: FailureUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeToken(sjRaw(tt.raw), "job_status", tt.required)
			if tt.kind != "" {
				assertDecodeError(t, derr, tt.kind, "job_status")
				assert.Empty(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
		})
	}
	_, derr := decodeToken(nil, "result", true)
	require.ErrorIs(t, derr.cause, errMissingField)
	_, derr = decodeToken([]byte(`"A"`), "result", true)
	require.ErrorIs(t, derr.cause, errInvalidToken)
}

// sjFlags returns a JSON array of n flags, each "flag<i>".
func sjFlags(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"flag` + strconv.Itoa(i) + `"`)
	}
	b.WriteByte(']')
	return b.String()
}

func TestDecodeFlags(t *testing.T) {
	sixtyFour := make(Flags, 64)
	for i := range sixtyFour {
		sixtyFour[i] = Flag("flag" + strconv.Itoa(i))
	}
	tests := []struct {
		name  string
		raw   string
		want  Flags
		kind  FailureKind
		field string
	}{
		{name: sjAbsent, raw: sjAbsent},
		{name: jsonNull, raw: jsonNull},
		{name: "empty list is nil", raw: `[]`},
		{name: "empty list with spaces is nil", raw: `[ ]`},
		{
			name: "order and duplicates kept", raw: `["smtp_connectable","has_dns","smtp_connectable","acedemic_host"]`,
			want: Flags{FlagSMTPConnectable, FlagHasDNS, FlagSMTPConnectable, "acedemic_host"},
		},
		{name: "escaped flag", raw: testutil.Esc(`["has~u005fdns"]`), want: Flags{FlagHasDNS}},
		{name: "64 flags", raw: sjFlags(64), want: sixtyFour},
		{name: "65 flags", raw: sjFlags(65), kind: FailureResponseTooLarge, field: sjFlagsField},
		{name: "65 flags with a bad 65th", raw: strings.TrimSuffix(sjFlags(64), "]") + `,"BAD"]`, kind: FailureResponseTooLarge, field: sjFlagsField},
		{name: "null element", raw: `["has_dns",null]`, kind: FailureMalformedResponse, field: "flags[1]"},
		{name: "number element", raw: `[1]`, kind: FailureMalformedResponse, field: "flags[0]"},
		{name: "object element", raw: `["has_dns","has_dns",{}]`, kind: FailureMalformedResponse, field: "flags[2]"},
		{name: "empty string element", raw: `["has_dns",""]`, kind: FailureUnexpectedResponse, field: "flags[1]"},
		{name: "bad token element", raw: `["has_dns","has_dns","Has DNS"]`, kind: FailureUnexpectedResponse, field: "flags[2]"},
		{name: "a string", raw: `"has_dns"`, kind: FailureMalformedResponse, field: sjFlagsField},
		{name: "an object", raw: `{"0":"has_dns"}`, kind: FailureMalformedResponse, field: sjFlagsField},
		{name: "a number", raw: `0`, kind: FailureMalformedResponse, field: sjFlagsField},
		{name: sjFalse, raw: sjFalse, kind: FailureMalformedResponse, field: sjFlagsField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeFlags(sjRaw(tt.raw), path{})
			if tt.kind != "" {
				assertDecodeError(t, derr, tt.kind, tt.field)
				assert.Nil(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
			if tt.want == nil {
				assert.Nil(t, got, "an empty list is nil, not an empty slice")
			}
		})
	}

	p := at(sjResultsField, 3, "verification")
	_, derr := decodeFlags([]byte(`["has_dns",7]`), p)
	assertDecodeError(t, derr, FailureMalformedResponse, "results[3].verification.flags[1]")
	_, derr = decodeFlags([]byte(sjFlags(65)), p)
	assertDecodeError(t, derr, FailureResponseTooLarge, "results[3].verification.flags")
	require.ErrorIs(t, derr.cause, errFieldTooLarge)
	_, derr = decodeFlags([]byte(`{}`), p)
	assertDecodeError(t, derr, FailureMalformedResponse, "results[3].verification.flags")
}

func TestDecodeParseInt(t *testing.T) {
	tests := []struct {
		lit         string
		n           int64
		ok, inRange bool
	}{
		{"0", 0, true, true},
		{"-0", 0, true, true},
		{"1", 1, true, true},
		{"-1", -1, true, true},
		{"412", 412, true, true},
		{"9223372036854775807", math.MaxInt64, true, true},
		{"-9223372036854775808", math.MinInt64, true, true},
		{"9223372036854775808", 0, true, false},
		{"-9223372036854775809", 0, true, false},
		{"99999999999999999999999999", 0, true, false},
		{"1.0", 0, false, false},
		{"1.5", 0, false, false},
		{"1e3", 0, false, false},
		{"1E3", 0, false, false},
		{"1e+3", 0, false, false},
		{"-1.5e-3", 0, false, false},
		{"99999999999999999999.5", 0, false, false},
		{"1e400", 0, false, false},
		{`"1"`, 0, false, false},
		{sjTrue, 0, false, false},
		{jsonNull, 0, false, false},
		{"[1]", 0, false, false},
		{"+1", 0, false, false},
		{"", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.lit, func(t *testing.T) {
			n, ok, inRange := parseInt([]byte(tt.lit))
			assert.Equal(t, tt.n, n)
			assert.Equal(t, tt.ok, ok, "ok")
			assert.Equal(t, tt.inRange, inRange, "inRange")
		})
	}
}

// TestDecodeLeadingZerosNeverReachParseInt proves that a leading zero, which
// strconv.ParseInt would accept, never reaches it: the strict check already
// rejects the body as invalid JSON.
func TestDecodeLeadingZerosNeverReachParseInt(t *testing.T) {
	for _, body := range []string{`{"job_id":01}`, `{"job_id":-01}`, `{"job_id":00}`} {
		kind, cause := checkStrict([]byte(body))
		assert.Equal(t, FailureMalformedResponse, kind, body)
		require.ErrorIs(t, cause, errInvalidJSON, body)
	}
}

func TestDecodeCount(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  *int64
		cause error
	}{
		{name: sjAbsent, raw: sjAbsent},
		{name: jsonNull, raw: jsonNull},
		{name: "zero", raw: `0`, want: new(int64(0))},
		{name: "negative zero", raw: `-0`, want: new(int64(0))},
		{name: "positive", raw: `4321`, want: new(int64(4321))},
		{name: "max int64", raw: `9223372036854775807`, want: new(int64(math.MaxInt64))},
		{name: "quoted", raw: `"1"`, cause: errWrongType},
		{name: "fraction", raw: `1.0`, cause: errWrongType},
		{name: "exponent", raw: `1e3`, cause: errWrongType},
		{name: "boolean", raw: sjTrue, cause: errWrongType},
		{name: "object", raw: `{}`, cause: errWrongType},
		{name: "array", raw: `[1]`, cause: errWrongType},
		{name: "negative", raw: `-1`, cause: errOutOfRange},
		{name: "most negative", raw: `-9223372036854775808`, cause: errOutOfRange},
		{name: "overflow", raw: `9223372036854775808`, cause: errOutOfRange},
		{name: "negative overflow", raw: `-9223372036854775809`, cause: errOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeCount(sjRaw(tt.raw), "total.records")
			if tt.cause != nil {
				assertDecodeError(t, derr, FailureMalformedResponse, "total.records")
				require.ErrorIs(t, derr.cause, tt.cause)
				assert.Nil(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodeRequiredCount(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  int64
		cause error
	}{
		{name: "zero", raw: `0`, want: 0},
		{name: "positive", raw: `3`, want: 3},
		{name: "max int64", raw: `9223372036854775807`, want: math.MaxInt64},
		{name: sjAbsent, raw: sjAbsent, cause: errMissingField},
		{name: jsonNull, raw: jsonNull, cause: errMissingField},
		{name: "quoted", raw: `"3"`, cause: errWrongType},
		{name: "fraction", raw: `1.0`, cause: errWrongType},
		{name: "exponent", raw: `1e3`, cause: errWrongType},
		{name: "negative", raw: `-1`, cause: errOutOfRange},
		{name: "overflow", raw: `9223372036854775808`, cause: errOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeRequiredCount(sjRaw(tt.raw), "total_pages")
			if tt.cause != nil {
				assertDecodeError(t, derr, FailureMalformedResponse, "total_pages")
				require.ErrorIs(t, derr.cause, tt.cause)
				assert.Zero(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodeJobID(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  JobID
		cause error
	}{
		{name: "one", raw: `1`, want: 1},
		{name: "typical", raw: `289022`, want: 289022},
		{name: "max int64", raw: `9223372036854775807`, want: math.MaxInt64},
		{name: "zero", raw: `0`, cause: errOutOfRange},
		{name: "negative zero", raw: `-0`, cause: errOutOfRange},
		{name: "negative", raw: `-5`, cause: errOutOfRange},
		{name: "overflow", raw: `9223372036854775808`, cause: errOutOfRange},
		{name: "quoted", raw: `"289022"`, cause: errWrongType},
		{name: "fraction", raw: `289022.0`, cause: errWrongType},
		{name: "exponent", raw: `2.89022e5`, cause: errWrongType},
		{name: "boolean", raw: sjTrue, cause: errWrongType},
		{name: sjAbsent, raw: sjAbsent, cause: errMissingField},
		{name: jsonNull, raw: jsonNull, cause: errMissingField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeJobID(sjRaw(tt.raw), "job_id")
			if tt.cause != nil {
				assertDecodeError(t, derr, FailureMalformedResponse, "job_id")
				require.ErrorIs(t, derr.cause, tt.cause)
				assert.Zero(t, got)
				return
			}
			assert.Nil(t, derr)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodePercent(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  float64
		null  bool
		cause error
	}{
		{name: "zero", raw: `0`, want: 0},
		{name: "hundred", raw: `100`, want: 100},
		{name: "fraction", raw: `37.5`, want: 37.5},
		{name: "exponent", raw: `1e2`, want: 100},
		{name: "uppercase exponent", raw: `3.75E1`, want: 37.5},
		{name: "negative exponent", raw: `5e-1`, want: 0.5},
		{name: "float zero", raw: `0.0`, want: 0},
		{name: "hundred as a float", raw: `100.0`, want: 100},
		{name: "rounds to hundred", raw: `100.00000000000000001`, want: 100},
		{name: "negative zero", raw: `-0`, want: 0},
		{name: "negative float zero", raw: `-0.0`, want: 0},
		{name: "negative underflow", raw: `-1e-400`, want: 0},
		{name: "positive underflow", raw: `1e-400`, want: 0},
		{name: sjAbsent, raw: sjAbsent, null: true},
		{name: jsonNull, raw: jsonNull, null: true},
		{name: "below zero", raw: `-0.1`, cause: errOutOfRange},
		{name: "above hundred", raw: `100.1`, cause: errOutOfRange},
		{name: "far above", raw: `1e3`, cause: errOutOfRange},
		{name: "infinite", raw: `1e400`, cause: errOutOfRange},
		{name: "negative infinite", raw: `-1e400`, cause: errOutOfRange},
		{name: "quoted", raw: `"50"`, cause: errWrongType},
		{name: "boolean", raw: sjTrue, cause: errWrongType},
		{name: "object", raw: `{"value":50}`, cause: errWrongType},
		{name: "array", raw: `[50]`, cause: errWrongType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodePercent(sjRaw(tt.raw), "percent_complete")
			switch {
			case tt.cause != nil:
				assertDecodeError(t, derr, FailureMalformedResponse, "percent_complete")
				require.ErrorIs(t, derr.cause, tt.cause)
				assert.Nil(t, got)
			case tt.null:
				assert.Nil(t, derr)
				assert.Nil(t, got)
			default:
				assert.Nil(t, derr)
				require.NotNil(t, got)
				assert.InDelta(t, tt.want, *got, 0)
				assert.False(t, math.Signbit(*got), "-0 decodes as +0")
			}
		})
	}
}

func TestDecodeData(t *testing.T) {
	const ls, ps = "\xe2\x80\xa8", "\xe2\x80\xa9" // U+2028 and U+2029
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "compact object", raw: `{"id":"1001","email":"avery@example.com"}`, want: `{"id":"1001","email":"avery@example.com"}`},
		{name: "array", raw: `[ "1001" , 1002 , null , true ]`, want: `["1001",1002,null,true]`},
		{name: "whitespace removed", raw: "{ \"a\" :\n\t[ 1 , { \"b\" : \"c d\" } ] }", want: `{"a":[1,{"b":"c d"}]}`},
		{name: "HTML characters escaped", raw: `{"a":"<b>&</b>"}`, want: testutil.Esc(`{"a":"~u003cb~u003e~u0026~u003c/b~u003e"}`)},
		{name: "line separators escaped", raw: `{"a":"x` + ls + `y` + ps + `"}`, want: testutil.Esc(`{"a":"x~u2028y~u2029"}`)},
		{name: "escapes kept as spelled", raw: testutil.Esc(`{"~u0061":"~n~/~u00e9"}`), want: testutil.Esc(`{"~u0061":"~n~/~u00e9"}`)},
		{
			name: "lone surrogates replaced", raw: testutil.Esc(`{"k":"~ud800","~udfff":["~ud83d~ude00"]}`),
			want: testutil.Esc(`{"k":"~ufffd","~ufffd":["~ud83d~ude00"]}`),
		},
		{name: "raw multibyte kept", raw: `["é€` + sjEmoji + `"]`, want: `["é€` + sjEmoji + `"]`},
		{name: "empty object", raw: `{}`, want: `{}`},
		{name: "empty array", raw: `[ ]`, want: `[]`},
		{name: "numbers kept verbatim", raw: `[1.50,-0,1E+2]`, want: `[1.50,-0,1E+2]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			got, derr := decodeData(raw, "results[0].data")
			require.Nil(t, derr)
			assert.Equal(t, []byte(tt.want), []byte(got))
			want, err := json.Marshal(json.RawMessage(replaceLoneSurrogates(raw)))
			require.NoError(t, err)
			assert.Equal(t, want, []byte(got), "normalized exactly as encoding/json marshals a RawMessage")
			again, err := json.Marshal(got)
			require.NoError(t, err)
			assert.Equal(t, []byte(got), again, "normalization is idempotent")
		})
	}
}

func TestDecodeDataDoesNotAlias(t *testing.T) {
	raw := []byte(`{"id":"1001"}`)
	original := bytes.Clone(raw)
	got, derr := decodeData(raw, "data")
	require.Nil(t, derr)
	require.Equal(t, original, []byte(got))
	assert.NotSame(t, &raw[0], &got[0], "the data is a copy")
	copy(raw, `{"id":"9999"}`)
	assert.Equal(t, original, []byte(got), "changing the body never changes the data")
}

func TestDecodeDataTypesAndBounds(t *testing.T) {
	exact := `{"k":"` + strings.Repeat("a", maxDataBytes-8) + `"}`
	require.Len(t, exact, maxDataBytes)
	padded := "[" + strings.Repeat(" ", 4*maxDataBytes-2) + "]"
	require.Len(t, padded, 4*maxDataBytes)
	tests := []struct {
		name string
		raw  string
		kind FailureKind
		null bool
	}{
		{name: sjAbsent, raw: sjAbsent, null: true},
		{name: jsonNull, raw: jsonNull, null: true},
		{name: "string", raw: `"a,b"`, kind: FailureMalformedResponse},
		{name: "number", raw: `12`, kind: FailureMalformedResponse},
		{name: "negative number", raw: `-12`, kind: FailureMalformedResponse},
		{name: sjTrue, raw: sjTrue, kind: FailureMalformedResponse},
		{name: sjFalse, raw: sjFalse, kind: FailureMalformedResponse},
		{name: "not JSON", raw: `{"a":`, kind: FailureMalformedResponse},
		{name: "exactly 64 KiB normalized", raw: exact},
		{name: "one byte over 64 KiB", raw: `{"k":"` + strings.Repeat("a", maxDataBytes-7) + `"}`, kind: FailureResponseTooLarge},
		{name: "over 64 KiB only once escaped", raw: `{"k":"` + strings.Repeat("<", maxDataBytes/6) + `"}`, kind: FailureResponseTooLarge},
		{name: "whitespace beyond 64 KiB compacts within", raw: "[" + strings.Repeat(" ", 2*maxDataBytes) + "1]"},
		{name: "exactly four times the bound", raw: padded},
		{name: "past four times the bound is refused early", raw: padded + " ", kind: FailureResponseTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, derr := decodeData(sjRaw(tt.raw), "results[4].data")
			switch {
			case tt.kind != "":
				assertDecodeError(t, derr, tt.kind, "results[4].data")
				assert.Nil(t, got)
			case tt.null:
				assert.Nil(t, derr)
				assert.Nil(t, got)
			default:
				assert.Nil(t, derr)
				assert.LessOrEqual(t, len(got), maxDataBytes)
				assert.True(t, json.Valid(got))
			}
		})
	}
	_, derr := decodeData([]byte(padded+" "), "d")
	require.ErrorIs(t, derr.cause, errFieldTooLarge)
	_, derr = decodeData([]byte(`"x"`), "d")
	require.ErrorIs(t, derr.cause, errWrongType)
}

func TestDecodeExecutionTime(t *testing.T) {
	tests := []struct {
		raw  string
		want *int64
	}{
		{`412`, new(int64(412))},
		{`0`, new(int64(0))},
		{`9223372036854775807`, new(int64(math.MaxInt64))},
		{sjAbsent, nil},
		{jsonNull, nil},
		{`-1`, nil},
		{`1.5`, nil},
		{`1e3`, nil},
		{`"412"`, nil},
		{sjTrue, nil},
		{`{}`, nil},
		{`9223372036854775808`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			assert.Equal(t, tt.want, executionTime(sjRaw(tt.raw)))
		})
	}
}

func TestDecodeQueueID(t *testing.T) {
	visible := make([]byte, 0, 0x7e-0x21+1)
	for c := byte(0x21); c <= 0x7e; c++ {
		visible = append(visible, c)
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "typical", raw: `"NB-PQ-EXAMPLE"`, want: "NB-PQ-EXAMPLE"},
		{name: "one byte", raw: `"q"`, want: "q"},
		{name: "every visible byte", raw: testutil.JSONQuote(string(visible)), want: string(visible)},
		{name: "128 bytes", raw: `"` + strings.Repeat("q", 128) + `"`, want: strings.Repeat("q", 128)},
		{name: "128 bytes once decoded", raw: `"` + strings.Repeat(testutil.Esc(`~u0071`), 128) + `"`, want: strings.Repeat("q", 128)},
		{name: "129 bytes", raw: `"` + strings.Repeat("q", 129) + `"`, want: ""},
		{name: "empty", raw: `""`, want: ""},
		{name: "space", raw: `"a b"`, want: ""},
		{name: "control escape", raw: testutil.Esc(`"a~u0001"`), want: ""},
		{name: "newline escape", raw: testutil.Esc(`"a~n"`), want: ""},
		{name: "DEL", raw: testutil.Esc(`"a~u007f"`), want: ""},
		{name: "non-ASCII", raw: `"é"`, want: ""},
		{name: "number", raw: `12345`, want: ""},
		{name: "object", raw: `{"id":"q"}`, want: ""},
		{name: jsonNull, raw: jsonNull, want: ""},
		{name: sjAbsent, raw: sjAbsent, want: ""},
		{name: "lone quote", raw: `"`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, queueID(sjRaw(tt.raw)))
		})
	}
}

func TestDecodeRetryToken(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "typical", raw: `"opaque-token"`, want: "opaque-token"},
		{name: "any text", raw: testutil.Esc(`"a b~n~"é"`), want: "a b\n\"é"},
		{name: "1 KiB", raw: `"` + strings.Repeat("r", 1024) + `"`, want: strings.Repeat("r", 1024)},
		{name: "1 KiB of three-byte runes", raw: `"` + strings.Repeat("€", 341) + `"`, want: strings.Repeat("€", 341)},
		{name: "1 KiB once decoded", raw: `"` + strings.Repeat(testutil.Esc(`~u0072`), 1024) + `"`, want: strings.Repeat("r", 1024)},
		{name: "one byte over", raw: `"` + strings.Repeat("r", 1025) + `"`, want: ""},
		{name: "over by a rune", raw: `"` + strings.Repeat("r", 1023) + `é"`, want: ""},
		{name: "empty", raw: `""`, want: ""},
		{name: "number", raw: `7`, want: ""},
		{name: "array", raw: `["t"]`, want: ""},
		{name: jsonNull, raw: jsonNull, want: ""},
		{name: sjAbsent, raw: sjAbsent, want: ""},
		{name: "lone quote", raw: `"`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, retryToken(sjRaw(tt.raw)))
		})
	}
}

func TestDecodeASCIIRange(t *testing.T) {
	assert.True(t, asciiRange("", 0x21, 0x7e))
	assert.True(t, asciiRange("!~", 0x21, 0x7e), "both bounds are inclusive")
	assert.False(t, asciiRange(" ", 0x21, 0x7e))
	assert.False(t, asciiRange("\x7f", 0x21, 0x7e))
	assert.False(t, asciiRange("ab\x00", 0x21, 0x7e))
	assert.False(t, asciiRange("é", 0x21, 0x7e))
	assert.True(t, asciiRange("abc", 'a', 'c'))
	assert.False(t, asciiRange("abcd", 'a', 'c'))
}

func TestDecodeTruncateUTF8(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{name: "shorter", s: "abc", n: 5, want: "abc"},
		{name: "exact", s: "abc", n: 3, want: "abc"},
		{name: "ASCII cut", s: "abcdef", n: 4, want: "abcd"},
		{name: "zero", s: "abc", n: 0, want: ""},
		{name: "before a two-byte rune", s: "ab" + "é", n: 2, want: "ab"},
		{name: "inside a two-byte rune", s: "ab" + "é", n: 3, want: "ab"},
		{name: "inside a three-byte rune", s: "a€", n: 3, want: "a"},
		{name: "after a three-byte rune", s: "a€b", n: 4, want: "a€"},
		{name: "inside a four-byte rune", s: "a\U0001F600", n: 4, want: "a"},
		{name: "four-byte rune at the start", s: sjEmoji, n: 3, want: ""},
		{name: "only continuation bytes", s: "\x80\x80\x80", n: 2, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncateUTF8(tt.s, tt.n))
		})
	}

	// For every cut of a mixed string: a prefix, within n, never splitting a
	// rune, and at most three bytes short.
	s := strings.Repeat("aé€\U0001F600", 40)
	for n := 0; n <= len(s)+1; n++ {
		got := truncateUTF8(s, n)
		require.True(t, strings.HasPrefix(s, got), "n=%d", n)
		require.LessOrEqual(t, len(got), n)
		require.True(t, utf8.ValidString(got), "n=%d", n)
		require.GreaterOrEqual(t, len(got), min(n, len(s))-3, "n=%d", n)
	}
}

// TestDecodeTextsNeverAliasTheBody proves that decoded text is a copy:
// changing the body afterwards changes nothing.
func TestDecodeTextsNeverAliasTheBody(t *testing.T) {
	body := []byte(`{"filename":"list.csv","queue_id":"Q1","retry_token":"r","flags":["has_dns"]}`)
	o := readObject(body, []string{"filename", "queue_id", "retry_token", "flags"})
	name, derr := decodeText(o.fields[0], sjFilenameField)
	require.Nil(t, derr)
	queue := queueID(o.fields[1])
	retry := retryToken(o.fields[2])
	flags, derr := decodeFlags(o.fields[3], path{})
	require.Nil(t, derr)
	for i := range body {
		body[i] = 'x'
	}
	assert.Equal(t, "list.csv", name)
	assert.Equal(t, "Q1", queue)
	assert.Equal(t, "r", retry)
	assert.Equal(t, Flags{FlagHasDNS}, flags)
	assert.False(t, bytes.Contains(body, []byte("list")))
}
