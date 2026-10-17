<div align="center">

# 📬&nbsp;&nbsp;go-neverbounce

**A small, typed Go SDK for the NeverBounce API v4.2: single and bulk email verification.**

<br/>

<a href="https://github.com/mrz1836/go-neverbounce/releases"><img src="https://img.shields.io/github/release-pre/mrz1836/go-neverbounce.svg?include_prereleases&style=flat-square&logo=github&color=black" alt="Release"></a>
<a href="https://golang.org/"><img src="https://img.shields.io/github/go-mod/go-version/mrz1836/go-neverbounce?style=flat-square&logo=go&color=00ADD8" alt="Go Version"></a>
<a href="https://github.com/mrz1836/go-neverbounce/blob/master/LICENSE"><img src="https://img.shields.io/github/license/mrz1836/go-neverbounce.svg?style=flat-square&color=blue" alt="License"></a>

<br/>

<table align="center" border="0">
  <tr>
    <td align="right">
       <code>CI / CD</code> &nbsp;&nbsp;
    </td>
    <td align="left">
       <a href="https://github.com/mrz1836/go-neverbounce/actions"><img src="https://img.shields.io/github/actions/workflow/status/mrz1836/go-neverbounce/fortress.yml?branch=master&label=build&logo=github&style=flat-square" alt="Build"></a>
       <a href="https://github.com/mrz1836/go-neverbounce/actions"><img src="https://img.shields.io/github/last-commit/mrz1836/go-neverbounce?style=flat-square&logo=git&logoColor=white&label=last%20update" alt="Last Commit"></a>
    </td>
    <td align="right">
       &nbsp;&nbsp;&nbsp;&nbsp; <code>Quality</code> &nbsp;&nbsp;
    </td>
    <td align="left">
       <a href="https://codecov.io/gh/mrz1836/go-neverbounce"><img src="https://codecov.io/gh/mrz1836/go-neverbounce/branch/master/graph/badge.svg?style=flat-square" alt="Coverage"></a>
    </td>
  </tr>

  <tr>
    <td align="right">
       <code>Security</code> &nbsp;&nbsp;
    </td>
    <td align="left">
       <a href="https://scorecard.dev/viewer/?uri=github.com/mrz1836/go-neverbounce"><img src="https://api.scorecard.dev/projects/github.com/mrz1836/go-neverbounce/badge?style=flat-square" alt="Scorecard"></a>
       <a href=".github/SECURITY.md"><img src="https://img.shields.io/badge/policy-active-success?style=flat-square&logo=security&logoColor=white" alt="Security"></a>
    </td>
    <td align="right">
       &nbsp;&nbsp;&nbsp;&nbsp; <code>Community</code> &nbsp;&nbsp;
    </td>
    <td align="left">
       <a href="https://github.com/mrz1836/go-neverbounce/graphs/contributors"><img src="https://img.shields.io/github/contributors/mrz1836/go-neverbounce?style=flat-square&color=orange" alt="Contributors"></a>
       <a href="https://mrz1818.com/"><img src="https://img.shields.io/badge/donate-bitcoin-ff9900?style=flat-square&logo=bitcoin" alt="Bitcoin"></a>
    </td>
  </tr>
</table>

</div>

<br/>
<br/>

<div align="center">

### <code>Project Navigation</code>

</div>

<table align="center">
  <tr>
    <td align="center" width="20%">
       🚀&nbsp;<a href="#-installation"><code>Installation</code></a>
    </td>
    <td align="center" width="20%">
       🏁&nbsp;<a href="#-quick-start"><code>Quick&nbsp;Start</code></a>
    </td>
    <td align="center" width="20%">
       💡&nbsp;<a href="#-usage"><code>Usage</code></a>
    </td>
    <td align="center" width="20%">
       ⚙️&nbsp;<a href="#%EF%B8%8F-behavior"><code>Behavior</code></a>
    </td>
    <td align="center" width="20%">
       📚&nbsp;<a href="#-documentation"><code>Documentation</code></a>
    </td>
  </tr>
  <tr>
    <td align="center">
       🧪&nbsp;<a href="#-examples--tests"><code>Examples&nbsp;&&nbsp;Tests</code></a>
    </td>
    <td align="center">
      ⚡&nbsp;<a href="#-benchmarks"><code>Benchmarks</code></a>
    </td>
    <td align="center">
      🛠️&nbsp;<a href="#%EF%B8%8F-code-standards"><code>Code&nbsp;Standards</code></a>
    </td>
    <td align="center">
      🤖&nbsp;<a href="#-ai-usage--assistant-guidelines"><code>AI&nbsp;Usage</code></a>
    </td>
    <td align="center">
       🤝&nbsp;<a href="#-contributing"><code>Contributing</code></a>
    </td>
  </tr>
</table>
<br/>

go-neverbounce calls [NeverBounce](https://neverbounce.com)'s API v4.2 to verify email addresses: one
address at a time with `single/check`, or whole lists through NeverBounce's bulk jobs (create, parse,
start, status, results and download). Use it when a Go service needs to know whether an address is
deliverable and has to stay in control of every billable call.

- **One attempt per call.** No retries, redirects, replays, hedging or hidden pagination: the SDK never
  repeats a request on its own, and Go's HTTP clients cannot resend one either.
- **Typed results that keep NeverBounce's meaning.** `unknown` is an answer, not an error; an absent count
  is `nil`, not `0`; undocumented verdicts, flags and job states are kept as sent when they have the
  documented token shape.
- **Nothing paid happens implicitly.** Every billed or state-changing step (a check, creating, parsing or
  starting a job) is a method you call. The helpers only read: `WaitForJob` polls status, and
  `JobResultPages` reads pages, each within a budget you set.
- **Bounded and streaming.** Every call has a timeout and a size limit; result CSVs stream straight to
  your `io.Writer`, byte for byte.
- **Private by construction.** Errors and printed clients never contain the API key, email addresses,
  URLs, bodies or NeverBounce's messages.
- **Zero runtime dependencies:** the standard library only.

Version 1 covers single verification and the bulk-jobs workflow of API v4.2; see
[what is not covered](#what-is-not-covered).

<br/>

## 📦 Installation

**go-neverbounce** requires a [supported release of Go](https://golang.org/doc/devel/release.html#policy) (Go 1.26 or newer).
```shell script
go get github.com/mrz1836/go-neverbounce
```

The library has **zero third-party runtime dependencies**: it uses only the standard library. `testify`
is used only by the tests and their internal helper package.

<br/>

### Credentials

You need a NeverBounce **v4 API key** (it starts with `secret_`), created in the NeverBounce dashboard;
v3 credentials are refused with `auth_failure`. go-neverbounce never reads an environment variable or a
configuration file for it: your code passes the key to `neverbounce.NewClient`, from wherever it keeps
secrets. The key travels only in the JSON body of each request, never in a URL or a header.

NeverBounce documents no sandbox and no free test addresses: **every `CheckEmail` call is billed**, and
so is every full job start. Try the SDK offline first with `go run ./examples`.

<br/>

## 🏁 Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mrz1836/go-neverbounce"
)

func main() {
	// The application chooses where the key comes from; the SDK reads no environment.
	client, err := neverbounce.NewClient(os.Getenv("NEVERBOUNCE_API_KEY"))
	if err != nil {
		log.Fatal(err) // ErrInvalidConfig: no call was made
	}

	// One billable call (1 credit), whatever the result.
	res, err := client.CheckEmail(context.Background(), neverbounce.CheckEmailRequest{
		Email:       "avery.example@example.com",
		AddressInfo: true,
	})
	if err != nil {
		log.Fatal(err) // the check may already be billed: do not resend blindly
	}
	fmt.Println(res.Result, res.Flags)
}
```

Illustrative output (the values are NeverBounce's):

```text
valid [has_dns has_dns_mx smtp_connectable]
```

<br/>

### Next steps

- [Set up a client once](#set-up-once) and [check one email](#check-one-email).
- [Verify a list](#verify-a-list) without paying for anything you did not inspect first.
- [Read results](#read-results) a page at a time, or [download the CSV](#download-the-csv).
- Handle [errors](#errors) without resending billed calls.
- Run the [offline example](examples): `go run ./examples`.

<br/>

## 💡 Usage

### Set up once

Create one `Client` per API key and share it: it is immutable and safe for concurrent use.

```go
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
```

| Option | Default | Accepted values |
|---|---|---|
| `WithTimeout(d)` | 60s (`DefaultTimeout`) | More than 0, at most 10 minutes. Each JSON call, including the whole body read. An earlier deadline on the call's context wins. |
| `WithMaxResponseBytes(n)` | 8 MiB (`DefaultMaxResponseBytes`) | 1 byte to 64 MiB. Every operation except `jobs/results` is also capped at 1 MiB. |
| `WithDownloadTimeout(d)` | 30 minutes (`DefaultDownloadTimeout`) | More than 0, at most 12 hours. A whole `DownloadJob`, streaming included. |
| `WithMaxDownloadBytes(n)` | 1 GiB (`DefaultMaxDownloadBytes`) | 1 byte to 64 GiB: the bytes `DownloadJob` writes. |
| `WithHTTPClient(hc)` | a transport the SDK builds | A non-nil `*http.Client`; see [the transport](#the-transport). |
| `WithTestBaseURL(url)` | `https://api.neverbounce.com` | Tests only: `https` on `127.0.0.0/8` or `[::1]`, an optional port, and no path, query, fragment or userinfo. |
| `WithProviderDiagnostics()` | off | Keeps NeverBounce's `message` on errors, behind `(*CallError).Diagnostics`, with the key scrubbed. |

Nil options are skipped and the last option of each kind wins. `NewClient` validates the key (1 to 256
bytes of visible ASCII, without `"` or `\`) and every option before returning, and performs no I/O. Its
errors name the argument or option, never the rejected value.

> **Holding many keys?** Without `WithHTTPClient`, each `Client` owns a connection pool that cannot be
> closed (idle connections close on their own after 90 seconds). Pass the same `*http.Client` to every
> `Client` so that they share one pool.

<br/>

### Check one email

`CheckEmail` verifies one address and **costs one credit per call**, duplicates and syntactically bad
addresses included.

```go
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
```

| Request field | Sent as | Meaning |
|---|---|---|
| `Email` | `email` | Sent verbatim: never trimmed, normalized or checked against an RFC. 1 to 1024 bytes of UTF-8 without control characters, and not only whitespace. A malformed address is a legitimate (billed) input. |
| `AddressInfo` | `address_info` | Ask for NeverBounce's parse of the address. Always sent. |
| `CreditsInfo` | `credits_info` | Ask for the account's credit counters. Always sent. |
| `Timeout` | `timeout` (seconds) | NeverBounce's own verification timeout; it then answers `unknown`. 0 is not sent. Whole seconds, 1s to 300s, and at least 10 seconds below `WithTimeout`. |
| `LeverageHistoricalData` | `request_meta_data.leverage_historical_data` | `false` asks for real-time verification only; `nil` is not sent. |

| `Result` | Meaning |
|---|---|
| `ResultValid` | Verified as a real address. |
| `ResultInvalid` | Verified as not valid. |
| `ResultDisposable` | A temporary, disposable address. |
| `ResultCatchall` | The domain accepts all mail, so the address itself could not be verified. |
| `ResultUnknown` | The server could not be reached. **A successful (and billed) answer, not an error.** |

`Flags` lists NeverBounce's observations, such as `FlagRoleAccount`, `FlagFreeEmailHost` or
`FlagHistoricalResponse`, in NeverBounce's order; the SDK defines all 22 documented flags. Good to know:

- **`unknown` is an answer.** The SDK never re-checks it on its own; whether to retry later is your
  policy, and a retry is another credit.
- **`SuggestedCorrection` is a suggestion.** The SDK never applies it or checks the suggested address.
- **`catchall` is not "safe".** There is deliberately no `IsValid` or "deliverable" helper: what to do with
  each verdict is your decision.
- **Unknown values are kept as sent** when they have the documented token shape (lowercase letters, digits
  and `_`, up to 64 bytes); anything else fails as `ErrUnexpectedResponse`. `Result`, `Flag` and `JobState`
  have `Known()`; the v4.1 spelling `acedemic_host` would come back verbatim, never normalized.
- **A verdict is an observation,** not consent, identity, ownership or a promise of future
  deliverability.

<br/>

### Verify a list

NeverBounce verifies lists as **jobs**. A job moves through these states:

```text
create ──► parsing ──► waiting ──► StartJob ──► queued / running ──► complete
              (AutoParse or ParseJob)   (billed here)                    or failed
```

| `JobState` | Meaning | What you do |
|---|---|---|
| `JobStateUploading`, `JobStateParsing` | NeverBounce is reading the list. | Wait. |
| `JobStateWaiting` | Parsed; `Totals.Billable` says what a full start costs. | Inspect, then call `StartJob`. |
| `JobStateWaitingAnalyzed` | A free sample finished (`RunSample`); `BounceEstimate` holds the estimate when NeverBounce reports it. | Call `StartJob` for the full run, on the same `JobID`. |
| `JobStateQueued`, `JobStateRunning` | Waiting for capacity, or verifying. | Wait. |
| `JobStateUnderReview` | In NeverBounce's manual review (`AllowManualReview`), up to one business day. | Wait. |
| `JobStateComplete` | Results are ready. | Read them or download them. |
| `JobStateFailed` | The job failed; `FailureReason` says why when NeverBounce knows. | Fix the list and create a new job. |

The deliberate flow creates a job that only parses, inspects the billable count, and starts it
explicitly:

```go
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
```

> **Billing.**
> - `AutoStart` on `CreateJob` or `ParseJob` **starts the job and deducts the credits** as soon as parsing
>   ends, without your review. The SDK always sends it explicitly, `false` included.
> - A full start (`StartJob` with `RunSample: false`) deducts credits for NeverBounce's `billable` count
>   (unique, syntactically valid addresses) and **cannot be stopped or restarted**.
> - A sample (`RunSample: true`) is free and estimates the bounce rate.
> - After any error from `CreateJob`, `StartJob`, or `ParseJob` with `AutoStart`, a timeout included, a job
>   may exist or may already be running: read `JobStatus` (or look for the job's unique `Filename` in the
>   dashboard) before trying again.

<br/>

### Inputs and limits

A list is either **supplied** in the request or **fetched** by NeverBounce:

- `SuppliedList` has `Columns` (1 to 128 non-blank names, unique ignoring ASCII case) and at least one of
  `Rows` (each with exactly one value per column, sent verbatim as JSON strings, up to 4096 bytes each and
  without control characters, so no line breaks or tabs). It maps one to one from a CSV header and its
  records; `EmailList(addresses...)` builds the common one-column case, named `email` as in NeverBounce's
  examples. NeverBounce documents no required column name.
- `RemoteURL` is a list NeverBounce downloads itself (a CSV file or one address per line), over any
  protocol NeverBounce supports, with credentials in the URL if needed. The SDK never fetches it.

The whole encoded `jobs/create` request, the key included, must fit `MaxJobRequestBytes`: NeverBounce's
"25 MB", read as 25,000,000 bytes. A larger list fails locally, before anything is sent; use `RemoteURL`
or smaller chunks. `CallbackURL` and `CallbackHeaders` ask NeverBounce to post job events to your server;
NeverBounce documents no signing for them, and the SDK does not receive them.

A request that fails any check returns a `*neverbounce.ValidationError` matching
`neverbounce.ErrInvalidRequest`, naming the first check that failed by field path, such as
`Supplied.Rows[17][1]` (or `request` when the whole body is too large), and never the value. No call is
made.

<br/>

### Wait for a job

`WaitForJob` polls `jobs/status`, first immediately and then `Interval` (10 seconds by default) after each
poll ends, within the `MaxWait` you must set. It never creates, parses, starts or resubmits anything, and
starts no background goroutine of its own.

| Observed | Returns |
|---|---|
| `complete`, `waiting`, `waiting_analyzed` | `(status, nil)`: switch on `status.State`. |
| `failed` | `(status, err)`, `err` matching `ErrJobFailed`; `status.FailureReason` says why. |
| An undocumented state | `(status, err)` matching `ErrUnknownJobState`: surfaced, not polled through. |
| `uploading`, `parsing`, `queued`, `running`, `under_review` | Keeps polling. |
| `MaxWait` ends: no poll starts at or after it, and one still running then is cut off | `(last, err)` matching `ErrBudgetExhausted`. |
| Your context ends | `(last, err)` matching `context.Canceled` or `context.DeadlineExceeded`. |
| A status call fails | `(last, that error)`, with no retry. |

```go
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
```

It makes at most `ceil(MaxWait/Interval)` calls. Canceling stops the polling, never the job or its
billing. NeverBounce suggests polling every 5 to 10 seconds for lists under 50,000 addresses, and less
often for larger lists.

<br/>

### Read results

`JobResults` reads one page (up to `MaxItemsPerPage`, 1000 rows); `JobResultPages` walks pages with a
budget. Each row's `Data` is your original record as raw JSON, and `Column` reads one value from it:

```go
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
```

A bounded walk, resumed where the page budget ran out:

```go
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
```

- **Correlate by your own column, never by position.** NeverBounce may return a list's header row as a
  record (`{"id":"id","email":"email"}`), may return duplicates, and does not document the order.
- **Values may come back as strings** even when you sent numbers; `Column` returns a number's literal
  text too.
- `Page` is the page the SDK asked for. NeverBounce's own echo of the query (whose documented example says
  page 0) is never read.
- The iterator makes one call per page, only after you consume the previous one, and stops after the last
  page. It refuses a page that contradicts the walk (changed totals, an empty page before the last, rows
  past the end, or more pages than `jobs/results` can number, past 2,147,483,647), and with more pages
  left after `MaxPages` it yields `ErrBudgetExhausted` once, with `CallError.Page` set to the page to resume
  at. A page holding more rows than requested fails as `ErrUnexpectedResponse`.

<br/>

### Download the CSV

`DownloadJob` streams the job's CSV to an `io.Writer` you own and returns the bytes written:

```go
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
```

- **Nothing is written until the response is known to be a file.** An HTTP error, a JSON envelope (such as
  a job that is not complete), a content type other than `application/octet-stream` or `text/csv`, or a
  declared length over the limit writes nothing.
- **The bytes are copied unchanged:** no CSV parsing, no BOM, no line-ending rewrite. NeverBounce's
  default line ending is `"\n\r"`, which `encoding/csv` reads as a stray `\r` at the start of each
  following record: set `LineFeedCRLF` or `LineFeedLF` if you parse the file.
- **The writer is yours:** the SDK never closes, flushes or inspects it. On an error after streaming
  began, it holds a partial file of `n` bytes: discard it. There is no resume.
- `DownloadSegments` selects records (valid, invalid, duplicates…) and `DownloadColumns` appends columns;
  a `nil` switch is not sent, leaving NeverBounce's default.

<br/>

### Errors

Every error falls into exactly one row of the [error reference](#error-reference). Match the classes with
`errors.Is`, and use `errors.AsType[*neverbounce.CallError]` for the details of a call that passed
validation:

```go
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
```

`*neverbounce.CallError` carries `Kind`, `Op`, `StatusCode` (HTTP), `APIStatus` (NeverBounce's envelope
status), `JobID`, `Page`, `Field` (a JSON path for shape failures) and `BytesWritten` (downloads). Its
`Error()` holds short codes only, never values or messages, such as `neverbounce: jobs/status: rate_limited (HTTP 200,
throttle_triggered)`. NeverBounce reports most failures inside an HTTP 200 envelope; any other HTTP status
decides the kind on its own.

| How to tell | |
|---|---|
| Nothing was sent | a `*ValidationError`. A `canceled`/`deadline_exceeded` error with `StatusCode` 0 means no response arrived: the request may still have been sent and billed |
| NeverBounce refused | `unauthorized`, `rate_limited` or `rejected`, with `StatusCode` and `APIStatus` |
| NeverBounce had a server error | `unavailable` or `upstream`: the request may still have been processed |
| A valid "unknown" verdict | `err == nil && res.Result == neverbounce.ResultUnknown` |
| The answer could not be read | `malformed_response`, `unexpected_response` or `response_too_large`: the call may have been billed, or a job created or started |
| Canceled or timed out | `context.Canceled`, `ErrTimeout`, `context.DeadlineExceeded` (check `ErrTimeout` first) |
| Bulk work stopped short | `ErrJobFailed`, `ErrUnknownJobState` or `ErrBudgetExhausted`, with the last status or the page to resume at |

<br/>

### Timeouts, billing and repeat calls

- The SDK never retries, hedges, falls back, splits a list, polls or paginates on its own, and never
  follows a redirect. It sends each request without a rewindable body, so Go's HTTP/1.1 and HTTP/2
  clients cannot resend it either.
- **A canceled or timed-out call may still be processed and billed.** Cancellation stops the SDK from
  waiting, not NeverBounce from working.
- `CheckEmailRequest.Timeout` is NeverBounce's verification timeout, not the client's: the whole request
  can take longer, so the SDK requires it to stay 10 seconds below `WithTimeout`. NeverBounce's guide
  suggests treating a client-side timeout as `unknown`.
- NeverBounce documents no idempotency mechanism, no rate-limit headers and no numeric rate limit; the
  SDK sends no idempotency key.

<br/>

### In a net/http handler

```go
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
```

<br/>

### Testing your application

Depend on a small interface of your own and fake it:

```go
type EmailChecker interface {
	CheckEmail(ctx context.Context, req neverbounce.CheckEmailRequest) (*neverbounce.CheckEmailResult, error)
}
var _ EmailChecker = (*neverbounce.Client)(nil)
```

```go
// fakeChecker is an application's test fake: it returns a canned answer.
type fakeChecker struct {
	res *neverbounce.CheckEmailResult
	err error
}

// CheckEmail returns the canned answer.
func (f fakeChecker) CheckEmail(context.Context, neverbounce.CheckEmailRequest) (*neverbounce.CheckEmailResult, error) {
	return f.res, f.err
}
```

A fake can return a hand-built `*neverbounce.CallError`; it matches `errors.Is` exactly like one the SDK
returns. To exercise the real SDK offline, serve synthetic responses from `httptest.NewTLSServer` and pass
`neverbounce.WithHTTPClient(srv.Client())` and `neverbounce.WithTestBaseURL(srv.URL)`, as the
[examples](examples) do.

<br/>

### Storing results

Every result type round-trips through `encoding/json` unchanged (`reflect.DeepEqual`), under
NeverBounce's snake_case names, keeping absent and zero counts apart. Unmarshaling a raw NeverBounce body
into these types skips every check the SDK makes, so store what the SDK returned:

```go
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
```

<br/>

## ⚙️ Behavior

### The transport

Without `WithHTTPClient`, each `Client` builds its own `*http.Transport` with the settings of
`http.DefaultTransport` (HTTP/2, and the standard dial, TLS and idle timeouts), plus a 64 KiB limit on
response headers. Later changes to `http.DefaultTransport` do not reach it. Like the default transport, it
takes a proxy from the `HTTPS_PROXY` and `NO_PROXY` environment variables: that is the standard library
reading the environment, not the SDK. To bypass proxies, inject a client whose transport sets no `Proxy`.

With `WithHTTPClient(hc)`, the SDK uses a copy of `hc` taken at `NewClient`, with redirects refused, every
3xx `Location` header removed, and no cookie jar. `hc` itself is never modified, its `Timeout` still
applies (to downloads too), and its `Transport` is shared. **An injected transport is trusted code:** it
sees the API key in every request body, and one that retries or replays requests on its own defeats the
one-attempt guarantee.

Every request is a `POST` to `https://api.neverbounce.com/v4.2/<operation>` with a JSON body whose first
member is `key`, `Content-Type: application/json`, `Accept: application/json` (`*/*` for downloads) and
`User-Agent: go-neverbounce`, and never an `Authorization`, cookie or idempotency header. Go's transport
may request gzip on its own and decompresses it; the limits apply to the decompressed bytes. A response
body is always closed. A JSON body is never read more than one byte past its limit; a download may read
up to 64 KiB ahead of what it writes.

<br/>

### Limits

| Bound | Value |
|---|---|
| API key | 1 to 256 bytes of visible ASCII |
| `jobs/create` request, key included | 25,000,000 bytes (`MaxJobRequestBytes`) |
| Any other request | 64 KiB |
| Supplied list | 1 to 128 columns of up to 256 bytes; values up to 4096 bytes |
| `RemoteURL`, `CallbackURL`, `Filename` | 8192, 2048 and 255 bytes |
| Callback headers | 32; names up to 128 bytes, values up to 1024 |
| JSON response | 8 MiB by default, up to 64 MiB; 1 MiB for every operation except `jobs/results` |
| Error response body read | 64 KiB, best-effort |
| Response headers (SDK-built transport) | 64 KiB |
| JSON objects and arrays / values in a response | 100,000 / 1,000,000 |
| One text field / flags per verdict / one row's `data` | 4 KiB / 64 / 64 KiB |
| Rows per results page | `ItemsPerPage`, or 1000 when not sent |
| Download | 1 GiB by default, up to 64 GiB; 30 minutes by default, up to 12 hours |
| `WaitForJob` | `Interval` 1 second to 10 minutes; `MaxWait` up to 72 hours |
| `JobResultPages` | `MaxPages` 1 to 100,000; 100 rows per page by default |

A field over its bound fails the call as `ErrResponseTooLarge`: the SDK never truncates data it returns.
A result, flag or state token over 64 bytes fails as `ErrUnexpectedResponse`. Metadata never fails a call:
an `execution_time`, `queue_id` (over 128 bytes) or `retry_token` (over 1 KiB) that does not fit is dropped.

<br/>

### Error reference

<details>
<summary><strong><code>Every error, and what it matches</code></strong></summary>
<br/>

| What happened | `Kind` | `errors.Is` matches |
|---|---|---|
| Invalid key or option, or a nil or zero client | (`*ValidationError`) | `ErrInvalidConfig` |
| Nil context, or a request failing validation | (`*ValidationError`) | `ErrInvalidRequest` |
| Context already done before the call (no attempt) | `canceled` / `deadline_exceeded` | `context.Canceled` / `context.DeadlineExceeded` |
| Your context canceled or expired during the call | `canceled` / `deadline_exceeded` | `context.Canceled` / `context.DeadlineExceeded` |
| `WithTimeout`, `WithDownloadTimeout`, the `http.Client` timeout, or a network timeout | `timeout` | `ErrTimeout` and `context.DeadlineExceeded` |
| DNS, connection, TLS or proxy failure, reset, truncated or stalled body | `transport` | `ErrTransport` |
| Envelope `auth_failure` or `bad_referrer`; HTTP 401 or 403 | `unauthorized` | `ErrUnauthorized` |
| Envelope `throttle_triggered`; HTTP 429 | `rate_limited` | `ErrRateLimited` |
| Envelope `temp_unavail`; HTTP 503 | `unavailable` | `ErrUnavailable` |
| Envelope `general_failure`; any other HTTP 4xx (413 included) | `rejected` | `ErrRejected` |
| Any other HTTP 5xx | `upstream` | `ErrUpstream` |
| HTTP 1xx, 3xx (never followed), a 2xx other than 200, or 600+; an undocumented envelope status; a body that is not a JSON object; an invalid token; another job's status; a results page with more rows than requested, or that contradicts the walk; an undecoded `Content-Encoding`; a download that is not CSV, or a success envelope instead of a file | `unexpected_response` | `ErrUnexpectedResponse` |
| Invalid UTF-8 or JSON, trailing data, a repeated key, a missing required field, a wrong type or an out-of-range number | `malformed_response` | `ErrMalformedResponse` |
| A body or download over its limit, a response over the JSON bounds, or a field over its bound | `response_too_large` | `ErrResponseTooLarge` |
| `DownloadJob`'s writer failed or wrote short | `write_failed` | `ErrWriteFailed` |
| `WaitForJob` saw `failed` | `job_failed` | `ErrJobFailed` |
| `WaitForJob` saw an undocumented state | `unknown_job_state` | `ErrUnknownJobState` |
| `WaitForJob`'s `MaxWait` or `JobResultPages`' `MaxPages` ran out | `budget_exhausted` | `ErrBudgetExhausted` |

The body of a non-200 response is read best-effort, up to 64 KiB, only for `APIStatus` and the opt-in
message: the status alone classifies it. `Unwrap` returns only a context error or one of the package's
fixed causes: never the underlying transport error, a URL, a decoding error or your writer's error.

</details>

<br/>

### Privacy and contact data

- Error strings, `GoString`, every `fmt` verb on a `Client` or an error, and every error in an `Unwrap`
  chain never contain the API key, email addresses, row data, remote or callback URLs, callback header
  values, NeverBounce's messages or URL query data. The key and base URL live only inside function values the
  `Client` holds, which `fmt` and reflection-based dumpers cannot see into.
- NeverBounce's message is kept only with `WithProviderDiagnostics`, cut to 1 KiB, with the key replaced
  by `[redacted]`; it may still echo the address you checked.
- `CreateJobRequest`'s `String`, `GoString` and `Format` print only its shape (counts, set or unset) for
  every `fmt` verb, because `RemoteURL`, `CallbackURL` and `CallbackHeaders` may carry third-party
  credentials. `fmt` cannot call them for `%p` on a value or for a request held in an unexported struct
  field, and reflection-based dumpers see every field: keep requests out of such output.
- The SDK has no logger, telemetry, cache or persistence.
- **Results are contact data.** Access control, logging, retention and lawful use are your
  responsibility, and hashing an address does not anonymize it.

<br/>

### NeverBounce's usage guidelines

The SDK documents these and enforces none:

- Do not verify an existing list or database one address at a time with `CheckEmail`: use a job.
- At most 10 concurrent jobs and 50 runs per day per account.
- No more than 10 jobs for every 100,000 items per hour; split millions of addresses into chunks of 1
  million.
- Supplied data up to 25 MB; otherwise NeverBounce answers HTTP 413 (the SDK refuses it locally first).

<br/>

### Where NeverBounce's documentation is ambiguous

<details>
<summary><strong><code>What NeverBounce's v4.2 documentation leaves open, and what the SDK does</code></strong></summary>
<br/>

| Question | SDK behavior |
|---|---|
| Single-check options are typed as integers in one schema and as booleans elsewhere. | JSON `true`/`false` everywhere, as the encoding rules allow. |
| `leverage_historical_data` is missing from the v4.2 single-check schema. | Sent only when you set it (`*bool`). |
| Percentages are typed as integers but described as floats. | Integers or floats, from 0 to 100. |
| Errors come in an HTTP 200 envelope, yet schemas show a 400 `{}`. | The envelope decides on 200; any other status decides alone. |
| `input` and `callback_headers` are typed as JSON strings. | An array of objects and an object of strings, as the prose and examples show. |
| The results `query` echo says page 0 for page 1. | Never decoded; `Page` is what the SDK requested. |
| `data.id` goes in as a number and comes back as a string; a header row comes back as a record. | `Data` stays raw JSON; nothing is dropped, deduplicated or correlated by position. |
| A guide shows an older, flat status shape. | Only the nested v4.2 shape is read. |
| `retry_token` appears only in a guide example. | Kept as opaque text, never sent back. |
| One page shows a header row in downloads, another does not. | Bytes are streamed unchanged; no claim either way. |
| Units of `execution_time`; time zone of timestamps; their value before a job starts. | Kept as sent: a raw integer, and text never parsed. |
| The single-check `timeout` default and range. | Not sent unless set; the SDK allows 1 to 300 seconds. |
| Result order, one row per input, duplicates. | NeverBounce's order is kept; nothing is assumed. |
| The error format of an early download. | A JSON envelope is recognized; any other non-CSV answer is refused; nothing is written. |
| Rate-limit headers, a numeric rate limit, idempotency, a sandbox, callback signing, ordering and delivery. | None assumed; nothing is retried or replayed. |

</details>

<br/>

### What is not covered

`account/info`, `jobs/search`, `jobs/delete` (permanent, so never used for cleanup), `poe/confirm`, and
receiving job callbacks. Older API versions (v4.0, v4.1) are never used. Each missing operation would be
added as one more method on `Client`; see [adding an operation](docs/adding-an-operation.md).

<br/>

## 📚 Documentation

View the generated [documentation](https://pkg.go.dev/github.com/mrz1836/go-neverbounce?tab=doc) and
[NeverBounce's API documentation](https://developers.neverbounce.com).

> **Heads up!** `go-neverbounce` is intentionally light on dependencies. The only external package it
uses is the excellent `testify` suite—and that's just for our tests. You can drop this library into
your projects without dragging along extra baggage.

<br/>

<details>
<summary><strong><code>Development Setup (Getting Started)</code></strong></summary>
<br/>

Install [MAGE-X](https://github.com/mrz1836/mage-x) build tool for development:

```bash
# Install MAGE-X for development and building
go install github.com/mrz1836/mage-x/cmd/magex@latest
magex update:install
```
</details>

<details>
<summary><strong><code>Library Deployment</code></strong></summary>
<br/>

This project uses [goreleaser](https://github.com/goreleaser/goreleaser) for streamlined library releases to GitHub. To get started, install it via:

```bash
brew install goreleaser
```

The release process is defined in the [.goreleaser.yml](.goreleaser.yml) configuration file.

Then create and push a new Git tag using:

```bash
magex version:bump bump=patch push=true branch=master
```

</details>

<details>
<summary><strong><code>Build Commands</code></strong></summary>
<br/>

View all build commands

```bash script
magex help
```

</details>

<details>
<summary><strong><code>GitHub Workflows</code></strong></summary>
<br/>

All workflows are driven by modular configuration in [`.github/env/`](.github/env/README.md) — no YAML editing required.

**[View all workflows and the control center →](.github/docs/workflows.md)**

</details>

<details>
<summary><strong><code>Updating Dependencies</code></strong></summary>
<br/>

To update all dependencies (Go modules, linters, and related tools), run:

```bash
magex deps:update
```

</details>

<br/>

## 🧪 Examples & Tests

All unit tests, fuzz tests and examples run via [GitHub Actions](https://github.com/mrz1836/go-neverbounce/actions) on Go 1.26.x, the supported baseline. View the [configuration file](.github/workflows/fortress.yml).

Run all tests (fast):

```bash script
magex test
```

Run all tests with race detector (slower):
```bash script
magex test:race
```

Runnable examples live in the package's `Example*` functions and in [examples/](examples), which plays a
synthetic NeverBounce from a local TLS server:

```bash
go run ./examples # offline: no key, no network, no cost
```

> **Note:** tests never touch the network. They use in-memory transports and local TLS servers over
HTTP/1.1 and HTTP/2, with synthetic data only.

<br/>

### Live tests

`live_probe_internal_test.go` (build tag `live`) runs the SDK against the real NeverBounce API with your
own key and logs the shape of each answer, never its values. It is opt-in, so it never runs by default
or in CI: it skips unless `NEVERBOUNCE_LIVE=1`, `CI` is unset and `NEVERBOUNCE_LIVE_KEY` is set.

```bash
NEVERBOUNCE_LIVE=1 NEVERBOUNCE_LIVE_KEY=secret_... go test -tags live -count=1 -p 1 -run 'TestLiveProbeTier0$' -v .
```

The free tier (`TestLiveProbeTier0`) reads an `auth_failure` and a missing job, and bills nothing. The paid
tier (`TestLiveProbeTier1`) also needs `NEVERBOUNCE_LIVE_ACK_BILLING=yes-i-accept-charges` and an address
you own in `NEVERBOUNCE_LIVE_EMAIL`: it **spends one credit** and **creates a sample job that stays in your
account**.

<br/>

## ⚡ Benchmarks

Run the Go benchmarks:

```bash script
magex bench
```

<br/>

### 📊 Performance Results

| Benchmark | Iterations | ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| [DecodeCheckEmail](benchmark_internal_test.go) | 3,251,857 | 3,696 | 1,440 | 30 |
| [CheckEmailEndToEnd](benchmark_test.go) | 1,989,206 | 6,032 | 5,258 | 68 |
| [DecodeResultsPage1000](benchmark_internal_test.go) | 3,625 | 3,313,265 | 572,156 | 16,016 |
| [EncodeCreateJob10k](benchmark_internal_test.go) | 14,036 | 853,493 | 466,992 | 3 |
| [CheckStrict8MiB](benchmark_internal_test.go) | 535 | 22,439,044 | 824 | 7 |
| [DownloadCopy](benchmark_test.go) | 5,053 | 2,380,973 | 110,968 | 42 |

> DecodeCheckEmail decodes a full single-check answer; CheckEmailEndToEnd makes a whole `CheckEmail` call
> against an in-memory transport; DecodeResultsPage1000 decodes a 1000-row results page; EncodeCreateJob10k
> validates and encodes a 10,000-row list; CheckStrict8MiB runs the strict JSON checks over an 8 MiB body;
> DownloadCopy streams 64 MiB to `io.Discard` (about 28 GB/s). They measure local CPU work only, executed
> with `magex bench` on an Apple M1 Max (ARM64) with Go 1.27.2, and make no claim about network or
> NeverBounce latency, which dominates real calls.

<br/>

## 🛠️ Code Standards
Read more about this Go project's [code standards](.github/CODE_STANDARDS.md).

<br/>

## 🤖 AI Usage & Assistant Guidelines
Read the [AI Usage & Assistant Guidelines](.github/tech-conventions/ai-compliance.md) for details on how AI is used in this project and how to interact with the AI assistants.

<br/>

## 👥 Maintainers
| [<img src="https://github.com/mrz1836.png" height="50" alt="MrZ" />](https://github.com/mrz1836) |
|:------------------------------------------------------------------------------------------------:|
|                                [MrZ](https://github.com/mrz1836)                                 |

<br/>

## 🤝 Contributing
View the [contributing guidelines](.github/CONTRIBUTING.md) and please follow the [code of conduct](.github/CODE_OF_CONDUCT.md).

### How can I help?
All kinds of contributions are welcome :raised_hands:!
The most basic way to show your support is to star :star2: the project, or to raise issues :speech_balloon:.
You can also support this project by [becoming a sponsor on GitHub](https://github.com/sponsors/mrz1836) :clap:
or by making a [**bitcoin donation**](https://mrz1818.com/?tab=tips&utm_source=github&utm_medium=sponsor-link&utm_campaign=go-neverbounce&utm_term=go-neverbounce&utm_content=go-neverbounce) to ensure this journey continues indefinitely! :rocket:


[![Stars](https://img.shields.io/github/stars/mrz1836/go-neverbounce?label=Please%20like%20us&style=social)](https://github.com/mrz1836/go-neverbounce/stargazers)

<br/>

## 📝 License

[![License](https://img.shields.io/github/license/mrz1836/go-neverbounce.svg?style=flat&v=1)](LICENSE)
