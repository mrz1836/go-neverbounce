package neverbounce_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// exCCHandler answers each operation from what the request itself carries, so
// that a response delivered to the wrong caller shows up as a mismatch:
// single/check echoes the email as address_info.original_email; jobs/create
// answers job_id N for the filename "cc-N"; jobs/status echoes the job ID as
// id and in the filename; jobs/results answers page P of 2 with one row whose
// id is "N-P"; jobs/download answers a CSV that names the job.
func exCCHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Filename string `json:"filename"`
		JobID    int64  `json:"job_id"`
		Page     int    `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", mediaJSON)
	switch r.URL.Path {
	case pathCheck:
		email, err := json.Marshal(req.Email)
		if err != nil {
			http.Error(w, "bad email", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","result":"valid","address_info":{"original_email":%s}}`, email)
	case pathCreate:
		id, err := strconv.ParseInt(strings.TrimPrefix(req.Filename, "cc-"), 10, 64)
		if err != nil {
			http.Error(w, "bad filename", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","job_id":%d}`, id)
	case pathStatus:
		_, _ = fmt.Fprintf(w, `{"status":"success","id":%d,"job_status":"complete","filename":"cc-%d.csv",`+
			`"total":{"records":%d}}`, req.JobID, req.JobID, req.JobID%97)
	case pathResults:
		_, _ = fmt.Fprintf(w, `{"status":"success","total_results":2,"total_pages":2,`+
			`"results":[{"data":{"id":"%d-%d"},"verification":{"result":"valid"}}]}`, req.JobID, req.Page)
	case pathDownload:
		w.Header().Set("Content-Type", mediaCSV)
		_, _ = fmt.Fprintf(w, "id,email_status\r\n%d,valid\r\n", req.JobID)
	default:
		http.NotFound(w, r)
	}
}

// exCCWorker is one goroutine's share of TestConcurrencySharedClient: a job ID
// and an address of its own, the requests it expects to have sent, and what
// it received.
type exCCWorker struct {
	id    neverbounce.JobID
	email string
	sent  []string
	rows  []neverbounce.ResultRow
	file  bytes.Buffer
}

// run makes every call, from the one at index first on, so that concurrent
// workers interleave different operations.
func (w *exCCWorker) run(ctx context.Context, t *testing.T, c *neverbounce.Client, first int) {
	ops := []func(context.Context, *testing.T, *neverbounce.Client){
		w.check, w.create, w.status, w.results, w.download, w.wait, w.walk,
	}
	for i := range ops {
		ops[(first+i)%len(ops)](ctx, t, c)
	}
}

// expect records a request the worker makes, as "path body".
func (w *exCCWorker) expect(op neverbounce.Operation, members string) {
	w.sent = append(w.sent, "/v4.2/"+string(op)+" "+keyMember+members+"}")
}

// jobMember is the job_id member of the worker's job requests.
func (w *exCCWorker) jobMember() string { return `,"job_id":` + w.id.String() }

func (w *exCCWorker) check(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpCheckEmail, `,"email":"`+w.email+`","address_info":true,"credits_info":false`)
	res, err := c.CheckEmail(ctx, neverbounce.CheckEmailRequest{Email: w.email, AddressInfo: true})
	if assert.NoError(t, err) && assert.NotNil(t, res.AddressInfo) {
		assert.Equal(t, w.email, res.AddressInfo.OriginalEmail)
	}
}

func (w *exCCWorker) create(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	name := "cc-" + w.id.String()
	w.expect(neverbounce.OpCreateJob, `,"input_location":"supplied","input":[{"id":"`+w.id.String()+
		`","email":"`+w.email+`"}],"filename":"`+name+`","auto_parse":true,"auto_start":false,`+
		`"run_sample":false,"allow_manual_review":false`)
	res, err := c.CreateJob(ctx, neverbounce.CreateJobRequest{
		Supplied: neverbounce.SuppliedList{Columns: []string{"id", "email"}, Rows: [][]string{{w.id.String(), w.email}}},
		Filename: name, AutoParse: true,
	})
	if assert.NoError(t, err) {
		assert.Equal(t, w.id, res.JobID)
	}
}

func (w *exCCWorker) status(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpJobStatus, w.jobMember())
	st, err := c.JobStatus(ctx, neverbounce.JobStatusRequest{JobID: w.id})
	if assert.NoError(t, err) && assert.NotNil(t, st.Totals.Records) {
		assert.Equal(t, "cc-"+w.id.String()+".csv", st.Filename)
		assert.Equal(t, int64(w.id)%97, *st.Totals.Records)
	}
}

func (w *exCCWorker) results(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpJobResults, w.jobMember()+`,"page":1,"items_per_page":1`)
	page, err := c.JobResults(ctx, neverbounce.JobResultsRequest{JobID: w.id, Page: 1, ItemsPerPage: 1})
	if assert.NoError(t, err) && assert.Len(t, page.Results, 1) {
		id, _ := page.Results[0].Column("id")
		assert.Equal(t, w.id.String()+"-1", id)
	}
}

func (w *exCCWorker) download(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpDownloadJob, w.jobMember()+`,"email_status":true,"line_feed_type":"LINEFEED_0D0A"`)
	n, err := c.DownloadJob(ctx, neverbounce.DownloadJobRequest{
		JobID: w.id, Append: neverbounce.DownloadColumns{EmailStatus: new(true)}, LineFeed: neverbounce.LineFeedCRLF,
	}, &w.file)
	want := "id,email_status\r\n" + w.id.String() + ",valid\r\n"
	if assert.NoError(t, err) {
		assert.Equal(t, int64(len(want)), n)
		assert.Equal(t, want, w.file.String())
	}
}

func (w *exCCWorker) wait(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpJobStatus, w.jobMember())
	st, err := c.WaitForJob(ctx, neverbounce.WaitForJobRequest{JobID: w.id, MaxWait: time.Minute})
	if assert.NoError(t, err) {
		assert.Equal(t, w.id, st.ID)
		assert.Equal(t, neverbounce.JobStateComplete, st.State)
	}
}

func (w *exCCWorker) walk(ctx context.Context, t *testing.T, c *neverbounce.Client) {
	w.expect(neverbounce.OpJobResults, w.jobMember()+`,"page":1,"items_per_page":1`)
	w.expect(neverbounce.OpJobResults, w.jobMember()+`,"page":2,"items_per_page":1`)
	var ids []string
	for page, err := range c.JobResultPages(ctx, neverbounce.JobResultPagesRequest{
		JobID: w.id, ItemsPerPage: 1, MaxPages: 5,
	}) {
		if assert.NoError(t, err) && assert.Len(t, page.Results, 1) { // an error ends the walk
			id, _ := page.Results[0].Column("id")
			ids = append(ids, id)
			w.rows = append(w.rows, page.Results[0])
		}
	}
	assert.Equal(t, []string{w.id.String() + "-1", w.id.String() + "-2"}, ids)
}

// exTransportSettings is the configuration of an *http.Transport that a
// client built on it must never change.
type exTransportSettings struct {
	tlsConfig       *tls.Config
	rootCAs         *x509.CertPool
	nextProtos      []string
	insecure        bool
	proxySet        bool
	dialSet         bool
	dialTLSSet      bool
	forceHTTP2      bool
	protocols       string
	keepAlivesOff   bool
	compressionOff  bool
	maxIdle         int
	maxIdlePerHost  int
	maxConnsPerHost int
	idleTimeout     time.Duration
	headerBytes     int64
	tlsNextProto    int
}

// exClientState is what a Client built on an injected *http.Client must
// never change in it.
type exClientState struct {
	transport *http.Transport
	redirect  bool
	jar       http.CookieJar
	timeout   time.Duration
	settings  exTransportSettings
}

// exStateOf captures hc's state. hc must use an *http.Transport.
func exStateOf(tb testing.TB, hc *http.Client) exClientState {
	tb.Helper()
	tr, ok := hc.Transport.(*http.Transport)
	require.True(tb, ok, "want an *http.Transport, got %T", hc.Transport)
	// net/http fills in its HTTP/2 defaults (TLSClientConfig.NextProtos and
	// TLSNextProto) once, on the transport's first use, whoever uses it.
	// Clone runs that same step first, so the state holds only what the SDK
	// could change.
	_ = tr.Clone()
	s := exTransportSettings{
		tlsConfig:       tr.TLSClientConfig,
		proxySet:        tr.Proxy != nil,
		dialSet:         tr.DialContext != nil,
		dialTLSSet:      tr.DialTLSContext != nil,
		forceHTTP2:      tr.ForceAttemptHTTP2,
		keepAlivesOff:   tr.DisableKeepAlives,
		compressionOff:  tr.DisableCompression,
		maxIdle:         tr.MaxIdleConns,
		maxIdlePerHost:  tr.MaxIdleConnsPerHost,
		maxConnsPerHost: tr.MaxConnsPerHost,
		idleTimeout:     tr.IdleConnTimeout,
		headerBytes:     tr.MaxResponseHeaderBytes,
		tlsNextProto:    len(tr.TLSNextProto),
	}
	if tr.Protocols != nil {
		s.protocols = tr.Protocols.String()
	}
	if cfg := tr.TLSClientConfig; cfg != nil {
		s.rootCAs = cfg.RootCAs
		s.nextProtos = slices.Clone(cfg.NextProtos)
		s.insecure = cfg.InsecureSkipVerify
	}
	return exClientState{
		transport: tr, redirect: hc.CheckRedirect != nil, jar: hc.Jar, timeout: hc.Timeout, settings: s,
	}
}

// exAssertSent asserts that srv received exactly the requests the workers
// expect, each once, over proto.
func exAssertSent(tb testing.TB, srv *testutil.Server, proto testutil.Protocol, workers []*exCCWorker) {
	tb.Helper()
	var want []string
	for _, w := range workers {
		want = append(want, w.sent...)
	}
	reqs := srv.Requests()
	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.Path+" "+string(r.Body))
		assert.Equal(tb, proto, testutil.Protocol(r.Proto))
		assert.Equal(tb, int64(len(r.Body)), r.ContentLength)
	}
	slices.Sort(want)
	slices.Sort(got)
	assert.Len(tb, got, len(workers)*8, "one request per call, two for the page walk")
	assert.Equal(tb, want, got, "each request carries exactly its caller's body, sent once")
}

// exAssertUnshared overwrites the first worker's first row and checks that
// every other row the workers received is as it was: results share no
// memory, with each other or with a response.
func exAssertUnshared(tb testing.TB, workers []*exCCWorker) {
	tb.Helper()
	first := workers[0]
	require.Len(tb, first.rows, 2)
	for i := range first.rows[0].Data {
		first.rows[0].Data[i] = ' '
	}
	id, _ := first.rows[1].Column("id")
	assert.Equal(tb, first.id.String()+"-2", id, "the same caller's other page")
	for _, w := range workers[1:] {
		for i, row := range w.rows {
			got, _ := row.Column("id")
			assert.Equal(tb, w.id.String()+"-"+strconv.Itoa(i+1), got)
		}
	}
}

// TestConcurrencySharedClient shares one Client across 64 goroutines, each
// running every operation with a job ID and an address of its own, in a
// rotated order, against a loopback TLS server over HTTP/1.1 and HTTP/2. Run
// it with -race. Every caller must get its own answer, every request must
// carry exactly its caller's body, and the injected *http.Client and its
// transport must be unchanged afterwards.
func TestConcurrencySharedClient(t *testing.T) {
	const goroutines = 64
	for _, proto := range testutil.Protocols() {
		t.Run(proto.Name(), func(t *testing.T) {
			srv := testutil.NewServerProto(t, proto, exCCHandler)
			hc := srv.Client()
			before := exStateOf(t, hc)

			c := newClient(t, neverbounce.WithTestBaseURL(srv.URL()), neverbounce.WithHTTPClient(hc))
			workers := make([]*exCCWorker, goroutines)
			var wg sync.WaitGroup
			for g := range goroutines {
				w := &exCCWorker{id: neverbounce.JobID(1000 + g), email: fmt.Sprintf("cc-%02d@example.com", g)}
				workers[g] = w
				wg.Go(func() { w.run(t.Context(), t, c, g) })
			}
			wg.Wait()

			exAssertSent(t, srv, proto, workers)
			exAssertUnshared(t, workers)
			after := exStateOf(t, hc)
			assert.Same(t, before.transport, hc.Transport, "the transport is wrapped in a copy, never replaced")
			assert.Same(t, before.settings.tlsConfig, after.settings.tlsConfig)
			assert.Same(t, before.settings.rootCAs, after.settings.rootCAs)
			assert.Equal(t, before, after, "the injected client and its transport are untouched")
		})
	}
}

// exSnapshotCase is one TestConcurrencyRequestSnapshot case. setup returns
// the call, which sends a request held by the case, and mutate, which changes
// every slice, map and pointed-to value that request shares with its caller.
type exSnapshotCase struct {
	name        string
	contentType string
	answer      string
	setup       func() (call func(context.Context, *neverbounce.Client) error, mutate func())
	want        string
}

// exSnapshotCases returns the TestConcurrencyRequestSnapshot cases.
func exSnapshotCases() []exSnapshotCase {
	return []exSnapshotCase{
		{
			name:        "create",
			contentType: mediaJSON,
			answer:      `{"status":"success","job_id":7}`,
			setup: func() (func(context.Context, *neverbounce.Client) error, func()) {
				req := neverbounce.CreateJobRequest{
					Supplied: neverbounce.SuppliedList{
						Columns: []string{"id", "email"},
						Rows:    [][]string{{"1001", testEmail}, {"1002", "blake.example@example.org"}},
					},
					Filename:               "snapshot",
					LeverageHistoricalData: new(false),
					CallbackURL:            "https://hooks.example.com/neverbounce",
					CallbackHeaders:        map[string]string{"Authorization": "Bearer example0000", "X-Trace": "t1"},
				}
				call := func(ctx context.Context, c *neverbounce.Client) error {
					_, err := c.CreateJob(ctx, req)
					return err
				}
				mutate := func() {
					req.Supplied.Columns[0] = "changed"
					req.Supplied.Rows[0][1] = "changed@example.com"
					req.Supplied.Rows[1] = []string{"x", "y"}
					req.CallbackHeaders["X-Trace"] = "changed"
					req.CallbackHeaders["X-Added"] = "added"
					delete(req.CallbackHeaders, "Authorization")
					*req.LeverageHistoricalData = true
				}
				return call, mutate
			},
			want: `,"input_location":"supplied","input":[{"id":"1001","email":"` + testEmail + `"},` +
				`{"id":"1002","email":"blake.example@example.org"}],"filename":"snapshot","auto_parse":false,` +
				`"auto_start":false,"run_sample":false,"allow_manual_review":false,` +
				`"request_meta_data":{"leverage_historical_data":false},` +
				`"callback_url":"https://hooks.example.com/neverbounce",` +
				`"callback_headers":{"Authorization":"Bearer example0000","X-Trace":"t1"}}`,
		},
		{
			name:        "check",
			contentType: mediaJSON,
			answer:      checkOK,
			setup: func() (func(context.Context, *neverbounce.Client) error, func()) {
				req := neverbounce.CheckEmailRequest{Email: testEmail, LeverageHistoricalData: new(false)}
				call := func(ctx context.Context, c *neverbounce.Client) error {
					_, err := c.CheckEmail(ctx, req)
					return err
				}
				return call, func() { *req.LeverageHistoricalData = true }
			},
			want: `,"email":"` + testEmail + `","address_info":false,"credits_info":false,` +
				`"request_meta_data":{"leverage_historical_data":false}}`,
		},
		{
			name:        "download",
			contentType: mediaCSV,
			answer:      "id\r\n7\r\n",
			setup: func() (func(context.Context, *neverbounce.Client) error, func()) {
				req := neverbounce.DownloadJobRequest{
					JobID:    7,
					Segments: neverbounce.DownloadSegments{Valids: new(true), OnlyDuplicates: new(false)},
					Append:   neverbounce.DownloadColumns{EmailStatus: new(true)},
				}
				call := func(ctx context.Context, c *neverbounce.Client) error {
					_, err := c.DownloadJob(ctx, req, io.Discard)
					return err
				}
				mutate := func() {
					*req.Segments.Valids = false
					*req.Segments.OnlyDuplicates = true
					*req.Append.EmailStatus = false
				}
				return call, mutate
			},
			want: `,"job_id":7,"valids":true,"only_duplicates":false,"email_status":true}`,
		},
	}
}

// TestConcurrencyRequestSnapshot proves that a request is encoded before any
// I/O, so the bytes sent are fixed by the time the transport runs. Changing a
// request's slices, maps or pointed-to values while its call runs is, in
// general, a data race the caller must avoid. This test therefore makes the
// change from inside the transport, after encoding, on the caller's own
// goroutine (http.Client calls RoundTrip synchronously), so there is no race,
// and checks that the body sent still holds the original values.
func TestConcurrencyRequestSnapshot(t *testing.T) {
	for _, tt := range exSnapshotCases() {
		t.Run(tt.name, func(t *testing.T) {
			call, mutate := tt.setup()
			var sent []testutil.Request
			rt := testutil.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				mutate() // the caller's values change while the request is in flight
				sent = append(sent, testutil.Record(r))
				resp := testutil.Response(r, http.StatusOK, io.NopCloser(strings.NewReader(tt.answer)),
					int64(len(tt.answer)))
				resp.Header.Set("Content-Type", tt.contentType)
				return resp, nil
			})
			require.NoError(t, call(t.Context(), newTransportClient(t, rt)))
			require.Len(t, sent, 1)
			assertBody(t, keyMember+tt.want, sent[0].Body)
			assert.Equal(t, int64(len(sent[0].Body)), sent[0].ContentLength)
		})
	}
}
