package neverbounce_test

import (
	"context"
	"crypto/tls"
	"maps"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// pvTraceRecorder collects everything a caller's httptrace hooks receive.
type pvTraceRecorder struct {
	mu      sync.Mutex
	calls   map[string]int
	seen    []string
	written []string // header field names, lowercased
}

// pvNewTraceRecorder returns an empty recorder.
func pvNewTraceRecorder() *pvTraceRecorder { return &pvTraceRecorder{calls: map[string]int{}} }

// record records one call of hook with the values it received.
func (r *pvTraceRecorder) record(hook string, values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[hook]++
	r.seen = append(r.seen, values...)
}

// snapshot returns the hooks called, with their counts, and every value seen.
func (r *pvTraceRecorder) snapshot() (calls map[string]int, seen, written []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls = make(map[string]int, len(r.calls))
	maps.Copy(calls, r.calls)
	return calls, slices.Clone(r.seen), slices.Clone(r.written)
}

// pvErrText returns err's text, or "" for nil.
func pvErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// pvMIMEText returns every name and value of h.
func pvMIMEText(h textproto.MIMEHeader) []string {
	var out []string
	for name, values := range h {
		out = append(append(out, name), values...)
	}
	return out
}

// trace returns a ClientTrace with every hook set, each recording what it
// receives.
func (r *pvTraceRecorder) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) { r.record("GetConn", hostPort) },
		GotConn: func(info httptrace.GotConnInfo) {
			r.record("GotConn", info.Conn.RemoteAddr().String(), info.Conn.LocalAddr().String())
		},
		PutIdleConn:          func(err error) { r.record("PutIdleConn", pvErrText(err)) },
		GotFirstResponseByte: func() { r.record("GotFirstResponseByte") },
		Got100Continue:       func() { r.record("Got100Continue") },
		Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
			r.record("Got1xxResponse", pvMIMEText(header)...)
			return nil
		},
		DNSStart: func(info httptrace.DNSStartInfo) { r.record("DNSStart", info.Host) },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			values := make([]string, 0, 1+len(info.Addrs))
			values = append(values, pvErrText(info.Err))
			for _, a := range info.Addrs {
				values = append(values, a.String())
			}
			r.record("DNSDone", values...)
		},
		ConnectStart: func(network, addr string) { r.record("ConnectStart", network, addr) },
		ConnectDone:  func(network, addr string, err error) { r.record("ConnectDone", network, addr, pvErrText(err)) },
		TLSHandshakeStart: func() {
			r.record("TLSHandshakeStart")
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			r.record("TLSHandshakeDone", cs.ServerName, cs.NegotiatedProtocol, pvErrText(err))
		},
		WroteHeaderField: func(key string, value []string) {
			r.record("WroteHeaderField", append([]string{key}, value...)...)
			r.mu.Lock()
			r.written = append(r.written, strings.ToLower(key))
			r.mu.Unlock()
		},
		WroteHeaders:    func() { r.record("WroteHeaders") },
		Wait100Continue: func() { r.record("Wait100Continue") },
		WroteRequest:    func(info httptrace.WroteRequestInfo) { r.record("WroteRequest", pvErrText(info.Err)) },
	}
}

// pvHookPointers returns the code pointer of every hook of trace, to detect a
// hook that was replaced.
func pvHookPointers(trace *httptrace.ClientTrace) []uintptr {
	v := reflect.ValueOf(trace).Elem()
	out := make([]uintptr, v.NumField())
	for i := range out {
		if f := v.Field(i); f.Kind() == reflect.Func && !f.IsNil() {
			out[i] = f.Pointer()
		}
	}
	return out
}

// pvEarlyHints answers with a 103 Early Hints response, then 200 with body.
func pvEarlyHints(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</hints.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("Content-Type", mediaJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// TestTraceNeverSeesSecrets proves that a caller's httptrace hooks, which
// tracing libraries use to record connections and request headers, never
// receive the API key, the address, row data or any other request value, over
// HTTP/1.1 and HTTP/2: those travel only in the body. Every hook that applies
// still runs, and the caller's context and trace are left as they were.
func TestTraceNeverSeesSecrets(t *testing.T) {
	ops := []struct {
		name   string
		answer string
		run    func(ctx context.Context, c *neverbounce.Client) error
	}{
		{pvOpCheck, checkOK, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: pvEmail, AddressInfo: true})
			return err
		}},
		{"CreateJob", `{"status":"success","job_id":42}`, func(ctx context.Context, c *neverbounce.Client) error {
			_, err := c.CreateJob(ctx, pvCreateRequest())
			return err
		}},
	}
	for _, proto := range testutil.Protocols() {
		for _, op := range ops {
			t.Run(proto.Name()+"/"+op.name, func(t *testing.T) {
				c, srv := newServerClientProto(t, proto, pvEarlyHints(op.answer))
				rec := pvNewTraceRecorder()
				trace := rec.trace()
				before := pvHookPointers(trace)
				type ctxKey struct{}
				ctx := httptrace.WithClientTrace(context.WithValue(t.Context(), ctxKey{}, "caller value"), trace)

				require.NoError(t, op.run(ctx, c))

				calls, seen, written := rec.snapshot()
				for _, hook := range []string{
					"GetConn", "GotConn", "ConnectStart", "ConnectDone", "TLSHandshakeStart", "TLSHandshakeDone",
					"WroteHeaderField", "WroteHeaders", "WroteRequest", "Got1xxResponse", "GotFirstResponseByte",
				} {
					assert.Positive(t, calls[hook], "hook %s still runs", hook)
				}
				testutil.AssertRedacted(t, strings.Join(seen, "\n"), pvSecrets(), "trace hooks")
				assert.Contains(t, written, "content-type")
				assert.Contains(t, written, "user-agent")
				assert.NotContains(t, written, "authorization")
				assert.NotContains(t, written, "cookie")

				r := testutil.OnlyRequest(t, srv)
				assert.Equal(t, string(proto), r.Proto)
				assert.True(t, strings.HasPrefix(string(r.Body), keyMember), "the key travels in the body")

				assert.Equal(t, before, pvHookPointers(trace), "the caller's hooks are never replaced")
				assert.Same(t, trace, httptrace.ContextClientTrace(ctx), "the caller's context keeps its trace")
				assert.Equal(t, "caller value", ctx.Value(ctxKey{}))
				require.NoError(t, ctx.Err(), "the call never ends the caller's context")
			})
		}
	}
}

// TestTraceReachesTransportUnchanged proves that the request context a
// transport sees carries the caller's own trace and values, and the caller's
// cancellation.
func TestTraceReachesTransportUnchanged(t *testing.T) {
	type ctxKey struct{}
	trace := pvNewTraceRecorder().trace()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "caller value"))
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, trace)

	var gotValue any
	var gotTrace *httptrace.ClientTrace
	rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotValue = r.Context().Value(ctxKey{})
		gotTrace = httptrace.ContextClientTrace(r.Context())
		// Cancel only once the request is in flight, so the cancellation must
		// travel through the context the SDK derived.
		cancel()
		return testutil.BlockUntilDone(r)
	})
	_, err := newTransportClient(t, rt).CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: pvEmail})
	assertCallError(t, err, wantCall{Kind: neverbounce.FailureCanceled, Op: neverbounce.OpCheckEmail}, pvEmail)
	assert.Equal(t, "caller value", gotValue)
	assert.Same(t, trace, gotTrace, "the caller's trace is passed on as it is")
}
