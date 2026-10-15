package neverbounce_test

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

func TestCheckEmailWireBodyVariants(t *testing.T) {
	tests := []struct {
		name string
		req  neverbounce.CheckEmailRequest
		want string
	}{
		{
			name: "all options",
			req: neverbounce.CheckEmailRequest{
				Email: testEmail, AddressInfo: true, CreditsInfo: true, Timeout: 20 * time.Second,
				LeverageHistoricalData: new(false),
			},
			want: `,"email":"` + testEmail + `","address_info":true,"credits_info":true,"timeout":20,` +
				`"request_meta_data":{"leverage_historical_data":false}}`,
		},
		{
			name: "leverage historical data true",
			req:  neverbounce.CheckEmailRequest{Email: testEmail, LeverageHistoricalData: new(true)},
			want: `,"email":"` + testEmail + `","address_info":false,"credits_info":false,` +
				`"request_meta_data":{"leverage_historical_data":true}}`,
		},
		{
			name: "leverage historical data nil is not sent",
			req:  neverbounce.CheckEmailRequest{Email: testEmail, LeverageHistoricalData: nil},
			want: `,"email":"` + testEmail + `","address_info":false,"credits_info":false}`,
		},
		{
			name: "address info only",
			req:  neverbounce.CheckEmailRequest{Email: testEmail, AddressInfo: true},
			want: `,"email":"` + testEmail + `","address_info":true,"credits_info":false}`,
		},
		{
			name: "credits info only",
			req:  neverbounce.CheckEmailRequest{Email: testEmail, CreditsInfo: true},
			want: `,"email":"` + testEmail + `","address_info":false,"credits_info":true}`,
		},
		{
			name: "escaped address sent verbatim",
			req:  neverbounce.CheckEmailRequest{Email: ` "quoted\back"@example.com `},
			want: `,"email":" \"quoted\\back\"@example.com ","address_info":false,"credits_info":false}`,
		},
		{
			name: "non-ASCII address sent verbatim",
			req:  neverbounce.CheckEmailRequest{Email: "zoë.ñandú@пример.example"},
			want: `,"email":"zoë.ñandú@пример.example","address_info":false,"credits_info":false}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, testutil.Step{Body: checkOK})
			_, err := c.CheckEmail(t.Context(), tt.req)
			require.NoError(t, err)
			r := testutil.OnlyRequest(t, s)
			assertWireRequest(t, r, pathCheck, mediaJSON)
			assertBody(t, keyMember+tt.want, r.Body)
		})
	}
}

func TestCheckEmailDecodesEveryField(t *testing.T) {
	const answer = `{"status":"success","result":"catchall","flags":["has_dns","acedemic_host","has_dns"],` +
		`"suggested_correction":"avery@example.org","address_info":{"original_email":"a","normalized_email":"b",` +
		`"addr":"c","alias":"d","host":"e","fqdn":"f","domain":"g","subdomain":"h","tld":"i"},` +
		`"credits_info":{"paid_credits_used":1,"free_credits_used":0,"paid_credits_remaining":99},` +
		`"retry_token":"opaque","execution_time":412,"Result":"invalid"}`
	res, err := callOnce(t, pathCheck, answer, pvCheck)
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.CheckEmailResult{
		Result:              neverbounce.ResultCatchall,
		Flags:               neverbounce.Flags{neverbounce.FlagHasDNS, "acedemic_host", neverbounce.FlagHasDNS},
		SuggestedCorrection: "avery@example.org",
		AddressInfo: &neverbounce.AddressInfo{
			OriginalEmail: "a", NormalizedEmail: "b", Addr: "c", Alias: "d", Host: "e", FQDN: "f",
			Domain: "g", Subdomain: "h", TLD: "i",
		},
		CreditsInfo: &neverbounce.CreditsInfo{
			PaidCreditsUsed: new(int64(1)), FreeCreditsUsed: new(int64(0)), PaidCreditsRemaining: new(int64(99)),
		},
		RetryToken:    "opaque",
		ExecutionTime: new(int64(412)),
	}, res)
	assert.False(t, res.Flags[1].Known(), "the v4.1 spelling is kept verbatim as unknown")
}

func TestCheckEmailUnknownIsAnAnswer(t *testing.T) {
	res, err := callOnce(t, pathCheck, `{"status":"success","result":"unknown"}`, pvCheck)
	require.NoError(t, err)
	assert.Equal(t, neverbounce.ResultUnknown, res.Result)
	assert.True(t, res.Result.Known())
}

func TestCheckEmailDiagnosticsOptIn(t *testing.T) {
	body := `{"status":"general_failure","message":"bad key ` + testKey + ` for ` + testEmail + `"}`
	c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body), neverbounce.WithProviderDiagnostics())
	_, err := c.CheckEmail(t.Context(), checkRequest())
	assertCallError(t, err, wantCall{
		Kind: neverbounce.FailureRejected, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
		API: neverbounce.APIStatusGeneralFailure,
	})
	assert.Equal(t, "bad key [redacted] for "+testEmail, callError(t, err).Diagnostics().Message)
}

func TestCheckEmailInvalidSendsNothing(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	c := newTransportClient(t, ct)
	tests := []struct {
		name   string
		req    neverbounce.CheckEmailRequest
		field  string
		reason string
	}{
		{"no address", neverbounce.CheckEmailRequest{}, "Email", reasonRequired},
		{"blank", neverbounce.CheckEmailRequest{Email: "   "}, "Email", "blank"},
		{"control", neverbounce.CheckEmailRequest{Email: "a\nb@example.com"}, "Email", reasonControlCharacter},
		{"invalid utf-8", neverbounce.CheckEmailRequest{Email: "a\xffb@example.com"}, "Email", pvReasonUTF8},
		{"fraction of a second", neverbounce.CheckEmailRequest{Email: testEmail, Timeout: 1500 * time.Millisecond}, pvFieldTimeout, "not_whole_seconds"},
		{"over 300s", neverbounce.CheckEmailRequest{Email: testEmail, Timeout: 301 * time.Second}, pvFieldTimeout, reasonOutOfRange},
		{"over client timeout", neverbounce.CheckEmailRequest{Email: testEmail, Timeout: 51 * time.Second}, pvFieldTimeout, "exceeds_client_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CheckEmail(t.Context(), tt.req)
			assertValidation(t, err, tt.field, tt.reason)
		})
	}
	assert.Zero(t, ct.Attempts())
}

// Values the single/check tests repeat.
const (
	pvReasonUTF8    = "invalid_utf8"
	pvFieldResult   = "result"
	pvFieldTimeout  = "Timeout"
	pvStatusAuth    = "auth_failure"
	pvStatusUnavail = "temp_unavail"
	pvOpCheck       = "CheckEmail"
)

// pvCheck makes one CheckEmail call with the minimal request, for callOnce.
func pvCheck(ctx context.Context, c *neverbounce.Client) (*neverbounce.CheckEmailResult, error) {
	return c.CheckEmail(ctx, checkRequest())
}

// pvCheckAnswer returns a success envelope whose result is valid, followed by
// members.
func pvCheckAnswer(members string) string {
	if members == "" {
		return checkOK
	}
	return `{"status":"success","result":"valid",` + members + `}`
}

// pvAddressMembers are the address_info member names, in provider order.
func pvAddressMembers() []string {
	return []string{
		"original_email", "normalized_email", "addr", "alias", "host", "fqdn", "domain", "subdomain", "tld",
	}
}

// pvCreditsMembers are the credits_info member names, in provider order.
func pvCreditsMembers() []string {
	return []string{"paid_credits_used", "free_credits_used", "paid_credits_remaining", "free_credits_remaining"}
}

// pvObjectOf returns a JSON object whose every member in names holds value.
func pvObjectOf(names []string, value string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = `"` + n + `":` + value
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestCheckEmailPresenceMatrix(t *testing.T) {
	zero := func() *int64 { return new(int64(0)) }
	tests := []struct {
		name    string
		members string
		want    func(r *neverbounce.CheckEmailResult)
	}{
		{name: "every optional member absent"},
		{name: "flags null", members: `"flags":null`},
		{name: "flags empty", members: `"flags":[]`},
		{name: "suggested_correction null", members: `"suggested_correction":null`},
		{name: "suggested_correction empty", members: `"suggested_correction":""`},
		{name: "address_info null", members: `"address_info":null`},
		{
			name: "address_info empty object", members: `"address_info":{}`,
			want: func(r *neverbounce.CheckEmailResult) { r.AddressInfo = &neverbounce.AddressInfo{} },
		},
		{
			name: "address_info members null", members: `"address_info":` + pvObjectOf(pvAddressMembers(), "null"),
			want: func(r *neverbounce.CheckEmailResult) { r.AddressInfo = &neverbounce.AddressInfo{} },
		},
		{
			name: "address_info members empty", members: `"address_info":` + pvObjectOf(pvAddressMembers(), `""`),
			want: func(r *neverbounce.CheckEmailResult) { r.AddressInfo = &neverbounce.AddressInfo{} },
		},
		{
			name: "address_info unknown and case-folded members ignored",
			members: `"address_info":{"Host":"a.example.com","HOST":"b.example.com","hostname":"c.example.com",` +
				`"extra":{"nested":[1,2]}}`,
			want: func(r *neverbounce.CheckEmailResult) { r.AddressInfo = &neverbounce.AddressInfo{} },
		},
		{name: "credits_info null", members: `"credits_info":null`},
		{
			name: "credits_info empty object", members: `"credits_info":{}`,
			want: func(r *neverbounce.CheckEmailResult) { r.CreditsInfo = &neverbounce.CreditsInfo{} },
		},
		{
			name: "credits_info members null", members: `"credits_info":` + pvObjectOf(pvCreditsMembers(), "null"),
			want: func(r *neverbounce.CheckEmailResult) { r.CreditsInfo = &neverbounce.CreditsInfo{} },
		},
		{
			name: "credits_info members zero", members: `"credits_info":` + pvObjectOf(pvCreditsMembers(), "0"),
			want: func(r *neverbounce.CheckEmailResult) {
				r.CreditsInfo = &neverbounce.CreditsInfo{
					PaidCreditsUsed: zero(), FreeCreditsUsed: zero(), PaidCreditsRemaining: zero(), FreeCreditsRemaining: zero(),
				}
			},
		},
		{
			name: "credits_info negative zero is zero", members: `"credits_info":{"free_credits_used":-0}`,
			want: func(r *neverbounce.CheckEmailResult) {
				r.CreditsInfo = &neverbounce.CreditsInfo{FreeCreditsUsed: zero()}
			},
		},
		{
			name:    "credits_info largest count",
			members: `"credits_info":{"paid_credits_remaining":9223372036854775807}`,
			want: func(r *neverbounce.CheckEmailResult) {
				r.CreditsInfo = &neverbounce.CreditsInfo{PaidCreditsRemaining: new(int64(math.MaxInt64))}
			},
		},
		{name: "retry_token null", members: `"retry_token":null`},
		{name: "retry_token empty", members: `"retry_token":""`},
		{name: "execution_time null", members: `"execution_time":null`},
		{
			name: "execution_time zero", members: `"execution_time":0`,
			want: func(r *neverbounce.CheckEmailResult) { r.ExecutionTime = zero() },
		},
		{
			name: "every optional member null at once",
			members: `"flags":null,"suggested_correction":null,"address_info":null,"credits_info":null,` +
				`"retry_token":null,"execution_time":null,"message":null,"query":{"email":"x"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := callOnce(t, pathCheck, pvCheckAnswer(tt.members), pvCheck)
			require.NoError(t, err)
			want := &neverbounce.CheckEmailResult{Result: neverbounce.ResultValid}
			if tt.want != nil {
				tt.want(want)
			}
			assert.Equal(t, want, res)
		})
	}
}

func TestCheckEmailCreditsNilIsNotZero(t *testing.T) {
	res, err := callOnce(t, pathCheck,
		pvCheckAnswer(`"credits_info":{"paid_credits_used":0,"free_credits_used":null}`), pvCheck)
	require.NoError(t, err)
	require.NotNil(t, res.CreditsInfo)
	require.NotNil(t, res.CreditsInfo.PaidCreditsUsed, "a reported zero is a pointer to 0")
	assert.Equal(t, int64(0), *res.CreditsInfo.PaidCreditsUsed)
	assert.Nil(t, res.CreditsInfo.FreeCreditsUsed, "null is not reported")
	assert.Nil(t, res.CreditsInfo.PaidCreditsRemaining, "absent is not reported")
	assert.Nil(t, res.CreditsInfo.FreeCreditsRemaining)
}

func TestCheckEmailEveryDocumentedResult(t *testing.T) {
	for _, r := range pvDocumentedResults() {
		t.Run(string(r), func(t *testing.T) {
			res, err := callOnce(t, pathCheck, `{"status":"success","result":"`+string(r)+`"}`, pvCheck)
			require.NoError(t, err)
			assert.Equal(t, r, res.Result)
			assert.True(t, res.Result.Known())
		})
	}
	t.Run("escaped spelling", func(t *testing.T) {
		res, err := callOnce(t, pathCheck, testutil.Esc(`{"status":"success","result":"~u0076alid"}`), pvCheck)
		require.NoError(t, err)
		assert.Equal(t, neverbounce.ResultValid, res.Result)
	})
	for _, r := range []string{"risky", "accept_all", "valid2", strings.Repeat("z", 64)} {
		t.Run("unknown "+r[:min(len(r), 12)], func(t *testing.T) {
			res, err := callOnce(t, pathCheck, `{"status":"success","result":"`+r+`"}`, pvCheck)
			require.NoError(t, err, "an undocumented result is an answer, kept as sent")
			assert.Equal(t, neverbounce.Result(r), res.Result)
			assert.False(t, res.Result.Known())
		})
	}
}

func TestCheckEmailEveryDocumentedFlag(t *testing.T) {
	flags := pvDocumentedFlags()
	quoted := make([]string, len(flags))
	for i, f := range flags {
		quoted[i] = `"` + string(f) + `"`
	}
	res, err := callOnce(t, pathCheck, pvCheckAnswer(`"flags":[`+strings.Join(quoted, ",")+`]`), pvCheck)
	require.NoError(t, err)
	assert.Equal(t, neverbounce.Flags(flags), res.Flags, "provider order is kept")
	for _, f := range res.Flags {
		assert.True(t, f.Known(), f)
		assert.True(t, res.Flags.Has(f), f)
	}

	t.Run("unknown flags kept verbatim", func(t *testing.T) {
		long := strings.Repeat("q", 64)
		body := pvCheckAnswer(`"flags":["acedemic_host","academic_host","new_flag_2027","` + long + `"]`)
		got, err := callOnce(t, pathCheck, body, pvCheck)
		require.NoError(t, err)
		assert.Equal(t, neverbounce.Flags{"acedemic_host", neverbounce.FlagAcademicHost, "new_flag_2027", neverbounce.Flag(long)},
			got.Flags)
		assert.False(t, got.Flags[0].Known(), "the v4.1 misspelling is never normalized")
		assert.True(t, got.Flags[1].Known())
		assert.False(t, got.Flags[2].Known())
		assert.False(t, got.Flags.Has(neverbounce.FlagHasDNS))
	})

	t.Run("64 flags are allowed", func(t *testing.T) {
		got, err := callOnce(t, pathCheck, pvCheckAnswer(`"flags":`+pvFlagList(64)), pvCheck)
		require.NoError(t, err)
		assert.Len(t, got.Flags, 64)
	})
}

// pvFlagList returns a JSON array of n has_dns flags.
func pvFlagList(n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(`"has_dns",`, n), ",") + "]"
}

// Causes of decoding failures, as their text reads through Unwrap.
const (
	pvCauseMissing  = "lacks a required field"
	pvCauseType     = "wrong JSON type"
	pvCauseRange    = "out of range"
	pvCauseVocab    = "not a valid token"
	pvCauseTooLarge = "exceeds its size bound"
)

// pvDecodeFailure is a single/check answer that fails to decode.
type pvDecodeFailure struct {
	name  string
	body  string
	kind  neverbounce.FailureKind
	field string
	cause string
}

// pvCheckTokenFailures are answers whose result or flags break the token
// rules.
func pvCheckTokenFailures() []pvDecodeFailure {
	const (
		unexpected = neverbounce.FailureUnexpectedResponse
		malformed  = neverbounce.FailureMalformedResponse
	)
	result := func(v string) string { return `{"status":"success","result":` + v + `}` }
	flags := func(v string) string { return pvCheckAnswer(`"flags":` + v) }
	return []pvDecodeFailure{
		{"result uppercase", result(`"Valid"`), unexpected, pvFieldResult, pvCauseVocab},
		{"result empty", result(`""`), unexpected, pvFieldResult, pvCauseVocab},
		{"result with a space", result(`"not valid"`), unexpected, pvFieldResult, pvCauseVocab},
		{"result with a dash", result(`"catch-all"`), unexpected, pvFieldResult, pvCauseVocab},
		{"result non-ASCII", result(`"válid"`), unexpected, pvFieldResult, pvCauseVocab},
		{"result 65 bytes", result(`"` + strings.Repeat("a", 65) + `"`), unexpected, pvFieldResult, pvCauseVocab},
		{"result escaped newline", result(testutil.Esc(`"valid~n"`)), unexpected, pvFieldResult, pvCauseVocab},
		{"result missing", `{"status":"success"}`, malformed, pvFieldResult, pvCauseMissing},
		{"result null", result(`null`), malformed, pvFieldResult, pvCauseMissing},
		{"result case-folded only", `{"status":"success","Result":"valid","RESULT":"valid"}`, malformed, pvFieldResult, pvCauseMissing},
		{"result number", result(`1`), malformed, pvFieldResult, pvCauseType},
		{"result boolean", result(`true`), malformed, pvFieldResult, pvCauseType},
		{"result array", result(`["valid"]`), malformed, pvFieldResult, pvCauseType},
		{"result object", result(`{"value":"valid"}`), malformed, pvFieldResult, pvCauseType},
		{"flag uppercase", flags(`["has_dns","Has_DNS"]`), unexpected, "flags[1]", pvCauseVocab},
		{"flag empty", flags(`[""]`), unexpected, "flags[0]", pvCauseVocab},
		{"flag with a space", flags(`["has_dns","has_dns","bad flag"]`), unexpected, "flags[2]", pvCauseVocab},
		{"flag 65 bytes", flags(`["` + strings.Repeat("f", 65) + `"]`), unexpected, "flags[0]", pvCauseVocab},
		{"flag number", flags(`["has_dns",1]`), malformed, "flags[1]", pvCauseType},
		{"flag null", flags(`["has_dns",null]`), malformed, "flags[1]", pvCauseType},
		{"flag array", flags(`[["has_dns"]]`), malformed, "flags[0]", pvCauseType},
		{"flags string", flags(`"has_dns"`), malformed, "flags", pvCauseType},
		{"flags object", flags(`{"0":"has_dns"}`), malformed, "flags", pvCauseType},
		{"flags boolean", flags(`true`), malformed, "flags", pvCauseType},
		{"65 flags", flags(pvFlagList(65)), neverbounce.FailureResponseTooLarge, "flags", pvCauseTooLarge},
		{"result before flags", result(`"Bad","flags":[1]`), unexpected, pvFieldResult, pvCauseVocab},
		{"flags before credits", flags(`[1],"credits_info":[]`), malformed, "flags[0]", pvCauseType},
	}
}

// pvCheckMemberFailures are answers whose optional members have the wrong
// type, range or size.
func pvCheckMemberFailures() []pvDecodeFailure {
	const malformed = neverbounce.FailureMalformedResponse
	member := func(name, v string) string { return pvCheckAnswer(`"` + name + `":` + v) }
	credits := func(v string) string { return member("credits_info", v) }
	tooLong := testutil.JSONQuote(strings.Repeat("s", 4097))
	out := []pvDecodeFailure{
		{"suggested_correction number", member("suggested_correction", "1"), malformed, "suggested_correction", pvCauseType},
		{"suggested_correction array", member("suggested_correction", `[]`), malformed, "suggested_correction", pvCauseType},
		{"suggested_correction object", member("suggested_correction", `{}`), malformed, "suggested_correction", pvCauseType},
		{"suggested_correction boolean", member("suggested_correction", `false`), malformed, "suggested_correction", pvCauseType},
		{
			"suggested_correction over 4 KiB", member("suggested_correction", tooLong),
			neverbounce.FailureResponseTooLarge, "suggested_correction", pvCauseTooLarge,
		},
		{"address_info array", member("address_info", `[]`), malformed, "address_info", pvCauseType},
		{"address_info string", member("address_info", `"x"`), malformed, "address_info", pvCauseType},
		{"address_info number", member("address_info", `1`), malformed, "address_info", pvCauseType},
		{"address_info boolean", member("address_info", `true`), malformed, "address_info", pvCauseType},
		{"credits_info array", credits(`[]`), malformed, "credits_info", pvCauseType},
		{"credits_info string", credits(`"5"`), malformed, "credits_info", pvCauseType},
		{"credits_info number", credits(`5`), malformed, "credits_info", pvCauseType},
	}
	for _, name := range pvAddressMembers() {
		field := "address_info." + name
		out = append(out,
			pvDecodeFailure{field + " number", member("address_info", `{"`+name+`":1}`), malformed, field, pvCauseType},
			pvDecodeFailure{field + " object", member("address_info", `{"`+name+`":{}}`), malformed, field, pvCauseType},
			pvDecodeFailure{
				field + " over 4 KiB", member("address_info", `{"`+name+`":`+tooLong+`}`),
				neverbounce.FailureResponseTooLarge, field, pvCauseTooLarge,
			},
		)
	}
	for _, name := range pvCreditsMembers() {
		field := "credits_info." + name
		obj := func(v string) string { return credits(`{"` + name + `":` + v + `}`) }
		out = append(out,
			pvDecodeFailure{field + " negative", obj("-1"), malformed, field, pvCauseRange},
			pvDecodeFailure{field + " overflow", obj("9223372036854775808"), malformed, field, pvCauseRange},
			pvDecodeFailure{field + " very negative", obj("-9223372036854775809"), malformed, field, pvCauseRange},
			pvDecodeFailure{field + " fraction", obj("1.5"), malformed, field, pvCauseType},
			pvDecodeFailure{field + " whole fraction", obj("2.0"), malformed, field, pvCauseType},
			pvDecodeFailure{field + " exponent", obj("1e2"), malformed, field, pvCauseType},
			pvDecodeFailure{field + " quoted", obj(`"5"`), malformed, field, pvCauseType},
			pvDecodeFailure{field + " boolean", obj("true"), malformed, field, pvCauseType},
			pvDecodeFailure{field + " object", obj("{}"), malformed, field, pvCauseType},
		)
	}
	return out
}

func TestCheckEmailDecodeFailures(t *testing.T) {
	cases := append(pvCheckTokenFailures(), pvCheckMemberFailures()...)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callOnce(t, pathCheck, tt.body, pvCheck)
			// A decoding failure has no failure status, so API stays empty.
			assertCallError(t, err, wantCall{
				Kind: tt.kind, Op: neverbounce.OpCheckEmail, Status: http.StatusOK, Field: tt.field,
			})
			assert.ErrorContains(t, callError(t, err).Unwrap(), tt.cause)
		})
	}
}

func TestCheckEmailTextBounds(t *testing.T) {
	exact := strings.Repeat("s", 4096)
	t.Run("4 KiB of text is allowed", func(t *testing.T) {
		members := `"suggested_correction":"` + exact + `","address_info":` + pvObjectOf(pvAddressMembers(), `"`+exact+`"`)
		res, err := callOnce(t, pathCheck, pvCheckAnswer(members), pvCheck)
		require.NoError(t, err)
		assert.Equal(t, exact, res.SuggestedCorrection)
		require.NotNil(t, res.AddressInfo)
		assert.Equal(t, exact, res.AddressInfo.Host)
		assert.Equal(t, exact, res.AddressInfo.TLD)
	})
	t.Run("the bound counts decoded bytes", func(t *testing.T) {
		escaped := testutil.Esc(strings.Repeat("~u0073", 4096))
		res, err := callOnce(t, pathCheck, pvCheckAnswer(`"suggested_correction":"`+escaped+`"`), pvCheck)
		require.NoError(t, err, "24 KiB of escapes that decode to 4 KiB")
		assert.Equal(t, exact, res.SuggestedCorrection)

		_, err = callOnce(t, pathCheck,
			pvCheckAnswer(`"address_info":{"host":"`+escaped+testutil.Esc("~u0073")+`"}`), pvCheck)
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureResponseTooLarge, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
			Field: "address_info.host",
		})
	})
	t.Run("multi-byte text is counted in bytes", func(t *testing.T) {
		_, err := callOnce(t, pathCheck, pvCheckAnswer(`"suggested_correction":"`+strings.Repeat("é", 2049)+`"`), pvCheck)
		assertCallError(t, err, wantCall{
			Kind: neverbounce.FailureResponseTooLarge, Op: neverbounce.OpCheckEmail, Status: http.StatusOK,
			Field: "suggested_correction",
		})
	})
}

// TestCheckEmailMetadataNeverFails checks retry_token, single/check's own
// metadata. execution_time, which every operation shares, is in
// envelope_test.go.
func TestCheckEmailMetadataNeverFails(t *testing.T) {
	maxToken := strings.Repeat("r", 1024)
	tests := []struct {
		name  string
		value string
		token string
	}{
		{name: "retry_token number", value: `"retry_token":1`},
		{name: "retry_token object", value: `"retry_token":{"t":"x"}`},
		{name: "retry_token array", value: `"retry_token":["x"]`},
		{name: "retry_token boolean", value: `"retry_token":true`},
		{name: "retry_token over 1 KiB", value: `"retry_token":"` + maxToken + `r"`},
		{name: "retry_token 1 KiB", value: `"retry_token":"` + maxToken + `"`, token: maxToken},
		{name: "retry_token escaped", value: testutil.Esc(`"retry_token":"a~u0062~/c"`), token: "ab/c"},
		{
			name: "both malformed with every verdict member",
			value: `"execution_time":"x","retry_token":7,"flags":["has_dns"],"suggested_correction":"",` +
				`"address_info":{},"credits_info":{}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := callOnce(t, pathCheck, pvCheckAnswer(tt.value), pvCheck)
			require.NoError(t, err, "metadata never fails a billed check")
			assert.Equal(t, neverbounce.ResultValid, res.Result)
			assert.Nil(t, res.ExecutionTime)
			assert.Equal(t, tt.token, res.RetryToken)
		})
	}
}

func TestCheckEmailSyntheticFixture(t *testing.T) {
	body := testutil.Fixture(t, "synthetic/check_full.json")
	res, err := callOnce(t, pathCheck, string(body), pvCheck)
	require.NoError(t, err)
	want := &neverbounce.CheckEmailResult{
		Result: neverbounce.ResultValid,
		Flags: neverbounce.Flags{
			neverbounce.FlagHasDNS, neverbounce.FlagHasDNSMX, neverbounce.FlagSMTPConnectable, neverbounce.FlagRoleAccount,
		},
		AddressInfo: &neverbounce.AddressInfo{
			OriginalEmail: "support@example.com", NormalizedEmail: "support@example.com", Addr: "support",
			Host: "example.com", FQDN: "example.com", Domain: "example", TLD: "com",
		},
		CreditsInfo: &neverbounce.CreditsInfo{
			PaidCreditsUsed: new(int64(1)), FreeCreditsUsed: new(int64(0)),
			PaidCreditsRemaining: new(int64(4321)), FreeCreditsRemaining: new(int64(0)),
		},
		ExecutionTime: new(int64(377)),
	}
	assert.Equal(t, want, res)
	assert.True(t, res.Flags.Has(neverbounce.FlagRoleAccount))

	back := assertRoundTrip(t, res, `{"result":"valid",`+
		`"flags":["has_dns","has_dns_mx","smtp_connectable","role_account"],`+
		`"address_info":{"original_email":"support@example.com","normalized_email":"support@example.com",`+
		`"addr":"support","host":"example.com","fqdn":"example.com","domain":"example","tld":"com"},`+
		`"credits_info":{"paid_credits_used":1,"free_credits_used":0,"paid_credits_remaining":4321,`+
		`"free_credits_remaining":0},"execution_time":377}`)
	assert.Equal(t, want, back, "the result survives a JSON round trip")
}

func TestCheckEmailEmailBoundaries(t *testing.T) {
	const domain = "@example.com"
	exact := strings.Repeat("a", 1024-len(domain)) + domain
	t.Run("accepted and sent verbatim", func(t *testing.T) {
		for _, email := range []string{
			exact,
			"a" + domain,
			" padded" + domain + " ",
			"line\xe2\x80\xa8separator" + domain,
			"zero\xe2\x80\x8bwidth" + domain,
			"not an address at all",
			"@",
		} {
			c, s := newScriptClient(t, testutil.Step{Body: checkOK})
			_, err := c.CheckEmail(t.Context(), neverbounce.CheckEmailRequest{Email: email})
			require.NoError(t, err, "%q", email)
			r := testutil.OnlyRequest(t, s)
			assertBody(t, keyMember+`,"email":"`+email+`","address_info":false,"credits_info":false}`, r.Body)
		}
	})

	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	c := newTransportClient(t, ct)
	tests := []struct {
		name   string
		email  string
		reason string
	}{
		{"1025 bytes", "a" + exact, "too_long"},
		{"too long before control", "\n" + exact, "too_long"},
		{"too long before invalid UTF-8", "\xff" + exact, "too_long"},
		{"no address", "", reasonRequired},
		{"spaces", "   ", "blank"},
		{"no-break spaces", "\xc2\xa0\xc2\xa0", "blank"},
		{"line separator only", "\xe2\x80\xa8", "blank"},
		{"tab only is a control character", "\t", reasonControlCharacter},
		{"NUL", "a\x00b" + domain, reasonControlCharacter},
		{"SOH", "a\x01b" + domain, reasonControlCharacter},
		{"tab", "a\tb" + domain, reasonControlCharacter},
		{"line feed", "a\nb" + domain, reasonControlCharacter},
		{"carriage return", "a\rb" + domain, reasonControlCharacter},
		{"escape", "a\x1bb" + domain, reasonControlCharacter},
		{"unit separator", "a\x1fb" + domain, reasonControlCharacter},
		{"DEL", "a\x7fb" + domain, reasonControlCharacter},
		{"C1 U+0080", "a\xc2\x80b" + domain, reasonControlCharacter},
		{"C1 next line U+0085", "a\xc2\x85b" + domain, reasonControlCharacter},
		{"C1 U+009F", "a\xc2\x9fb" + domain, reasonControlCharacter},
		{"invalid byte", "a\xffb" + domain, pvReasonUTF8},
		{"truncated sequence", "a" + domain + "\xc3", pvReasonUTF8},
		{"overlong encoding", "a\xc0\xafb" + domain, pvReasonUTF8},
		{"encoded surrogate", "a\xed\xa0\x80b" + domain, pvReasonUTF8},
		{"beyond U+10FFFF", "a\xf4\x90\x80\x80b" + domain, pvReasonUTF8},
		{"invalid UTF-8 before control", "a\xff\nb" + domain, pvReasonUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CheckEmail(t.Context(), neverbounce.CheckEmailRequest{Email: tt.email, Timeout: time.Hour})
			assertValidation(t, err, "Email", tt.reason)
			assert.NotContains(t, err.Error(), domain, "the error never quotes the address")
		})
	}
	assert.Zero(t, ct.Attempts(), "an invalid address is never sent")
}

func TestCheckEmailTimeoutBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		client time.Duration // WithTimeout; 0 keeps DefaultTimeout
		value  time.Duration
		reason string // "" when valid
	}{
		{name: "0 is not sent", value: 0},
		{name: "1s", value: time.Second},
		{name: "default client timeout minus 10s", value: 50 * time.Second},
		{name: "default client timeout minus 9s", value: 51 * time.Second, reason: "exceeds_client_timeout"},
		{name: "300s under the default client timeout", value: 300 * time.Second, reason: "exceeds_client_timeout"},
		{name: "300s with a 310s client", client: 310 * time.Second, value: 300 * time.Second},
		{name: "300s with a 309s client", client: 309 * time.Second, value: 300 * time.Second, reason: "exceeds_client_timeout"},
		{name: "300s with a 10m client", client: 10 * time.Minute, value: 300 * time.Second},
		{name: "301s with a 10m client", client: 10 * time.Minute, value: 301 * time.Second, reason: reasonOutOfRange},
		{name: "exactly client timeout minus 10s", client: 11 * time.Second, value: time.Second},
		{name: "one second over", client: 11 * time.Second, value: 2 * time.Second, reason: "exceeds_client_timeout"},
		{name: "client timeout of 10s", client: 10 * time.Second, value: time.Second, reason: "exceeds_client_timeout"},
		{name: "fractional client timeout", client: 20500 * time.Millisecond, value: 10 * time.Second},
		{name: "1ns", value: time.Nanosecond, reason: "not_whole_seconds"},
		{name: "999ms", value: 999 * time.Millisecond, reason: "not_whole_seconds"},
		{name: "1.5s", value: 1500 * time.Millisecond, reason: "not_whole_seconds"},
		{name: "negative 1s", value: -time.Second, reason: reasonOutOfRange},
		{name: "negative 300s", value: -300 * time.Second, reason: reasonOutOfRange},
		{name: "negative 1.5s", value: -1500 * time.Millisecond, reason: "not_whole_seconds"},
		{name: "fraction checked before range", client: 10 * time.Minute, value: 400500 * time.Millisecond, reason: "not_whole_seconds"},
		{name: "range checked before client timeout", value: 301 * time.Second, reason: reasonOutOfRange},
		{name: "largest duration", value: math.MaxInt64, reason: "not_whole_seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testutil.NewScript(testutil.Step{Body: checkOK})
			var opts []neverbounce.Option
			if tt.client != 0 {
				opts = append(opts, neverbounce.WithTimeout(tt.client))
			}
			c := newTransportClient(t, s, opts...)
			_, err := c.CheckEmail(t.Context(), neverbounce.CheckEmailRequest{Email: testEmail, Timeout: tt.value})
			if tt.reason != "" {
				assertValidation(t, err, pvFieldTimeout, tt.reason)
				assert.Zero(t, s.Calls())
				return
			}
			require.NoError(t, err)
			r := testutil.OnlyRequest(t, s)
			want := keyMember + `,"email":"` + testEmail + `","address_info":false,"credits_info":false`
			if tt.value != 0 {
				want += `,"timeout":` + strconv.Itoa(int(tt.value/time.Second))
			}
			assertBody(t, want+"}", r.Body)
		})
	}
}

// TestCheckEmailValidationOrder checks the order of CheckEmail's own rules:
// the Email before the Timeout. The order every method shares (Client, then
// ctx, then the request, then the context's state) is in client_test.go.
func TestCheckEmailValidationOrder(t *testing.T) {
	ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, checkOK)}
	c := newTransportClient(t, ct)
	_, err := c.CheckEmail(t.Context(), neverbounce.CheckEmailRequest{Email: "\n", Timeout: time.Nanosecond})
	assertValidation(t, err, "Email", reasonControlCharacter)
	assert.Zero(t, ct.Attempts())
}
