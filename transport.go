package neverbounce

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
)

// Transport constants.
const (
	// userAgent is the User-Agent header of every call.
	userAgent = "go-neverbounce"

	// maxErrorBodyBytes bounds the excerpt read from a response whose status
	// is not 200, which only feeds APIStatus and the opt-in diagnostics.
	maxErrorBodyBytes = 64 << 10

	// maxResponseHeaderBytes bounds the response headers on the transport the
	// SDK builds itself.
	maxResponseHeaderBytes = 64 << 10

	// maxPrealloc bounds the buffer sized from a declared length before any
	// byte arrives, so that a response that declares a large body and then
	// stalls cannot pin that much memory until the timeout.
	maxPrealloc = 1 << 20

	// maxEmptyReads is how many reads in a row may return nothing before a
	// body counts as stalled.
	maxEmptyReads = 100
)

// sender makes the one attempt of a call: it writes the key member into the
// space body reserves for it, and POSTs body to op on the configured origin.
// Everything it needs, the API key included, is captured inside the function:
// fmt prints a func value only as an address, and reflection-based dumpers
// cannot see what a closure captures. An error is errBuildRequest or the
// http.Client's own error, which the caller classifies and drops.
type sender func(ctx context.Context, op Operation, body []byte) (*http.Response, error)

// newSender returns the sender for a validated configuration.
func newSender(hc *http.Client, origin, keyMember string) sender {
	return func(ctx context.Context, op Operation, body []byte) (*http.Response, error) {
		copy(body, keyMember)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+apiRoot+string(op), bytes.NewReader(body))
		if err != nil {
			// The standard library's error quotes the URL.
			return nil, errBuildRequest
		}
		// A rewindable body lets the standard library resend the request on
		// its own: HTTP/1 when a reused connection fails before anything is
		// written, and HTTP/2 after a refused stream, a GOAWAY or a protocol
		// error, even once the server has read the whole body. Without GetBody
		// every call is one attempt, and a 307 or 308 cannot be followed
		// either. The body always holds the key, so it is never empty and
		// never http.NoBody, which HTTP/2 would treat as safe to resend.
		req.GetBody = nil
		req.Header = requestHeader(op)
		return hc.Do(req)
	}
}

// requestHeader returns a fresh header map for one call. The values share one
// backing array, allocated per call so that no two requests share header
// memory; each is a full slice expression, so an append to one cannot
// overwrite the next.
//
// It never sets Authorization, Cookie or an idempotency key: with an
// Idempotency-Key header, the standard library may replay a POST.
// Accept-Encoding is left to the transport, which then decodes gzip itself.
func requestHeader(op Operation) http.Header {
	accept := "application/json"
	if op == OpDownloadJob {
		accept = "*/*"
	}
	v := append(make([]string, 0, 3), "application/json", accept, userAgent)
	h := make(http.Header, 3)
	h["Content-Type"] = v[0:1:1]
	h["Accept"] = v[1:2:2]
	h["User-Agent"] = v[2:3:3]
	return h
}

// exchange is a received HTTP response. For status 200, body is the whole
// body, within the size limit. For any other status, body is the body if it
// could be read within maxErrorBodyBytes, else nil: it only feeds APIStatus
// and the opt-in diagnostics.
type exchange struct {
	status int
	body   []byte
}

// exchange makes the one attempt of a JSON call and reads its response, all
// within the Client's timeout. A context that is already done sends nothing.
func (c *Client) exchange(ctx context.Context, op Operation, body []byte) (*exchange, *CallError) {
	if err := ctx.Err(); err != nil {
		return nil, contextFailure(op, err, 0)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, release, cerr := open(ctx, callCtx, c.send, op, body) //nolint:bodyclose // closed exactly once by release
	if cerr != nil {
		return nil, cerr
	}
	defer release()
	return readExchange(ctx, callCtx, op, resp, c.responseLimit(op))
}

// open makes the one attempt of a call within callCtx and returns the
// response. Its body is closed exactly once: when callCtx ends, so that a read
// that does not watch the context still stops, or when release runs, which
// the caller must do before returning. A close error after a complete read
// changes nothing.
func open(ctx, callCtx context.Context, send sender, op Operation, body []byte) (*http.Response, func(), *CallError) {
	resp, err := send(callCtx, op, body)
	if err != nil {
		if errors.Is(err, errBuildRequest) {
			return nil, nil, newCallError(op, FailureTransport, 0, errBuildRequest)
		}
		return nil, nil, classify(ctx, callCtx, op, err, 0)
	}
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = resp.Body.Close() }) }
	stop := context.AfterFunc(callCtx, closeBody)
	return resp, func() {
		stop()
		closeBody()
	}, nil
}

// readExchange reads a JSON call's response within maxBytes.
func readExchange(ctx, callCtx context.Context, op Operation, resp *http.Response, maxBytes int64) (*exchange, *CallError) {
	switch {
	case resp.StatusCode < http.StatusOK:
		// A 1xx that reaches here, such as 101 Switching Protocols, has the
		// raw connection as its body, which no deadline watches: never read it.
		return &exchange{status: resp.StatusCode}, nil
	case resp.StatusCode != http.StatusOK:
		// The status decides the outcome; the body only feeds diagnostics, so
		// an oversized or unreadable one is skipped.
		return &exchange{status: resp.StatusCode, body: excerpt(resp.Body, resp.ContentLength)}, nil
	case encoded(resp.Header):
		return nil, newCallError(op, FailureUnexpectedResponse, resp.StatusCode, errContentEncoding)
	case resp.ContentLength > maxBytes:
		return nil, newCallError(op, FailureResponseTooLarge, resp.StatusCode, errResponseTooLarge)
	}
	data, err := readBody(resp.Body, resp.ContentLength, maxBytes)
	switch {
	case err != nil:
		return nil, classify(ctx, callCtx, op, err, resp.StatusCode)
	case int64(len(data)) > maxBytes:
		return nil, newCallError(op, FailureResponseTooLarge, resp.StatusCode, errResponseTooLarge)
	case truncated(int64(len(data)), resp.ContentLength):
		// The standard transport reports a short body as io.ErrUnexpectedEOF;
		// an injected one might end it cleanly instead. Either way it is
		// incomplete.
		return nil, newCallError(op, FailureTransport, resp.StatusCode, errTransportClosed)
	}
	return &exchange{status: resp.StatusCode, body: data}, nil
}

// readBody reads r to EOF, but never more than maxBytes+1 bytes, so a result
// longer than maxBytes means the body is too large. A declared length, already
// known to be within the limit, sizes the buffer exactly when it is small,
// with one spare byte to see EOF without growing. A body of unknown or large
// declared length is read by io.ReadAll, which grows as bytes arrive.
func readBody(r io.Reader, contentLength, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(&progressReader{r: r}, maxBytes+1)
	if contentLength < 0 || contentLength > maxPrealloc {
		return io.ReadAll(limited)
	}
	buf := make([]byte, 0, contentLength+1)
	for len(buf) < cap(buf) {
		n, err := limited.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
	// The body is longer than declared: read on, still within the limit.
	rest, err := io.ReadAll(limited)
	return append(buf, rest...), err
}

// truncated reports whether a body of n bytes falls short of, or runs past,
// its declared length (-1 for unknown).
func truncated(n, declared int64) bool { return declared >= 0 && n != declared }

// progressReader fails a body that makes no progress: maxEmptyReads reads in
// a row that return neither bytes nor an error. Without it, a body from an
// injected transport that keeps answering (0, nil) would spin a CPU until the
// call's timeout.
type progressReader struct {
	r     io.Reader
	empty int
}

// Read reads from the wrapped reader, counting reads that make no progress.
func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 || err != nil || len(b) == 0 {
		p.empty = 0
		return n, err
	}
	if p.empty++; p.empty >= maxEmptyReads {
		return 0, errTransportStalled
	}
	return 0, nil
}

// excerpt returns the body of a response whose status is not 200 when it
// reads completely within maxErrorBodyBytes, and nil when it is declared or
// found longer, or its read fails. A stalled read ends with the call's
// context, which closes the body.
func excerpt(body io.Reader, contentLength int64) []byte {
	if contentLength > maxErrorBodyBytes {
		return nil
	}
	b, err := readBody(body, contentLength, maxErrorBodyBytes)
	if err != nil || len(b) > maxErrorBodyBytes {
		return nil
	}
	return b
}

// encoded reports whether h carries a Content-Encoding other than identity.
// The transport removes the header when it decodes gzip itself, so one that
// remains names an encoding nobody decoded. Every value counts, as a list of
// codings; empty list elements are ignored.
func encoded(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for coding := range strings.SplitSeq(v, ",") {
			coding = strings.Trim(coding, " \t")
			if coding != "" && !strings.EqualFold(coding, "identity") {
				return true
			}
		}
	}
	return false
}

// classify maps a failed exchange onto a CallError: the caller's context
// first, then a timeout (this call's deadline, an http.Client Timeout, or any
// network timeout), else a transport failure of a fixed class. The original
// error is never kept.
func classify(caller, callCtx context.Context, op Operation, err error, status int) *CallError {
	if cerr := caller.Err(); cerr != nil {
		return contextFailure(op, cerr, status)
	}
	if callCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || isNetTimeout(err) {
		return newCallError(op, FailureTimeout, status, context.DeadlineExceeded)
	}
	return newCallError(op, FailureTransport, status, transportClass(err))
}

// isNetTimeout reports whether err is a network error that timed out, such as
// a dial or TLS handshake timeout.
func isNetTimeout(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// transportClass reduces a transport error to one of a few fixed classes.
func transportClass(err error) error {
	if errors.Is(err, errTransportStalled) {
		return errTransportStalled
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return errTransportDNS
	}
	var op string
	if oe, ok := errors.AsType[*net.OpError](err); ok {
		op = oe.Op
	}
	if op == "proxyconnect" {
		return errTransportProxy
	}
	if isTLSError(err) {
		return errTransportTLS
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return errTransportReset
	}
	if op == "dial" {
		return errTransportDial
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return errTransportClosed
	}
	return errTransportOther
}

// isTLSError reports whether err comes from the TLS handshake or record layer.
func isTLSError(err error) bool {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	_, ok := errors.AsType[tls.AlertError](err)
	return ok
}
