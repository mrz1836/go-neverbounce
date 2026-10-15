package neverbounce

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
)

// Download bounds.
const (
	// downloadSniffBytes is how much of a download is buffered before the
	// first byte is written: a body that ends within it and is a JSON envelope
	// is an error, not a file.
	downloadSniffBytes = 64 << 10

	// downloadCopyBuffer is the copy loop's read size.
	downloadCopyBuffer = 32 << 10
)

// DownloadSegments selects which records a download holds. A nil field is
// not sent, leaving the provider's default; a non-nil one is sent as a JSON
// boolean.
type DownloadSegments struct {
	// Valids, Invalids, Catchalls, Unknowns and Disposables include or exclude
	// the records with each result (the provider includes all five by
	// default).
	Valids      *bool
	Invalids    *bool
	Catchalls   *bool
	Unknowns    *bool
	Disposables *bool

	// IncludeDuplicates includes every instance of a duplicated record (off by
	// default).
	IncludeDuplicates *bool

	// OnlyDuplicates returns only the duplicated records, overriding the
	// other segment options.
	OnlyDuplicates *bool

	// OnlyBadSyntax returns only the records with bad syntax, overriding the
	// other segment options.
	OnlyBadSyntax *bool
}

// DownloadColumns appends columns to a download. A nil field is not sent,
// leaving the provider's default; a non-nil one is sent as a JSON boolean.
type DownloadColumns struct {
	BadSyntax           *bool // bad_syntax: a yes/no column
	FreeEmailHost       *bool // free_email_host: a yes/no column
	RoleAccount         *bool // role_account: a yes/no column
	Addr                *bool // addr: the address part before any alias
	Alias               *bool // alias
	Host                *bool // host
	Subdomain           *bool // subdomain
	Domain              *bool // domain
	TLD                 *bool // tld
	FQDN                *bool // fqdn
	Network             *bool // network: the FQDN of the primary MX record
	HasDNSInfo          *bool // has_dns_info: a yes/no column
	HasMailServer       *bool // has_mail_server: a yes/no column
	MailServerReachable *bool // mail_server_reachable: a yes/no column
	EmailStatusInt      *bool // email_status_int: the result as 0 to 4; the mapping is not documented
	EmailStatus         *bool // email_status: the result as text (on by default)
}

// BinaryFormat is how a download spells yes/no columns
// ("binary_operators_type").
type BinaryFormat string

// Binary formats.
const (
	BinaryOneZero        BinaryFormat = "BIN_1_0" // 1 and 0, the provider's default
	BinaryUpperYN        BinaryFormat = "BIN_Y_N"
	BinaryLowerYN        BinaryFormat = "BIN_y_n"
	BinaryLowerYesNo     BinaryFormat = "BIN_yes_no"
	BinaryTitleYesNo     BinaryFormat = "BIN_Yes_No"
	BinaryLowerTrueFalse BinaryFormat = "BIN_true_false"
)

// valid reports whether f is "" or one of the constants.
func (f BinaryFormat) valid() bool {
	switch f {
	case "", BinaryOneZero, BinaryUpperYN, BinaryLowerYN, BinaryLowerYesNo, BinaryTitleYesNo, BinaryLowerTrueFalse:
		return true
	}
	return false
}

// LineFeed is a download's line ending ("line_feed_type").
//
// The provider's default is LineFeedLFCR, "\n\r", which encoding/csv reads as
// a stray "\r" at the start of every following record: set LineFeedCRLF or
// LineFeedLF when the file will be parsed.
type LineFeed string

// Line endings.
const (
	LineFeedLFCR LineFeed = "LINEFEED_0A0D" // "\n\r", the provider's default
	LineFeedCRLF LineFeed = "LINEFEED_0D0A" // "\r\n"
	LineFeedLF   LineFeed = "LINEFEED_0A"   // "\n"
	LineFeedCR   LineFeed = "LINEFEED_0D"   // "\r"
)

// valid reports whether f is "" or one of the constants.
func (f LineFeed) valid() bool {
	switch f {
	case "", LineFeedLFCR, LineFeedCRLF, LineFeedLF, LineFeedCR:
		return true
	}
	return false
}

// DownloadJobRequest is one jobs/download call.
type DownloadJobRequest struct {
	// JobID is the job whose results to download.
	JobID JobID

	// Segments selects records.
	Segments DownloadSegments

	// Append adds columns.
	Append DownloadColumns

	// BinaryFormat spells yes/no columns. "" is not sent.
	BinaryFormat BinaryFormat

	// LineFeed is the line ending. "" is not sent (the provider's default is
	// "\n\r"; see LineFeed).
	LineFeed LineFeed
}

// DownloadJob streams a job's results, a CSV file, to w: POST
// https://api.neverbounce.com/v4.2/jobs/download. It returns the number of
// bytes written to w, on error too.
//
// Free and read-only. It makes exactly one attempt and never retries or
// resumes. The response is classified before any byte reaches w: an HTTP
// status other than 200, a JSON envelope (such as a job that is not complete),
// a content type other than application/octet-stream or text/csv, and a
// declared length over WithMaxDownloadBytes all write nothing. The bytes are
// then copied unchanged: no CSV parsing, no BOM, no line-ending rewrite.
//
// w belongs to the caller: the SDK never closes, flushes or inspects it. On
// any error after streaming began, w holds a partial file of BytesWritten
// bytes: discard it. WithDownloadTimeout bounds the whole call;
// WithMaxDownloadBytes bounds the bytes written, and a longer download fails
// with ErrResponseTooLarge after exactly that many. Canceling ctx stops the
// download at its next read, but cannot interrupt a Write to w that blocks.
//
// Errors are a *ValidationError (nothing sent) or a *CallError whose JobID is
// req.JobID and whose BytesWritten is the count returned.
func (c *Client) DownloadJob(ctx context.Context, req DownloadJobRequest, w io.Writer) (int64, error) {
	if err := c.prepare(ctx); err != nil {
		return 0, err
	}
	b, err := req.encode(c)
	if err != nil {
		return 0, err
	}
	if w == nil {
		return 0, invalidRequest("w", reasonNil)
	}
	n, cerr := c.download(ctx, b, w)
	if cerr != nil {
		cerr.BytesWritten = n
		return n, cerr.forJob(req.JobID, 0)
	}
	return n, nil
}

// encode validates the request and returns its JSON body.
func (r *DownloadJobRequest) encode(c *Client) ([]byte, error) {
	if err := checkJobID(r.JobID); err != nil {
		return nil, err
	}
	if !r.BinaryFormat.valid() {
		return nil, invalidRequest("BinaryFormat", reasonInvalidValue)
	}
	if !r.LineFeed.valid() {
		return nil, invalidRequest("LineFeed", reasonInvalidValue)
	}
	b := c.newRequestBody(640).integer("job_id", int64(r.JobID))
	s, a := &r.Segments, &r.Append
	switches := [...]struct {
		name string
		v    *bool
	}{
		{"valids", s.Valids},
		{"invalids", s.Invalids},
		{"catchalls", s.Catchalls},
		{"unknowns", s.Unknowns},
		{"disposables", s.Disposables},
		{"include_duplicates", s.IncludeDuplicates},
		{"only_duplicates", s.OnlyDuplicates},
		{"only_bad_syntax", s.OnlyBadSyntax},
		{"bad_syntax", a.BadSyntax},
		{"free_email_host", a.FreeEmailHost},
		{"role_account", a.RoleAccount},
		{"addr", a.Addr},
		{"alias", a.Alias},
		{"host", a.Host},
		{"subdomain", a.Subdomain},
		{"domain", a.Domain},
		{"tld", a.TLD},
		{"fqdn", a.FQDN},
		{"network", a.Network},
		{"has_dns_info", a.HasDNSInfo},
		{"has_mail_server", a.HasMailServer},
		{"mail_server_reachable", a.MailServerReachable},
		{"email_status_int", a.EmailStatusInt},
		{"email_status", a.EmailStatus},
	}
	for _, sw := range switches {
		if sw.v != nil {
			b = b.boolean(sw.name, *sw.v)
		}
	}
	if r.BinaryFormat != "" {
		b = b.str("binary_operators_type", string(r.BinaryFormat))
	}
	if r.LineFeed != "" {
		b = b.str("line_feed_type", string(r.LineFeed))
	}
	return b.finish(maxSmallRequestBytes)
}

// download makes the one attempt of a jobs/download call within the download
// timeout and streams an accepted response to w.
func (c *Client) download(ctx context.Context, body []byte, w io.Writer) (int64, *CallError) {
	if err := ctx.Err(); err != nil {
		return 0, contextFailure(OpDownloadJob, err, 0)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.downloadTimeout)
	defer cancel()
	resp, release, cerr := open(ctx, callCtx, c.send, OpDownloadJob, body) //nolint:bodyclose // closed exactly once by release
	if cerr != nil {
		return 0, cerr
	}
	defer release()
	r, cerr := c.acceptDownload(ctx, callCtx, resp)
	if cerr != nil {
		return 0, cerr
	}
	return copyBounded(ctx, callCtx, w, r, resp.ContentLength, c.maxDownloadBytes)
}

// acceptDownload classifies a download response before anything is written,
// and returns the reader to stream from when it is a file.
func (c *Client) acceptDownload(ctx, callCtx context.Context, resp *http.Response) (io.Reader, *CallError) {
	const op = OpDownloadJob
	status := resp.StatusCode
	switch {
	case status < http.StatusOK:
		// A 1xx body is the raw connection: never read it.
		return nil, newCallError(op, FailureUnexpectedResponse, status, errStatus)
	case status != http.StatusOK:
		return nil, c.statusFailure(op, &exchange{status: status, body: excerpt(resp.Body, resp.ContentLength)})
	case encoded(resp.Header):
		return nil, newCallError(op, FailureUnexpectedResponse, status, errContentEncoding)
	case resp.ContentLength > c.maxDownloadBytes:
		return nil, newCallError(op, FailureResponseTooLarge, status, errResponseTooLarge)
	}
	switch mediaType(resp.Header) {
	case "application/json":
		return nil, c.jsonDownload(ctx, callCtx, resp)
	case "application/octet-stream", "text/csv":
	default:
		return nil, newCallError(op, FailureUnexpectedResponse, status, errContentType)
	}
	// The progress check sits under the buffer, so a body that stalls is
	// reported the same way whether it stalls while sniffing or streaming.
	br := bufio.NewReaderSize(&progressReader{r: resp.Body}, downloadSniffBytes+1)
	head, err := br.Peek(downloadSniffBytes + 1)
	switch {
	case errors.Is(err, io.EOF):
		// The whole body fits the sniff window.
		if truncated(int64(len(head)), resp.ContentLength) {
			return nil, newCallError(op, FailureTransport, status, errTransportClosed)
		}
		if statusRaw, messageRaw, ok := envelopeOf(head); ok {
			return nil, c.downloadEnvelope(statusRaw, messageRaw)
		}
	case err != nil:
		return nil, classify(ctx, callCtx, op, err, status)
	}
	return br, nil
}

// mediaType returns the lowercased media type of the one Content-Type header,
// or "" when there is none, more than one, or it does not parse.
func mediaType(h http.Header) string {
	values := h.Values("Content-Type")
	if len(values) != 1 {
		return ""
	}
	media, _, err := mime.ParseMediaType(values[0])
	if err != nil {
		return ""
	}
	return media
}

// envelopeOf reports whether data, a whole download body, is a provider
// envelope rather than a file: a strict JSON object whose status member is a
// string. It returns the raw status and message members.
func envelopeOf(data []byte) (statusRaw, messageRaw []byte, ok bool) {
	if i := skipSpace(data, 0); i >= len(data) || data[i] != '{' {
		return nil, nil, false
	}
	if kind, _ := checkStrict(data); kind != "" {
		return nil, nil, false
	}
	obj := readObject(data, envelopeNames())
	if !isString(obj.fields[envStatus]) {
		return nil, nil, false
	}
	return obj.fields[envStatus], obj.fields[envMessage], true
}

// downloadEnvelope returns the CallError for a download answered with an
// envelope: its failure, or unexpected_response for a success, which carries
// no file.
func (c *Client) downloadEnvelope(statusRaw, messageRaw []byte) *CallError {
	if cerr := c.envelopeFailure(OpDownloadJob, statusRaw, messageRaw); cerr != nil {
		return cerr
	}
	ce := newCallError(OpDownloadJob, FailureUnexpectedResponse, http.StatusOK, errJSONDownload)
	ce.APIStatus = APIStatusSuccess
	return ce
}

// jsonDownload reads a download answered as application/json, at most
// downloadSniffBytes of it, and returns the failure it reports.
func (c *Client) jsonDownload(ctx, callCtx context.Context, resp *http.Response) *CallError {
	const op = OpDownloadJob
	status := resp.StatusCode
	if resp.ContentLength > downloadSniffBytes {
		return newCallError(op, FailureUnexpectedResponse, status, errJSONDownload)
	}
	data, err := readBody(resp.Body, resp.ContentLength, downloadSniffBytes)
	switch {
	case err != nil:
		return classify(ctx, callCtx, op, err, status)
	case len(data) > downloadSniffBytes:
		return newCallError(op, FailureUnexpectedResponse, status, errJSONDownload)
	case truncated(int64(len(data)), resp.ContentLength):
		return newCallError(op, FailureTransport, status, errTransportClosed)
	}
	if kind, cause := checkStrict(data); cause != nil {
		return newCallError(op, kind, status, cause)
	}
	obj := readObject(data, envelopeNames())
	return c.downloadEnvelope(obj.fields[envStatus], obj.fields[envMessage])
}

// copyBounded copies r to w, at most limit bytes, and returns the bytes
// written. It is its own loop rather than io.Copy so that a failed read and a
// failed write stay apart: a writer that fails, or writes short or reports an
// impossible count, is write_failed; a read error, a stall reported by
// progressReader included, is classified like any transport failure; a body
// that ends before its declared length is a truncated transport failure; and
// a body longer than limit is response_too_large after exactly limit bytes,
// found by a 1-byte probe read.
func copyBounded(ctx, callCtx context.Context, w io.Writer, r io.Reader, contentLength, limit int64) (int64, *CallError) {
	const op = OpDownloadJob
	buf := make([]byte, min(downloadCopyBuffer, limit))
	var written int64
	for {
		probing := written == limit
		want := min(int64(len(buf)), limit-written)
		if probing {
			want = 1
		}
		n, rerr := readLive(callCtx, r, buf[:want])
		if n > 0 {
			if probing {
				return written, newCallError(op, FailureResponseTooLarge, http.StatusOK, errResponseTooLarge)
			}
			wn, ok := writeChunk(w, buf[:n])
			written += int64(wn)
			if !ok {
				return written, newCallError(op, FailureWriteFailed, http.StatusOK, errWriter)
			}
		}
		if done, cerr := readEnd(ctx, callCtx, rerr, written, contentLength); done {
			return written, cerr
		}
	}
}

// readLive reads from r unless callCtx is done: once the call is over,
// nothing more reaches w, neither bytes already buffered nor those of a body
// that keeps answering after the context closed it.
func readLive(callCtx context.Context, r io.Reader, p []byte) (int, error) {
	if err := callCtx.Err(); err != nil {
		return 0, err
	}
	return r.Read(p)
}

// writeChunk writes p to w and returns the bytes it counts as written, and
// whether the write succeeded in full. A count outside 0..len(p) counts as
// nothing written.
func writeChunk(w io.Writer, p []byte) (int, bool) {
	n, err := w.Write(p)
	if n < 0 || n > len(p) {
		return 0, false
	}
	return n, err == nil && n == len(p)
}

// readEnd reports whether the copy ends after a read that returned rerr, and
// with which failure: none at a clean end of the declared length, a
// truncated transport failure at a short one, or the read error classified.
func readEnd(ctx, callCtx context.Context, rerr error, written, contentLength int64) (bool, *CallError) {
	switch {
	case rerr == nil:
		return false, nil
	case errors.Is(rerr, io.EOF):
		if truncated(written, contentLength) {
			return true, newCallError(OpDownloadJob, FailureTransport, http.StatusOK, errTransportClosed)
		}
		return true, nil
	}
	return true, classify(ctx, callCtx, OpDownloadJob, rerr, http.StatusOK)
}
