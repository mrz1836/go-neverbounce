# Adding an operation

go-neverbounce implements NeverBounce API v4.2 single verification (`single/check`) and the bulk-jobs
workflow (`jobs/create`, `jobs/parse`, `jobs/start`, `jobs/status`, `jobs/results`,
`jobs/download`). This note describes how a future operation, such as `account/info` or
`jobs/search`, would be added without breaking existing callers. Each one needs the owner's approval
first: it is a new surface with its own data, costs and contract evidence. `jobs/delete` is
permanent and must never be used for automatic cleanup.

## The pattern

An operation is **one method on `*Client`** with **its own request and result types**, named after
the method. There is no registry, generic executor, service object or subpackage.

```go
// AccountInfo would read the account's credits and job counts: POST /v4.2/account/info.
func (c *Client) AccountInfo(ctx context.Context, req AccountInfoRequest) (*AccountInfoResult, error)
```

Even an operation without parameters takes a request struct, so fields can be added later without
breaking callers. The method follows the same steps as `JobStatus` in `job_status.go`:

1. `c.prepare(ctx)`: `ErrInvalidConfig` (`Client`, `not_initialized`) for a nil or zero Client, and
   `ErrInvalidRequest` (`ctx`, `nil`) for a nil context.
2. Validate the request with the operation's own rules, returning a `*ValidationError` that names Go
   fields and fixed reasons, never values.
3. Encode the body with `c.newRequestBody` and the `body` helpers in `validate.go`. The body starts
   with space reserved for the `key` member, which only the sender writes, so encoders never see the
   API key. Send every boolean explicitly; send optional values only when set. Close it with
   `finish`, which checks the exact size.
4. Call `c.call(ctx, Op…, body, envelopeNames(…))`. It returns a `canceled` or `deadline_exceeded`
   `*CallError` without sending when the context is already done, and otherwise makes one attempt:
   no retries, no redirects, the call timeout over the whole body read, the response limit, strict
   JSON, the envelope status, and sanitized errors all come with it.
5. Decode the operation's members from the returned object with the helpers in `decode.go`, and set
   `JobID` (and `Page`) on every `*CallError` the method returns with `forJob`.

Two limits apply to a new operation's answer: `readObject` reads at most `maxFields` (13) names, 3 of
which are the envelope's, so raise `maxFields` for an answer with more than 10 members; and every
operation except `jobs/results` is capped at 1 MiB (`Client.responseLimit`).

A new `Operation` constant names the path. Add a new `FailureKind` only when no existing kind fits,
and give it a sentinel in `errors.go`.

## Rules for the new types

- **Request.** A plain struct passed by value, read only while it is encoded. Values are sent
  verbatim after validation; limits the provider does not document are labelled as SDK limits.
- **Result.** Required members are checked and fail as `malformed_response` when missing. Optional
  numbers and booleans are pointers, so absent stays apart from zero. Provider text (timestamps,
  IDs) is never parsed. Open vocabularies are string types with `Known()`. Metadata such as
  `execution_time` never fails a call. JSON tags use the provider's snake_case names and round-trip
  through `encoding/json` unchanged.
- **Fixtures.** Write fixtures from scratch to the documented shape with synthetic values, under
  `testdata/synthetic/` with the operation's prefix; never copy the provider's examples.
- **Tests.** The same evidence as the existing operations: the exact wire body, the HTTP status and
  envelope tables, zero calls on invalid input or a done context, exactly one attempt on every
  failure, privacy markers across fmt verbs and error chains, and fuzzing of the decoder.

## What must not change

- No general base URL option: the API key reaches only the provider or loopback, in the body only.
- No retries, polling, pagination, fallbacks or follow-on calls inside a method; helpers that loop
  (like `WaitForJob`) take an explicit, required budget and only read.
- `Error()` strings stay free of values, URLs, bodies and provider text.
- The library stays standard-library only, starts no background goroutine of its own, and never logs,
  prints, stores or caches data, or reads credentials or configuration from the environment.
