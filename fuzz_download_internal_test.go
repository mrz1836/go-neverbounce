package neverbounce

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// Declared-length modes of FuzzDownloadClassifier.
const (
	dlFuzzDeclared  = iota // Content-Length is the body's length
	dlFuzzUnknown          // no Content-Length
	dlFuzzTruncated        // Content-Length one past the body
	dlFuzzModes
)

// dlFuzzTypes returns the Content-Type values FuzzDownloadClassifier chooses
// from; "" means no header at all.
func dlFuzzTypes() []string {
	return []string{
		dlOctetMedia, "text/csv; charset=utf-8", "TEXT/CSV", dlEnvMedia,
		"application/json; charset=utf-8", "text/html", "",
	}
}

// dlOracleEnvelope reports, independently of the SDK's scanners, whether body
// is a provider envelope: one strict JSON object (valid UTF-8, nothing after
// it, no repeated key) whose top-level status member is a JSON string.
func dlOracleEnvelope(body []byte) bool {
	valid, dup := testutil.TokenWalk(body)
	if !valid || dup {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}
	raw, ok := top["status"]
	return ok && len(raw) > 0 && raw[0] == '"'
}

// dlFuzzResult is one DownloadJob run of FuzzDownloadClassifier.
type dlFuzzResult struct {
	n    int64
	err  error
	w    *testutil.CountingWriter
	body *testutil.Body
}

// dlFuzzDownload serves body in memory with the Content-Type ct and the
// declared-length mode, and runs DownloadJob into a counting writer.
func dlFuzzDownload(t *testing.T, body []byte, ct string, mode int) dlFuzzResult {
	t.Helper()
	declared := int64(len(body))
	switch mode {
	case dlFuzzUnknown:
		declared = -1
	case dlFuzzTruncated:
		declared++
	}
	h := http.Header{}
	if ct != "" {
		h["Content-Type"] = []string{ct}
	}
	rb := testutil.NewBody(string(body))
	ctr := &testutil.CountingTransport{Next: testutil.TransportWith(http.StatusOK, h, declared, rb)}
	c := newInternalClient(t, WithTestBaseURL("https://127.0.0.1"), WithHTTPClient(&http.Client{Transport: ctr}))
	w := &testutil.CountingWriter{}
	n, err := c.DownloadJob(t.Context(), DownloadJobRequest{JobID: 99}, w)
	require.Equal(t, 1, ctr.Attempts())
	return dlFuzzResult{n: n, err: err, w: w, body: rb}
}

// FuzzDownloadClassifier checks the download classifier on arbitrary bodies of
// at most the sniff window: a success streams exactly the body; an error
// before streaming leaves the writer empty; nothing panics; and for a CSV
// content type with an honest length, failing is exactly being an envelope.
func FuzzDownloadClassifier(f *testing.F) {
	seeds := []string{
		"", "x", "{", "id,email\r\n1,a@example.com\r\n", "{id},email\r\n", "\xef\xbb\xbfid\r\n", "\xff\xfe\x00",
		`{"status":"general_failure","message":"m"}`, " \r\n\t{\"status\":\"success\"} ", `{"status":"maintenance"}`,
		`{"status":1}`, `{"status":null}`, `{"message":"x"}`, `{"Status":"x"}`, `{"status":"a","status":"b"}`,
		testutil.Esc(`{"st~u0061tus":"temp_unavail"}`), testutil.Esc(`{"status":"x","~u0073tatus":"y"}`),
		`[{"status":"x"}]`, `{"status":"x"} trailing`, `{"a":{"status":"x"}}`, "{\"status\":\"x\xff\"}",
	}
	for i, s := range seeds {
		for ct := range len(dlFuzzTypes()) {
			f.Add([]byte(s), uint8(ct), uint8(i%dlFuzzModes))
		}
	}
	f.Fuzz(func(t *testing.T, body []byte, ctIndex, modeIndex uint8) {
		if len(body) > downloadSniffBytes {
			return
		}
		types := dlFuzzTypes()
		ct := types[int(ctIndex)%len(types)]
		mode := int(modeIndex) % dlFuzzModes
		res := dlFuzzDownload(t, body, ct, mode)
		n, err, w := res.n, res.err, res.w

		require.Equal(t, 1, res.body.Closes(), "the body is closed exactly once")
		require.Equal(t, int64(w.Len()), n, "the count is what the writer holds")
		require.True(t, bytes.HasPrefix(body, w.Bytes()), "the writer holds a prefix of the body")
		if err == nil {
			require.Equal(t, string(body), string(w.Bytes()), "a success streams the body unchanged")
			require.Equal(t, int64(len(body)), n)
		} else {
			ce := asCallError(t, err)
			require.Equal(t, n, ce.BytesWritten)
			require.Equal(t, JobID(99), ce.JobID)
			require.Zero(t, n, "every failure on a body within the sniff window happens before streaming")
		}

		media := mediaType(http.Header{"Content-Type": {ct}})
		switch {
		case media == dlEnvMedia:
			require.Error(t, err, "a JSON answer is never a file")
		case media != dlOctetMedia && media != dlCSVMedia:
			require.ErrorIs(t, err, ErrUnexpectedResponse)
		case mode == dlFuzzTruncated:
			require.ErrorIs(t, err, ErrTransport)
		default:
			envelope := dlOracleEnvelope(body)
			assert.Equal(t, envelope, err != nil, "a CSV answer fails exactly when it is an envelope")
			if err != nil {
				require.NotErrorIs(t, err, ErrTransport)
			}
		}
	})
}
