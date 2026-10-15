package neverbounce_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/mrz1836/go-neverbounce"
)

// Synthetic provider answers for the examples. They follow the documented
// v4.2 shapes, but every value is made up.
const (
	exCheckTypo = `{"status":"success","result":"invalid",` +
		`"flags":["has_dns","has_dns_mx","spelling_mistake"],` +
		`"suggested_correction":"avery.example@example.com",` +
		`"address_info":{"original_email":"avery.example@example.net",` +
		`"normalized_email":"avery.example@example.net","addr":"avery.example","alias":"",` +
		`"host":"example.net","fqdn":"example.net","domain":"example","subdomain":"","tld":"net"},` +
		`"credits_info":{"paid_credits_used":1,"free_credits_used":0,` +
		`"paid_credits_remaining":4999,"free_credits_remaining":0},"execution_time":412}`
	exCheckValid = `{"status":"success","result":"valid","flags":["has_dns","has_dns_mx"],` +
		`"credits_info":{"paid_credits_used":1,"free_credits_used":0},"execution_time":7}`
	exCreated      = `{"status":"success","job_id":4242,"execution_time":31}`
	exStarted      = `{"status":"success","queue_id":"NB-EXAMPLE-QUEUE","execution_time":12}`
	exStatusParsed = `{"status":"success","id":4242,"job_status":"waiting","filename":"signups-2026-10-15-a7f3",` +
		`"total":{"records":2,"billable":2,"processed":0},"percent_complete":0}`
	exStatusDone = `{"status":"success","id":4242,"job_status":"complete","filename":"signups-2026-10-15-a7f3",` +
		`"total":{"records":2,"billable":2,"processed":2,"valid":1,"invalid":0,"catchall":1},` +
		`"bounce_estimate":0,"percent_complete":100}`
	exStatusSampled = `{"status":"success","id":4242,"job_status":"waiting_analyzed",` +
		`"total":{"records":2,"billable":2},"bounce_estimate":12.5,"percent_complete":100}`
	exResults = `{"status":"success","total_results":3,"total_pages":1,"query":{"page":0},"results":[` +
		`{"data":{"id":"id","email":"email"},"verification":{"result":"invalid"}},` +
		`{"data":{"id":"1001","email":"avery.example@example.com"},"verification":{"result":"valid"}},` +
		`{"data":{"id":1002,"email":"blake.example@example.org"},"verification":{"result":"catchall"}}]}`
	exThrottled = `{"status":"throttle_triggered","message":"Synthetic: too many requests, slow down."}`
	exCSV       = "id,email,email_status\r\n" +
		"1001,avery.example@example.com,valid\r\n" +
		"1002,blake.example@example.org,catchall\r\n"
)

// exPage returns a synthetic jobs/results answer: page n of 4, with one row.
func exPage(n int) string {
	return fmt.Sprintf(`{"status":"success","total_results":4,"total_pages":4,"results":[`+
		`{"data":{"id":"%d"},"verification":{"result":"valid"}}]}`, 1000+n)
}

// exServer starts a local TLS server that stands in for NeverBounce, so the
// examples run offline, need no key and cost nothing. It answers each
// operation with the bodies given for it in turn, repeating the last one; for
// jobs/results it answers with the body for the page requested. Bodies are
// JSON, except for jobs/download, which answers with a CSV file.
func exServer(answers map[neverbounce.Operation][]string) *httptest.Server {
	var mu sync.Mutex
	calls := map[neverbounce.Operation]int{}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := neverbounce.Operation(strings.TrimPrefix(r.URL.Path, "/v4.2/"))
		var req struct {
			Page int `json:"page"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "not JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		i := calls[op]
		calls[op]++
		mu.Unlock()
		if op == neverbounce.OpJobResults {
			i = max(req.Page, 1) - 1
		}
		bodies := answers[op]
		if len(bodies) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if op == neverbounce.OpDownloadJob {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = io.WriteString(w, bodies[min(i, len(bodies)-1)])
	}))
}

// ExampleNewClient builds a client once, to be shared: it is safe for
// concurrent use. NewClient makes no call and reads no environment variable
// or file: the application decides where the key comes from.
func ExampleNewClient() {
	apiKey := "secret_example0000"     // from your application's secret store
	sharedHTTPClient := &http.Client{} // your own transport settings, if any

	client, err := neverbounce.NewClient(apiKey,
		neverbounce.WithTimeout(45*time.Second),
		neverbounce.WithHTTPClient(sharedHTTPClient), // copied; redirects refused; never mutated
	)
	if err != nil {
		fmt.Println("config:", err) // names the option, never the key
		return
	}
	fmt.Println(client) // settings only, never the key

	_, err = neverbounce.NewClient(apiKey, neverbounce.WithTimeout(time.Hour))
	fmt.Println(err)
	// Output:
	// neverbounce.Client{timeout: 45s, maxResponseBytes: 8388608, downloadTimeout: 30m0s, maxDownloadBytes: 1073741824, testBaseURL: false, providerDiagnostics: false}
	// neverbounce: invalid configuration: Timeout: out_of_range
}

// ExampleClient_CheckEmail verifies one address. Every check is billed one
// credit, whatever its result.
func ExampleClient_CheckEmail() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpCheckEmail: {exCheckTypo}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}

	res, err := client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{
		Email:       "avery.example@example.net",
		AddressInfo: true,
		CreditsInfo: true,
		Timeout:     20 * time.Second, // the provider's own verification timeout
	})
	if err != nil {
		fmt.Println("check:", err) // it may already be billed: never resend blindly
		return
	}
	switch {
	case res.Result == neverbounce.ResultUnknown:
		fmt.Println("unknown: a valid answer, not an error")
	case !res.Result.Known():
		fmt.Println("undocumented result, kept as sent:", res.Result)
	default:
		fmt.Println("result:", res.Result)
	}
	fmt.Println("flags:", res.Flags)
	fmt.Println("spelling mistake:", res.Flags.Has(neverbounce.FlagSpellingMistake))
	if res.SuggestedCorrection != "" {
		fmt.Println("did you mean:", res.SuggestedCorrection) // a suggestion: never applied or rechecked
	}
	if ai := res.AddressInfo; ai != nil {
		fmt.Println("host:", ai.Host, "normalized:", ai.NormalizedEmail)
	}
	if ci := res.CreditsInfo; ci != nil && ci.PaidCreditsRemaining != nil {
		fmt.Println("paid credits remaining:", *ci.PaidCreditsRemaining)
	}
	// Output:
	// result: invalid
	// flags: [has_dns has_dns_mx spelling_mistake]
	// spelling mistake: true
	// did you mean: avery.example@example.com
	// host: example.net normalized: avery.example@example.net
	// paid credits remaining: 4999
}

// ExampleClient_CreateJob verifies a list deliberately: create it with
// parsing only, wait until it is parsed, check the billable count against a
// budget, then start it explicitly and wait for the results.
func ExampleClient_CreateJob() {
	srv := exServer(map[neverbounce.Operation][]string{
		neverbounce.OpCreateJob: {exCreated},
		neverbounce.OpJobStatus: {exStatusParsed, exStatusDone},
		neverbounce.OpStartJob:  {exStarted},
	})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	ctx := context.Background()
	const budget = 500 // the most credits this list may use

	created, err := client.CreateJob(ctx, neverbounce.CreateJobRequest{
		Supplied: neverbounce.SuppliedList{
			Columns: []string{"id", "email"},
			Rows:    [][]string{{"1001", "avery.example@example.com"}, {"1002", "blake.example@example.org"}},
		},
		Filename:  "signups-2026-10-15-a7f3", // unique: findable in the dashboard after an ambiguous create
		AutoParse: true,                      // AutoStart false is sent explicitly: nothing is billed yet
	})
	if err != nil {
		fmt.Println("create:", err) // a job may exist anyway: look for its Filename before resubmitting
		return
	}
	st, err := client.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: created.JobID, MaxWait: 15 * time.Minute})
	if err != nil {
		fmt.Println("parse:", err) // st, when not nil, is the last status read
		return
	}
	if st.State != neverbounce.JobStateWaiting || st.Totals.Billable == nil || *st.Totals.Billable > budget {
		fmt.Println("not starting job", created.JobID, "in state", st.State)
		return
	}
	fmt.Println("job", created.JobID, "is parsed; billable:", *st.Totals.Billable)

	// PAID AND IRREVERSIBLE: the provider deducts credits for the billable count.
	if _, err = client.StartJob(ctx, neverbounce.StartJobRequest{JobID: created.JobID}); err != nil {
		fmt.Println("start:", err) // it may have started anyway: read JobStatus before ever starting again
		return
	}
	st, err = client.WaitForJob(ctx, neverbounce.WaitForJobRequest{
		JobID: created.JobID, Interval: 15 * time.Second, MaxWait: 2 * time.Hour,
	})
	if err != nil {
		fmt.Println("verify:", err)
		return
	}
	fmt.Println("job", created.JobID, "is", st.State)
	// Output:
	// job 4242 is parsed; billable: 2
	// job 4242 is complete
}

// ExampleClient_WaitForJob polls a job's status until it needs you, has
// ended, or the wait budget is used up. It only reads the status: it never
// starts, resubmits or stops a job.
func ExampleClient_WaitForJob() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpJobStatus: {exStatusSampled}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}

	st, err := client.WaitForJob(context.Background(), neverbounce.WaitForJobRequest{
		JobID:    4242,
		Interval: 10 * time.Second, // the provider suggests 5 to 10 seconds for lists under 50,000
		MaxWait:  2 * time.Hour,    // manual review can take a business day: size it to the list
	})
	switch {
	case errors.Is(err, neverbounce.ErrJobFailed):
		fmt.Println("job failed:", st.FailureReason) // "" means the provider gave no reason
	case errors.Is(err, neverbounce.ErrBudgetExhausted):
		fmt.Println("still in progress; the job keeps running") // st is the last status read, or nil
	case err != nil:
		fmt.Println("wait:", err) // an undocumented state, or a failed poll: never retried
	case st.State == neverbounce.JobStateComplete:
		fmt.Println("complete: read the results")
	case st.State == neverbounce.JobStateWaiting:
		fmt.Println("parsed: a full StartJob verifies the list (billed)")
	case st.State == neverbounce.JobStateWaitingAnalyzed:
		fmt.Println("sample done: a full StartJob verifies the list (billed)")
		if st.BounceEstimate != nil {
			fmt.Printf("estimated bounce rate: %.1f%%\n", *st.BounceEstimate)
		}
	}
	// Output:
	// sample done: a full StartJob verifies the list (billed)
	// estimated bounce rate: 12.5%
}

// ExampleClient_JobResults reads one page of results and matches each row to
// the application's own record through a column it submitted.
func ExampleClient_JobResults() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpJobResults: {exResults}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	signups := map[string]string{"1001": "Avery", "1002": "Blake"} // your records, by the id you submitted

	page, err := client.JobResults(context.Background(), neverbounce.JobResultsRequest{
		JobID: 4242, Page: 1, ItemsPerPage: 500,
	})
	if err != nil {
		fmt.Println("results:", err)
		return
	}
	fmt.Printf("page %d of %d, %d results\n", page.Page, page.TotalPages, page.TotalResults)
	for _, row := range page.Results {
		// Correlate by your own column, never by position: the provider may
		// return the header row as a record, and may return duplicates.
		id, _ := row.Column("id")
		name, ok := signups[id]
		if !ok {
			fmt.Printf("skipped a row that is not one of yours (id %q)\n", id)
			continue
		}
		fmt.Println(name, row.Verification.Result)
	}
	// Output:
	// page 1 of 1, 3 results
	// skipped a row that is not one of yours (id "id")
	// Avery valid
	// Blake catchall
}

// ExampleClient_JobResultPages walks a job's result pages within a page
// budget, resumes where the budget ran out, and stops early with break.
func ExampleClient_JobResultPages() {
	srv := exServer(map[neverbounce.Operation][]string{
		neverbounce.OpJobResults: {exPage(1), exPage(2), exPage(3), exPage(4)},
	})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	ctx := context.Background()
	const want = 3 // rows this run stores; a later run goes on from there
	stored := 0

	req := neverbounce.JobResultPagesRequest{JobID: 4242, ItemsPerPage: 1, MaxPages: 2}
	for page, err := range client.JobResultPages(ctx, req) {
		if ce, ok := errors.AsType[*neverbounce.CallError](err); ok && ce.Kind == neverbounce.FailureBudgetExhausted {
			fmt.Println("page budget used up; resume at page", ce.Page)
			req.StartPage = ce.Page // the pages before it were already handled
			break
		}
		if err != nil {
			fmt.Println("results:", err) // pages already handled stay handled
			return
		}
		id, _ := page.Results[0].Column("id")
		fmt.Printf("page %d of %d: id %s\n", page.Page, page.TotalPages, id)
		stored += len(page.Results)
	}

	// Resume where the budget ran out.
	for page, err := range client.JobResultPages(ctx, req) {
		if err != nil {
			fmt.Println("results:", err)
			return
		}
		id, _ := page.Results[0].Column("id")
		fmt.Printf("page %d of %d: id %s\n", page.Page, page.TotalPages, id)
		if stored += len(page.Results); stored >= want {
			break // the walk stops at once: page 4 is never requested
		}
	}
	// Output:
	// page 1 of 4: id 1001
	// page 2 of 4: id 1002
	// page budget used up; resume at page 3
	// page 3 of 4: id 1003
}

// ExampleClient_DownloadJob streams a job's results, a CSV file, into a
// writer the caller owns.
func ExampleClient_DownloadJob() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpDownloadJob: {exCSV}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}

	// file stands in for a file the application creates, such as one from
	// os.Create. The SDK never closes, flushes or removes it.
	var file bytes.Buffer
	n, err := client.DownloadJob(context.Background(), neverbounce.DownloadJobRequest{
		JobID:    4242,
		Append:   neverbounce.DownloadColumns{EmailStatus: new(true)},
		LineFeed: neverbounce.LineFeedCRLF, // the provider's default, "\n\r", trips encoding/csv
	}, &file)
	if err != nil {
		file.Reset() // discard the partial file (n bytes): never append a second attempt to it
		fmt.Println("download:", err, "after", n, "bytes")
		return
	}
	fmt.Println("bytes written:", n)
	for line := range strings.Lines(file.String()) {
		fmt.Printf("%q\n", line)
	}
	// Output:
	// bytes written: 102
	// "id,email,email_status\r\n"
	// "1001,avery.example@example.com,valid\r\n"
	// "1002,blake.example@example.org,catchall\r\n"
}

// ExampleCallError reads a failed call. Its Error text holds only the
// operation, the kind, the HTTP status, the provider's status token and a
// field path, so it is safe to log. The provider's message is kept only with
// WithProviderDiagnostics.
func ExampleCallError() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpCheckEmail: {exThrottled}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
		neverbounce.WithProviderDiagnostics(),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}

	_, err = client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{Email: "avery.example@example.com"})
	fmt.Println("log line:", err)
	fmt.Println("rate limited:", errors.Is(err, neverbounce.ErrRateLimited)) // the SDK never retries
	if ce, ok := errors.AsType[*neverbounce.CallError](err); ok {
		fmt.Println(ce.Op, ce.Kind, ce.StatusCode, ce.APIStatus)
		fmt.Println("provider says:", ce.Diagnostics().Message) // may echo request data: think before logging
	}
	// Output:
	// log line: neverbounce: single/check: rate_limited (HTTP 200, throttle_triggered)
	// rate limited: true
	// single/check rate_limited 200 throttle_triggered
	// provider says: Synthetic: too many requests, slow down.
}

// Example_errors classifies failures. A *ValidationError means nothing was
// sent. Any other failure may follow a call the provider already processed
// and billed, so none of these branches resends blindly.
func Example_errors() {
	// A loopback address with no server: even a bug could not reach the provider.
	client, err := neverbounce.NewClient("secret_example0000", neverbounce.WithTestBaseURL("https://127.0.0.1:9"))
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, invalid := client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{Email: " "})
	_, deadline := client.CheckEmail(expired, neverbounce.CheckEmailRequest{Email: "avery.example@example.com"})

	describe := func(err error) string {
		if ve, ok := errors.AsType[*neverbounce.ValidationError](err); ok {
			return "nothing sent; fix " + ve.Field + " (" + ve.Reason + ")"
		}
		switch {
		case errors.Is(err, neverbounce.ErrTimeout): // before context.DeadlineExceeded, which it also matches
			return "the SDK's timeout expired; the call may already be billed"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return "the caller's context ended"
		case errors.Is(err, neverbounce.ErrUnauthorized), errors.Is(err, neverbounce.ErrRateLimited):
			return "refused; check the key or slow down"
		case errors.Is(err, neverbounce.ErrBudgetExhausted), errors.Is(err, neverbounce.ErrJobFailed):
			return "bulk work incomplete"
		}
		return "other failure"
	}
	for _, err := range []error{
		invalid,
		deadline,
		// Errors as the SDK reports them, built by hand here.
		&neverbounce.CallError{Kind: neverbounce.FailureTimeout, Op: neverbounce.OpCheckEmail},
		&neverbounce.CallError{
			Kind: neverbounce.FailureUnauthorized, Op: neverbounce.OpJobStatus, JobID: 4242,
			StatusCode: http.StatusOK, APIStatus: neverbounce.APIStatusAuthFailure,
		},
		&neverbounce.CallError{Kind: neverbounce.FailureBudgetExhausted, Op: neverbounce.OpJobResults, JobID: 4242, Page: 3},
	} {
		fmt.Println(describe(err))
		fmt.Println("  log line:", err)
	}
	// Output:
	// nothing sent; fix Email (blank)
	//   log line: neverbounce: invalid request: Email: blank
	// the caller's context ended
	//   log line: neverbounce: single/check: deadline_exceeded
	// the SDK's timeout expired; the call may already be billed
	//   log line: neverbounce: single/check: timeout
	// refused; check the key or slow down
	//   log line: neverbounce: jobs/status: unauthorized (HTTP 200, auth_failure)
	// bulk work incomplete
	//   log line: neverbounce: jobs/results: budget_exhausted
}

// signupHandler is an application's handler for a signup form: it checks
// the posted address and answers with a small JSON verdict.
type signupHandler struct {
	client *neverbounce.Client
}

// ServeHTTP checks the form's email field.
func (h signupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// r.Context() ends when the visitor goes away; the application adds its
	// own deadline on top. Either may end a check the provider still bills.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := h.client.CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: r.FormValue("email")})
	switch {
	case errors.Is(err, neverbounce.ErrInvalidRequest):
		http.Error(w, "enter an email address", http.StatusBadRequest)
		return
	case err != nil:
		// err holds no key, address or provider text: safe to log.
		http.Error(w, "email check unavailable", http.StatusBadGateway)
		return
	}

	type verdict struct {
		Result     neverbounce.Result `json:"result"`
		Suggestion string             `json:"suggestion,omitempty"`
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(verdict{Result: res.Result, Suggestion: res.SuggestedCorrection}); err != nil {
		return // the visitor went away
	}
}

// Example_httpHandler serves a signup form's email check from a net/http
// handler, exercised here with httptest.NewRecorder.
func Example_httpHandler() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpCheckEmail: {exCheckValid}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	h := signupHandler{client: client} // in an application: mux.Handle("POST /signup/check", h)

	for _, email := range []string{"avery.example@example.com", ""} {
		form := strings.NewReader(url.Values{"email": {email}}.Encode())
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/signup/check", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Print(rec.Code, " ", rec.Body.String())
	}
	// Output:
	// 200 {"result":"valid"}
	// 400 enter an email address
}

// fakeChecker is an application's test fake: it returns a canned answer.
type fakeChecker struct {
	res *neverbounce.CheckEmailResult
	err error
}

// CheckEmail returns the canned answer.
func (f fakeChecker) CheckEmail(context.Context, neverbounce.CheckEmailRequest) (*neverbounce.CheckEmailResult, error) {
	return f.res, f.err
}

// Example_fake tests application code without the provider: the application
// depends on a one-method interface of its own, which the real Client
// satisfies, and its tests pass a fake. A CallError built from its exported
// fields matches errors.Is like one the SDK returns.
func Example_fake() {
	type EmailChecker interface {
		CheckEmail(ctx context.Context, req neverbounce.CheckEmailRequest) (*neverbounce.CheckEmailResult, error)
	}
	var _ EmailChecker = (*neverbounce.Client)(nil)

	var checker EmailChecker = fakeChecker{err: &neverbounce.CallError{
		Kind: neverbounce.FailureRateLimited, Op: neverbounce.OpCheckEmail,
		StatusCode: http.StatusOK, APIStatus: neverbounce.APIStatusThrottleTriggered,
	}}
	_, err := checker.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{Email: "avery.example@example.com"})
	fmt.Println(errors.Is(err, neverbounce.ErrRateLimited))
	fmt.Println(err)
	// Output:
	// true
	// neverbounce: single/check: rate_limited (HTTP 200, throttle_triggered)
}

// Example_storingResults stores a result as JSON and reads it back unchanged:
// the provider's snake_case names, with an unreported counter kept apart from
// a zero one. Unmarshal only what the SDK returned: a raw provider body
// unmarshaled into these types skips every check the SDK makes.
func Example_storingResults() {
	srv := exServer(map[neverbounce.Operation][]string{neverbounce.OpCheckEmail: {exCheckValid}})
	defer srv.Close()

	// A real application passes its own key and leaves out the two test options.
	client, err := neverbounce.NewClient("secret_example0000",
		neverbounce.WithTestBaseURL(srv.URL),
		neverbounce.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		fmt.Println("config:", err)
		return
	}
	res, err := client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{
		Email: "avery.example@example.com", CreditsInfo: true,
	})
	if err != nil {
		fmt.Println("check:", err)
		return
	}

	stored, err := json.Marshal(res)
	if err != nil {
		fmt.Println("store:", err)
		return
	}
	var loaded neverbounce.CheckEmailResult
	if err = json.Unmarshal(stored, &loaded); err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Println(string(stored))
	fmt.Println("unchanged:", reflect.DeepEqual(*res, loaded))
	// Output:
	// {"result":"valid","flags":["has_dns","has_dns_mx"],"credits_info":{"paid_credits_used":1,"free_credits_used":0},"execution_time":7}
	// unchanged: true
}
