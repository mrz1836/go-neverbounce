package neverbounce_test

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// TestBulkLifecycle walks the deliberate flow end to end, in memory: create
// (parse only), wait for "waiting", inspect billable, start explicitly, wait
// for "complete", walk the result pages, and download the CSV.
func TestBulkLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const csv = "id,email,email_status\r\n\"1001\",\"" + testEmail + "\",valid\r\n"
		status := func(state string, billable int) testutil.Step {
			return testutil.Step{Body: `{"status":"success","id":42,"job_status":"` + state +
				`","total":{"records":2,"billable":` + strconv.Itoa(billable) + `},"percent_complete":0}`}
		}
		row := `{"data":{"id":"1001","email":"` + testEmail + `"},"verification":{"result":"valid","flags":[]}}`
		c, s := newScriptClient(t,
			testutil.Step{Body: `{"status":"success","job_id":42,"execution_time":12}`},
			status("parsing", 0),
			status("waiting", 1),
			testutil.Step{Body: `{"status":"success","queue_id":"NB-PQ-EXAMPLE"}`},
			status("running", 1),
			status("complete", 1),
			testutil.Step{Body: `{"status":"success","total_results":2,"total_pages":2,"query":{"page":0},"results":[` + row + `]}`},
			testutil.Step{Body: `{"status":"success","total_results":2,"total_pages":2,"results":[` + row + `]}`},
			testutil.Step{Header: http.Header{"Content-Type": {mediaOctet}}, Body: csv},
		)
		ctx := t.Context()

		created, err := c.CreateJob(ctx, neverbounce.CreateJobRequest{
			Supplied: neverbounce.SuppliedList{
				Columns: []string{"id", "email"},
				Rows:    [][]string{{"1001", testEmail}, {"1002", "blake.example@example.com"}},
			},
			Filename:  "signups-example",
			AutoParse: true,
		})
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobID(42), created.JobID)

		start := time.Now()
		st, err := c.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: created.JobID, MaxWait: time.Minute})
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobStateWaiting, st.State)
		assert.Equal(t, 10*time.Second, time.Since(start), "one default interval between the two polls")
		require.NotNil(t, st.Totals.Billable)
		assert.Equal(t, int64(1), *st.Totals.Billable)

		started, err := c.StartJob(ctx, neverbounce.StartJobRequest{JobID: created.JobID})
		require.NoError(t, err)
		assert.Equal(t, "NB-PQ-EXAMPLE", started.QueueID)

		st, err = c.WaitForJob(ctx, neverbounce.WaitForJobRequest{
			JobID: created.JobID, Interval: 15 * time.Second, MaxWait: time.Hour,
		})
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobStateComplete, st.State)

		var pages []int
		for p, err := range c.JobResultPages(ctx, neverbounce.JobResultPagesRequest{
			JobID: created.JobID, ItemsPerPage: 1, MaxPages: 10,
		}) {
			require.NoError(t, err)
			pages = append(pages, p.Page)
			id, ok := p.Results[0].Column("id")
			assert.True(t, ok)
			assert.Equal(t, "1001", id)
		}
		assert.Equal(t, []int{1, 2}, pages)

		var file bytes.Buffer
		n, err := c.DownloadJob(ctx, neverbounce.DownloadJobRequest{
			JobID: created.JobID, Append: neverbounce.DownloadColumns{EmailStatus: new(true)},
			LineFeed: neverbounce.LineFeedCRLF,
		}, &file)
		require.NoError(t, err)
		assert.Equal(t, int64(len(csv)), n)
		assert.Equal(t, csv, file.String())

		assert.Equal(t, []string{
			pathCreate, pathStatus, pathStatus, pathStart, pathStatus, pathStatus, pathResults, pathResults, pathDownload,
		}, s.Paths())
		reqs := s.Requests()
		assertBody(t, keyMember+`,"input_location":"supplied","input":[{"id":"1001","email":"`+testEmail+
			`"},{"id":"1002","email":"blake.example@example.com"}],"filename":"signups-example",`+
			`"auto_parse":true,"auto_start":false,"run_sample":false,"allow_manual_review":false}`, reqs[0].Body)
		assertBody(t, keyMember+`,"job_id":42}`, reqs[1].Body)
		assertBody(t, keyMember+`,"job_id":42,"run_sample":false}`, reqs[3].Body)
		assertBody(t, keyMember+`,"job_id":42,"page":2,"items_per_page":1}`, reqs[7].Body)
		assertBody(t, keyMember+`,"job_id":42,"email_status":true,"line_feed_type":"LINEFEED_0D0A"}`, reqs[8].Body)
		assert.Equal(t, "*/*", reqs[8].Header.Get("Accept"))
	})
}
