package neverbounce_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// HTTP/2 frame types, flags and error codes the raw server uses (RFC 9113).
const (
	rpFrameData     = 0x0
	rpFrameHeaders  = 0x1
	rpFrameRST      = 0x3
	rpFrameSettings = 0x4
	rpFramePing     = 0x6
	rpFrameGoAway   = 0x7
	rpFrameWindow   = 0x8
	rpFlagEnd       = 0x1 // END_STREAM on HEADERS and DATA, ACK on SETTINGS and PING
	rpFlagHeaders   = 0x4 // END_HEADERS
	rpCodeNoError   = 0x0
	rpCodeProtocol  = 0x1 // PROTOCOL_ERROR
	rpCodeRefused   = 0x7 // REFUSED_STREAM
	rpPreface       = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	rpMaxFrameBytes = 1 << 24
)

// rpGoAwayMarker is GOAWAY debug data. The standard library quotes it in its
// error text, so it must never reach anything the SDK returns.
const rpGoAwayMarker = "rp-goaway-debug-marker-8e2d" // gitleaks:allow (synthetic test marker)

// rpTimeout is the SDK timeout of the replay calls. A call that fails must
// fail at once, never retry until this expires.
const rpTimeout = 10 * time.Second

var (
	errRPFrameTooLarge = errors.New("rp: frame too large")
	errRPHPACKTooLong  = errors.New("rp: header string too long for this encoder")
)

// rpAction answers the failing request once its stream has ended: it writes
// frames to w and reports whether the server then hangs up.
type rpAction func(w io.Writer, stream uint32) (hangUp bool)

// rpH2Server is a minimal HTTP/2 server built from hand-written frames. It
// reads each request completely, as a server that may already have acted on
// it would, and answers it with reply, except request number failAt (from 1;
// 0 for none), which gets action instead. A request the client resent would
// thus be answered with success, and counted.
type rpH2Server struct {
	ln      net.Listener
	reply   testutil.Step
	failAt  int64
	action  rpAction
	headers atomic.Int64 // HEADERS frames received, on every connection
	bodies  atomic.Int64 // requests received in full
	notH2   atomic.Bool  // a client spoke something other than HTTP/2

	mu      sync.Mutex
	conns   []net.Conn
	perConn []int64 // HEADERS frames per accepted connection, in accept order
	wg      sync.WaitGroup
}

// rpStartH2 starts an rpH2Server and returns it with an HTTP/2 client
// transport that trusts its certificate, and the count of that transport's
// dials.
func rpStartH2(t *testing.T, reply testutil.Step, failAt int64,
	action rpAction,
) (*rpH2Server, *http.Transport, *atomic.Int64) {
	t.Helper()
	cert, tr := rpTLSBase(t)
	tr.ForceAttemptHTTP2 = true
	dials := &atomic.Int64{}
	dialer := &net.Dialer{}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return dialer.DialContext(ctx, network, addr)
	}

	lc := net.ListenConfig{}
	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &rpH2Server{
		ln: tls.NewListener(inner, &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h2"},
			MinVersion:   tls.VersionTLS12,
		}),
		reply: reply, failAt: failAt, action: action,
	}
	s.wg.Go(s.accept)
	t.Cleanup(func() {
		_ = s.ln.Close()
		tr.CloseIdleConnections()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s, tr, dials
}

func (s *rpH2Server) origin() string { return "https://" + s.ln.Addr().String() }

// connHeaders returns the HEADERS frames received on each connection, in
// accept order.
func (s *rpH2Server) connHeaders() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.perConn)
}

// requireH2 skips the test when the client did not speak HTTP/2, so that the
// replay checks never pass vacuously.
func (s *rpH2Server) requireH2(t *testing.T) {
	t.Helper()
	if s.notH2.Load() {
		t.Skip("HTTP/2 was not negotiated (is GODEBUG=http2client=0 set?)")
	}
}

func (s *rpH2Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.perConn = append(s.perConn, 0)
		idx := len(s.perConn) - 1
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(c, idx) })
	}
}

func (s *rpH2Server) serve(c net.Conn, idx int) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	preface := make([]byte, len(rpPreface))
	if _, err := io.ReadFull(r, preface); err != nil || string(preface) != rpPreface {
		s.notH2.Store(true)
		return
	}
	if rpWriteFrame(c, rpFrameSettings, 0, 0, nil) != nil {
		return
	}
	numbers := map[uint32]int64{} // request number of each stream
	for {
		typ, flags, stream, payload, err := rpReadFrame(r)
		if err != nil {
			return
		}
		if s.handle(c, idx, numbers, typ, flags, stream, payload) {
			// Hang up: end the write side with a TLS close_notify, so the
			// client reads every frame sent and then EOF, and drain until the
			// client closes. Closing outright with unread client frames
			// pending could reset the connection and discard the GOAWAY.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.CloseWrite()
			}
			_, _ = io.Copy(io.Discard, r)
			return
		}
	}
}

// handle answers one client frame: it acknowledges settings and pings, counts
// requests, returns connection flow-control credit, and answers each request
// once its stream has ended. It reports whether to hang up.
func (s *rpH2Server) handle(w io.Writer, idx int, numbers map[uint32]int64, typ, flags byte, stream uint32,
	payload []byte,
) bool {
	end := flags&rpFlagEnd != 0
	switch typ {
	case rpFrameSettings:
		if !end {
			_ = rpWriteFrame(w, rpFrameSettings, rpFlagEnd, 0, nil)
		}
		return false
	case rpFramePing:
		if !end {
			_ = rpWriteFrame(w, rpFramePing, rpFlagEnd, 0, payload)
		}
		return false
	case rpFrameHeaders:
		numbers[stream] = s.headers.Add(1)
		s.mu.Lock()
		s.perConn[idx]++
		s.mu.Unlock()
	case rpFrameData:
		if n := len(payload); n > 0 && n <= rpMaxFrameBytes {
			_ = rpWriteFrame(w, rpFrameWindow, 0, 0, binary.BigEndian.AppendUint32(nil, uint32(n)))
		}
	default:
		return false
	}
	if !end {
		return false
	}
	s.bodies.Add(1)
	if s.action != nil && numbers[stream] == s.failAt {
		return s.action(w, stream)
	}
	return rpWriteReply(w, stream, s.reply) != nil
}

func rpReadFrame(r io.Reader) (typ, flags byte, stream uint32, payload []byte, err error) {
	var hdr [9]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
	if n > rpMaxFrameBytes {
		return 0, 0, 0, nil, errRPFrameTooLarge
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, 0, nil, err
	}
	return hdr[3], hdr[4], binary.BigEndian.Uint32(hdr[5:]) & 0x7fffffff, payload, nil
}

func rpWriteFrame(w io.Writer, typ, flags byte, stream uint32, payload []byte) error {
	n := len(payload)
	if n > rpMaxFrameBytes {
		return errRPFrameTooLarge
	}
	frame := make([]byte, 9, 9+n)
	binary.BigEndian.PutUint32(frame[0:4], uint32(n)<<8) // 24-bit length, then the type byte
	frame[3], frame[4] = typ, flags
	binary.BigEndian.PutUint32(frame[5:], stream)
	_, err := w.Write(append(frame, payload...))
	return err
}

// rpWriteReply answers stream as a Script answers with reply: one HEADERS
// frame, then the body, if any, in one DATA frame.
func rpWriteReply(w io.Writer, stream uint32, reply testutil.Step) error {
	block, err := rpHeaderBlock(reply)
	if err != nil {
		return err
	}
	if reply.Body == "" {
		return rpWriteFrame(w, rpFrameHeaders, rpFlagHeaders|rpFlagEnd, stream, block)
	}
	if err = rpWriteFrame(w, rpFrameHeaders, rpFlagHeaders, stream, block); err != nil {
		return err
	}
	return rpWriteFrame(w, rpFrameData, rpFlagEnd, stream, []byte(reply.Body))
}

// rpHeaderBlock encodes reply's status (200 when zero) and header fields
// (Content-Type application/json when nil) as an HPACK block of literal fields
// that are never indexed, so no table state is involved. After :status come
// the header fields, sorted by name, each name in lower case as HTTP/2
// requires.
func rpHeaderBlock(reply testutil.Step) ([]byte, error) {
	status, header := reply.Status, reply.Header
	if status == 0 {
		status = http.StatusOK
	}
	if header == nil {
		header = http.Header{"Content-Type": {mediaJSON}}
	}
	fields := [][2]string{{":status", strconv.Itoa(status)}}
	for _, name := range slices.Sorted(maps.Keys(header)) {
		for _, value := range header[name] {
			fields = append(fields, [2]string{strings.ToLower(name), value})
		}
	}
	var block []byte
	for _, f := range fields {
		// A literal header field without indexing, with a literal name
		// (RFC 7541 section 6.2.2): a zero byte, then the name and value as
		// raw strings, each prefixed by its 7-bit length.
		block = append(block, 0)
		for _, s := range f {
			n := len(s)
			if n > 126 {
				return nil, errRPHPACKTooLong
			}
			block = append(block, byte(n))
			block = append(block, s...)
		}
	}
	return block, nil
}

// rpReset resets the request's stream with code and keeps the connection, so
// that a resend could reuse it.
func rpReset(code uint32) rpAction {
	return func(w io.Writer, stream uint32) bool {
		_ = rpWriteFrame(w, rpFrameRST, 0, stream, binary.BigEndian.AppendUint32(nil, code))
		return false
	}
}

// rpGoAway sends GOAWAY with code and the marker as debug data, and hangs up.
// When covered, its last stream ID is the request's: the server claims it
// received the request, and the client fails it with an error quoting the
// debug data. Otherwise the last stream ID is the stream before (0 on a new
// connection): the server claims it never processed the request, which tells
// the client it may resend it, though the server has in fact read it in full.
func rpGoAway(code uint32, covered bool) rpAction {
	return func(w io.Writer, stream uint32) bool {
		last := stream
		if !covered {
			last = max(stream, 2) - 2
		}
		payload := binary.BigEndian.AppendUint32(nil, last)
		payload = binary.BigEndian.AppendUint32(payload, code)
		payload = append(payload, rpGoAwayMarker...)
		_ = rpWriteFrame(w, rpFrameGoAway, 0, 0, payload)
		return true
	}
}

// rpH2Scenario is one way an HTTP/2 server refuses or drops a request it has
// read in full. With failAt 2, an earlier call succeeds on the connection
// first, so the failing request travels on a reused connection.
//
// "Before" and "after the stream" name the GOAWAY's last stream ID; every
// action runs once the request's body has been read, the case in which a
// resend duplicates work. A GOAWAY written before the client sends the
// request at all is left out on purpose: whether the client reads it before
// or after it opens the stream is a race, so neither the frame count nor the
// error is deterministic there, and a resend in that case would carry a body
// the server never read.
type rpH2Scenario struct {
	name   string
	action rpAction
	failAt int64
}

func rpH2Scenarios() []rpH2Scenario {
	return []rpH2Scenario{
		{"refused stream", rpReset(rpCodeRefused), 1},
		{"refused stream on a reused connection", rpReset(rpCodeRefused), 2},
		{"protocol error", rpReset(rpCodeProtocol), 1},
		{"protocol error on a reused connection", rpReset(rpCodeProtocol), 2},
		{"GOAWAY before the stream", rpGoAway(rpCodeNoError, false), 1},
		{"GOAWAY with error before the stream", rpGoAway(rpCodeProtocol, false), 1},
		{"GOAWAY before the stream on a reused connection", rpGoAway(rpCodeNoError, false), 2},
		{"GOAWAY with error before the stream on a reused connection", rpGoAway(rpCodeProtocol, false), 2},
		{"GOAWAY after the stream", rpGoAway(rpCodeNoError, true), 1},
		{"GOAWAY with error after the stream", rpGoAway(rpCodeProtocol, true), 1},
		{"GOAWAY after the stream on a reused connection", rpGoAway(rpCodeProtocol, true), 2},
	}
}

// rpAssertTransport asserts op's transport failure, with no HTTP status and
// no trace of the GOAWAY debug data, the key, the address or the filename.
func rpAssertTransport(t *testing.T, err error, op rpOp) {
	t.Helper()
	assertCallError(t, err, op.want(wantCall{Kind: neverbounce.FailureTransport}), rpGoAwayMarker, rpFilename)
}

// TestReplayNeverOverHTTP2 guards the one-attempt contract against the
// standard HTTP/2 client, which resends a request whose body it can rewind
// after a refused stream or a GOAWAY that disowns it (and, on Go 1.26, after
// a PROTOCOL_ERROR reset), even once the server has read the whole body. For
// the paid check, the job creation and the download, on a new and on a
// reused connection, the server sees the failing request once: one HEADERS
// frame and one body, on the one connection ever dialed. The call fails at
// once as a transport failure that never quotes the GOAWAY debug data.
func TestReplayNeverOverHTTP2(t *testing.T) {
	for _, sc := range rpH2Scenarios() {
		for _, op := range rpOps() {
			t.Run(sc.name+" "+op.name, func(t *testing.T) {
				s, tr, dials := rpStartH2(t, op.ok, sc.failAt, sc.action)
				c := newClient(t,
					neverbounce.WithTestBaseURL(s.origin()),
					neverbounce.WithHTTPClient(&http.Client{Transport: tr}),
					neverbounce.WithTimeout(rpTimeout),
					neverbounce.WithDownloadTimeout(rpTimeout),
				)
				for range sc.failAt - 1 {
					require.NoError(t, op.run(t, c), "an earlier call succeeds and keeps the connection")
				}

				start := time.Now()
				err := op.run(t, c)
				elapsed := time.Since(start)
				s.requireH2(t)

				rpAssertTransport(t, err, op)
				assert.Equal(t, sc.failAt, s.headers.Load(), "HEADERS frames: the failed request is sent once")
				assert.Equal(t, sc.failAt, s.bodies.Load(), "request bodies received")
				assert.Equal(t, []int64{sc.failAt}, s.connHeaders(), "HEADERS per connection: one connection only")
				assert.Equal(t, int64(1), dials.Load(), "no second connection is dialed to resend")
				assert.Less(t, elapsed, rpTimeout/2, "the call fails at once instead of resending until the timeout")
			})
		}
	}
}

// rpRawPost sends one rewindable POST of keyMember+`}` to target through hc
// and returns the response status or the error. The body is fully read and
// closed.
func rpRawPost(t *testing.T, hc *http.Client, target string) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), rpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(keyMember+`}`)))
	require.NoError(t, err)
	require.NotNil(t, req.GetBody, "a bytes.Reader body is rewindable")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, nil
}

// TestReplayHTTP2ControlStdlibResends shows the raw server detects a resend:
// the same transport, sending a rewindable POST directly, resends it after
// the refusals above, and the resent request is answered with success. Go
// 1.26 also resends after a PROTOCOL_ERROR reset; Go 1.27 no longer does, so
// that control runs on Go 1.26 only. A GOAWAY that covers the stream is never
// resent, and a GOAWAY with an error on a connection's first stream is not
// either, so neither has a control.
func TestReplayHTTP2ControlStdlibResends(t *testing.T) {
	tests := []struct {
		rpH2Scenario

		go126Only bool
		wantDials int64
	}{
		{rpH2Scenario: rpH2Scenario{"refused stream", rpReset(rpCodeRefused), 1}, wantDials: 1},
		{rpH2Scenario: rpH2Scenario{"refused stream on a reused connection", rpReset(rpCodeRefused), 2}, wantDials: 1},
		{rpH2Scenario: rpH2Scenario{"protocol error", rpReset(rpCodeProtocol), 1}, go126Only: true, wantDials: 2},
		{
			rpH2Scenario: rpH2Scenario{"protocol error on a reused connection", rpReset(rpCodeProtocol), 2},
			go126Only:    true, wantDials: 2,
		},
		{rpH2Scenario: rpH2Scenario{"GOAWAY before the stream", rpGoAway(rpCodeNoError, false), 1}, wantDials: 2},
		{
			rpH2Scenario: rpH2Scenario{
				"GOAWAY with error before the stream on a reused connection", rpGoAway(rpCodeProtocol, false), 2,
			},
			wantDials: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.go126Only && !strings.HasPrefix(runtime.Version(), "go1.26.") {
				t.Skip("the standard library resends after PROTOCOL_ERROR only up to Go 1.26")
			}
			op := rpOps()[0]
			s, tr, dials := rpStartH2(t, op.ok, tt.failAt, tt.action)
			hc := &http.Client{Transport: tr}
			for i := range tt.failAt {
				status, err := rpRawPost(t, hc, s.origin()+op.path)
				s.requireH2(t)
				require.NoError(t, err, "call %d", i+1)
				assert.Equal(t, http.StatusOK, status)
			}
			assert.Equal(t, tt.failAt+1, s.headers.Load(), "the standard library resent the request")
			assert.Equal(t, tt.failAt+1, s.bodies.Load(), "and the server read its body twice")
			assert.Equal(t, tt.wantDials, dials.Load())
		})
	}
}

// TestReplayHTTP2ControlStdlibQuotesGoAwayDebug shows that the standard
// library's own error quotes GOAWAY debug data, which is why the SDK
// discards that error.
func TestReplayHTTP2ControlStdlibQuotesGoAwayDebug(t *testing.T) {
	op := rpOps()[0]
	s, tr, _ := rpStartH2(t, op.ok, 1, rpGoAway(rpCodeProtocol, true))
	ctx, cancel := context.WithTimeout(t.Context(), rpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.origin()+op.path,
		bytes.NewReader([]byte(keyMember+`}`)))
	require.NoError(t, err)
	req.GetBody = nil
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	s.requireH2(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), rpGoAwayMarker)
	assert.Equal(t, int64(1), s.headers.Load())
}

// rpH1Server starts an HTTP/1.1 TLS server that answers every request with
// reply, except request number failAt (from 1; 0 for none): the recording
// server reads that one in full, and the handler then closes the connection
// without answering, as a server that may already have acted on it would.
func rpH1Server(t *testing.T, reply testutil.Step, failAt int64) *testutil.Server {
	t.Helper()
	var n atomic.Int64
	serve := testutil.Serve(reply)
	return testutil.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) != failAt {
			serve(w, r)
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
}

// rpArmedConn fails its next write, once armed, with nothing written: what a
// kept-alive connection that the server has meanwhile closed looks like.
type rpArmedConn struct {
	net.Conn

	armed atomic.Bool
}

func (c *rpArmedConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
	}
	return c.Conn.Write(p)
}

// rpH1Client is an HTTP/1.1 transport to one test server that counts its
// dials, keeps its connections so that a test can arm them, and records, for
// each connection a request got, whether it was a reused one.
type rpH1Client struct {
	tr    *http.Transport
	dials atomic.Int64

	mu     sync.Mutex
	conns  []*rpArmedConn
	reused []bool
}

func rpNewH1Client(t *testing.T, srv *testutil.Server) *rpH1Client {
	t.Helper()
	base, ok := srv.Client().Transport.(*http.Transport)
	require.True(t, ok)
	h := &rpH1Client{tr: base.Clone()}
	dialer := &net.Dialer{}
	h.tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		h.dials.Add(1)
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		ac := &rpArmedConn{Conn: c}
		h.mu.Lock()
		h.conns = append(h.conns, ac)
		h.mu.Unlock()
		return ac, nil
	}
	t.Cleanup(h.tr.CloseIdleConnections)
	return h
}

// RoundTrip traces which connection r gets, then passes it on.
func (h *rpH1Client) RoundTrip(r *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		h.mu.Lock()
		h.reused = append(h.reused, info.Reused)
		h.mu.Unlock()
	}}
	return h.tr.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
}

// arm makes every connection dialed so far fail its next write.
func (h *rpH1Client) arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.armed.Store(true)
	}
}

// reusedConns returns, for each connection a request got, whether it was
// reused.
func (h *rpH1Client) reusedConns() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reused)
}

// TestReplayNeverOverHTTP1 closes the connection after the server has read
// the whole request, on a new connection and on a reused keep-alive one, and
// fails the write on a reused connection with nothing written, the paths on
// which the standard HTTP/1 client resends a request it can rewind. For the
// paid check, the job creation and the download, the failing request reaches
// the server at most once, on the one connection ever dialed, and the call
// fails as a transport failure.
func TestReplayNeverOverHTTP1(t *testing.T) {
	tests := []struct {
		name     string
		failAt   int64 // the request the server drops; 0 for none
		earlier  int   // calls that succeed first
		arm      bool  // fail the next write on the kept-alive connection
		wantHits int
	}{
		{name: "new connection closed after the request", failAt: 1, wantHits: 1},
		{name: "reused connection closed after the request", failAt: 2, earlier: 1, wantHits: 2},
		{name: "reused connection with nothing written", earlier: 1, arm: true, wantHits: 1},
	}
	for _, tt := range tests {
		for _, op := range rpOps() {
			t.Run(tt.name+" "+op.name, func(t *testing.T) {
				srv := rpH1Server(t, op.ok, tt.failAt)
				h := rpNewH1Client(t, srv)
				c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(&http.Client{Transport: h}))
				for range tt.earlier {
					require.NoError(t, op.run(t, c), "an earlier call succeeds and leaves an idle connection")
				}
				if tt.arm {
					h.arm()
				}

				err := op.run(t, c)

				rpAssertTransport(t, err, op)
				assert.Equal(t, tt.wantHits, srv.Hits(), "the failing request reaches the server at most once")
				assert.Equal(t, int64(1), h.dials.Load(), "no second connection is dialed to resend")
				reused := h.reusedConns()
				require.Len(t, reused, tt.earlier+1, "one connection per call: no resend")
				assert.Equal(t, tt.earlier > 0, reused[tt.earlier], "the failing call reuses the kept-alive connection")
			})
		}
	}
}

// rpControlCase is one raw-client request shape for the HTTP/1 controls.
type rpControlCase struct {
	name      string
	header    string // the idempotency header, if any
	noGetBody bool
	failAt    int64 // the request the server drops; 0 for none
	arm       bool  // fail the next write on the kept-alive connection
	resent    bool  // the standard library resends the second request
}

// post sends one raw POST of tc's shape through hc and drains the answer.
func (tc rpControlCase) post(t *testing.T, hc *http.Client, target string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, bytes.NewReader([]byte(keyMember+`}`)))
	require.NoError(t, err)
	if tc.noGetBody {
		req.GetBody = nil
	}
	if tc.header != "" {
		req.Header.Set(tc.header, "rp-idempotency-0001")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// TestReplayHTTP1ControlStdlibResends shows the harness detects a resend: a
// raw client, sending a rewindable POST with an idempotency key, has it resent
// on a new connection after the server drops the reused one, and a rewindable
// POST is resent after a write with nothing written even without a key. The
// SDK's request is never rewindable and never carries a key. A key without a
// rewindable body, or a rewindable body without a key, is not resent after
// the request was written.
func TestReplayHTTP1ControlStdlibResends(t *testing.T) {
	tests := []rpControlCase{
		{name: "stale connection with Idempotency-Key", header: "Idempotency-Key", failAt: 2, resent: true},
		{name: "stale connection with X-Idempotency-Key", header: "X-Idempotency-Key", failAt: 2, resent: true},
		{name: "stale connection without a key", failAt: 2},
		{name: "stale connection with a key but no GetBody", header: "Idempotency-Key", noGetBody: true, failAt: 2},
		{name: "nothing written", arm: true, resent: true},
		{name: "nothing written without GetBody", arm: true, noGetBody: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := rpOps()[0]
			srv := rpH1Server(t, op.ok, tt.failAt)
			h := rpNewH1Client(t, srv)
			hc := &http.Client{Transport: h}
			require.NoError(t, tt.post(t, hc, srv.URL()+op.path), "the first call succeeds and leaves an idle connection")
			if tt.arm {
				h.arm()
			}
			err := tt.post(t, hc, srv.URL()+op.path)
			reused := h.reusedConns()
			require.GreaterOrEqual(t, len(reused), 2)
			assert.True(t, reused[1], "the second call starts on the kept-alive connection")
			// The first request always arrives; the second unless its write
			// failed; a resend adds one more, on a second connection.
			wantHits, wantDials := 2, int64(1)
			if tt.arm {
				wantHits = 1
			}
			if tt.resent {
				require.NoError(t, err, "the resent request is answered")
				wantHits++
				wantDials = 2
			} else {
				require.Error(t, err)
			}
			assert.Equal(t, wantHits, srv.Hits())
			assert.Equal(t, wantDials, h.dials.Load())
		})
	}
}

// rpShapeCases returns every method's case, and CreateJob with a remote URL,
// whose body has a shape of its own.
func rpShapeCases() []opCase {
	remote := opCaseNamed("CreateJob")
	remote.name = "CreateJob remote URL"
	remote.call = func(ctx context.Context, c *neverbounce.Client) error {
		return noValueWith(c.CreateJob(ctx, neverbounce.CreateJobRequest{RemoteURL: "https://example.com/rp-list.csv"}))
	}
	return append(opCases(), remote)
}

// TestReplayRequestsCannotBeRewound checks, in memory, the request property
// the one-attempt contract rests on, for every method: GetBody is nil, so no
// transport can rewind the body to resend it; the body is never http.NoBody,
// which HTTP/2 would resend freely; its declared length is exact; and no
// idempotency header marks it as safe to replay.
func TestReplayRequestsCannotBeRewound(t *testing.T) {
	for _, tc := range rpShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			c, s := newScriptClient(t, tc.ok)
			require.NoError(t, tc.call(t.Context(), c))
			reqs := s.Requests()
			require.Len(t, reqs, 1, "one request")
			for i, r := range reqs {
				t.Run(strconv.Itoa(i), func(t *testing.T) {
					assert.Equal(t, http.MethodPost, r.Method)
					assert.False(t, r.GetBody, "GetBody must be nil")
					assert.False(t, r.NoBody, "the body must not be http.NoBody")
					assert.NotEmpty(t, r.Body)
					assert.Equal(t, int64(len(r.Body)), r.ContentLength, "the declared length is exact")
					assert.True(t, bytes.HasPrefix(r.Body, []byte(keyMember)), "the body holds the key first")
					for _, h := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
						assert.Empty(t, r.Header.Values(h), h)
					}
				})
			}
		})
	}
}

// TestReplayRequestsCannotBeRewoundOnTheWire checks the same properties on
// the *http.Request a caller's transport receives over each protocol path:
// the request handed to the injected transport is the SDK's own.
func TestReplayRequestsCannotBeRewoundOnTheWire(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		for _, op := range rpOps() {
			t.Run(fmt.Sprintf("%s %s", proto.Name(), op.name), func(t *testing.T) {
				srv := testutil.NewServerProto(t, proto, testutil.Serve(op.ok))
				inner := srv.Client().Transport
				var seen []testutil.Request
				var mu sync.Mutex
				rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					seen = append(seen, testutil.Request{
						GetBody: r.GetBody != nil, NoBody: r.Body == http.NoBody, ContentLength: r.ContentLength,
					})
					mu.Unlock()
					return inner.RoundTrip(r)
				})
				c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(&http.Client{Transport: rt}))
				require.NoError(t, op.run(t, c))
				r := testutil.OnlyRequest(t, srv)
				mu.Lock()
				defer mu.Unlock()
				require.Len(t, seen, 1)
				assert.False(t, seen[0].GetBody, "GetBody must be nil")
				assert.False(t, seen[0].NoBody)
				assert.Equal(t, int64(len(r.Body)), seen[0].ContentLength)
				assert.Equal(t, string(proto), r.Proto)
			})
		}
	}
}
