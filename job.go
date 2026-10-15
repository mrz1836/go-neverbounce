package neverbounce

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// jobs/create bounds.
const (
	maxColumns          = 128
	maxColumnBytes      = 256
	maxValueBytes       = 4 << 10
	maxRemoteURLBytes   = 8 << 10
	maxFilenameBytes    = 255
	maxCallbackURLBytes = 2 << 10
	maxCallbackHeaders  = 32
	maxHeaderNameBytes  = 128
	maxHeaderValueBytes = 1 << 10
)

// JobID is the provider's integer handle for a bulk job. Valid IDs are greater
// than 0. It marshals as a JSON number.
type JobID int64

// String returns the decimal form, such as "289022".
func (id JobID) String() string { return strconv.FormatInt(int64(id), 10) }

// SuppliedList is a list sent in the jobs/create request body
// (input_location "supplied") as a JSON array of objects: each row becomes one
// object whose keys are Columns, in order, which the provider uses as the
// list's column headers. It maps one to one from a CSV header row and its
// records.
//
// The SDK requires no particular column: the provider documents none, and
// its examples name the address column "email". Headerless (array) rows are
// not offered.
type SuppliedList struct {
	// Columns are the column names: 1 to 128 of them, each 1 to 256 bytes of
	// valid UTF-8 without control characters, unique ignoring ASCII case.
	Columns []string

	// Rows are the records: at least one, each with exactly len(Columns)
	// values in Columns order. Each value is sent verbatim as a JSON string:
	// at most 4096 bytes of valid UTF-8 without control characters (so no
	// embedded line breaks); empty values are allowed.
	Rows [][]string
}

// EmailList returns a SuppliedList with the single column "email" (the name
// the provider's examples use) and one row per address, in order. It copies
// emails, so later changes to the slice do not affect the list.
func EmailList(emails ...string) SuppliedList {
	rows := make([][]string, len(emails))
	for i, e := range emails {
		rows[i] = []string{e}
	}
	return SuppliedList{Columns: []string{"email"}, Rows: rows}
}

// set reports whether either part of the list is filled in.
func (l *SuppliedList) set() bool { return len(l.Columns) > 0 || len(l.Rows) > 0 }

// CreateJobRequest is one jobs/create call. Set exactly one of Supplied and
// RemoteURL. Every boolean is always sent, false included, so a provider
// default can never start a billed job unseen. The request is read only while
// CreateJob encodes it, before any I/O, and is never kept or modified;
// changing its slices or maps while the call runs is a data race.
type CreateJobRequest struct {
	// Supplied is a list sent in the request body. The whole encoded body,
	// the API key included, must fit MaxJobRequestBytes.
	Supplied SuppliedList

	// RemoteURL is a list the provider fetches itself (input_location
	// "remote_url"): a line-separated list of addresses or a CSV file. It must
	// be an absolute URL of at most 8192 bytes; any scheme the provider
	// supports, and credentials in the URL, are allowed. The SDK never fetches
	// it and never prints it.
	RemoteURL string

	// Filename is the job's name in the NeverBounce dashboard: at most 255
	// bytes, sent when non-empty. A unique value is the way to find a job
	// whose creation timed out, because the SDK cannot learn its ID then.
	Filename string

	// AutoParse starts parsing the list right after creation. When false, call
	// ParseJob.
	AutoParse bool

	// AutoStart starts the full verification right after parsing, which
	// DEDUCTS CREDITS for the provider's billable count without any review
	// on your side. Leave it false to inspect JobStatus Totals.Billable and
	// call StartJob deliberately.
	AutoStart bool

	// RunSample runs a free sample verification that estimates the list's
	// bounce rate instead of verifying it (the job then reaches
	// JobStateWaitingAnalyzed).
	RunSample bool

	// AllowManualReview lets the provider hold the job for manual review,
	// which may take up to one business day (JobStateUnderReview).
	AllowManualReview bool

	// LeverageHistoricalData, when non-nil, is sent as
	// request_meta_data.leverage_historical_data; false asks for real-time
	// verification only. nil is not sent (the provider's default is true).
	LeverageHistoricalData *bool

	// CallbackURL is an optional http or https URL of at most 2048 bytes that
	// the provider sends job events to. It may carry basic-auth credentials;
	// the SDK never prints it. The provider documents no signing of these
	// events.
	CallbackURL string

	// CallbackHeaders are optional headers the provider sends with each event,
	// such as an authorization token: at most 32, names RFC 9110 tokens of up
	// to 128 bytes, unique ignoring case, values up to 1024 bytes without
	// control characters. They require CallbackURL. Errors and String never
	// include a header name or value.
	CallbackHeaders map[string]string
}

// String describes the request's shape only, such as
// "neverbounce.CreateJobRequest{input: supplied, columns: 2, rows: 3, ...}":
// counts and set or unset, never rows, URLs, the filename or header values,
// which may hold contact data or third-party credentials.
func (r CreateJobRequest) String() string {
	var b strings.Builder
	b.WriteString("neverbounce.CreateJobRequest{input: ")
	switch supplied, remote := r.Supplied.set(), r.RemoteURL != ""; {
	case supplied && remote:
		b.WriteString("supplied+remote_url")
	case supplied:
		b.WriteString("supplied")
	case remote:
		b.WriteString("remote_url")
	default:
		b.WriteString("none")
	}
	writeField := func(name, value string) {
		b.WriteString(", ")
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(value)
	}
	writeField("columns", strconv.Itoa(len(r.Supplied.Columns)))
	writeField("rows", strconv.Itoa(len(r.Supplied.Rows)))
	writeField("filename", setOrUnset(r.Filename != ""))
	writeField("autoParse", strconv.FormatBool(r.AutoParse))
	writeField("autoStart", strconv.FormatBool(r.AutoStart))
	writeField("runSample", strconv.FormatBool(r.RunSample))
	writeField("allowManualReview", strconv.FormatBool(r.AllowManualReview))
	writeField("leverageHistoricalData", setOrUnset(r.LeverageHistoricalData != nil))
	writeField("callbackURL", setOrUnset(r.CallbackURL != ""))
	writeField("callbackHeaders", strconv.Itoa(len(r.CallbackHeaders)))
	b.WriteString("}")
	return b.String()
}

// GoString returns the same text as String, so that %#v prints the shape only.
func (r CreateJobRequest) GoString() string { return r.String() }

// Format prints the same text as String for every fmt verb (quoted for %q),
// so that no verb, %d and %t included, reaches the rows, URLs, filename or
// callback headers by reflection. fmt cannot call it for %p on a value, nor
// for a request held in an unexported struct field: keep requests out of
// formatted output there.
func (r CreateJobRequest) Format(f fmt.State, verb rune) {
	s := r.String()
	if verb == 'q' {
		s = strconv.Quote(s)
	}
	_, _ = io.WriteString(f, s)
}

// setOrUnset names whether a field is filled in.
func setOrUnset(set bool) string {
	if set {
		return "set"
	}
	return "unset"
}

// CreateJobResult is the provider's answer to jobs/create.
type CreateJobResult struct {
	// JobID is the new job's handle.
	JobID JobID `json:"job_id"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// Members of a jobs/create answer after the envelope.
const createJobID = envFirst

// CreateJob creates a bulk verification job: POST
// https://api.neverbounce.com/v4.2/jobs/create.
//
// STATE-CHANGING, and PAID when AutoStart is true: the job is durable, and
// with AutoStart the provider deducts credits for the list's billable count
// as soon as parsing ends. Without AutoStart nothing is billed until StartJob,
// or ParseJob with AutoStart.
// It makes exactly one attempt and never retries. After a timeout,
// cancellation, transport failure or unreadable response a job may exist
// anyway, possibly started: do not resubmit blindly; look for its Filename in
// the dashboard first.
//
// The provider's usage guidelines allow 10 concurrent jobs and 50 runs a day,
// no more than 10 jobs per 100,000 items per hour, and suggest chunks of at
// most 1 million addresses. The SDK documents these and enforces none.
//
// Errors are a *ValidationError (nothing sent) or a *CallError.
func (c *Client) CreateJob(ctx context.Context, req CreateJobRequest) (*CreateJobResult, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, err
	}
	b, err := req.encode(c)
	if err != nil {
		return nil, err
	}
	obj, cerr := c.call(ctx, OpCreateJob, b, envelopeNames("job_id"))
	if cerr != nil {
		return nil, cerr
	}
	id, derr := decodeJobID(obj.fields[createJobID], "job_id")
	if derr != nil {
		return nil, derr.callError(OpCreateJob)
	}
	return &CreateJobResult{JobID: id, ExecutionTime: executionTime(obj.fields[envExecutionTime])}, nil
}

// encode validates the request and returns its JSON body, with the key member
// first and every boolean explicit. A supplied list is validated and encoded
// row by row, so a list over MaxJobRequestBytes stops as soon as it is known
// to be too large.
func (r CreateJobRequest) encode(c *Client) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	var b body
	if r.RemoteURL != "" {
		b = c.newRequestBody(len(r.RemoteURL) + 512)
		b = b.str("input_location", "remote_url").str("input", r.RemoteURL)
	} else {
		var err error
		if b, err = r.Supplied.encode(c); err != nil {
			return nil, err
		}
	}
	if r.Filename != "" {
		b = b.str("filename", r.Filename)
	}
	b = b.boolean("auto_parse", r.AutoParse).boolean("auto_start", r.AutoStart).
		boolean("run_sample", r.RunSample).boolean("allow_manual_review", r.AllowManualReview)
	b = appendRequestMetaData(b, r.LeverageHistoricalData)
	if r.CallbackURL != "" {
		b = b.str("callback_url", r.CallbackURL)
	}
	if len(r.CallbackHeaders) > 0 {
		b = append(b.name("callback_headers"), '{')
		for i, name := range slices.Sorted(maps.Keys(r.CallbackHeaders)) {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendString(b, name)
			b = append(b, ':')
			b = appendString(b, r.CallbackHeaders[name])
		}
		b = append(b, '}')
	}
	return b.finish(MaxJobRequestBytes)
}

// validate checks every field except the rows, which encode checks as it
// goes.
func (r CreateJobRequest) validate() error {
	supplied := r.Supplied.set()
	switch {
	case supplied && r.RemoteURL != "":
		return invalidRequest("RemoteURL", reasonConflict)
	case supplied:
		if err := r.Supplied.validateColumns(); err != nil {
			return err
		}
	case r.RemoteURL != "":
		if err := checkRemoteURL(r.RemoteURL); err != nil {
			return err
		}
	default:
		return invalidRequest("Supplied", reasonRequired)
	}
	if err := checkText("Filename", r.Filename, maxFilenameBytes, false); err != nil {
		return err
	}
	if r.CallbackURL != "" {
		if err := checkCallbackURL(r.CallbackURL); err != nil {
			return err
		}
	}
	return checkCallbackHeaders(r.CallbackHeaders, r.CallbackURL != "")
}

// validateColumns checks the column names.
func (l *SuppliedList) validateColumns() error {
	switch n := len(l.Columns); {
	case n == 0:
		return invalidRequest("Supplied.Columns", reasonRequired)
	case n > maxColumns:
		return invalidRequest("Supplied.Columns", reasonTooMany)
	}
	for i, name := range l.Columns {
		field := "Supplied.Columns[" + strconv.Itoa(i) + "]"
		if err := checkText(field, name, maxColumnBytes, true); err != nil {
			return err
		}
		for _, earlier := range l.Columns[:i] {
			if asciiLower(earlier) == asciiLower(name) {
				return invalidRequest(field, reasonDuplicate)
			}
		}
	}
	if len(l.Rows) == 0 {
		return invalidRequest("Supplied.Rows", reasonRequired)
	}
	return nil
}

// encode writes input_location and input for the list, validating each row
// and stopping as soon as the body exceeds MaxJobRequestBytes.
func (l *SuppliedList) encode(c *Client) (body, error) {
	b := c.newRequestBody(l.sizeHint())
	b = b.str("input_location", "supplied")
	b = append(b.name("input"), '[')
	for i, row := range l.Rows {
		if len(row) != len(l.Columns) {
			return nil, invalidRequest("Supplied.Rows["+strconv.Itoa(i)+"]", reasonLengthMismatch)
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '{')
		for j, value := range row {
			if reason := valueProblem(value, maxValueBytes); reason != "" {
				return nil, invalidRequest(cellField(i, j), reason)
			}
			if j > 0 {
				b = append(b, ',')
			}
			b = appendString(b, l.Columns[j])
			b = append(b, ':')
			b = appendString(b, value)
		}
		b = append(b, '}')
		if len(b) > MaxJobRequestBytes {
			return nil, invalidRequest("request", reasonTooLarge)
		}
	}
	return append(b, ']'), nil
}

// sizeHint estimates the encoded size of the list, capped just past the
// request limit, so that encoding rarely reallocates.
func (l *SuppliedList) sizeHint() int {
	n := 512
	for _, row := range l.Rows {
		n += 2
		for j, value := range row {
			if j < len(l.Columns) {
				n += len(l.Columns[j])
			}
			n += len(value) + 6
		}
		if n > MaxJobRequestBytes {
			return MaxJobRequestBytes + 1
		}
	}
	return n
}

// cellField names the value at row i, column j.
func cellField(i, j int) string {
	return "Supplied.Rows[" + strconv.Itoa(i) + "][" + strconv.Itoa(j) + "]"
}

// checkRemoteURL validates RemoteURL: an absolute URL with a scheme and a
// host. Credentials in it are allowed. The error never quotes it.
func checkRemoteURL(raw string) error {
	const field = "RemoteURL"
	if err := checkValue(field, raw, maxRemoteURLBytes); err != nil {
		return err
	}
	if _, ok := absoluteURL(raw); !ok {
		return invalidRequest(field, reasonInvalidURL)
	}
	return nil
}

// checkCallbackURL validates CallbackURL: an absolute http or https URL with a
// host. Credentials in it are allowed. The error never quotes it.
func checkCallbackURL(raw string) error {
	const field = "CallbackURL"
	if err := checkValue(field, raw, maxCallbackURLBytes); err != nil {
		return err
	}
	scheme, ok := absoluteURL(raw)
	switch {
	case !ok:
		return invalidRequest(field, reasonInvalidURL)
	case scheme != "http" && scheme != "https":
		return invalidRequest(field, reasonUnsupportedScheme)
	}
	return nil
}

// absoluteURL reports whether raw parses as an absolute URL with a scheme and
// a host and holds no whitespace, and returns its lowercased scheme.
func absoluteURL(raw string) (string, bool) {
	if strings.ContainsFunc(raw, unicode.IsSpace) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Opaque != "" || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme), true
}

// checkCallbackHeaders validates the callback headers. Errors name the field
// only, never a header name or value.
func checkCallbackHeaders(h map[string]string, haveURL bool) error {
	const field = "CallbackHeaders"
	switch {
	case len(h) == 0:
		return nil
	case !haveURL:
		return invalidRequest(field, reasonRequiresCallbackURL)
	case len(h) > maxCallbackHeaders:
		return invalidRequest(field, reasonTooMany)
	}
	names := slices.Sorted(maps.Keys(h))
	for i, name := range names {
		if !headerToken(name) {
			return invalidRequest(field, reasonInvalidHeaderName)
		}
		if v := h[name]; len(v) > maxHeaderValueBytes || !utf8.ValidString(v) || strings.ContainsFunc(v, isCTL) {
			return invalidRequest(field, reasonInvalidHeaderValue)
		}
		for _, earlier := range names[:i] {
			if strings.EqualFold(earlier, name) {
				return invalidRequest(field, reasonDuplicate)
			}
		}
	}
	return nil
}

// headerToken reports whether s is an RFC 9110 token of 1 to 128 bytes.
func headerToken(s string) bool {
	if s == "" || len(s) > maxHeaderNameBytes {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// isCTL reports an ASCII control character, DEL included.
func isCTL(r rune) bool { return r < 0x20 || r == 0x7f }

// asciiLower lowercases the ASCII letters of s only.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}
