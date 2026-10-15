package neverbounce_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Redirect and replay fixtures. rpLocationMarker is planted in every Location
// and redirect body: it must never surface in an error, however printed or
// unwrapped.
const (
	rpLocationMarker = "rp-location-marker-4b7e19" // gitleaks:allow (synthetic test marker)
	rpTargetPath     = "/rp-target/" + rpLocationMarker
	rpUnparsable     = "https://[::1" + rpLocationMarker + "/next"
	rpJobID          = 42
	rpFilename       = "rp-signups-example"
	rpTextPlain      = "text/plain; charset=utf-8"

	// rpPermissiveTimeout is the injected permissive client's own timeout,
	// which the SDK's copy keeps and the caller's client must still hold.
	rpPermissiveTimeout = 7 * time.Second
)

// rpOp is an operation case the redirect and replay suites run, the paid
// single check, the state-changing job creation and the streaming download,
// with a call of its own that also checks what the method returned.
type rpOp struct {
	opCase

	// run makes one call and returns its error. On failure it asserts the zero
	// result (nil, or no byte written); on success, the expected one.
	run func(t *testing.T, c *neverbounce.Client) error
}

// rpOps returns the operations of both suites.
func rpOps() []rpOp {
	return []rpOp{
		{opCase: opCaseNamed("CheckEmail"), run: rpCheckEmail},
		{opCase: opCaseNamed("CreateJob"), run: rpCreateJob},
		{opCase: opCaseNamed("DownloadJob"), run: rpDownloadJob},
	}
}

func rpCheckEmail(t *testing.T, c *neverbounce.Client) error {
	t.Helper()
	res, err := c.CheckEmail(t.Context(), checkRequest())
	if err != nil {
		assert.Nil(t, res, "a failed call returns no result")
		return err
	}
	assert.Equal(t, neverbounce.ResultValid, res.Result)
	return nil
}

func rpCreateJob(t *testing.T, c *neverbounce.Client) error {
	t.Helper()
	res, err := c.CreateJob(t.Context(), neverbounce.CreateJobRequest{
		Supplied: neverbounce.EmailList(testEmail),
		Filename: rpFilename,
	})
	if err != nil {
		assert.Nil(t, res, "a failed call returns no result")
		return err
	}
	assert.Equal(t, neverbounce.JobID(rpJobID), res.JobID)
	return nil
}

func rpDownloadJob(t *testing.T, c *neverbounce.Client) error {
	t.Helper()
	var file bytes.Buffer
	n, err := c.DownloadJob(t.Context(), neverbounce.DownloadJobRequest{JobID: rpJobID}, &file)
	if err != nil {
		assert.Zero(t, n, "a refused download writes nothing")
		assert.Zero(t, file.Len(), "a refused download writes nothing")
		assert.Zero(t, callError(t, err).BytesWritten)
		return err
	}
	assert.Equal(t, int64(len(csvFile)), n)
	assert.Equal(t, csvFile, file.String())
	return nil
}

// rpRedirectStatuses are the redirect statuses, plus 304, which redirects
// nowhere but may still carry a Location.
func rpRedirectStatuses() []int {
	return []int{
		http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusNotModified, http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
}

// rpFollowedStatuses are the statuses the standard client follows.
func rpFollowedStatuses() []int {
	return []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
}

// Where a redirect points.
type rpWhere int

const (
	rpSameOriginRelative rpWhere = iota
	rpSameOriginAbsolute
	rpCrossOrigin
)

// rpWheres names every Location kind.
func rpWheres() []struct {
	name  string
	where rpWhere
} {
	return []struct {
		name  string
		where rpWhere
	}{
		{"same origin relative", rpSameOriginRelative},
		{"same origin absolute", rpSameOriginAbsolute},
		{"cross origin", rpCrossOrigin},
	}
}

// rpRedirect is a redirect answer: status with the Location loc, and a text
// body naming loc when status allows a body.
func rpRedirect(status int, loc string) testutil.Step {
	step := testutil.Step{Status: status, Header: http.Header{"Content-Type": {rpTextPlain}, "Location": {loc}}}
	if status != http.StatusNotModified {
		step.Body = "moved to " + loc
	}
	return step
}

// rpRedirectPair is an origin that redirects every request to the target
// path, on itself or on a second server. Both answer the target path with
// reply, so a followed redirect would make the call succeed.
type rpRedirectPair struct {
	origin   *testutil.Server
	target   *testutil.Server
	location atomic.Pointer[string]
}

// rpNewRedirectPair starts the servers of one redirect case.
func rpNewRedirectPair(t *testing.T, proto testutil.Protocol, status int, where rpWhere,
	reply testutil.Step,
) *rpRedirectPair {
	t.Helper()
	p := &rpRedirectPair{}
	serveTarget := testutil.Serve(reply)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == rpTargetPath {
			serveTarget(w, r)
			return
		}
		redirect := rpRedirect(status, *p.location.Load())
		redirect.Header.Set("Set-Cookie", "rp="+rpLocationMarker+"; Path=/")
		testutil.Serve(redirect)(w, r)
	}
	p.origin = testutil.NewServerProto(t, proto, handler)
	p.target = p.origin
	loc := rpTargetPath + "?next=" + rpLocationMarker
	switch where {
	case rpSameOriginRelative:
	case rpSameOriginAbsolute:
		loc = p.origin.URL() + loc
	case rpCrossOrigin:
		p.target = testutil.NewServerProto(t, proto, handler)
		loc = p.target.URL() + loc
	}
	p.location.Store(&loc)
	return p
}

// targetHits counts the requests that reached the target path.
func (p *rpRedirectPair) targetHits() int {
	n := 0
	for _, r := range p.target.Requests() {
		if r.Path == rpTargetPath {
			n++
		}
	}
	return n
}

// rpCaller is the *http.Client a caller injects, with what the test needs to
// prove afterward that the SDK left it alone.
type rpCaller struct {
	hc       *http.Client
	followed *atomic.Int32
	jar      *cookiejar.Jar

	transport, cookieJar, checkRedirect string
	timeout                             time.Duration
}

// rpServerCaller injects the test server's own client.
func rpServerCaller(srv *testutil.Server) *rpCaller {
	return rpWatch(srv.Client(), &atomic.Int32{}, nil)
}

// rpPermissiveCaller injects a client over base that follows every redirect
// and keeps every cookie. The test servers share one certificate, so base
// trusts every one of them: only the SDK stands between the key and a
// redirect target.
func rpPermissiveCaller(t *testing.T, base http.RoundTripper) *rpCaller {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	followed := &atomic.Int32{}
	hc := &http.Client{
		Transport: base,
		Jar:       jar,
		Timeout:   rpPermissiveTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			followed.Add(1)
			return nil
		},
	}
	return rpWatch(hc, followed, jar)
}

func rpWatch(hc *http.Client, followed *atomic.Int32, jar *cookiejar.Jar) *rpCaller {
	return &rpCaller{
		hc: hc, followed: followed, jar: jar,
		transport:     rpIdentity(hc.Transport),
		cookieJar:     rpIdentity(hc.Jar),
		checkRedirect: rpIdentity(hc.CheckRedirect),
		timeout:       hc.Timeout,
	}
}

// rpIdentity identifies a pointer or function value by type and address.
func rpIdentity(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.IsNil() {
		return fmt.Sprintf("%T(nil)", v)
	}
	return fmt.Sprintf("%T@%x", v, rv.Pointer())
}

// assertUntouched asserts that the caller's client was never consulted about
// a redirect, received no cookie for any of origins, and holds the same
// transport, jar, redirect policy and timeout as before.
func (c *rpCaller) assertUntouched(t *testing.T, origins ...string) {
	t.Helper()
	assert.Zero(t, c.followed.Load(), "the caller's CheckRedirect is never consulted")
	assert.Equal(t, c.transport, rpIdentity(c.hc.Transport), "the caller's transport is kept")
	assert.Equal(t, c.cookieJar, rpIdentity(c.hc.Jar), "the caller's jar is kept")
	assert.Equal(t, c.checkRedirect, rpIdentity(c.hc.CheckRedirect), "the caller's redirect policy is kept")
	assert.Equal(t, c.timeout, c.hc.Timeout, "the caller's timeout is kept")
	if c.jar == nil {
		return
	}
	for _, o := range origins {
		u, err := url.Parse(o)
		require.NoError(t, err)
		assert.Empty(t, c.jar.Cookies(u), "the caller's jar receives no cookie")
	}
}

// rpAssertRefused asserts the outcome every redirect must have: op's
// unexpected_response carrying the 3xx status, with no trace of the Location,
// the key, the address, the filename or any of secrets, however printed or
// unwrapped.
func rpAssertRefused(t *testing.T, err error, op rpOp, status int, secrets ...string) {
	t.Helper()
	assertCallError(t, err, op.want(wantCall{Kind: neverbounce.FailureUnexpectedResponse, Status: status}),
		append([]string{rpLocationMarker, rpFilename}, secrets...)...)
}

// rpRedirectCase is one cell of the redirect matrix.
type rpRedirectCase struct {
	name       string
	proto      testutil.Protocol
	status     int
	where      rpWhere
	permissive bool
	op         rpOp
}

// rpRedirectCases returns protocol × status × Location kind × injected
// client × operation.
func rpRedirectCases() []rpRedirectCase {
	cases := make([]rpRedirectCase, 0, 2*len(rpRedirectStatuses())*len(rpWheres())*2*len(rpOps()))
	for _, proto := range testutil.Protocols() {
		for _, status := range rpRedirectStatuses() {
			for _, w := range rpWheres() {
				for _, permissive := range []bool{false, true} {
					client := "server client"
					if permissive {
						client = "permissive client"
					}
					for _, op := range rpOps() {
						cases = append(cases, rpRedirectCase{
							name:  fmt.Sprintf("%s %d %s %s %s", proto.Name(), status, w.name, client, op.name),
							proto: proto, status: status, where: w.where, permissive: permissive, op: op,
						})
					}
				}
			}
		}
	}
	return cases
}

// TestRedirectNeverFollowed runs every redirect status, to the same origin
// (relative and absolute) and to another server, over HTTP/1.1 and HTTP/2,
// through the test server's own client and through a caller's client that
// would follow anything, for the paid check, the job creation and the
// download. Each call makes exactly one request and fails as
// unexpected_response with the 3xx status; the target, which would answer
// with success, receives nothing, so neither the key nor the input ever
// reaches it.
func TestRedirectNeverFollowed(t *testing.T) {
	for _, tc := range rpRedirectCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := rpNewRedirectPair(t, tc.proto, tc.status, tc.where, tc.op.ok)
			caller := rpServerCaller(p.origin)
			if tc.permissive {
				caller = rpPermissiveCaller(t, p.origin.Client().Transport)
			}
			c := newClient(t, neverbounce.WithTestBaseURL(p.origin.URL()), neverbounce.WithHTTPClient(caller.hc))

			err := tc.op.run(t, c)

			rpAssertRefused(t, err, tc.op, tc.status, *p.location.Load(), p.target.URL()+rpTargetPath)
			assert.Equal(t, tc.op.path, testutil.OnlyRequest(t, p.origin).Path)
			assert.Zero(t, p.targetHits(), "the redirect target is never reached")
			if p.target != p.origin {
				assert.Zero(t, p.target.Hits(), "the other origin receives nothing")
			}
			caller.assertUntouched(t, p.origin.URL(), p.target.URL())
		})
	}
}

// TestRedirectControlRawClientFollows proves the harness would notice a
// followed redirect: the permissive client of TestRedirectNeverFollowed, used
// directly with a rewindable POST, follows every redirect status the standard
// library follows, to the target, which answers with success.
func TestRedirectControlRawClientFollows(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		for _, status := range rpFollowedStatuses() {
			for _, w := range rpWheres() {
				t.Run(fmt.Sprintf("%s %d %s", proto.Name(), status, w.name), func(t *testing.T) {
					op := rpOps()[0]
					p := rpNewRedirectPair(t, proto, status, w.where, op.ok)
					caller := rpPermissiveCaller(t, p.origin.Client().Transport)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.origin.URL()+op.path,
						bytes.NewReader([]byte(keyMember+`}`)))
					require.NoError(t, err)
					resp, err := caller.hc.Do(req)
					require.NoError(t, err)
					body, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())

					assert.Equal(t, http.StatusOK, resp.StatusCode)
					assert.JSONEq(t, checkOK, string(body))
					assert.Equal(t, 1, p.targetHits(), "the raw client reached the target")
					assert.Equal(t, int32(1), caller.followed.Load(), "the raw client consulted its CheckRedirect")
					u, err := url.Parse(p.origin.URL())
					require.NoError(t, err)
					assert.NotEmpty(t, caller.jar.Cookies(u), "the raw client filled its jar")
				})
			}
		}
	}
}

// rpLocationServer starts a server that answers every request with status and
// the Location loc.
func rpLocationServer(t *testing.T, proto testutil.Protocol, status int, loc string) *testutil.Server {
	t.Helper()
	return testutil.NewServerProto(t, proto, testutil.Serve(rpRedirect(status, loc)))
}

// TestRedirectUnparsableLocation sends a Location that url.Parse rejects. The
// standard client parses the Location of 301, 302 and 303 before it consults
// CheckRedirect, and its error quotes the value; the SDK still reports
// unexpected_response with the status, after exactly one request.
func TestRedirectUnparsableLocation(t *testing.T) {
	for _, proto := range testutil.Protocols() {
		for _, status := range rpRedirectStatuses() {
			for _, permissive := range []bool{false, true} {
				for _, op := range rpOps() {
					name := fmt.Sprintf("%s %d permissive=%t %s", proto.Name(), status, permissive, op.name)
					t.Run(name, func(t *testing.T) {
						srv := rpLocationServer(t, proto, status, rpUnparsable)
						caller := rpServerCaller(srv)
						if permissive {
							caller = rpPermissiveCaller(t, srv.Client().Transport)
						}
						c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(caller.hc))
						err := op.run(t, c)
						rpAssertRefused(t, err, op, status, rpUnparsable)
						assert.Equal(t, 1, srv.Hits(), "exactly one request")
						caller.assertUntouched(t, srv.URL())
					})
				}
			}
		}
	}
}

// rpTLSBase returns the certificate of the httptest servers and a client
// transport that trusts it.
func rpTLSBase(t *testing.T) (tls.Certificate, *http.Transport) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	base, ok := ts.Client().Transport.(*http.Transport)
	require.True(t, ok)
	return ts.TLS.Certificates[0], base.Clone()
}

// rpRawServer is an HTTP/1.1 TLS server that reads each request in full and
// answers it with fixed bytes, so that the response can hold what net/http
// would never write.
type rpRawServer struct {
	ln       net.Listener
	response string
	requests atomic.Int64
	wg       sync.WaitGroup
}

// rpStartRawServer starts an rpRawServer and returns it with a transport
// that trusts it.
func rpStartRawServer(t *testing.T, response string) (*rpRawServer, *http.Transport) {
	t.Helper()
	cert, tr := rpTLSBase(t)
	lc := net.ListenConfig{}
	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &rpRawServer{
		ln:       tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}),
		response: response,
	}
	s.wg.Go(s.accept)
	t.Cleanup(func() {
		_ = s.ln.Close()
		tr.CloseIdleConnections()
		s.wg.Wait()
	})
	return s, tr
}

func (s *rpRawServer) origin() string { return "https://" + s.ln.Addr().String() }

func (s *rpRawServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Go(func() { s.serve(c) })
	}
}

// serve answers one request and closes the connection.
func (s *rpRawServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	s.requests.Add(1)
	_, _ = io.WriteString(c, s.response)
}

// rpRawRedirect is a raw HTTP/1.1 redirect response with Location loc.
func rpRawRedirect(status int, loc string) string {
	return "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" +
		"Location: " + loc + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
}

// rpTabLocation holds a tab: valid in a header field, but a control character
// url.Parse rejects. net/http's own server would write it too; the raw
// servers put it on the wire byte for byte.
const rpTabLocation = "https://127.0.0.1/\t" + rpLocationMarker

// TestRedirectControlByteLocationRawResponse sends, over a raw HTTP/1.1 and
// a raw HTTP/2 response, a Location holding a tab. Both transports accept the
// header and url.Parse rejects it: the SDK reports unexpected_response with
// the status, after exactly one request.
func TestRedirectControlByteLocationRawResponse(t *testing.T) {
	for _, status := range rpRedirectStatuses() {
		for _, op := range rpOps() {
			t.Run(fmt.Sprintf("http1 %d %s", status, op.name), func(t *testing.T) {
				s, tr := rpStartRawServer(t, rpRawRedirect(status, rpTabLocation))
				c := newClient(t, neverbounce.WithTestBaseURL(s.origin()),
					neverbounce.WithHTTPClient(&http.Client{Transport: tr}))
				err := op.run(t, c)
				rpAssertRefused(t, err, op, status, rpTabLocation)
				assert.Equal(t, int64(1), s.requests.Load(), "exactly one request")
			})
			t.Run(fmt.Sprintf("http2 %d %s", status, op.name), func(t *testing.T) {
				reply := testutil.Step{Status: status, Header: http.Header{"Location": {rpTabLocation}}}
				s, tr, dials := rpStartH2(t, reply, 0, nil)
				c := newClient(t, neverbounce.WithTestBaseURL(s.origin()),
					neverbounce.WithHTTPClient(&http.Client{Transport: tr}))
				err := op.run(t, c)
				s.requireH2(t)
				rpAssertRefused(t, err, op, status, rpTabLocation)
				assert.Equal(t, int64(1), s.headers.Load(), "exactly one request")
				assert.Equal(t, int64(1), dials.Load())
			})
		}
	}
}

// TestRedirectControlByteRejectedByTransport sends a raw HTTP/1.1 redirect
// whose Location holds a byte that no header field may hold. The transport
// rejects the response before any redirect logic runs, with an error that
// quotes the header line, so the SDK reports a transport failure that quotes
// nothing.
func TestRedirectControlByteRejectedByTransport(t *testing.T) {
	for _, b := range []string{"\x01", "\x7f"} {
		loc := "https://127.0.0.1/" + b + rpLocationMarker
		for _, op := range rpOps() {
			t.Run(fmt.Sprintf("%q %s", b, op.name), func(t *testing.T) {
				s, tr := rpStartRawServer(t, rpRawRedirect(http.StatusFound, loc))
				c := newClient(t, neverbounce.WithTestBaseURL(s.origin()),
					neverbounce.WithHTTPClient(&http.Client{Transport: tr}))
				err := op.run(t, c)
				assertCallError(t, err, op.want(wantCall{Kind: neverbounce.FailureTransport}), rpLocationMarker, loc, rpFilename)
				assert.Equal(t, int64(1), s.requests.Load(), "exactly one request")
			})
		}
	}
}

// TestRedirectControlUnguardedClientQuotesLocation shows why the SDK strips
// Location instead of relying on CheckRedirect alone: a client that refuses
// redirects the same way, but without the guard, fails on an unparsable
// Location with an error that quotes it, from a net/http server and from raw
// responses alike.
func TestRedirectControlUnguardedClientQuotesLocation(t *testing.T) {
	refuse := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(t *testing.T, hc *http.Client, origin string) error {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/v4.2/single/check",
			bytes.NewReader([]byte(keyMember+`}`)))
		require.NoError(t, err)
		req.GetBody = nil
		resp, err := hc.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	t.Run("net/http server", func(t *testing.T) {
		for _, proto := range testutil.Protocols() {
			srv := rpLocationServer(t, proto, http.StatusFound, rpUnparsable)
			err := post(t, &http.Client{Transport: srv.Client().Transport, CheckRedirect: refuse}, srv.URL())
			require.Error(t, err)
			assert.Contains(t, err.Error(), rpLocationMarker, proto.Name())
		}
	})
	t.Run("raw tab", func(t *testing.T) {
		s, tr := rpStartRawServer(t, rpRawRedirect(http.StatusFound, rpTabLocation))
		err := post(t, &http.Client{Transport: tr, CheckRedirect: refuse}, s.origin())
		require.Error(t, err)
		assert.Contains(t, err.Error(), rpLocationMarker)
	})
	t.Run("raw control byte", func(t *testing.T) {
		s, tr := rpStartRawServer(t, rpRawRedirect(http.StatusFound, "https://127.0.0.1/\x01"+rpLocationMarker))
		err := post(t, &http.Client{Transport: tr, CheckRedirect: refuse}, s.origin())
		require.Error(t, err)
		assert.Contains(t, err.Error(), rpLocationMarker)
	})
}

// rpGuardStatuses are the statuses on each side of the guard's 3xx range, and
// 200, which succeeds.
func rpGuardStatuses() []int {
	return []int{
		101, 199, 200, 204, 299, 300, 301, 302, 303, 304, 305, 306, 307, 308, 309, 399,
		400, 404, 429, 500, 503, 599, 600, 999,
	}
}

// TestRedirectGuardStripsLocationInMemory checks the redirect guard on
// responses from an injected in-memory transport, which hands the SDK the
// very header map the test keeps. Every 3xx loses its Location, and nothing
// else, before the http.Client sees it: the Location here is unparsable, and
// for 301, 302 and 303 the client would otherwise fail with an error quoting
// it. Every other status keeps all of its headers, Location included, and
// fails with its own kind, or succeeds for 200.
func TestRedirectGuardStripsLocationInMemory(t *testing.T) {
	for _, status := range rpGuardStatuses() {
		for _, op := range rpOps() {
			t.Run(fmt.Sprintf("%d %s", status, op.name), func(t *testing.T) {
				header := http.Header{
					"Content-Type": {op.ok.Header.Get("Content-Type")},
					"Location":     {rpUnparsable},
					"Set-Cookie":   {"rp=" + rpLocationMarker + "; Path=/"},
					"X-Rp-Marker":  {rpLocationMarker, "second"},
				}
				want := header.Clone()
				body := "moved to " + rpUnparsable
				if status == http.StatusOK {
					body = op.ok.Body
				}
				var seen *http.Response
				ct := &testutil.CountingTransport{Next: testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
					testutil.Drain(r)
					seen = testutil.Response(r, status, io.NopCloser(bytes.NewReader([]byte(body))), int64(len(body)))
					seen.Header = header
					return seen, nil
				})}
				caller := rpWatch(&http.Client{Transport: ct}, &atomic.Int32{}, nil)
				c := newClient(t, neverbounce.WithTestBaseURL(loopbackOrigin), neverbounce.WithHTTPClient(caller.hc))

				err := op.run(t, c)

				assert.Equal(t, 1, ct.Attempts(), "exactly one attempt")
				require.NotNil(t, seen)
				if status >= 300 && status < 400 {
					want.Del("Location")
				}
				if status == http.StatusOK {
					require.NoError(t, err)
				} else {
					want := op.want(wantCall{Kind: statusKind(status), Status: status})
					assertCallError(t, err, want, rpLocationMarker, rpUnparsable, rpFilename)
				}
				assert.Equal(t, want, seen.Header, "the guard removes Location from a 3xx only, and nothing else")
				caller.assertUntouched(t)
			})
		}
	}
}

// TestRedirectGuardNilHeader checks that a 3xx without a header map, which
// only an injected transport can produce, is reported like any other.
func TestRedirectGuardNilHeader(t *testing.T) {
	for _, op := range rpOps() {
		t.Run(op.name, func(t *testing.T) {
			rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				testutil.Drain(r)
				resp := testutil.Response(r, http.StatusFound, http.NoBody, 0)
				resp.Header = nil
				return resp, nil
			})
			err := op.run(t, newTransportClient(t, rt))
			rpAssertRefused(t, err, op, http.StatusFound)
		})
	}
}

// rpUnparsableLocations are Locations url.Parse rejects. An in-memory
// transport delivers bytes no wire format would carry.
func rpUnparsableLocations() []struct{ name, loc string } {
	return []struct{ name, loc string }{
		{"unclosed bracket", rpUnparsable},
		{"control byte", "https://127.0.0.1/\x01" + rpLocationMarker},
		{"delete byte", "https://127.0.0.1/\x7f" + rpLocationMarker},
		{"nul byte", "https://127.0.0.1/\x00" + rpLocationMarker},
		{"newline", "https://127.0.0.1/\r\n" + rpLocationMarker},
		{"missing scheme", "://" + rpLocationMarker},
		{"bad escape", "https://127.0.0.1/%zz" + rpLocationMarker},
		{"space in host", "https://exa mple.com/" + rpLocationMarker},
	}
}

// TestRedirectUnparsableLocationInMemory sends every kind of unparsable
// Location, for every redirect status, from an in-memory transport: one
// attempt, unexpected_response with the status, the redirect body closed
// exactly once, and no trace of the Location.
func TestRedirectUnparsableLocationInMemory(t *testing.T) {
	for _, status := range rpRedirectStatuses() {
		for _, l := range rpUnparsableLocations() {
			for _, op := range rpOps() {
				t.Run(fmt.Sprintf("%d %s %s", status, l.name, op.name), func(t *testing.T) {
					body := testutil.NewBody("moved to " + l.loc)
					ct := &testutil.CountingTransport{Next: testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
						testutil.Drain(r)
						resp := testutil.Response(r, status, body, -1)
						resp.Header.Set("Location", l.loc)
						return resp, nil
					})}
					err := op.run(t, newTransportClient(t, ct))
					rpAssertRefused(t, err, op, status, l.loc)
					assert.Equal(t, 1, ct.Attempts(), "exactly one attempt")
					assert.Equal(t, 1, body.Closes(), "the redirect body is closed exactly once")
				})
			}
		}
	}
}

// TestRedirectInjectedClientWithoutTransport checks that a caller's client
// with no transport, which means http.DefaultTransport, is wrapped without
// being modified. That transport cannot trust the test certificate, so this
// checks construction only; the in-memory tests cover the guard itself.
func TestRedirectInjectedClientWithoutTransport(t *testing.T) {
	hc := &http.Client{}
	caller := rpWatch(hc, &atomic.Int32{}, nil)
	_, err := neverbounce.NewClient(testKey, neverbounce.WithHTTPClient(hc))
	require.NoError(t, err)
	assert.Nil(t, hc.Transport, "the caller's client is never modified")
	assert.Nil(t, hc.CheckRedirect)
	assert.Nil(t, hc.Jar)
	caller.assertUntouched(t)
}

// rpContextKey keys a context value, so that the test can see a context pass
// through untouched.
type rpContextKey struct{}

// TestRedirectGuardKeepsRequestContext checks that the guard passes the
// SDK's request on as it is: its context, and so the call's deadline, reach
// the caller's transport.
func TestRedirectGuardKeepsRequestContext(t *testing.T) {
	var got any
	var deadline bool
	rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Context().Value(rpContextKey{})
		_, deadline = r.Context().Deadline()
		testutil.Drain(r)
		return testutil.Response(r, http.StatusOK, io.NopCloser(bytes.NewReader([]byte(checkOK))), int64(len(checkOK))), nil
	})
	c := newTransportClient(t, rt)
	ctx := context.WithValue(t.Context(), rpContextKey{}, rpLocationMarker)
	_, err := c.CheckEmail(ctx, checkRequest())
	require.NoError(t, err)
	assert.Equal(t, rpLocationMarker, got)
	assert.True(t, deadline, "the SDK's timeout reaches the transport")
}
