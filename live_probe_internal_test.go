//go:build live

package neverbounce

// The live contract probe makes real calls to https://api.neverbounce.com,
// the provider's production API (there is no sandbox), to observe what the
// v4.2 documentation leaves open: error envelopes and status codes, the
// download error format and Content-Type, the shapes of check, create and
// status answers, and timestamp nulls. It never runs unless the repository
// owner has approved the run: the tier, the operations, the call count, the
// address used, the credit budget and the job it leaves behind. CI and
// automation never run it, and the live build tag keeps it out of every
// normal build.
//
// What each call costs:
//
// TestLiveProbeTier0 is free and read-only: 4 calls, nothing billed, nothing
// created.
//   - jobs/status with a deliberately invalid synthetic key, so the provider
//     refuses it (auth_failure expected).
//   - jobs/status, jobs/results and jobs/download with the real key for a job
//     ID that does not exist in the account.
//
// TestLiveProbeTier1 is PAID and PERSISTENT: at most 43 calls.
//   - single/check on the approved address: 1 credit, billed.
//   - jobs/create of a 2-row list (the approved address and a reserved
//     example.com address) with auto_parse and run_sample, auto_start false:
//     the sample is free, but the job PERSISTS in the account; the probe never
//     deletes it. Its filename, logged, starts go-neverbounce-live-probe-.
//   - jobs/status polls by WaitForJob, every 15 seconds for at most 10
//     minutes (at most 41 calls): free.
//
// Tier 0 skips unless all of these hold:
//
//	NEVERBOUNCE_LIVE=1
//	NEVERBOUNCE_LIVE_KEY=<the account's API key>
//	CI is unset
//
// and reads NEVERBOUNCE_LIVE_MISSING_JOB_ID=<a job ID that does not exist>,
// by default a made-up ID. Tier 1 additionally requires:
//
//	NEVERBOUNCE_LIVE_ACK_BILLING=yes-i-accept-charges
//	NEVERBOUNCE_LIVE_EMAIL=<the address approved for one billed check>
//
// Run one tier at a time, by hand:
//
//	go test -tags live -count=1 -p 1 -run 'TestLiveProbeTier0$' -v .
//	go test -tags live -count=1 -p 1 -run 'TestLiveProbeTier1$' -v .
//
// Calls are never retried, and a rate-limit answer stops the remaining calls.
// The output is shape only, recorded by a transport wrapped around the SDK's
// own: per response, the HTTP status, the media type, the header names, the
// JSON type tree with safe key names (testutil.JSONShape), and the SDK's error
// text, which never holds values. No key, address, job ID, header value,
// provider message or other response value is ever printed or written.

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Live probe settings.
const (
	// bzLiveInvalidKey is a well-formed key no account has.
	bzLiveInvalidKey = "secret_liveprobeinvalidkey0000000000000000" // gitleaks:allow (synthetic test marker)

	// bzLiveMissingJobID is the default job ID that does not exist: made up,
	// large, and within 32 bits in case the provider types it so.
	bzLiveMissingJobID = 2_100_000_001

	// bzLiveSecondAddress is the second row of the Tier 1 list, on a reserved
	// domain.
	bzLiveSecondAddress = "live-probe.nobody@example.com"

	// bzLiveShapeBytes is how much of a response the recorder reads to take
	// its shape; anything longer is passed on unread and unshaped.
	bzLiveShapeBytes = 64 << 10

	bzLiveTimeout      = 45 * time.Second
	bzLiveCheckTimeout = 20 * time.Second
	bzLiveInterval     = 15 * time.Second
	bzLiveMaxWait      = 10 * time.Minute
)

// bzLiveSettings are the probe's inputs, read from the environment only here.
type bzLiveSettings struct {
	key       string
	missingID JobID
}

// bzLiveEnv returns the settings, or skips t unless the owner enabled the
// probe: NEVERBOUNCE_LIVE=1, NEVERBOUNCE_LIVE_KEY set, CI unset. It fails,
// before anything is sent, on a malformed setting.
func bzLiveEnv(t *testing.T) bzLiveSettings {
	t.Helper()
	if _, ci := os.LookupEnv("CI"); ci {
		t.Skip("live probes never run in CI")
	}
	if os.Getenv("NEVERBOUNCE_LIVE") != "1" {
		t.Skip("live probes are opt-in; see the comment at the top of live_probe_internal_test.go")
	}
	key := os.Getenv("NEVERBOUNCE_LIVE_KEY")
	if key == "" {
		t.Skip("NEVERBOUNCE_LIVE_KEY is not set")
	}
	// The error names the setting, never its value.
	require.NoError(t, checkKey(key), "NEVERBOUNCE_LIVE_KEY is not a valid key; nothing was sent")
	s := bzLiveSettings{key: key, missingID: bzLiveMissingJobID}
	if v := os.Getenv("NEVERBOUNCE_LIVE_MISSING_JOB_ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		require.True(t, err == nil && n > 0, "NEVERBOUNCE_LIVE_MISSING_JOB_ID must be a positive integer; nothing was sent")
		s.missingID = JobID(n)
	}
	return s
}

func TestLiveProbeTier0(t *testing.T) {
	s := bzLiveEnv(t)
	ctx := t.Context()
	missing := s.missingID

	bad := bzNewLiveClient(t, bzLiveInvalidKey)
	_, err := bad.client.JobStatus(ctx, JobStatusRequest{JobID: missing})
	bad.report(t, "jobs/status, invalid key", err)
	if bzLiveStop(t, err) {
		return
	}
	require.ErrorIs(t, err, ErrUnauthorized, "an invalid key must be refused; no further calls are made")

	lc := bzNewLiveClient(t, s.key)
	_, err = lc.client.JobStatus(ctx, JobStatusRequest{JobID: missing})
	lc.report(t, "jobs/status, missing job", err)
	bzLiveExpectFailure(t, "jobs/status", err)
	if bzLiveStop(t, err) {
		return
	}

	_, err = lc.client.JobResults(ctx, JobResultsRequest{JobID: missing, Page: 1, ItemsPerPage: 10})
	lc.report(t, "jobs/results, missing job", err)
	bzLiveExpectFailure(t, "jobs/results", err)
	if bzLiveStop(t, err) {
		return
	}

	var w bzLiveCountingWriter
	n, err := lc.client.DownloadJob(ctx, DownloadJobRequest{JobID: missing, LineFeed: LineFeedCRLF}, &w)
	lc.report(t, "jobs/download, missing job", err)
	t.Logf("jobs/download, missing job: %d bytes written (writer saw %d)", n, w.n)
	bzLiveExpectFailure(t, "jobs/download", err)
}

func TestLiveProbeTier1(t *testing.T) {
	s := bzLiveEnv(t)
	if os.Getenv("NEVERBOUNCE_LIVE_ACK_BILLING") != "yes-i-accept-charges" {
		t.Skip("Tier 1 is billed and leaves a job behind: set NEVERBOUNCE_LIVE_ACK_BILLING once the owner approves")
	}
	email := os.Getenv("NEVERBOUNCE_LIVE_EMAIL")
	if email == "" {
		t.Skip("NEVERBOUNCE_LIVE_EMAIL is not set")
	}
	ctx := t.Context()
	lc := bzNewLiveClient(t, s.key)

	// Exactly one billed check (1 credit).
	_, err := lc.client.CheckEmail(ctx, CheckEmailRequest{
		Email: email, AddressInfo: true, CreditsInfo: true, Timeout: bzLiveCheckTimeout,
	})
	_, local := errors.AsType[*ValidationError](err)
	require.False(t, local, "NEVERBOUNCE_LIVE_EMAIL fails validation (%v); nothing was sent", err)
	lc.report(t, "single/check", err)
	if err != nil {
		t.Log("single/check failed: stopping before any job is created")
		return
	}

	// One sample-only job: free, but persistent.
	filename := "go-neverbounce-live-probe-" + time.Now().UTC().Format("20060102T150405Z")
	created, err := lc.client.CreateJob(ctx, CreateJobRequest{
		Supplied:  EmailList(email, bzLiveSecondAddress),
		Filename:  filename,
		AutoParse: true,
		RunSample: true,
	})
	lc.report(t, "jobs/create", err)
	if err != nil {
		t.Logf("jobs/create failed; a job may exist anyway: look for filename %s in the dashboard", filename)
		return
	}
	t.Logf("jobs/create: a job named %s now persists in the account", filename)

	st, err := lc.client.WaitForJob(ctx, WaitForJobRequest{
		JobID: created.JobID, Interval: bzLiveInterval, MaxWait: bzLiveMaxWait,
	})
	lc.report(t, "jobs/status, WaitForJob", err)
	if st != nil {
		state := "<undocumented>"
		if st.State.Known() {
			state = string(st.State)
		}
		t.Logf("jobs/status, WaitForJob: last state %s", state)
	}
}

// bzLiveExpectFailure marks the test failed, and lets the probe go on, when a
// call for a job that does not exist succeeded.
func bzLiveExpectFailure(t *testing.T, label string, err error) {
	t.Helper()
	assert.Error(t, err, "%s: a job that does not exist was answered with success", label)
}

// bzLiveStop reports whether the probe must stop: after a rate limit, so no
// further call adds to it.
func bzLiveStop(t *testing.T, err error) bool {
	t.Helper()
	if errors.Is(err, ErrRateLimited) {
		t.Log("rate limited: stopping; no further calls are made")
		return true
	}
	return false
}

// bzLiveClient is a client whose responses a bzLiveRecorder sees.
type bzLiveClient struct {
	client *Client
	rec    *bzLiveRecorder
}

// bzNewLiveClient returns a client for key on the production origin, over the
// SDK's own transport wrapped by a recorder.
func bzNewLiveClient(t *testing.T, key string) *bzLiveClient {
	t.Helper()
	rec := &bzLiveRecorder{next: newTransport()}
	c, err := NewClient(key,
		WithHTTPClient(&http.Client{Transport: rec}),
		WithTimeout(bzLiveTimeout),
		WithDownloadTimeout(2*time.Minute),
		WithMaxDownloadBytes(1<<20),
	)
	require.NoError(t, err, "the client cannot be built; nothing was sent")
	return &bzLiveClient{client: c, rec: rec}
}

// report logs the shapes recorded since the last report, each run of equal
// shapes once with its count, and the SDK's outcome, whose error text holds
// no values.
func (lc *bzLiveClient) report(t *testing.T, label string, err error) {
	t.Helper()
	shapes := lc.rec.take()
	for i := 0; i < len(shapes); {
		j := i + 1
		for j < len(shapes) && shapes[j] == shapes[i] {
			j++
		}
		t.Logf("%s: response x%d\n%s", label, j-i, shapes[i])
		i = j
	}
	if len(shapes) == 0 {
		t.Logf("%s: no response recorded", label)
	}
	if err != nil {
		t.Logf("%s: SDK outcome: %v", label, err)
		return
	}
	t.Logf("%s: SDK outcome: success", label)
}

// bzLiveRecorder passes requests to next and records a value-free description
// of each response, reading at most bzLiveShapeBytes of its body and handing
// the body on unchanged.
type bzLiveRecorder struct {
	next   http.RoundTripper
	mu     sync.Mutex
	shapes []string
}

// RoundTrip records the response to req.
func (r *bzLiveRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil || resp == nil {
		// The error may quote the URL or server bytes: record its absence only.
		r.add("  transport failure (error not printed)")
		return resp, err
	}
	var b strings.Builder
	b.WriteString("  path " + req.URL.Path + ", HTTP " + strconv.Itoa(resp.StatusCode))
	b.WriteString(", content type " + bzLiveMediaType(resp.Header.Values("Content-Type")))
	b.WriteString(", headers " + strings.Join(testutil.HeaderNames(resp.Header), " "))
	if resp.StatusCode >= http.StatusOK {
		head, rerr := io.ReadAll(io.LimitReader(resp.Body, bzLiveShapeBytes+1))
		rest := io.Reader(resp.Body)
		if rerr != nil {
			rest = bzLiveErrReader{err: rerr}
		}
		resp.Body = bzLiveBody{Reader: io.MultiReader(bytes.NewReader(head), rest), Closer: resp.Body}
		b.WriteString("\n  body: " + bzLiveBodyShape(head, rerr))
	}
	r.add(b.String())
	return resp, nil
}

// add records one description.
func (r *bzLiveRecorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shapes = append(r.shapes, s)
}

// take returns the descriptions recorded so far and forgets them.
func (r *bzLiveRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.shapes)
	r.shapes = r.shapes[:0]
	return out
}

// bzLiveBodyShape describes a body prefix without any of its values.
func bzLiveBodyShape(head []byte, rerr error) string {
	switch {
	case rerr != nil:
		return strconv.Itoa(len(head)) + " bytes, then a read error (not printed)"
	case len(head) > bzLiveShapeBytes:
		return "longer than " + strconv.Itoa(bzLiveShapeBytes) + " bytes; no shape taken"
	case len(head) == 0:
		return "empty"
	}
	shape, ok := testutil.JSONShape(head)
	if !ok {
		return strconv.Itoa(len(head)) + " bytes, not JSON"
	}
	return strconv.Itoa(len(head)) + " bytes of JSON:\n" + shape
}

// bzLiveMediaType describes Content-Type values: each media type with its
// parameter names, and a parameter value only when it is a short plain token
// such as a charset name.
func bzLiveMediaType(values []string) string {
	if len(values) == 0 {
		return "<none>"
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		media, params, err := mime.ParseMediaType(v)
		if err != nil {
			out = append(out, "<unparsable>")
			continue
		}
		parts := []string{media}
		for _, name := range slices.Sorted(maps.Keys(params)) {
			value := "<value>"
			if bzLivePlainToken(params[name]) {
				value = params[name]
			}
			parts = append(parts, name+"="+value)
		}
		out = append(out, strings.Join(parts, "; "))
	}
	return strings.Join(out, " | ")
}

// bzLivePlainToken reports whether s is 1 to 32 letters, digits, '.', '_' or
// '-'.
func bzLivePlainToken(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// bzLiveBody is a response body re-assembled after the recorder's read.
type bzLiveBody struct {
	io.Reader
	io.Closer
}

// bzLiveErrReader fails every read with err, replaying a read error the
// recorder met.
type bzLiveErrReader struct{ err error }

// Read returns the recorded error.
func (r bzLiveErrReader) Read([]byte) (int, error) { return 0, r.err }

// bzLiveCountingWriter counts and discards what a download writes.
type bzLiveCountingWriter struct{ n int64 }

// Write discards p.
func (w *bzLiveCountingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
