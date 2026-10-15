// Command examples shows the go-neverbounce SDK end to end, offline: a local
// TLS server plays a synthetic NeverBounce, so the demo needs no API key,
// makes no network call and costs nothing.
//
//	go run ./examples
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrz1836/go-neverbounce"
)

// Synthetic answers. They follow the documented v4.2 shapes, but every value
// is made up.
const (
	checkAnswer = `{"status":"success","result":"valid","flags":["has_dns","has_dns_mx","smtp_connectable"],
		"suggested_correction":"","address_info":{"original_email":"avery.example@example.com",
		"normalized_email":"avery.example@example.com","addr":"avery.example","alias":"","host":"example.com",
		"fqdn":"example.com","domain":"example","subdomain":"","tld":"com"},
		"credits_info":{"paid_credits_used":1,"free_credits_used":0,"paid_credits_remaining":4999,
		"free_credits_remaining":0},"execution_time":215}`
	createAnswer = `{"status":"success","job_id":4242,"execution_time":31}`
	startAnswer  = `{"status":"success","queue_id":"NB-EXAMPLE-QUEUE","execution_time":12}`
	statusFormat = `{"status":"success","id":4242,"job_status":%q,"filename":"signups-2026-10-15-a7f3",
		"total":{"records":3,"billable":3,"processed":%d,"valid":%d,"invalid":%d},"percent_complete":%d}`
	resultsPage1 = `{"status":"success","total_results":4,"total_pages":2,"query":{"page":0},"results":[
		{"data":{"id":"id","email":"email"},"verification":{"result":"invalid"}},
		{"data":{"id":"1001","email":"avery.example@example.com"},
		 "verification":{"result":"valid","flags":["has_dns","smtp_connectable"]}}]}`
	resultsPage2 = `{"status":"success","total_results":4,"total_pages":2,"query":{"page":0},"results":[
		{"data":{"id":"1002","email":"blake.example@example.org"},
		 "verification":{"result":"invalid","flags":["spelling_mistake"],
		 "suggested_correction":"blake.example@example.com"}},
		{"data":{"id":1003,"email":"casey.example@example.net"},"verification":{"result":"valid"}}]}`
	downloadCSV = "id,email,email_status\r\n" +
		"1001,avery.example@example.com,valid\r\n" +
		"1002,blake.example@example.org,invalid\r\n" +
		"1003,casey.example@example.net,valid\r\n"
)

// creditBudget is the most credits the demo's list may use.
const creditBudget = 100

// errOverBudget stops a job before its paid start.
var errOverBudget = errors.New("the list is not ready to start within the credit budget")

// api is what the demo runs against: a handler that plays the provider and
// reports how many status reads the current job has had.
type api interface {
	http.Handler
	statusReads() int
}

func main() {
	if err := run(context.Background(), os.Stdout, &fakeNeverBounce{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "examples:", err)
		os.Exit(1)
	}
}

// run serves fake locally, walks through every operation against it, and
// writes what came back to w.
func run(ctx context.Context, w io.Writer, fake api) error {
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()

	// A real application passes its own key, from wherever it keeps secrets,
	// and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTimeout(45*time.Second),
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		return describe(w, err)
	}
	if err = checkOne(ctx, w, client); err != nil {
		return describe(w, err)
	}
	id, err := verifyList(ctx, w, client, fake)
	if err != nil {
		return describe(w, err)
	}
	if err = readResults(ctx, w, client, id); err != nil {
		return describe(w, err)
	}
	if err = download(ctx, w, client, id); err != nil {
		return describe(w, err)
	}
	return nil
}

// checkOne verifies one address.
func checkOne(ctx context.Context, w io.Writer, client *neverbounce.Client) error {
	_, _ = fmt.Fprintln(w, "Single check (1 credit on the real API)")
	res, err := client.CheckEmail(ctx, neverbounce.CheckEmailRequest{
		Email: "avery.example@example.com", AddressInfo: true, CreditsInfo: true,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "  avery.example@example.com: %s\n", res.Result)
	_, _ = fmt.Fprintf(w, "  flags: %s\n", joinFlags(res.Flags))
	if ai := res.AddressInfo; ai != nil {
		_, _ = fmt.Fprintf(w, "  host: %s, normalized: %s\n", ai.Host, ai.NormalizedEmail)
	}
	if ci := res.CreditsInfo; ci != nil {
		_, _ = fmt.Fprintf(w, "  paid credits: %s used, %s remaining\n",
			count(ci.PaidCreditsUsed), count(ci.PaidCreditsRemaining))
	}
	return nil
}

// verifyList runs the deliberate bulk flow: create with parsing only, wait
// for the parse, check the billable count against the budget, start
// explicitly, and wait for the verification. It returns the job's ID.
func verifyList(ctx context.Context, w io.Writer, client *neverbounce.Client, fake api) (neverbounce.JobID, error) {
	_, _ = fmt.Fprintln(w, "\nBulk job: create, wait, check the billable count, start, wait")
	created, err := client.CreateJob(ctx, neverbounce.CreateJobRequest{
		Supplied: neverbounce.SuppliedList{
			Columns: []string{"id", "email"},
			Rows: [][]string{
				{"1001", "avery.example@example.com"},
				{"1002", "blake.example@example.org"},
				{"1003", "casey.example@example.net"},
			},
		},
		Filename:  "signups-2026-10-15-a7f3", // unique: findable in the dashboard after an ambiguous create
		AutoParse: true,                      // AutoStart false is sent explicitly: nothing is billed yet
	})
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(w, "  created job %s with parsing only: nothing billed yet\n", created.JobID)

	// The demo polls every second; for a real list the provider suggests 5 to
	// 10 seconds, and longer for large lists.
	wait := neverbounce.WaitForJobRequest{JobID: created.JobID, Interval: time.Second, MaxWait: time.Minute}
	st, err := client.WaitForJob(ctx, wait)
	if err != nil {
		return 0, err
	}
	if st.State != neverbounce.JobStateWaiting || st.Totals.Billable == nil || *st.Totals.Billable > creditBudget {
		return 0, errOverBudget
	}
	_, _ = fmt.Fprintf(w, "  job %s is %s after %d status reads: %s records, %s billable (budget %d)\n",
		st.ID, st.State, fake.statusReads(), count(st.Totals.Records), count(st.Totals.Billable), creditBudget)

	started, err := client.StartJob(ctx, neverbounce.StartJobRequest{JobID: created.JobID})
	if err != nil {
		return 0, err // it may have started anyway: read JobStatus before ever starting again
	}
	_, _ = fmt.Fprintf(w, "  started job %s (queue %s): the real API deducts the billable credits now\n",
		created.JobID, started.QueueID)

	if st, err = client.WaitForJob(ctx, wait); err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(w, "  job %s is %s after %d status reads: %s valid, %s invalid\n",
		st.ID, st.State, fake.statusReads(), count(st.Totals.Valid), count(st.Totals.Invalid))
	return created.JobID, nil
}

// readResults walks the result pages and matches each row to the
// application's own record by the id column it submitted.
func readResults(ctx context.Context, w io.Writer, client *neverbounce.Client, id neverbounce.JobID) error {
	_, _ = fmt.Fprintln(w, "\nResults, matched by your own id column")
	signups := map[string]string{"1001": "Avery", "1002": "Blake", "1003": "Casey"}
	for page, err := range client.JobResultPages(ctx, neverbounce.JobResultPagesRequest{
		JobID: id, ItemsPerPage: 2, MaxPages: 10,
	}) {
		if err != nil {
			return err // pages already handled stay handled
		}
		_, _ = fmt.Fprintf(w, "  page %d of %d\n", page.Page, page.TotalPages)
		for _, row := range page.Results {
			rowID, _ := row.Column("id")
			name, ok := signups[rowID]
			if !ok {
				_, _ = fmt.Fprintf(w, "    skipped a row that is not one of yours (id %q)\n", rowID)
				continue
			}
			v := row.Verification
			_, _ = fmt.Fprintf(w, "    %s %s: %s", rowID, name, v.Result)
			if v.SuggestedCorrection != "" {
				_, _ = fmt.Fprintf(w, " (did you mean %s?)", v.SuggestedCorrection)
			}
			_, _ = fmt.Fprintln(w)
		}
	}
	return nil
}

// download streams the job's CSV into a buffer that stands in for a file the
// application owns.
func download(ctx context.Context, w io.Writer, client *neverbounce.Client, id neverbounce.JobID) error {
	_, _ = fmt.Fprintln(w, "\nDownload: the CSV, streamed unchanged")
	var file strings.Builder
	n, err := client.DownloadJob(ctx, neverbounce.DownloadJobRequest{
		JobID:    id,
		Append:   neverbounce.DownloadColumns{EmailStatus: new(true)},
		LineFeed: neverbounce.LineFeedCRLF, // the provider's default, "\n\r", trips encoding/csv
	}, &file)
	if err != nil {
		return err // discard the partial file: never append a second attempt to it
	}
	_, _ = fmt.Fprintf(w, "  %d bytes\n", n)
	for line := range strings.Lines(file.String()) {
		_, _ = fmt.Fprintf(w, "  %q\n", line)
	}
	return nil
}

// describe writes what a failed call means and returns err. A call that was
// sent may already have been processed and billed, so none of these branches
// resends it.
func describe(w io.Writer, err error) error {
	var what string
	switch {
	case errors.Is(err, neverbounce.ErrInvalidConfig), errors.Is(err, neverbounce.ErrInvalidRequest):
		what = "fix the request; nothing was sent"
	case errors.Is(err, neverbounce.ErrTimeout): // before context.DeadlineExceeded, which it also matches
		what = "the SDK's timeout expired; the call may already have been billed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		what = "stopped by the caller; the call may already have been processed and billed"
	case errors.Is(err, neverbounce.ErrUnauthorized):
		what = "check the API key"
	case errors.Is(err, neverbounce.ErrRateLimited), errors.Is(err, neverbounce.ErrUnavailable):
		what = "slow down or try later; the SDK never retries"
	case errors.Is(err, neverbounce.ErrJobFailed), errors.Is(err, neverbounce.ErrUnknownJobState),
		errors.Is(err, neverbounce.ErrBudgetExhausted), errors.Is(err, errOverBudget):
		what = "the job is not where it should be; read its status before acting again"
	default:
		what = "the outcome is unknown; do not resend blindly"
	}
	_, _ = fmt.Fprintln(w, what)
	return err
}

// joinFlags lists flags, or "none".
func joinFlags(flags neverbounce.Flags) string {
	if len(flags) == 0 {
		return "none"
	}
	names := make([]string, len(flags))
	for i, f := range flags {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// count prints a reported count, or "not reported" for nil.
func count(n *int64) string {
	if n == nil {
		return "not reported"
	}
	return strconv.FormatInt(*n, 10)
}

// fakeNeverBounce plays the NeverBounce API v4.2 with synthetic answers. Its
// job moves one state on with each status read: parsing, then waiting until
// it is started, then running, then complete.
type fakeNeverBounce struct {
	mu      sync.Mutex
	started bool
	reads   int
}

// ServeHTTP answers one call.
func (f *fakeNeverBounce) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Page int `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "the body is not JSON", http.StatusBadRequest)
		return
	}
	op := neverbounce.Operation(strings.TrimPrefix(r.URL.Path, "/v4.2/"))
	body := f.answer(op, req.Page)
	if body == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if op == neverbounce.OpDownloadJob {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	_, _ = io.WriteString(w, body)
}

// answer returns the body for op, or "" when there is none.
func (f *fakeNeverBounce) answer(op neverbounce.Operation, page int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case neverbounce.OpCheckEmail:
		return checkAnswer
	case neverbounce.OpCreateJob:
		f.started, f.reads = false, 0
		return createAnswer
	case neverbounce.OpStartJob:
		f.started, f.reads = true, 0
		return startAnswer
	case neverbounce.OpJobStatus:
		f.reads++
		switch {
		case !f.started && f.reads == 1:
			return fmt.Sprintf(statusFormat, neverbounce.JobStateParsing, 0, 0, 0, 0)
		case !f.started:
			return fmt.Sprintf(statusFormat, neverbounce.JobStateWaiting, 0, 0, 0, 0)
		case f.reads == 1:
			return fmt.Sprintf(statusFormat, neverbounce.JobStateRunning, 1, 1, 0, 33)
		}
		return fmt.Sprintf(statusFormat, neverbounce.JobStateComplete, 3, 2, 1, 100)
	case neverbounce.OpJobResults:
		switch page {
		case 1:
			return resultsPage1
		case 2:
			return resultsPage2
		}
	case neverbounce.OpDownloadJob:
		return downloadCSV
	case neverbounce.OpParseJob:
	}
	return ""
}

// statusReads returns the number of status reads since the job was last
// created or started.
func (f *fakeNeverBounce) statusReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}
