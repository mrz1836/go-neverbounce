package neverbounce_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// bzCreated is a jobs/create answer.
const bzCreated = `{"status":"success","job_id":1001,"execution_time":12}`

// bzRow returns results row i, whose data carries pad bytes of note text.
func bzRow(i, pad int) string {
	id := strconv.Itoa(i)
	email := "user." + id + "@example.com"
	return `{"data":{"id":"` + id + `","email":"` + email + `","note":"` + strings.Repeat("n", pad) + `"},` +
		`"verification":{"result":"valid","flags":["has_dns","has_dns_mx","smtp_connectable"],` +
		`"suggested_correction":"","address_info":{"original_email":"` + email + `","normalized_email":"` + email +
		`","addr":"user.` + id + `","alias":"","host":"example.com","fqdn":"example.com","domain":"example",` +
		`"subdomain":"","tld":"com"}}}`
}

// bzPage returns a one-page jobs/results answer of rows rows, each with pad
// bytes of note text.
func bzPage(rows, pad int) string {
	var b strings.Builder
	n := strconv.Itoa(rows)
	b.WriteString(`{"status":"success","total_results":` + n + `,"total_pages":1,` +
		`"query":{"job_id":1001,"page":0,"items_per_page":` + n + `},"results":[`)
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(bzRow(i+1, pad))
	}
	b.WriteString(`],"execution_time":18}`)
	return b.String()
}

// bzEmailListNear returns an email list whose jobs/create body, with every
// boolean false and no other option, is at most limit bytes and within 1 KiB
// of it.
func bzEmailListNear(limit int) neverbounce.SuppliedList {
	const (
		head = len(keyMember + `,"input_location":"supplied","input":[`)
		tail = len(`],"auto_parse":false,"auto_start":false,"run_sample":false,"allow_manual_review":false}`)
		row  = len(`,{"email":""}`)
	)
	size := head + tail - 1 // the first row has no comma
	var emails []string
	for i := 1; ; i++ {
		e := "user." + strconv.Itoa(i) + "@example.com"
		if size+row+len(e) > limit {
			break
		}
		size += row + len(e)
		emails = append(emails, e)
	}
	return neverbounce.EmailList(emails...)
}

// TestMemoryAllocationCeilings guards the allocation counts of the common
// paths, in-memory transport included. The ceilings sit about 25% above the
// counts measured with Go 1.27.2 on darwin/arm64.
func TestMemoryAllocationCeilings(t *testing.T) {
	testutil.SkipUnderRace(t)
	check := newTransportClient(t, testutil.StaticTransport(http.StatusOK,
		string(testutil.Fixture(t, "synthetic/check_full.json"))))
	status := newTransportClient(t, testutil.StaticTransport(http.StatusOK,
		string(testutil.Fixture(t, "synthetic/status_complete.json"))))
	results := newTransportClient(t, testutil.StaticTransport(http.StatusOK, bzPage(100, 16)))
	ctx := t.Context()

	checkReq := neverbounce.CheckEmailRequest{Email: testEmail, AddressInfo: true, CreditsInfo: true}
	res, err := check.CheckEmail(ctx, checkReq)
	require.NoError(t, err)
	require.NotNil(t, res.AddressInfo)
	require.NotNil(t, res.CreditsInfo)
	statusReq := neverbounce.JobStatusRequest{JobID: 1001}
	st, err := status.JobStatus(ctx, statusReq)
	require.NoError(t, err)
	require.Equal(t, neverbounce.JobStateComplete, st.State)
	resultsReq := neverbounce.JobResultsRequest{JobID: 1001, Page: 1, ItemsPerPage: 100}
	page, err := results.JobResults(ctx, resultsReq)
	require.NoError(t, err)
	require.Len(t, page.Results, 100)

	testutil.CheckCeilings(t, []testutil.Ceiling{
		// Measured: 67.
		{Name: "CheckEmail end to end", Max: 85, Run: func() { _, _ = check.CheckEmail(ctx, checkReq) }},
		// Measured: 67.
		{Name: "JobStatus decode", Max: 84, Run: func() { _, _ = status.JobStatus(ctx, statusReq) }},
		// Measured: 1653, about 16 a row.
		{Name: "JobResults 100 rows", Max: 2100, Run: func() { _, _ = results.JobResults(ctx, resultsReq) }},
	})
}

// TestMemoryCreateJobNearLimit encodes and sends a jobs/create body just
// under MaxJobRequestBytes and checks that the call allocates at most 1.25
// times the body: the body is built once, at its estimated size, and never
// copied.
func TestMemoryCreateJobNearLimit(t *testing.T) {
	testutil.SkipUnderRace(t)
	list := bzEmailListNear(neverbounce.MaxJobRequestBytes)
	var sent int64
	created := 0
	c := newTransportClient(t, testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = testutil.Drain(r)
		created++
		return testutil.Response(r, http.StatusOK, io.NopCloser(strings.NewReader(bzCreated)), int64(len(bzCreated))), nil
	}))
	req := neverbounce.CreateJobRequest{Supplied: list}
	call := func() {
		res, err := c.CreateJob(t.Context(), req)
		if assert.NoError(t, err) {
			assert.Equal(t, neverbounce.JobID(1001), res.JobID)
		}
	}
	growth := testutil.HeapGrowth(call)
	require.Equal(t, 2, created)
	require.LessOrEqual(t, sent, int64(neverbounce.MaxJobRequestBytes))
	require.Greater(t, sent, int64(neverbounce.MaxJobRequestBytes-1024))
	t.Logf("%d rows, %d-byte body: %d bytes allocated (%.2fx the body)",
		len(list.Rows), sent, growth, float64(growth)/float64(sent))
	// Measured: 1.00x.
	assert.LessOrEqual(t, float64(growth), 1.25*float64(sent))
}

// TestMemoryLargeResultsPage reads and decodes a 1000-row page of about 4.5
// MB and checks that the call allocates a small multiple of the body.
func TestMemoryLargeResultsPage(t *testing.T) {
	testutil.SkipUnderRace(t)
	body := bzPage(neverbounce.MaxItemsPerPage, 4096)
	c := newTransportClient(t, testutil.StaticTransport(http.StatusOK, body))
	req := neverbounce.JobResultsRequest{JobID: 1001, Page: 1, ItemsPerPage: neverbounce.MaxItemsPerPage}
	call := func() {
		page, err := c.JobResults(t.Context(), req)
		if assert.NoError(t, err) {
			assert.Len(t, page.Results, neverbounce.MaxItemsPerPage)
		}
	}
	growth := testutil.HeapGrowth(call)
	t.Logf("%d-byte page: %d bytes allocated (%.2fx the body)", len(body), growth, float64(growth)/float64(len(body)))
	// Measured: 3.49x (the read buffers about 2x, the normalized row data
	// about 1x, the decoded strings the rest).
	assert.LessOrEqual(t, float64(growth), 4.4*float64(len(body)))
}
