package neverbounce_test

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// jbStatusJob is the job the status tests read.
const jbStatusJob neverbounce.JobID = 1001

// jbStatusAnswer returns a jobs/status success answer for jbStatusJob in the
// state running, with the members extra appended.
func jbStatusAnswer(extra string) string {
	return `{"status":"success","id":1001,"job_status":"running"` + extra + `}`
}

// jbGetStatus reads the status of jbStatusJob: the call the status tests make
// through callOnce.
func jbGetStatus(ctx context.Context, c *neverbounce.Client) (*neverbounce.JobStatus, error) {
	return c.JobStatus(ctx, neverbounce.JobStatusRequest{JobID: jbStatusJob})
}

// jbStatusFailure returns the CallError of a jobs/status answer for
// jbStatusJob, received with HTTP 200, that fails to decode with kind at field.
func jbStatusFailure(kind neverbounce.FailureKind, field string) wantCall {
	return wantCall{Kind: kind, Op: neverbounce.OpJobStatus, Status: http.StatusOK, JobID: jbStatusJob, Field: field}
}

func TestJobStatusDecodesFixture(t *testing.T) {
	c, s := newScriptClient(t, testutil.Step{Body: string(testutil.Fixture(t, "synthetic/status_complete.json"))})
	st, err := c.JobStatus(t.Context(), neverbounce.JobStatusRequest{JobID: jbStatusJob})
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.JobStatus{
		ID:         jbStatusJob,
		State:      neverbounce.JobStateComplete,
		Filename:   "signups-example.csv",
		CreatedAt:  "2026-10-15 09:00:00",
		StartedAt:  "2026-10-15 09:00:12",
		FinishedAt: "2026-10-15 09:01:40",
		Totals: neverbounce.JobTotals{
			Records: new(int64(4)), Billable: new(int64(3)), Processed: new(int64(4)), Valid: new(int64(2)),
			Invalid: new(int64(1)), Catchall: new(int64(0)), Disposable: new(int64(0)), Unknown: new(int64(0)),
			Duplicates: new(int64(1)), BadSyntax: new(int64(1)),
		},
		BounceEstimate:  new(12.5),
		PercentComplete: new(100.0),
		ExecutionTime:   new(int64(31)),
	}, st)

	r := testutil.OnlyRequest(t, s)
	assertWireRequest(t, r, pathStatus, mediaJSON)
	assertBody(t, keyMember+`,"job_id":1001}`, r.Body)

	// A JSON round trip keeps the status.
	assertRoundTrip(t, st, `{"id":1001,"job_status":"complete","filename":"signups-example.csv",`+
		`"created_at":"2026-10-15 09:00:00","started_at":"2026-10-15 09:00:12","finished_at":"2026-10-15 09:01:40",`+
		`"total":{"records":4,"billable":3,"processed":4,"valid":2,"invalid":1,"catchall":0,"disposable":0,`+
		`"unknown":0,"duplicates":1,"bad_syntax":1},"bounce_estimate":12.5,"percent_complete":100,"execution_time":31}`)
}

// TestJobStatusIgnoresLegacyFlatShape reads an answer in the flat shape some
// provider examples show: its total_* members are not the v4.2 "total"
// object, so Totals stays unreported rather than guessed.
func TestJobStatusIgnoresLegacyFlatShape(t *testing.T) {
	st, err := callOnce(t, pathStatus, string(testutil.Fixture(t, "synthetic/status_legacy_flat.json")), jbGetStatus)
	require.NoError(t, err)
	assert.Equal(t, &neverbounce.JobStatus{
		ID:              jbStatusJob,
		State:           neverbounce.JobStateComplete,
		Filename:        "signups-legacy.csv",
		BounceEstimate:  new(12.5),
		PercentComplete: new(100.0),
		ExecutionTime:   new(int64(21)),
	}, st)
	assert.Equal(t, neverbounce.JobTotals{}, st.Totals)
}

func TestJobStatusTotalsPresence(t *testing.T) {
	for _, extra := range []string{``, `,"total":null`, `,"total":{}`, `,"Total":{"records":4}`, `,"total":{"Records":4}`} {
		t.Run("unreported "+extra, func(t *testing.T) {
			st, err := callOnce(t, pathStatus, jbStatusAnswer(extra), jbGetStatus)
			require.NoError(t, err)
			assert.Equal(t, neverbounce.JobTotals{}, st.Totals)
		})
	}

	t.Run("every member mapped to its own field", func(t *testing.T) {
		st, err := callOnce(t, pathStatus, jbStatusAnswer(`,"total":{"records":1,"billable":2,"processed":3,"valid":4,`+
			`"invalid":5,"catchall":6,"disposable":7,"unknown":8,"duplicates":9,"bad_syntax":10,"other":11}`), jbGetStatus)
		require.NoError(t, err)
		assert.Equal(t, neverbounce.JobTotals{
			Records: new(int64(1)), Billable: new(int64(2)), Processed: new(int64(3)), Valid: new(int64(4)),
			Invalid: new(int64(5)), Catchall: new(int64(6)), Disposable: new(int64(7)), Unknown: new(int64(8)),
			Duplicates: new(int64(9)), BadSyntax: new(int64(10)),
		}, st.Totals)
	})

	members := []struct {
		name string
		get  func(*neverbounce.JobTotals) *int64
	}{
		{"records", func(t *neverbounce.JobTotals) *int64 { return t.Records }},
		{"billable", func(t *neverbounce.JobTotals) *int64 { return t.Billable }},
		{"processed", func(t *neverbounce.JobTotals) *int64 { return t.Processed }},
		{"valid", func(t *neverbounce.JobTotals) *int64 { return t.Valid }},
		{"invalid", func(t *neverbounce.JobTotals) *int64 { return t.Invalid }},
		{"catchall", func(t *neverbounce.JobTotals) *int64 { return t.Catchall }},
		{"disposable", func(t *neverbounce.JobTotals) *int64 { return t.Disposable }},
		{"unknown", func(t *neverbounce.JobTotals) *int64 { return t.Unknown }},
		{"duplicates", func(t *neverbounce.JobTotals) *int64 { return t.Duplicates }},
		{"bad_syntax", func(t *neverbounce.JobTotals) *int64 { return t.BadSyntax }},
	}
	for _, m := range members {
		t.Run(m.name+" reported as zero, the rest absent", func(t *testing.T) {
			st, err := callOnce(t, pathStatus, jbStatusAnswer(`,"total":{"`+m.name+`":0}`), jbGetStatus)
			require.NoError(t, err)
			require.NotNil(t, m.get(&st.Totals), "zero is reported, not conflated with absent")
			assert.Equal(t, int64(0), *m.get(&st.Totals))
			for _, other := range members {
				if other.name != m.name {
					assert.Nil(t, other.get(&st.Totals), other.name)
				}
			}
		})
		t.Run(m.name+" null", func(t *testing.T) {
			st, err := callOnce(t, pathStatus, jbStatusAnswer(`,"total":{"`+m.name+`":null}`), jbGetStatus)
			require.NoError(t, err)
			assert.Nil(t, m.get(&st.Totals))
		})
		for _, bad := range []string{`"5"`, `1.5`, `-1`, `1e3`, `true`, `[]`, `9223372036854775808`} {
			t.Run(m.name+" "+bad, func(t *testing.T) {
				_, err := callOnce(t, pathStatus, jbStatusAnswer(`,"total":{"`+m.name+`":`+bad+`}`), jbGetStatus)
				assertCallError(t, err, jbStatusFailure(neverbounce.FailureMalformedResponse, "total."+m.name))
			})
		}
	}

	for _, bad := range []string{`[]`, `5`, `"records"`, `true`} {
		t.Run("total "+bad, func(t *testing.T) {
			_, err := callOnce(t, pathStatus, jbStatusAnswer(`,"total":`+bad), jbGetStatus)
			assertCallError(t, err, jbStatusFailure(neverbounce.FailureMalformedResponse, "total"))
		})
	}
}

func TestJobStatusFailureReason(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		want  neverbounce.JobFailureReason
		known bool
	}{
		{"absent", ``, "", false},
		{"json null", `,"failure_reason":null`, "", false},
		{"empty", `,"failure_reason":""`, "", false},
		{"unknown_file_encoding", `,"failure_reason":"unknown_file_encoding"`, neverbounce.JobFailureUnknownFileEncoding, true},
		{"empty_file", `,"failure_reason":"empty_file"`, neverbounce.JobFailureEmptyFile, true},
		{"file_too_large", `,"failure_reason":"file_too_large"`, neverbounce.JobFailureFileTooLarge, true},
		{"file_corrupt", `,"failure_reason":"file_corrupt"`, neverbounce.JobFailureFileCorrupt, true},
		{"undocumented token kept", `,"failure_reason":"quota_exceeded_2"`, "quota_exceeded_2", false},
		{"escaped token", testutil.Esc(`,"failure_reason":"empty~u005ffile"`), neverbounce.JobFailureEmptyFile, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := callOnce(t, pathStatus, `{"status":"success","id":1001,"job_status":"failed"`+tt.extra+`}`, jbGetStatus)
			require.NoError(t, err)
			assert.Equal(t, neverbounce.JobStateFailed, st.State)
			assert.Equal(t, tt.want, st.FailureReason)
			assert.Equal(t, tt.known, st.FailureReason.Known())
		})
	}

	bad := []struct {
		name  string
		extra string
		kind  neverbounce.FailureKind
	}{
		{"uppercase token", `,"failure_reason":"File_Corrupt"`, neverbounce.FailureUnexpectedResponse},
		{"spaced text", `,"failure_reason":"file corrupt"`, neverbounce.FailureUnexpectedResponse},
		{"65-byte token", `,"failure_reason":"` + strings.Repeat("x", 65) + `"`, neverbounce.FailureUnexpectedResponse},
		{"number", `,"failure_reason":3`, neverbounce.FailureMalformedResponse},
		{"object", `,"failure_reason":{}`, neverbounce.FailureMalformedResponse},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callOnce(t, pathStatus, `{"status":"success","id":1001,"job_status":"failed"`+tt.extra+`}`, jbGetStatus)
			assertCallError(t, err, jbStatusFailure(tt.kind, "failure_reason"))
		})
	}
}

func TestJobStatusTextKeptVerbatim(t *testing.T) {
	members := []struct {
		name string
		get  func(*neverbounce.JobStatus) string
	}{
		{"filename", func(s *neverbounce.JobStatus) string { return s.Filename }},
		{"created_at", func(s *neverbounce.JobStatus) string { return s.CreatedAt }},
		{"started_at", func(s *neverbounce.JobStatus) string { return s.StartedAt }},
		{"finished_at", func(s *neverbounce.JobStatus) string { return s.FinishedAt }},
	}
	values := []struct {
		name string
		raw  string
		want string
	}{
		{"provider format", `"2026-10-15 14:52:27"`, "2026-10-15 14:52:27"},
		{"with a zone", `"2026-10-15T14:52:27+02:00"`, "2026-10-15T14:52:27+02:00"},
		{"zero date", `"0000-00-00 00:00:00"`, "0000-00-00 00:00:00"},
		{"any text", `"yesterday, around noon"`, "yesterday, around noon"},
		{"escapes decoded", testutil.Esc(`"~u00e9t~u00e9 ~"q~" ~~"`), "\xc3\xa9t\xc3\xa9 \"q\" \\"},
		{"4096 bytes", `"` + strings.Repeat("t", 4096) + `"`, strings.Repeat("t", 4096)},
		{"empty", `""`, ""},
		{"json null", `null`, ""},
	}
	for _, m := range members {
		for _, v := range values {
			t.Run(m.name+" "+v.name, func(t *testing.T) {
				st, err := callOnce(t, pathStatus, jbStatusAnswer(`,"`+m.name+`":`+v.raw), jbGetStatus)
				require.NoError(t, err)
				assert.Equal(t, v.want, m.get(st))
			})
		}
		t.Run(m.name+" absent", func(t *testing.T) {
			st, err := callOnce(t, pathStatus, jbStatusAnswer(``), jbGetStatus)
			require.NoError(t, err)
			assert.Empty(t, m.get(st))
		})
		for _, bad := range []string{`1508334657`, `true`, `{}`, `["2026"]`} {
			t.Run(m.name+" "+bad, func(t *testing.T) {
				_, err := callOnce(t, pathStatus, jbStatusAnswer(`,"`+m.name+`":`+bad), jbGetStatus)
				assertCallError(t, err, jbStatusFailure(neverbounce.FailureMalformedResponse, m.name))
			})
		}
		t.Run(m.name+" over 4 KiB", func(t *testing.T) {
			_, err := callOnce(t, pathStatus, jbStatusAnswer(`,"`+m.name+`":"`+strings.Repeat("t", 4097)+`"`), jbGetStatus)
			assertCallError(t, err, jbStatusFailure(neverbounce.FailureResponseTooLarge, m.name))
		})
	}
}

func TestJobStatusPercentages(t *testing.T) {
	members := []struct {
		name string
		get  func(*neverbounce.JobStatus) *float64
	}{
		{"bounce_estimate", func(s *neverbounce.JobStatus) *float64 { return s.BounceEstimate }},
		{"percent_complete", func(s *neverbounce.JobStatus) *float64 { return s.PercentComplete }},
	}
	good := []struct {
		raw  string
		want *float64
	}{
		{`0`, new(0.0)},
		{`100`, new(100.0)},
		{`37.5`, new(37.5)},
		{`1e2`, new(100.0)},
		{`1E1`, new(10.0)},
		{`0.0001`, new(0.0001)},
		{`99.99999`, new(99.99999)},
		{`100.0`, new(100.0)},
		{`-0`, new(0.0)},
		{`-0.0`, new(0.0)},
		{`null`, nil},
	}
	for _, m := range members {
		for _, g := range good {
			t.Run(m.name+" "+g.raw, func(t *testing.T) {
				st, err := callOnce(t, pathStatus, jbStatusAnswer(`,"`+m.name+`":`+g.raw), jbGetStatus)
				require.NoError(t, err)
				got := m.get(st)
				assert.Equal(t, g.want, got)
				if got != nil {
					assert.False(t, math.Signbit(*got), "-0 decodes as +0")
				}
			})
		}
		t.Run(m.name+" absent", func(t *testing.T) {
			st, err := callOnce(t, pathStatus, jbStatusAnswer(``), jbGetStatus)
			require.NoError(t, err)
			assert.Nil(t, m.get(st))
		})
		for _, bad := range []string{`"50"`, `-0.1`, `100.1`, `-1`, `101`, `1e400`, `true`, `{}`, `[50]`} {
			t.Run(m.name+" "+bad, func(t *testing.T) {
				_, err := callOnce(t, pathStatus, jbStatusAnswer(`,"`+m.name+`":`+bad), jbGetStatus)
				assertCallError(t, err, jbStatusFailure(neverbounce.FailureMalformedResponse, m.name))
			})
		}
	}
}

func TestJobStatusID(t *testing.T) {
	t.Run("another job's answer", func(t *testing.T) {
		for _, id := range []string{`1002`, `1`, `10010`, `9223372036854775807`} {
			_, err := callOnce(t, pathStatus, `{"status":"success","id":`+id+`,"job_status":"complete"}`, jbGetStatus)
			assertCallError(t, err, jbStatusFailure(neverbounce.FailureUnexpectedResponse, "id"))
		}
	})
	for _, bad := range []struct{ name, member string }{
		{"missing", ``},
		{"json null", `"id":null,`},
		{"zero", `"id":0,`},
		{"negative", `"id":-1001,`},
		{"quoted", `"id":"1001",`},
		{"fraction", `"id":1001.0,`},
		{"exponent", `"id":1.001e3,`},
		{"overflow", `"id":9223372036854775808,`},
		{"object", `"id":{"id":1001},`},
		{"other case only", `"ID":1001,"Id":1001,`},
		{"job_id instead", `"job_id":1001,`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := callOnce(t, pathStatus, `{"status":"success",`+bad.member+`"job_status":"complete"}`, jbGetStatus)
			assertCallError(t, err, jbStatusFailure(neverbounce.FailureMalformedResponse, "id"))
		})
	}
}

func TestJobStatusState(t *testing.T) {
	for _, state := range []neverbounce.JobState{
		neverbounce.JobStateUploading, neverbounce.JobStateParsing, neverbounce.JobStateWaiting,
		neverbounce.JobStateWaitingAnalyzed, neverbounce.JobStateQueued, neverbounce.JobStateRunning,
		neverbounce.JobStateUnderReview, neverbounce.JobStateComplete, neverbounce.JobStateFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			st, err := callOnce(t, pathStatus, `{"status":"success","id":1001,"job_status":"`+string(state)+`"}`, jbGetStatus)
			require.NoError(t, err)
			assert.Equal(t, state, st.State)
			assert.True(t, st.State.Known())
		})
	}
	for _, token := range []string{"archived", "paused_2", strings.Repeat("z", 64)} {
		t.Run("undocumented "+token[:min(len(token), 12)], func(t *testing.T) {
			st, err := callOnce(t, pathStatus, `{"status":"success","id":1001,"job_status":"`+token+`"}`, jbGetStatus)
			require.NoError(t, err)
			assert.Equal(t, neverbounce.JobState(token), st.State, "kept as sent")
			assert.False(t, st.State.Known())
		})
	}
	bad := []struct {
		name   string
		member string
		kind   neverbounce.FailureKind
	}{
		{"missing", ``, neverbounce.FailureMalformedResponse},
		{"json null", `,"job_status":null`, neverbounce.FailureMalformedResponse},
		{"number", `,"job_status":3`, neverbounce.FailureMalformedResponse},
		{"array", `,"job_status":["complete"]`, neverbounce.FailureMalformedResponse},
		{"other case only", `,"Job_Status":"complete"`, neverbounce.FailureMalformedResponse},
		{"uppercase", `,"job_status":"COMPLETE"`, neverbounce.FailureUnexpectedResponse},
		{"empty", `,"job_status":""`, neverbounce.FailureUnexpectedResponse},
		{"hyphenated", `,"job_status":"under-review"`, neverbounce.FailureUnexpectedResponse},
		{"65 bytes", `,"job_status":"` + strings.Repeat("z", 65) + `"`, neverbounce.FailureUnexpectedResponse},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := callOnce(t, pathStatus, `{"status":"success","id":1001`+tt.member+`}`, jbGetStatus)
			assertCallError(t, err, jbStatusFailure(tt.kind, "job_status"))
		})
	}
}

func TestJobStateMethods(t *testing.T) {
	tests := []struct {
		state                                    neverbounce.JobState
		known, inProgress, needsAction, terminal bool
	}{
		{neverbounce.JobStateUploading, true, true, false, false},
		{neverbounce.JobStateParsing, true, true, false, false},
		{neverbounce.JobStateWaiting, true, false, true, false},
		{neverbounce.JobStateWaitingAnalyzed, true, false, true, false},
		{neverbounce.JobStateQueued, true, true, false, false},
		{neverbounce.JobStateRunning, true, true, false, false},
		{neverbounce.JobStateUnderReview, true, true, false, false},
		{neverbounce.JobStateComplete, true, false, false, true},
		{neverbounce.JobStateFailed, true, false, false, true},
		{"archived", false, false, false, false},
		{"Complete", false, false, false, false},
		{"", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			assert.Equal(t, tt.known, tt.state.Known(), "Known")
			assert.Equal(t, tt.inProgress, tt.state.InProgress(), "InProgress")
			assert.Equal(t, tt.needsAction, tt.state.NeedsAction(), "NeedsAction")
			assert.Equal(t, tt.terminal, tt.state.Terminal(), "Terminal")
		})
	}
	assert.Equal(t, "uploading parsing waiting waiting_analyzed queued running under_review complete failed",
		strings.Join([]string{
			string(neverbounce.JobStateUploading), string(neverbounce.JobStateParsing),
			string(neverbounce.JobStateWaiting), string(neverbounce.JobStateWaitingAnalyzed),
			string(neverbounce.JobStateQueued), string(neverbounce.JobStateRunning),
			string(neverbounce.JobStateUnderReview), string(neverbounce.JobStateComplete),
			string(neverbounce.JobStateFailed),
		}, " "), "the documented spellings")
}

func TestJobFailureReasonKnown(t *testing.T) {
	for _, r := range []neverbounce.JobFailureReason{
		neverbounce.JobFailureUnknownFileEncoding, neverbounce.JobFailureEmptyFile,
		neverbounce.JobFailureFileTooLarge, neverbounce.JobFailureFileCorrupt,
	} {
		assert.True(t, r.Known(), string(r))
	}
	assert.Equal(t, []string{"unknown_file_encoding", "empty_file", "file_too_large", "file_corrupt"}, []string{
		string(neverbounce.JobFailureUnknownFileEncoding), string(neverbounce.JobFailureEmptyFile),
		string(neverbounce.JobFailureFileTooLarge), string(neverbounce.JobFailureFileCorrupt),
	})
	for _, r := range []neverbounce.JobFailureReason{"", "file_missing", "EMPTY_FILE"} {
		assert.False(t, r.Known(), string(r))
	}
}
