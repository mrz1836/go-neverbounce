package neverbounce

import (
	"bytes"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// sjAmplification is the most bytes a call may allocate in total per byte of
// a hostile body, reading it included. The worst shapes, bodies made only of
// keys, cost about 6 to 8 times their size, mostly in the growth of the key
// spans the duplicate check keeps.
const sjAmplification = 10

// sjCheckHead is a successful single/check answer without its closing brace.
const sjCheckHead = `{"status":"success","result":"valid"`

// sjBody pads sjCheckHead to just under size bytes with ,"pad":[el,el,...]}
// and returns it.
func sjBody(el string, size int) []byte {
	const open, end = `,"pad":[`, `]}`
	n := (size - len(sjCheckHead) - len(open) - len(end) + 1) / (len(el) + 1)
	return sjFill(sjCheckHead+open, el, n)
}

// sjSpaces pads head, a JSON object without its closing brace, with
// whitespace to the 1 MiB single-check limit, closing brace included.
func sjSpaces(head string) []byte {
	return []byte(head + strings.Repeat(" ", smallResponseCap-len(head)-1) + "}")
}

// sjKeys returns sjCheckHead followed by distinct members "<prefix><i>":0 up
// to just under size bytes, and the closing brace. prefix is spelled as JSON,
// so it may hold escapes.
func sjKeys(prefix string, size int) []byte {
	var b bytes.Buffer
	b.Grow(size)
	b.WriteString(sjCheckHead)
	for i := 0; ; i++ {
		member := `,"` + prefix + strconv.Itoa(i) + `":0`
		if b.Len()+len(member)+1 > size {
			break
		}
		b.WriteString(member)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// TestBoundsHostileBodiesStayCheap guards against memory amplification: a
// body at the 1 MiB single-check limit built to be expensive to walk (only
// keys, nesting to json.Valid's depth limit, many tiny values, many escaped
// keys) costs a small multiple of its size for the whole call, reading
// included, and never hundreds of MiB.
func TestBoundsHostileBodiesStayCheap(t *testing.T) {
	testutil.SkipUnderRace(t)
	const size = smallResponseCap
	const head = sjCheckHead
	deep := sjNest(9998) // inside the object and one array: depth 10,000
	// Every key distinct but the last, which repeats the first.
	dup := sjKeys("k", size-8)
	dup = append(dup[:len(dup)-1], `,"k0":1}`...)
	tests := []struct {
		name string
		body []byte
		kind FailureKind // "" for a successful decode
	}{
		{name: "only keys", body: sjKeys("k", size)},
		{name: "only long keys", body: sjKeys(strings.Repeat("k", 200), size)},
		{name: "many escaped keys", body: sjKeys(testutil.Esc(`~u006b`), size)},
		{name: "keys escaped on every byte", body: sjKeys(strings.Repeat(testutil.Esc(`~u006b`), 30), size)},
		{name: "keys in nested objects", body: sjBody(sjManyKeys("k", 40, ""), size)},
		{name: "nesting to json.Valid's limit, repeated", body: sjBody(deep, size), kind: FailureResponseTooLarge},
		{name: "nesting to json.Valid's limit, padded", body: sjSpaces(head + `,"deep":[` + deep + `]`)},
		{name: "nesting past json.Valid's limit", body: sjSpaces(head + `,"deep":[[` + deep + `]]`), kind: FailureMalformedResponse},
		{name: "many tiny values", body: sjBody("0", size)},
		{name: "many empty strings", body: sjBody(`""`, size)},
		{name: "many empty objects", body: sjBody(`{}`, size), kind: FailureResponseTooLarge},
		{name: "many lone surrogates", body: sjBody(testutil.Esc(`"~ud800"`), size)},
		{name: "duplicate at the very end", body: dup, kind: FailureMalformedResponse},
		{name: "trailing data after the object", body: append(sjBody("0", size-2), " x"...), kind: FailureMalformedResponse},
		{name: "unterminated object", body: sjBody("0", size+1)[:size], kind: FailureMalformedResponse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.LessOrEqual(t, len(tt.body), size)
			require.Greater(t, len(tt.body), size-(64<<10), "the body fills the limit")
			c := sjClient(t, http.StatusOK, tt.body)
			req := CheckEmailRequest{Email: sjEmail}
			res, err := c.CheckEmail(t.Context(), req)
			if tt.kind == "" {
				require.NoError(t, err)
				assert.Equal(t, ResultValid, res.Result)
			} else {
				assert.Equal(t, tt.kind, asCallError(t, err).Kind)
			}
			grew := testutil.HeapGrowth(func() { _, _ = c.CheckEmail(t.Context(), req) })
			t.Logf("%d bytes allocated for a %d-byte body (%.2fx)", grew, len(tt.body), float64(grew)/float64(len(tt.body)))
			assert.Less(t, grew, uint64(sjAmplification*len(tt.body)), "a call must cost a small multiple of the body")
		})
	}
}

// TestBoundsResultsPageStaysCheap does the same for a jobs/results page of
// 1,000 rows with data near 1 KiB each, which the decoder copies.
func TestBoundsResultsPageStaysCheap(t *testing.T) {
	testutil.SkipUnderRace(t)
	row := `{"data":{"id":"1001","email":"avery.example@example.com","note":"` + strings.Repeat("n", 900) + `"},` +
		`"verification":{"result":"valid","flags":["has_dns","has_dns_mx","smtp_connectable"],"suggested_correction":""}}`
	body := sjFill(`{"status":"success","total_results":1000,"total_pages":1,"results":[`, row, MaxItemsPerPage)
	c := sjClient(t, http.StatusOK, body)
	req := JobResultsRequest{JobID: 7}
	page, err := c.JobResults(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.Results, MaxItemsPerPage)
	grew := testutil.HeapGrowth(func() { _, _ = c.JobResults(t.Context(), req) })
	t.Logf("%d bytes allocated for a %d-byte body (%.2fx)", grew, len(body), float64(grew)/float64(len(body)))
	assert.Less(t, grew, uint64(sjAmplification*len(body)))
}

// sjStrictObject runs the strict check and the envelope on body as call does
// after reading it, and returns the members of names.
func sjStrictObject(tb testing.TB, body []byte, names []string) object {
	tb.Helper()
	kind, cause := checkStrict(body)
	require.NoError(tb, cause)
	require.Empty(tb, kind)
	obj := readObject(body, names)
	require.Nil(tb, newInternalClient(tb).envelopeFailure(OpJobStatus, obj.fields[envStatus], obj.fields[envMessage]))
	return obj
}

// TestBoundsDecodingNeverRetainsTheBody proves that a decoded result holds no
// reference into the response body: changing the body afterwards changes
// nothing, and once the body is unreachable the collector frees it while the
// result is still alive. Row data, text and tokens are all copies.
func TestBoundsDecodingNeverRetainsTheBody(t *testing.T) {
	pad := `,"zz_pad":"` + strings.Repeat("p", 64<<10) + `"}`
	padded := func(tb testing.TB, fixture string) []byte {
		tb.Helper()
		data := bytes.TrimSpace(testutil.Fixture(tb, "synthetic/"+fixture))
		return append(data[:len(data)-1:len(data)-1], pad...)
	}
	tests := []struct {
		name   string
		body   func(tb testing.TB) []byte
		decode func(tb testing.TB, body []byte) any
	}{
		{
			name: "check", body: func(tb testing.TB) []byte { return padded(tb, "check_full.json") },
			decode: func(tb testing.TB, body []byte) any {
				obj := sjStrictObject(tb, body, checkNames())
				res, derr := decodeCheckEmail(&obj)
				require.Nil(tb, derr)
				return res
			},
		},
		{
			name: "status", body: func(tb testing.TB) []byte { return padded(tb, "status_complete.json") },
			decode: func(tb testing.TB, body []byte) any {
				obj := sjStrictObject(tb, body, statusNames())
				st, derr := decodeJobStatus(&obj, 1001)
				require.Nil(tb, derr)
				return st
			},
		},
		{
			name: "results page", body: func(tb testing.TB) []byte { return padded(tb, "results_page.json") },
			decode: func(tb testing.TB, body []byte) any {
				obj := sjStrictObject(tb, body, envelopeNames("total_results", "total_pages", sjResultsField))
				page, derr := decodeResultsPage(&obj, 1, MaxItemsPerPage)
				require.Nil(tb, derr)
				require.NotEmpty(tb, page.Results[1].Data)
				return page
			},
		},
		{
			name: "queue ID", body: func(testing.TB) []byte { return []byte(`{"status":"success","queue_id":"Q-1"` + pad) },
			decode: func(tb testing.TB, body []byte) any {
				obj := sjStrictObject(tb, body, envelopeNames("queue_id"))
				return queueID(obj.fields[controlQueueID])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body(t)
			want := tt.decode(t, bytes.Clone(body))
			got := tt.decode(t, body)
			require.Equal(t, want, got)

			for i := range body {
				body[i] = '?'
			}
			assert.Equal(t, want, got, "changing the body changes nothing decoded")

			freed := weak.Make(&body[0])
			body = nil
			runtime.GC()
			runtime.GC()
			assert.Nil(t, freed.Value(), "the result keeps no reference into the body")
			assert.Nil(t, body)
			runtime.KeepAlive(got)
		})
	}
}
