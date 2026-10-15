// Package neverbounce is a small, typed, standard-library-only Go client for
// the NeverBounce API v4.2 (https://api.neverbounce.com/v4.2): single-email
// verification and the bulk-jobs workflow.
//
// Create one Client with NewClient and your v4 API key, share it, and call one
// method per operation. Every method takes a context and a request struct.
// Each operation method makes exactly one HTTP attempt; the two helpers make
// one per poll or page, within a budget you set.
//
//	client, err := neverbounce.NewClient(apiKey)
//	if err != nil {
//		return err
//	}
//	res, err := client.CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: "avery.example@example.com"})
//	if err != nil {
//		return err // the check may already be billed: never resend blindly
//	}
//	if res.Result == neverbounce.ResultUnknown {
//		// a successful answer, not an error
//	}
//
// # Operations and billing
//
// Nothing paid or state-changing happens unless you call the method that does
// it:
//
//	Method          Operation       Effect
//	CheckEmail      single/check    PAID: 1 credit per call, duplicates and bad syntax included
//	CreateJob       jobs/create     creates a durable job; PAID when AutoStart is true
//	ParseJob        jobs/parse      parses a job once; PAID when AutoStart is true
//	StartJob        jobs/start      PAID and irreversible, unless RunSample (free)
//	JobStatus       jobs/status     free, read-only
//	JobResults      jobs/results    free, read-only, one page
//	DownloadJob     jobs/download   free, read-only, streams the CSV to your writer
//	WaitForJob      jobs/status     free: polls status only, within MaxWait
//	JobResultPages  jobs/results    free: an iterator, one call per page, within MaxPages
//
// The provider documents no sandbox and no free test addresses. The SDK never
// retries, hedges, follows a redirect, paginates on its own, or creates,
// parses, starts, polls or resubmits a job you did not ask for, and it disables
// the standard library's automatic resends. After a timeout, a cancellation, a
// transport failure or an unreadable response, the provider may already have
// billed the check or created or started the job: inspect before repeating.
// Requests that fail local validation, and calls whose context is already
// done, send nothing.
//
// # Verifying a list
//
// The deliberate flow creates a job that only parses, waits for it, inspects
// the provider's billable count, and starts it explicitly:
//
//	created, err := client.CreateJob(ctx, neverbounce.CreateJobRequest{
//		Supplied:  neverbounce.EmailList(addresses...),
//		Filename:  "signups-2026-10-15-a7f3", // unique: findable after an ambiguous create
//		AutoParse: true,                      // AutoStart stays false: nothing billed yet
//	})
//	if err != nil {
//		return err // a job may exist anyway: look for its Filename before resubmitting
//	}
//	st, err := client.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: created.JobID, MaxWait: 15 * time.Minute})
//	if err != nil {
//		return err
//	}
//	if st.State != neverbounce.JobStateWaiting || st.Totals.Billable == nil || *st.Totals.Billable > budget {
//		return errOverBudget // Totals.Billable is what a full start costs
//	}
//	_, err = client.StartJob(ctx, neverbounce.StartJobRequest{JobID: created.JobID}) // PAID and irreversible
//
// Then read the results a page at a time with JobResults or JobResultPages, or
// stream the CSV with DownloadJob. Correlate rows by a column of your own, never
// by position: the provider may return a list's header row as a record.
//
// The provider's usage guidelines, which the SDK documents and never enforces:
// never verify an existing list one address at a time with CheckEmail; run at
// most 10 concurrent jobs and 50 runs a day; create no more than 10 jobs per
// 100,000 items per hour; keep a supplied list within 25 MB
// (MaxJobRequestBytes); split millions of addresses into chunks of 1 million;
// and poll every 5 to 10 seconds for lists under 50,000 addresses, less often
// for larger ones.
//
// # Errors
//
// Validation failures are *ValidationError values matching ErrInvalidConfig or
// ErrInvalidRequest: nothing was sent. Every other failure is a *CallError,
// whose Kind matches one sentinel (or context.Canceled or
// context.DeadlineExceeded) with errors.Is, and whose Op, StatusCode,
// APIStatus, JobID, Page, Field and BytesWritten say where it happened. The
// provider reports most errors inside an HTTP 200 envelope: its status decides
// the kind (auth_failure and bad_referrer are ErrUnauthorized,
// throttle_triggered ErrRateLimited, temp_unavail ErrUnavailable and
// general_failure ErrRejected). Any other HTTP status decides on its own.
// ErrTimeout also matches context.DeadlineExceeded, so check it first.
//
// Error strings hold only the operation, the kind, the HTTP and API statuses
// and a JSON field path: never the API key, email addresses, row data, URLs,
// header values, response bodies or provider messages. The provider's message
// is kept only with WithProviderDiagnostics, behind (*CallError).Diagnostics,
// with the key scrubbed.
//
// # Personal data
//
// Requests and results hold contact data. The SDK never logs, prints, stores or
// caches anything, and never reads credentials or configuration from the
// environment, but its result types have no redacting formatter: access
// control, logging, retention, security and lawful use are the caller's
// responsibility, and hashing an address does not anonymize it. A verification
// result is the provider's observation: it does not establish consent,
// identity, ownership or future deliverability. RemoteURL, CallbackURL and
// CallbackHeaders may carry third-party credentials; CreateJobRequest's
// String, GoString and Format print only the request's shape for every fmt
// verb, except %p on a value and a request held in an unexported struct
// field, which fmt prints without calling them.
//
// # Proxies
//
// The default transport honors HTTPS_PROXY and NO_PROXY through
// http.ProxyFromEnvironment, as the standard library's default transport does.
// That is the standard library reading the environment, not the SDK. To bypass
// proxies, inject an http.Client whose transport sets no Proxy.
//
// # Where the documentation is ambiguous
//
// NeverBounce's v4.2 documentation leaves some points open or contradicts
// itself. The SDK sends booleans as JSON true and false (the encoding rule,
// although one schema types them as integers); sends leverage_historical_data
// only when set; accepts percentages as integers or floats from 0 to 100;
// never decodes the results "query" echo, whose example says page 0; keeps
// each row's data as raw JSON, because values may come back as strings;
// models only the nested v4.2 status shape; keeps retry_token as opaque text
// it never sends back; and streams download bytes unchanged, making no claim
// about a header row. The units of execution_time, the time zone of the
// timestamps, the single-check timeout's default and range, the error format
// of an early download, the results' ordering, rate-limit headers or a
// numeric rate limit, idempotency, and the signing, ordering and delivery of
// job callbacks are undocumented: the SDK assumes none of them and never
// retries or replays a request.
//
// # Scope and extension
//
// Implemented: single/check and jobs/create, parse, start, status, results and
// download. Not implemented: account/info, jobs/search, jobs/delete,
// poe/confirm, and a callback receiver. Each would be added later as one more
// method on Client with its own request and result types, without breaking
// existing callers (see docs/adding-an-operation.md in the repository).
package neverbounce
