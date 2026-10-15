package neverbounce

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Literals the strict, decode and envelope tests repeat.
const (
	sjTrue          = "true"
	sjFalse         = "false"
	sjEmoji         = "\U0001F600"
	sjFlagsField    = "flags"
	sjResultsField  = "results"
	sjFilenameField = "filename"
	sjArrayMember   = `{"a":[`
	sjObjectMember  = `{"a":`
	sjEmail         = "sj.example@example.net"
)

// sjManyKeys returns a JSON object with n distinct keys, prefix0 to
// prefix(n-1), each holding its index, plus extra appended verbatim before the
// closing brace. prefix is spelled as JSON, so it may hold escapes.
func sjManyKeys(prefix string, n int, extra string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + prefix + strconv.Itoa(i) + `":` + strconv.Itoa(i))
	}
	b.WriteString(extra)
	b.WriteByte('}')
	return b.String()
}

// sjFill returns prefix, which opens an array inside an object, then n copies
// of el separated by commas, then the closing ]}.
func sjFill(prefix, el string, n int) []byte {
	const suffix = "]}"
	var b bytes.Buffer
	b.Grow(len(prefix) + n*(len(el)+1) + len(suffix))
	b.WriteString(prefix)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(el)
	}
	b.WriteString(suffix)
	return b.Bytes()
}

// sjNest returns depth nested empty arrays, such as [[[]]] for depth 3.
func sjNest(depth int) string { return strings.Repeat("[", depth) + strings.Repeat("]", depth) }

// sjValidCorpus returns valid JSON documents for the strict scanner: duplicate
// keys spelled every way, the linear and the sorted comparison, and the
// shapes the scanners must skip correctly. The oracle decides what each one
// should be.
func sjValidCorpus() []string {
	return []string{
		`{}`, `[]`, `1`, `"a"`, jsonNull, sjTrue, ` { } `, "\t{\"a\":1}\r\n",
		`{"a":1,"b":2}`, `{"a":1,"a":2}`, `{"a":{"a":{"a":1}}}`, `{"a":"b","b":"a"}`, `{"a":1,"A":2}`,
		`{"status":1,"Status":2,"STATUS":3}`,
		`[{"a":1},{"a":1}]`, `[{"a":1,"b":{"c":1,"c":1}}]`, `{"a":{"a":1},"b":[{"c":1,"c":2}]}`,
		`{"x":[1,2,{"y":[{"z":1},{"z":2}]}]}`, ` { "a" : 1 , "a" : 2 } `, `{"a":[{"b":{"c":{"d":[{"e":1,"e":2}]}}}]}`,
		testutil.Esc(`{"~u0073tatus":1,"status":2}`), testutil.Esc(`{"status":1,"st~u0061tus":2}`),
		testutil.Esc(`{"~u0061":1,"a":2}`), testutil.Esc(`{"~u0041":1,"a":2}`), testutil.Esc(`{"S":1,"~u0053":2}`),
		testutil.Esc(`{"~ud800":1,"~udc00":2}`), testutil.Esc(`{"~ud800":1,"` + string(utf8.RuneError) + `":2}`),
		testutil.Esc(`{"~ud83d~ude00":1,"` + sjEmoji + `":2}`), testutil.Esc(`{"~ud83d~ude00":1,"~ud83d":2}`),
		testutil.Esc(`{"~uD83D~uDE00":1,"~ud83d~ude00":2}`), testutil.Esc(`{"~ud800~u0041":1,"~ufffdA":2}`),
		testutil.Esc(`{"a~"b":1,"a~u0022b":2}`), testutil.Esc(`{"~n":1,"~u000a":2}`), testutil.Esc(`{"~/":1,"/":2}`),
		testutil.Esc(`{"~~":1,"~u005c":2}`), testutil.Esc(`{"~b~f~r~t":1,"~u0008~u000c~u000d~u0009":2}`),
		`{"é":1,"é":2}`, testutil.Esc(`{"é":1,"e~u0301":2}`), testutil.Esc(`{"é":1,"~u00e9":2}`),
		testutil.Esc(`{"a":"~"","b":"~"","a~"":1}`), `{"a":"a:","b":"b"}`, `{"a":["a",":"],"b":{"a":"b"}}`,
		`{"":1,"":2}`, `{"":1," ":2}`,
		sjManyKeys("k", 16, ""), sjManyKeys("k", 15, `,"k3":0`), sjManyKeys("k", 17, ""), sjManyKeys("k", 17, `,"k3":0`),
		sjManyKeys("k", 20, `,"k19":1`), sjManyKeys("k", 100, `,"k0":1`), sjManyKeys("k", 40, ""),
		sjManyKeys("k", 17, testutil.Esc(`,"~u006b16":0`)), sjManyKeys(testutil.Esc("~u006b"), 30, `,"k29":0`),
		sjObjectMember + sjManyKeys("k", 20, `,"k7":7`) + `}`, sjArrayMember + sjManyKeys("k", 18, "") + `,` + sjManyKeys("k", 18, "") + `]}`,
		sjObjectMember + sjNest(50) + `,"b":{"c":` + sjNest(10) + `}}`,
	}
}

// sjInvalidCorpus returns inputs that are not one valid JSON value, or not
// valid UTF-8: no scanner may panic on them.
func sjInvalidCorpus() []string {
	return []string{
		"", " ", "\n\t\r ", "{", "}", "[", "]", `"`, `\`, testutil.Esc(`"~`), testutil.Esc(`"~u12`), `{"a"`, `{"a":`,
		`{"a":1,`, `{"a" 1}`, `[1,`, `[,]`, `{,}`, `{"a":1]`, `[{"a":1}`, `]]]}}}`, `{{{{`, `"abc`, `tru`, `-`, `1e`,
		"\xff\xfe", testutil.Esc(`{"a":"~`), testutil.Esc(`{"~`), `:::`, `,,,`, `{"a":[}`, "\x00", `{"k":"v"}garbage`,
		`{"a":{"b":{"c":`, `[[[[[[`, `{"":""`, testutil.Esc(`"~"~"~"`), `{"a":1,"a":`, `{} {}`, `{}x`, `{}]`, `1 2`,
		"\xef\xbb\xbf{}", "{\"a\":\"\xff\"}", "{\"a\":\"\xe2\x82\"}", "{}\xff", `{"a":01}`, `{"a":1.}`, `{"a":.5}`,
		`{'a':1}`, `{"a":NaN}`, `{"a":Infinity}`, `{"a":+1}`, "{\"a\":\"\x01\"}", `{"a":1,}`, `{"a" : }`,
		testutil.Esc(`{"a":"~x"}`), testutil.Esc(`{"a":"~u00zz"}`), testutil.Esc(`{"~ud800":"~udc0"}`),
		sjNest(10001), sjObjectMember + sjNest(10000) + `}`,
	}
}

// sjFixtures returns the synthetic fixture bodies.
func sjFixtures(tb testing.TB) [][]byte {
	tb.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "synthetic", "*.json"))
	require.NoError(tb, err)
	require.NotEmpty(tb, names)
	out := make([][]byte, 0, len(names))
	for _, name := range names {
		out = append(out, testutil.Fixture(tb, "synthetic/"+filepath.Base(name)))
	}
	return out
}

// sjSweep runs every scanner over data, the index-based ones at every index,
// so that a panic on any input fails the caller. Sweeping every index is
// quadratic, so only short inputs get it.
func sjSweep(data []byte) {
	_, _ = checkStrict(data)
	_, _ = countValues(data)
	_ = duplicateKey(data)
	for k, v := range members(data) {
		_ = keyBytes(k)
		_ = decodeKey(v)
	}
	for _, el := range elements(data) {
		_ = appendUnquoted(nil, el)
	}
	_ = readObject(data, statusNames())
	_ = replaceLoneSurrogates(data)
	_ = keyBytes(data)
	_ = appendUnquoted(nil, data)
	_ = isNull(data)
	if len(data) > 4<<10 {
		return
	}
	for i := 0; i <= len(data)+1; i++ {
		_ = valueEnd(data, i)
		_ = skipSpace(data, i)
		_ = isKey(data, i)
		_ = hexEscape(data, i)
		if i < len(data) {
			_ = stringEnd(data, i)
		}
	}
}

// sjOracleCount counts containers and values (keys excluded) with
// encoding/json's tokenizer, as an oracle for countValues. data must be one
// valid JSON value.
func sjOracleCount(data []byte) (containers, values int) {
	type frame struct{ object, wantKey bool }
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return containers, values
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			if _, ok := tok.(string); ok {
				stack[n-1].wantKey = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			containers++
			values++
			stack = append(stack, frame{object: tok == json.Delim('{'), wantKey: true})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		default:
			values++
		}
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
}

// sjValidDepth is the deepest nesting json.Valid accepts. The token-walk
// oracle does not enforce it on every toolchain (go1.26 accepts deeper
// documents), so deeper inputs are checked apart from the oracle.
const sjValidDepth = 10000

// sjMaxDepth returns the deepest nesting of objects and arrays in data,
// skipping strings.
func sjMaxDepth(data []byte) int {
	depth, deepest := 0, 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '{', '[':
			depth++
			deepest = max(deepest, depth)
		case '}', ']':
			depth--
		case '"':
			i = stringEnd(data, i) - 1
		}
	}
	return deepest
}

// sjSyntaxCause reports whether cause is one of the causes checkStrict gives
// for input that is not one valid JSON value in valid UTF-8.
func sjSyntaxCause(cause error) bool {
	return errors.Is(cause, errInvalidUTF8) || errors.Is(cause, errInvalidJSON) || errors.Is(cause, errTrailingData)
}

func TestStrictCheckStrict(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		kind  FailureKind
		cause error
	}{
		{name: "valid object", data: `{"a":1,"b":[{"c":true}],"d":null}`},
		{name: "empty object", data: `{}`},
		{name: "surrounding whitespace", data: " \n\t{\"a\":1} \r\n"},
		{name: "unknown members kept apart by case", data: `{"status":"success","Status":"x","STATUS":"y"}`},
		{name: "lone surrogate escapes", data: testutil.Esc(`{"a":"~ud800","b":"~udfff","~udbff":1}`)},
		{name: "paired surrogate escape", data: testutil.Esc(`{"a":"~ud83d~ude00","~ud83d~ude00":1}`)},

		{name: "invalid UTF-8 in a string", data: "{\"a\":\"\xff\"}", kind: FailureMalformedResponse, cause: errInvalidUTF8},
		{name: "invalid UTF-8 in a key", data: "{\"\xc3\":1}", kind: FailureMalformedResponse, cause: errInvalidUTF8},
		{name: "truncated multibyte sequence", data: "{\"a\":\"\xe2\x82\"}", kind: FailureMalformedResponse, cause: errInvalidUTF8},
		{name: "invalid UTF-8 after the object", data: "{}\xff", kind: FailureMalformedResponse, cause: errInvalidUTF8},
		{name: "encoded surrogate half", data: "{\"a\":\"\xed\xa0\x80\"}", kind: FailureMalformedResponse, cause: errInvalidUTF8},
		{name: "UTF-8 byte order mark", data: "\xef\xbb\xbf{}", kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "empty body", data: "", kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "whitespace-only body", data: " \n\t\r ", kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "missing value", data: `{"a":}`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "unterminated object", data: `{"a":1`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "unterminated string", data: `{"a":"b`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "trailing comma", data: `{"a":[1,]}`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "leading zero", data: `{"a":01}`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "single quotes", data: `{'a':1}`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "raw control character", data: "{\"a\":\"\x01\"}", kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "invalid escape", data: testutil.Esc(`{"a":"~x"}`), kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "nesting beyond json.Valid's limit", data: sjObjectMember + sjNest(10000) + `}`, kind: FailureMalformedResponse, cause: errInvalidJSON},
		{name: "second object", data: `{} {}`, kind: FailureMalformedResponse, cause: errTrailingData},
		{name: "trailing garbage", data: `{}x`, kind: FailureMalformedResponse, cause: errTrailingData},
		{name: "trailing bracket", data: `{"a":1}]`, kind: FailureMalformedResponse, cause: errTrailingData},
		{name: "trailing NUL", data: "{}\x00", kind: FailureMalformedResponse, cause: errTrailingData},
		{name: "two numbers", data: `1 2`, kind: FailureMalformedResponse, cause: errTrailingData},
		{name: "top-level array", data: `[{"a":1}]`, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "top-level array with a duplicate", data: `[{"a":1,"a":2}]`, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "top-level string", data: `"x"`, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "top-level number", data: `42`, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "top-level null", data: jsonNull, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "top-level true", data: ` true `, kind: FailureUnexpectedResponse, cause: errNotObject},
		{name: "duplicate at top level", data: `{"a":1,"a":2}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "duplicate status", data: `{"status":"success","status":"success"}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "duplicate in a nested object", data: `{"a":{"b":1,"c":2,"b":3}}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "duplicate in an object inside an array", data: `{"a":[{"x":1},{"y":1,"y":2}]}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "duplicate five levels deep", data: `{"a":[{"b":{"c":[{"d":{"e":1,"e":2}}]}}]}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "duplicate in an ignored subtree", data: `{"status":"success","query":{"page":0,"page":1}}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "escaped status spelling first", data: testutil.Esc(`{"~u0073tatus":1,"status":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "escaped status spelling second", data: testutil.Esc(`{"status":1,"st~u0061tus":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "two escaped spellings", data: testutil.Esc(`{"~u0073tatus":1,"stat~u0075s":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "uppercase hex escape", data: testutil.Esc(`{"S":1,"~u0053":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "solidus escape", data: testutil.Esc(`{"/":1,"~/":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "newline escapes", data: testutil.Esc(`{"~n":1,"~u000a":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "quote escapes", data: testutil.Esc(`{"a~"b":1,"a~u0022b":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "lone surrogates both decode to U+FFFD", data: testutil.Esc(`{"~ud800":1,"~udfff":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "lone surrogate and a raw U+FFFD", data: testutil.Esc(`{"~udc00":1,"` + string(utf8.RuneError) + `":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "surrogate pair and the raw rune", data: testutil.Esc(`{"~ud83d~ude00":1,"` + sjEmoji + `":2}`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "pair and its lone high half differ", data: testutil.Esc(`{"~ud83d~ude00":1,"~ud83d":2}`)},
		{name: "same key in sibling objects", data: `{"a":{"k":1},"b":{"k":2},"c":[{"k":3},{"k":4}]}`},
		{name: "same key at different depths", data: `{"k":{"k":{"k":1}}}`},
		{name: "keys differing only by case", data: `{"a":1,"A":2,"status":null,"Status":null}`},
		{name: "a key equal to a string value", data: `{"a":"a","b":"a"}`},
		{name: "a string value holding a colon", data: `{"a":"b:","b":["a",":"]}`},
		{name: "16 distinct keys", data: sjManyKeys("k", 16, "")},
		{name: "16 keys with a duplicate", data: sjManyKeys("k", 15, `,"k0":0`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "17 distinct keys", data: sjManyKeys("k", 17, "")},
		{name: "17 keys with a duplicate", data: sjManyKeys("k", 16, `,"k15":0`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "40 keys with a duplicate", data: sjManyKeys("k", 40, `,"k5":0`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "17 keys with an escaped duplicate", data: sjManyKeys("k", 17, testutil.Esc(`,"~u006b16":0`)), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "30 escaped keys with a plain duplicate", data: sjManyKeys(testutil.Esc("~u006b"), 30, `,"k29":0`), kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "nested 20 keys with a duplicate", data: `{"a":[{"b":` + sjManyKeys("k", 20, `,"k19":1`) + `}]}`, kind: FailureMalformedResponse, cause: errDuplicateKey},
		{name: "two sibling 18-key objects", data: sjArrayMember + sjManyKeys("k", 18, "") + `,` + sjManyKeys("k", 18, "") + `]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, cause := checkStrict([]byte(tt.data))
			assert.Equal(t, tt.kind, kind)
			if tt.cause == nil {
				require.NoError(t, cause)
				return
			}
			require.ErrorIs(t, cause, tt.cause)
		})
	}
}

func TestStrictInvalidJSONCause(t *testing.T) {
	tests := []struct {
		data string
		want error
	}{
		{``, errInvalidJSON},
		{` `, errInvalidJSON},
		{`{`, errInvalidJSON},
		{`{"a":1,}`, errInvalidJSON},
		{`[1,]`, errInvalidJSON},
		{`{} {}`, errTrailingData},
		{`{}x`, errTrailingData},
		{`{"a":1} ,`, errTrailingData},
		{`[] {`, errTrailingData},
		{`1 2`, errTrailingData},
	}
	for _, tt := range tests {
		t.Run(tt.data, func(t *testing.T) {
			require.ErrorIs(t, invalidJSONCause([]byte(tt.data)), tt.want)
		})
	}
}

// TestStrictValueCaps proves the whole-body bounds at their exact edges, with
// bodies built in code: 100,000 containers and 1,000,000 values pass, and one
// more of either fails as response_too_large.
func TestStrictValueCaps(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		containers int
		values     int
		tooLarge   bool
	}{
		{
			name: "containers at the bound", body: sjFill(sjArrayMember, `{}`, maxContainers-2),
			containers: maxContainers, values: maxContainers,
		},
		{
			name: "one container over", body: sjFill(sjArrayMember, `[]`, maxContainers-1),
			containers: maxContainers + 1, values: maxContainers + 1, tooLarge: true,
		},
		{
			name: "values at the bound", body: sjFill(sjArrayMember, `0`, maxValues-2),
			containers: 2, values: maxValues,
		},
		{
			name: "one value over", body: sjFill(sjArrayMember, `0`, maxValues-1),
			containers: 2, values: maxValues + 1, tooLarge: true,
		},
		{
			name: "string values at the bound", body: sjFill(sjArrayMember, `""`, maxValues-2),
			containers: 2, values: maxValues,
		},
		{
			name: "keys are not values, containers still are", body: sjFill(sjArrayMember, `{"k":0}`, (maxValues-2)/2),
			containers: 2 + (maxValues-2)/2, values: maxValues, tooLarge: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers, values := countValues(tt.body)
			assert.Equal(t, tt.containers, containers)
			assert.Equal(t, tt.values, values)
			kind, cause := checkStrict(tt.body)
			if !tt.tooLarge {
				assert.Empty(t, kind)
				require.NoError(t, cause)
				return
			}
			assert.Equal(t, FailureResponseTooLarge, kind)
			require.ErrorIs(t, cause, errTooManyValues)
		})
	}
}

func TestStrictCountValues(t *testing.T) {
	tests := []struct {
		data               string
		containers, values int
	}{
		{`{}`, 1, 1},
		{`[]`, 1, 1},
		{`{"a":1}`, 1, 2},
		{`{"a":"b"}`, 1, 2},
		{` { "a" : [ 1 , "x" , true , null , { } , [ ] ] } `, 4, 8},
		{`[{"a":1,"b":2},"c",{"d":"e"}]`, 3, 7},
		{testutil.Esc(`{"a~"]":"~"}{","b":-1.5e+3}`), 1, 3},
		{`{"a":{"b":{"c":[]}},"d":false}`, 4, 5},
		{`{"a":[{"b":"c"},"d"],"e":"f"}`, 3, 6},
		{`[[["a","b"]],{"c":{"d":["e"]}}]`, 6, 9},
		{sjManyKeys("k", 20, ""), 1, 21},
		{`"a"`, 0, 1},
		{`-0.5e-7`, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.data, func(t *testing.T) {
			containers, values := countValues([]byte(tt.data))
			assert.Equal(t, tt.containers, containers, "containers")
			assert.Equal(t, tt.values, values, "values")
		})
	}
}

// TestStrictScannersMatchOracle compares the hand-written scanners with the
// encoding/json token walk on every corpus document and fixture: validity,
// duplicate keys and the value count must agree.
func TestStrictScannersMatchOracle(t *testing.T) {
	docs := sjFixtures(t)
	for _, s := range append(sjValidCorpus(), sjInvalidCorpus()...) {
		docs = append(docs, []byte(s))
	}
	for _, data := range docs {
		kind, cause := checkStrict(data)
		if sjMaxDepth(data) > sjValidDepth {
			assert.Equal(t, FailureMalformedResponse, kind)
			assert.True(t, sjSyntaxCause(cause), "nesting past json.Valid's limit is invalid")
			continue
		}
		valid, oracleDup := testutil.TokenWalk(data)
		assert.Equal(t, !valid, sjSyntaxCause(cause), "validity of %q", data)
		if !valid {
			assert.Equal(t, FailureMalformedResponse, kind, "%q", data)
			continue
		}
		assert.Equal(t, oracleDup, duplicateKey(data), "duplicateKey of %q", data)
		wantContainers, wantValues := sjOracleCount(data)
		containers, values := countValues(data)
		assert.Equal(t, wantContainers, containers, "containers of %q", data)
		assert.Equal(t, wantValues, values, "values of %q", data)
		if data[skipSpace(data, 0)] != '{' {
			require.ErrorIs(t, cause, errNotObject)
			continue
		}
		assert.Equal(t, oracleDup, errors.Is(cause, errDuplicateKey), "%q", data)
		if cause == nil {
			var decoded map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Len(t, sjMembers(data), len(decoded), "members sees every member of %q once", data)
		}
	}
}

func TestStrictScannersNeverPanic(t *testing.T) {
	inputs := append(sjInvalidCorpus(), sjValidCorpus()...)
	for _, s := range inputs {
		data := []byte(s)
		assert.NotPanics(t, func() { sjSweep(data) }, "%q", data)
	}
	assert.NotPanics(t, func() { sjSweep(nil) })
}

// sjMember is one member as members yields it, with the key decoded.
type sjMember struct{ key, value string }

// sjMembers collects what members yields.
func sjMembers(data []byte) []sjMember {
	var out []sjMember
	for k, v := range members(data) {
		out = append(out, sjMember{key: decodeKey(k), value: string(v)})
	}
	return out
}

// sjElements collects what elements yields, checking that each element comes
// with its position as its index.
func sjElements(tb testing.TB, data []byte) []string {
	tb.Helper()
	var out []string
	for i, el := range elements(data) {
		assert.Equal(tb, len(out), i, "the index of %q", el)
		out = append(out, string(el))
	}
	return out
}

func TestStrictMembers(t *testing.T) {
	data := []byte(" {\n \"a\" : 1 , \"b\":\"x,y\" ,\"c\":{\"d\":[1,2]}, \"e\":[ ], \"f\":null, \"g\":true, " +
		testutil.Esc(`"~u0068":-1.5e3, "i~"":"~"}"`) + " } ")
	want := []sjMember{
		{"a", `1`},
		{"b", `"x,y"`},
		{"c", `{"d":[1,2]}`},
		{"e", `[ ]`},
		{"f", jsonNull},
		{"g", sjTrue},
		{"h", `-1.5e3`},
		{`i"`, testutil.Esc(`"~"}"`)},
	}
	assert.Equal(t, want, sjMembers(data))
	assert.Empty(t, sjMembers([]byte(`{}`)))
	assert.Empty(t, sjMembers([]byte(` { } `)))
	assert.Nil(t, sjMembers([]byte(`[1]`)))
	assert.Nil(t, sjMembers(nil))

	// Malformed input yields what it can and stops.
	assert.Equal(t, []sjMember{{"a", "1"}}, sjMembers([]byte(`{"a":1,`)))
	assert.Equal(t, []sjMember{{"a", "1"}}, sjMembers([]byte(`{"a":1 "b":2}`)))
	assert.Equal(t, []sjMember{{"a", ""}}, sjMembers([]byte(`{"a":`)))
	assert.Nil(t, sjMembers([]byte(`{"a" 1}`)))
	assert.Nil(t, sjMembers([]byte(`{1:2}`)))
	assert.Nil(t, sjMembers([]byte(`{"a"`)))
	assert.Nil(t, sjMembers([]byte(` x`)))

	// Breaking out of the loop stops the walk.
	n := 0
	for range members(data) {
		n++
		break
	}
	assert.Equal(t, 1, n)
}

func TestStrictElements(t *testing.T) {
	data := []byte(` [ 1 , "a]" , {"b":[2]} , [3,[4]] , null , true , -0.5 ] `)
	assert.Equal(t, []string{`1`, `"a]"`, `{"b":[2]}`, `[3,[4]]`, jsonNull, sjTrue, `-0.5`}, sjElements(t, data))
	assert.Empty(t, sjElements(t, []byte(`[]`)))
	assert.Empty(t, sjElements(t, []byte(`[ ]`)))
	assert.Nil(t, sjElements(t, []byte(`{}`)))
	assert.Nil(t, sjElements(t, nil))

	// Malformed input yields what it can and stops.
	assert.Equal(t, []string{"1"}, sjElements(t, []byte(`[1,`)))
	assert.Equal(t, []string{"1"}, sjElements(t, []byte(`[1 2]`)))
	assert.Equal(t, []string{""}, sjElements(t, []byte(`[,]`)))
	assert.Nil(t, sjElements(t, []byte(`[`)))

	// Breaking out of the loop stops the walk.
	n := 0
	for range elements(data) {
		n++
		break
	}
	assert.Equal(t, 1, n)
}

func TestStrictValueEnd(t *testing.T) {
	tests := []struct {
		data string
		end  int
	}{
		{`"xyz",`, 5},
		{testutil.Esc(`"a~"b",`), 6},
		{testutil.Esc(`"a~~",`), 5},
		{`{"a":"}"},`, 9},
		{`[1,[2,"]"],3] `, 13},
		{`{"a":{"b":[]}}]`, 14},
		{`123,`, 3},
		{`-1.5e+3}`, 7},
		{`true]`, 4},
		{`null `, 4},
		{`false:`, 5},
		{"1\n", 1},
		{"1\t", 1},
		{"1\r", 1},
		{`{"a":`, 5},
		{`"abc`, 4},
		{`[[`, 2},
		{``, 0},
	}
	for _, tt := range tests {
		t.Run(tt.data, func(t *testing.T) {
			assert.Equal(t, tt.end, valueEnd([]byte(tt.data), 0))
		})
	}
	assert.Equal(t, 3, valueEnd([]byte("xyz"), 5), "an index past the end returns the length")
	assert.Equal(t, 6, valueEnd([]byte(`{"a":1}`), 5), "a value inside a document")
}

func TestStrictStringEnd(t *testing.T) {
	tests := []struct {
		data string
		end  int
	}{
		{`""`, 2},
		{`"a"x`, 3},
		{testutil.Esc(`"~""`), 4},
		{testutil.Esc(`"~~"`), 4},
		{testutil.Esc(`"~~~""`), 6},
		{testutil.Esc(`"~u0022"`), 8},
		{`"é"`, 4},
		{`"abc`, 4},
		{testutil.Esc(`"~`), 2},
		{`"`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.data, func(t *testing.T) {
			assert.Equal(t, tt.end, stringEnd([]byte(tt.data), 0))
		})
	}
	assert.Equal(t, 9, stringEnd([]byte(`{"a":"bc"}`), 5), "a string inside a document")
}

func TestStrictSkipSpace(t *testing.T) {
	assert.Equal(t, 4, skipSpace([]byte(" \t\n\rx"), 0))
	assert.Equal(t, 0, skipSpace([]byte("x "), 0))
	assert.Equal(t, 2, skipSpace([]byte("  "), 0))
	assert.Equal(t, 3, skipSpace([]byte("x  "), 1))
	assert.Equal(t, 1, skipSpace([]byte("x "), 1), "only JSON whitespace is skipped")
	assert.Equal(t, 0, skipSpace([]byte("\vx"), 0), "only JSON whitespace is skipped")
	assert.Equal(t, 0, skipSpace([]byte("\fx"), 0), "only JSON whitespace is skipped")
	assert.Equal(t, 5, skipSpace([]byte("xyz"), 5))
	assert.Equal(t, 0, skipSpace(nil, 0))
}

func TestStrictIsNullAndIsKey(t *testing.T) {
	assert.True(t, isNull(nil))
	assert.True(t, isNull([]byte{}))
	assert.True(t, isNull([]byte(jsonNull)))
	assert.False(t, isNull([]byte("nul")))
	assert.False(t, isNull([]byte("null ")))
	assert.False(t, isNull([]byte(`"null"`)))
	assert.False(t, isNull([]byte("0")))

	data := []byte(`{"a" : "b", "c":"d", "e"` + "\n\t:" + `["f"]}`)
	assert.True(t, isKey(data, 4), `"a" is a key`)
	assert.False(t, isKey(data, 10), `"b" is a value`)
	assert.True(t, isKey(data, 15), `"c" is a key`)
	assert.False(t, isKey(data, 19), `"d" is a value`)
	assert.True(t, isKey(data, 24), `"e" is a key across whitespace`)
	assert.False(t, isKey(data, 31), `"f" is an element`)
	assert.False(t, isKey(data, len(data)))
	assert.False(t, isKey(data, len(data)+3))
}

func TestStrictKeyBytes(t *testing.T) {
	lit := []byte(`"status"`)
	got := keyBytes(lit)
	assert.Equal(t, "status", string(got))
	assert.Same(t, &lit[1], &got[0], "a key without escapes is returned in place")
	assert.Len(t, got, cap(got), "the in-place key cannot be appended into the body")

	escaped := []byte(testutil.Esc(`"~u0073tatus"`))
	dec := keyBytes(escaped)
	assert.Equal(t, "status", string(dec))
	assert.NotSame(t, &escaped[1], &dec[0], "an escaped key is decoded into a copy")
	assert.Equal(t, testutil.Esc(`"~u0073tatus"`), string(escaped), "decoding leaves the literal unchanged")

	assert.Empty(t, keyBytes([]byte(`""`)))
	assert.Empty(t, keyBytes([]byte(`"`)))
	assert.Empty(t, keyBytes(nil))
	assert.Equal(t, "a\"b", string(keyBytes([]byte(testutil.Esc(`"a~"b"`)))))
	assert.Equal(t, "é", string(keyBytes([]byte(`"é"`))))

	assert.True(t, unescaped([]byte(`""`)))
	assert.True(t, unescaped([]byte(`"a/b"`)))
	assert.False(t, unescaped([]byte(testutil.Esc(`"a~/b"`))))
	assert.False(t, unescaped([]byte(`"`)))
	assert.False(t, unescaped(nil))
}

func TestStrictScannerAllocations(t *testing.T) {
	testutil.SkipUnderRace(t)
	plain := []byte(`"status"`)
	escaped := []byte(testutil.Esc(`"~u0073tatus"`))
	doc := []byte(`{"status":"success","result":"valid","flags":["has_dns","free_email_host"],"execution_time":12}`)
	arr := []byte(`["has_dns","free_email_host",1,{"a":2}]`)
	names := checkNames()
	var sink int
	testutil.CheckCeilings(t, []testutil.Ceiling{
		{Name: "keyBytes without escapes", Max: 0, Run: func() { sink += len(keyBytes(plain)) }},
		{Name: "keyBytes with escapes", Max: 1, Run: func() { sink += len(keyBytes(escaped)) }},
		{Name: "replaceLoneSurrogates without any", Max: 0, Run: func() { sink += len(replaceLoneSurrogates(doc)) }},
		{Name: "members", Max: 0, Run: func() {
			for k, v := range members(doc) {
				sink += len(k) + len(v)
			}
		}},
		{Name: "elements", Max: 0, Run: func() {
			for _, el := range elements(arr) {
				sink += len(el)
			}
		}},
		{Name: "readObject", Max: 0, Run: func() { sink += len(readObject(doc, names).fields[envStatus]) }},
	})
	assert.Positive(t, sink)
}

func TestStrictAppendUnquoted(t *testing.T) {
	const fffd = string(utf8.RuneError)
	tests := []struct {
		name string
		lit  string
		want string
	}{
		{name: "plain", lit: `"status"`, want: "status"},
		{name: "empty literal", lit: `""`, want: ""},
		{name: "every two-byte escape", lit: testutil.Esc(`"~"~~~/~b~f~n~r~t"`), want: "\"\\/\b\f\n\r\t"},
		{name: "lowercase hex", lit: testutil.Esc(`"~u00e9"`), want: "é"},
		{name: "uppercase hex", lit: testutil.Esc(`"~u00E9"`), want: "é"},
		{name: "NUL escape", lit: testutil.Esc(`"a~u0000b"`), want: "a\x00b"},
		{name: "three-byte rune", lit: testutil.Esc(`"~u20ac"`), want: "€"},
		{name: "line separator", lit: testutil.Esc(`"~u2028"`), want: string(rune(0x2028))},
		{name: "surrogate pair", lit: testutil.Esc(`"~ud83d~ude00"`), want: sjEmoji},
		{name: "surrogate pair in mixed case", lit: testutil.Esc(`"~uD83d~uDe00"`), want: sjEmoji},
		{name: "lowest pair", lit: testutil.Esc(`"~ud800~udc00"`), want: string(rune(0x10000))},
		{name: "highest pair", lit: testutil.Esc(`"~udbff~udfff"`), want: string(rune(0x10ffff))},
		{name: "lone high", lit: testutil.Esc(`"~ud800"`), want: fffd},
		{name: "lone low", lit: testutil.Esc(`"~udc00"`), want: fffd},
		{name: "lone high then text", lit: testutil.Esc(`"~udbffx"`), want: fffd + "x"},
		{name: "high then a non-surrogate escape", lit: testutil.Esc(`"~ud800~u0041"`), want: fffd + "A"},
		{name: "high then a two-byte escape", lit: testutil.Esc(`"~ud800~n"`), want: fffd + "\n"},
		{name: "high then a valid pair", lit: testutil.Esc(`"~ud800~ud800~udc00"`), want: fffd + string(rune(0x10000))},
		{name: "reversed pair", lit: testutil.Esc(`"~ude00~ud83d"`), want: fffd + fffd},
		{name: "two lone lows", lit: testutil.Esc(`"~udc00~udfff"`), want: fffd + fffd},
		{name: "raw multibyte kept", lit: "\"é€" + sjEmoji + "\"", want: "é€" + sjEmoji},
		{name: "raw U+FFFD kept", lit: `"` + fffd + `"`, want: fffd},
		{name: "escaped backslash before u", lit: testutil.Esc(`"~~u0041"`), want: testutil.Esc(`~u0041`)},

		// Malformed escapes, which a valid document never has, stop decoding.
		{name: "unknown escape stops", lit: testutil.Esc(`"ab~xcd"`), want: "ab"},
		{name: "truncated unicode escape stops", lit: testutil.Esc(`"ab~u12"`), want: "ab"},
		{name: "bad hex digit stops", lit: testutil.Esc(`"ab~u12g4cd"`), want: "ab"},
		{name: "backslash at the end stops", lit: testutil.Esc(`"ab~"`), want: "ab"},
		{name: "high then a truncated escape", lit: testutil.Esc(`"~ud800~u12"`), want: fffd},
		{name: "too short", lit: `"`, want: ""},
		{name: "nothing", lit: ``, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lit := []byte(tt.lit)
			assert.Equal(t, tt.want, string(appendUnquoted(nil, lit)))
			assert.Equal(t, "pre:"+tt.want, string(appendUnquoted([]byte("pre:"), lit)), "appends after dst")
			assert.Equal(t, tt.want, decodeKey(lit))
			if json.Valid(lit) {
				var want string
				require.NoError(t, json.Unmarshal(lit, &want))
				assert.Equal(t, want, decodeKey(lit), "decodes as encoding/json does")
			}
		})
	}
}

func TestStrictHexEscape(t *testing.T) {
	tests := []struct {
		data string
		i    int
		want rune
	}{
		{testutil.Esc(`~u0041`), 0, 0x41},
		{testutil.Esc(`~uabcd`), 0, 0xabcd},
		{testutil.Esc(`~uABCD`), 0, 0xabcd},
		{testutil.Esc(`~uFfFf`), 0, 0xffff},
		{testutil.Esc(`~u0000`), 0, 0},
		{testutil.Esc(`~u9a9F`), 0, 0x9a9f},
		{testutil.Esc(`x~ud83dy`), 1, 0xd83d},
		{testutil.Esc(`~u004`), 0, -1},
		{testutil.Esc(`~x0041`), 0, -1},
		{`u0041x`, 0, -1},
		{testutil.Esc(`~U0041`), 0, -1},
		{testutil.Esc(`~u004g`), 0, -1},
		{testutil.Esc(`~ug041`), 0, -1},
		{testutil.Esc(`~u0/41`), 0, -1},
		{testutil.Esc(`~u0:41`), 0, -1},
		{testutil.Esc(`~u0@41`), 0, -1},
		{testutil.Esc(`~u0G41`), 0, -1},
		{testutil.Esc("~u0`41"), 0, -1},
		{testutil.Esc(`~u0 41`), 0, -1},
		{testutil.Esc(`~u0041`), 1, -1},
		{testutil.Esc(`~u0041`), 6, -1},
		{testutil.Esc(`~u0041`), 9, -1},
		{``, 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.data+"@"+strconv.Itoa(tt.i), func(t *testing.T) {
			assert.Equal(t, tt.want, hexEscape([]byte(tt.data), tt.i))
		})
	}
}

// TestStrictReplaceLoneSurrogates proves that no unpaired UTF-16 surrogate
// escape survives, which encoding/json/v2 refuses to marshal, while valid
// pairs and every other escape are kept as they are.
func TestStrictReplaceLoneSurrogates(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "no escapes", in: `{"k":"v"}`, want: `{"k":"v"}`},
		{name: "no strings", in: `[1,2,{}]`, want: `[1,2,{}]`},
		{name: "lone high", in: testutil.Esc(`{"k":"~ud800"}`), want: testutil.Esc(`{"k":"~ufffd"}`)},
		{name: "lone low", in: testutil.Esc(`["~udc00"]`), want: testutil.Esc(`["~ufffd"]`)},
		{name: "high then non-low", in: testutil.Esc(`"~ud800~u0041"`), want: testutil.Esc(`"~ufffd~u0041"`)},
		{name: "high then text", in: testutil.Esc(`"~udbffz"`), want: testutil.Esc(`"~ufffdz"`)},
		{name: "valid pair", in: testutil.Esc(`"~ud83d~ude00"`), want: testutil.Esc(`"~ud83d~ude00"`)},
		{name: "valid pair in uppercase", in: testutil.Esc(`"~uD83D~uDE00"`), want: testutil.Esc(`"~uD83D~uDE00"`)},
		{name: "reversed pair", in: testutil.Esc(`"~ude00~ud83d"`), want: testutil.Esc(`"~ufffd~ufffd"`)},
		{name: "high then valid pair", in: testutil.Esc(`"~ud800~ud800~udc00"`), want: testutil.Esc(`"~ufffd~ud800~udc00"`)},
		{
			name: "mixed", in: testutil.Esc(`{"a":"x~udc00y~ud83d~ude00z~ud800","b":["~u00e9","~udfff"]}`),
			want: testutil.Esc(`{"a":"x~ufffdy~ud83d~ude00z~ufffd","b":["~u00e9","~ufffd"]}`),
		},
		{name: "in a key", in: testutil.Esc(`{"~udbff":1}`), want: testutil.Esc(`{"~ufffd":1}`)},
		{name: "escaped backslash before u", in: testutil.Esc(`"~~ud800"`), want: testutil.Esc(`"~~ud800"`)},
		{name: "escaped quote before an escape", in: testutil.Esc(`"~"~ud800"`), want: testutil.Esc(`"~"~ufffd"`)},
		{name: "other escapes", in: testutil.Esc(`"~n~"~/~u00e9"`), want: testutil.Esc(`"~n~"~/~u00e9"`)},
		{name: "uppercase hex", in: testutil.Esc(`"~uD800"`), want: testutil.Esc(`"~ufffd"`)},
		{name: "surrogate-like text outside escapes", in: `"ud800"`, want: `"ud800"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceLoneSurrogates([]byte(tt.in))
			assert.Equal(t, []byte(tt.want), got)
			require.True(t, json.Valid(got))
			var before, after any
			require.NoError(t, json.Unmarshal([]byte(tt.in), &before))
			require.NoError(t, json.Unmarshal(got, &after))
			assert.Equal(t, before, after, "encoding/json decodes both the same")
		})
	}
	unchanged := []byte(testutil.Esc(`{"k":"v~n~ud83d~ude00"}`))
	assert.Same(t, &unchanged[0], &replaceLoneSurrogates(unchanged)[0], "no copy when nothing changes")
	input := []byte(testutil.Esc(`["~ud800"]`))
	_ = replaceLoneSurrogates(input)
	assert.Equal(t, testutil.Esc(`["~ud800"]`), string(input), "the input is never modified")
}

func TestStrictDecodeKey(t *testing.T) {
	tests := []struct {
		name string
		lit  string
		want string
	}{
		{name: "plain", lit: `"status"`, want: statusMember},
		{name: "empty key", lit: `""`, want: ""},
		{name: "unicode escape", lit: testutil.Esc(`"~u0073tatus"`), want: statusMember},
		{name: "control escapes", lit: testutil.Esc(`"a~nb~tc~/d~~e~"f"`), want: "a\nb\tc/d\\e\"f"},
		{name: "surrogate pair", lit: testutil.Esc(`"~ud83d~ude00"`), want: sjEmoji},
		{name: "lone surrogate", lit: testutil.Esc(`"~ud800"`), want: string(utf8.RuneError)},
		{name: "raw multibyte", lit: `"é"`, want: "é"},
		{name: "too short", lit: `"`, want: ""},
		{name: "nothing", lit: ``, want: ""},
		{name: "invalid escape decodes to the prefix", lit: testutil.Esc(`"a~x"`), want: "a"},
		{name: "invalid hex digit decodes to the prefix", lit: testutil.Esc(`"a~u12zz"`), want: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, decodeKey([]byte(tt.lit)))
		})
	}
}
