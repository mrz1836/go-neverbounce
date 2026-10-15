package neverbounce

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
	"github.com/stretchr/testify/require"
)

// These benchmarks measure local CPU and allocations only: decoding, strict
// checking and encoding over in-memory bytes. They are not evidence of
// network or provider performance.

// bzStrictBodyBytes is the size of the strict-check body: the default
// response limit, the largest body a JSON call reads by default.
const bzStrictBodyBytes = DefaultMaxResponseBytes

// bzAppendRow appends results row i: a data object with an id, an address and
// pad bytes of note text, and a full verification with address_info.
func bzAppendRow(b []byte, i, pad int) []byte {
	id := strconv.Itoa(i)
	email := "user." + id + "@example.com"
	b = append(b, `{"data":{"id":"`...)
	b = append(b, id...)
	b = append(b, `","email":"`...)
	b = append(b, email...)
	b = append(b, `","note":"`...)
	b = append(b, bytes.Repeat([]byte{'n'}, pad)...)
	b = append(b, `"},"verification":{"result":"valid","flags":["has_dns","has_dns_mx","smtp_connectable"],`...)
	b = append(b, `"suggested_correction":"","address_info":{"original_email":"`...)
	b = append(b, email...)
	b = append(b, `","normalized_email":"`...)
	b = append(b, email...)
	b = append(b, `","addr":"user.`...)
	b = append(b, id...)
	b = append(b, `","alias":"","host":"example.com","fqdn":"example.com","domain":"example","subdomain":"","tld":"com"}}}`...)
	return b
}

// bzPageHead opens a jobs/results answer for total rows on one page.
func bzPageHead(total int) []byte {
	n := strconv.Itoa(total)
	return []byte(`{"status":"success","total_results":` + n + `,"total_pages":1,` +
		`"query":{"job_id":1001,"page":0,"items_per_page":` + n + `},"results":[`)
}

// bzPageTail closes a jobs/results answer.
const bzPageTail = `],"execution_time":18}`

// bzResultsPage returns a jobs/results answer holding rows rows.
func bzResultsPage(rows int) []byte {
	b := bzPageHead(rows)
	for i := range rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = bzAppendRow(b, i+1, 16)
	}
	return append(b, bzPageTail...)
}

// bzStrictBody returns a jobs/results-shaped answer of exactly size bytes:
// as many rows as fit, then spaces before the closing brace. Each row holds
// five containers and about 22 values, so the body stays within the
// container and value bounds.
func bzStrictBody(tb testing.TB, size int) []byte {
	tb.Helper()
	const pad = 256
	b := make([]byte, 0, size)
	b = append(b, bzPageHead(0)...)
	row := make([]byte, 0, 1024)
	for i := 1; ; i++ {
		row = bzAppendRow(row[:0], i, pad)
		if len(b)+1+len(row)+len(bzPageTail) > size {
			break
		}
		if i > 1 {
			b = append(b, ',')
		}
		b = append(b, row...)
	}
	b = append(b, bzPageTail[:len(bzPageTail)-1]...)
	b = append(b, bytes.Repeat([]byte{' '}, size-len(b)-1)...)
	b = append(b, '}')
	require.Len(tb, b, size, "strict body size")
	kind, cause := checkStrict(b)
	require.NoError(tb, cause, "strict body fails the check: %s", kind)
	return b
}

// bzDecode decodes body as an HTTP 200 answer to op the way call and the
// operation's decoder do: the strict check, the member read, the envelope
// status and then decode.
func bzDecode[T any](c *Client, op Operation, body []byte, names []string,
	decode func(*object) (T, *decodeError),
) (T, error) {
	var zero T
	if kind, cause := checkStrict(body); cause != nil {
		return zero, newCallError(op, kind, http.StatusOK, cause)
	}
	obj := readObject(body, names)
	if cerr := c.envelopeFailure(op, obj.fields[envStatus], obj.fields[envMessage]); cerr != nil {
		return zero, cerr
	}
	v, derr := decode(&obj)
	if derr != nil {
		return zero, derr.callError(op)
	}
	return v, nil
}

func BenchmarkDecodeCheckEmail(b *testing.B) {
	body := testutil.Fixture(b, "synthetic/check_full.json")
	c := newInternalClient(b)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		res, err := bzDecode(c, OpCheckEmail, body, checkNames(), decodeCheckEmail)
		if err != nil {
			b.Fatal(err)
		}
		if res.Result != ResultValid || res.AddressInfo == nil || res.CreditsInfo == nil {
			b.Fatal("the full fixture must decode every member")
		}
	}
}

func BenchmarkDecodeResultsPage1000(b *testing.B) {
	const rows = MaxItemsPerPage
	body := bzResultsPage(rows)
	c := newInternalClient(b)
	decode := func(o *object) (*JobResultsPage, *decodeError) { return decodeResultsPage(o, 1, rows) }
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		page, err := bzDecode(c, OpJobResults, body, envelopeNames("total_results", "total_pages", "results"), decode)
		if err != nil {
			b.Fatal(err)
		}
		if len(page.Results) != rows {
			b.Fatalf("%d rows, want %d", len(page.Results), rows)
		}
	}
}

func BenchmarkCheckStrict8MiB(b *testing.B) {
	body := bzStrictBody(b, bzStrictBodyBytes)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := checkStrict(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeCreateJob10k(b *testing.B) {
	const rows = 10_000
	list := SuppliedList{Columns: []string{"id", "email"}, Rows: make([][]string, rows)}
	for i := range rows {
		id := strconv.Itoa(1001 + i)
		list.Rows[i] = []string{id, "user." + id + "@example.com"}
	}
	req := CreateJobRequest{Supplied: list, Filename: "signups-example-10k", AutoParse: true}
	c := newInternalClient(b)
	body, err := req.encode(c)
	require.NoError(b, err)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := req.encode(c); err != nil {
			b.Fatal(err)
		}
	}
}
