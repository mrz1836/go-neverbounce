package neverbounce_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// hpRow returns a results row for record id.
func hpRow(id int) string {
	n := strconv.Itoa(id)
	return `{"data":{"id":"` + n + `","email":"row` + n + `@example.com"},` +
		`"verification":{"result":"valid","flags":["has_dns"]}}`
}

// hpResultsBody returns a jobs/results success body with the given totals and
// one row per id. Its query echo says page 0, as the provider's documented
// example does, whatever page was asked for.
func hpResultsBody(totalResults, totalPages int64, ids ...int) string {
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = hpRow(id)
	}
	return `{"status":"success","total_results":` + strconv.FormatInt(totalResults, 10) +
		`,"total_pages":` + strconv.FormatInt(totalPages, 10) +
		`,"query":{"job_id":42,"valids":1,"page":0,"items_per_page":10},"results":[` +
		strings.Join(rows, ",") + `],"execution_time":7}`
}

// hpResults returns hpResultsBody as a scripted step.
func hpResults(totalResults, totalPages int64, ids ...int) testutil.Step {
	return testutil.Step{Body: hpResultsBody(totalResults, totalPages, ids...)}
}

// hpPagesRequestBody returns the exact jobs/results request body for page and
// size.
func hpPagesRequestBody(page, size int) string {
	return keyMember + `,"job_id":42,"page":` + strconv.Itoa(page) + `,"items_per_page":` + strconv.Itoa(size) + `}`
}

// hpYield is one value a JobResultPages iterator yielded, with the number of
// calls made when it arrived.
type hpYield struct {
	page  *neverbounce.JobResultsPage
	err   error
	calls int
}

// hpCollect ranges over seq to the end and records every yield. It never
// breaks, so a yield after an error would be recorded too.
func hpCollect(seq iter.Seq2[*neverbounce.JobResultsPage, error], s *testutil.Script) []hpYield {
	var out []hpYield
	for p, err := range seq {
		out = append(out, hpYield{page: p, err: err, calls: s.Calls()})
	}
	return out
}

// hpAssertPages asserts that the first yields are error-free pages numbered
// pages, each yielded right after its own call (no prefetch).
func hpAssertPages(t *testing.T, ys []hpYield, pages ...int) {
	t.Helper()
	require.GreaterOrEqual(t, len(ys), len(pages), "yields")
	for i, want := range pages {
		y := ys[i]
		require.NoError(t, y.err, "yield %d", i)
		require.NotNil(t, y.page, "yield %d", i)
		assert.Equal(t, want, y.page.Page, "yield %d is the page the SDK requested", i)
		assert.Equal(t, i+1, y.calls, "yield %d arrives after exactly its own call", i)
	}
}

// hpAssertRequests asserts that s received one jobs/results request per page,
// in order, each keeping the wire policy and asking for that page with size
// items.
func hpAssertRequests(t *testing.T, s *testutil.Script, size int, pages ...int) {
	t.Helper()
	reqs := s.Requests()
	require.Len(t, reqs, len(pages), "one call per page")
	for i, page := range pages {
		assertWireRequest(t, reqs[i], pathResults, mediaJSON)
		assertBody(t, hpPagesRequestBody(page, size), reqs[i].Body)
	}
}

// hpAssertWalkError asserts that y is an error yield of a walk: no page, and
// the *CallError want describes, which never shows a row's address either.
func hpAssertWalkError(t *testing.T, y hpYield, want wantCall) {
	t.Helper()
	assert.Nil(t, y.page, "an error yield carries no page")
	assertCallError(t, y.err, want, "row1@example.com")
}

// hpPagesRequest returns a walk request for hpJobID.
func hpPagesRequest(startPage, itemsPerPage, maxPages int) neverbounce.JobResultPagesRequest {
	return neverbounce.JobResultPagesRequest{
		JobID: hpJobID, StartPage: startPage, ItemsPerPage: itemsPerPage, MaxPages: maxPages,
	}
}

func TestJobResultPagesRequestsEachPageInTurn(t *testing.T) {
	threePages := []testutil.Step{
		hpResults(5, 3, 1, 2), hpResults(5, 3, 3, 4), hpResults(5, 3, 5), hpResults(5, 3, 6),
	}
	tests := []struct {
		name      string
		req       neverbounce.JobResultPagesRequest
		steps     []testutil.Step
		wantSize  int
		wantPages []int
	}{
		{"start page 0 means 1", hpPagesRequest(0, 2, 10), threePages, 2, []int{1, 2, 3}},
		{"start page 1", hpPagesRequest(1, 2, 10), threePages, 2, []int{1, 2, 3}},
		{"start page 2", hpPagesRequest(2, 2, 10), threePages[1:], 2, []int{2, 3}},
		{"start page 3 is the last", hpPagesRequest(3, 2, 10), threePages[2:], 2, []int{3}},
		{"items per page 0 sends the default", hpPagesRequest(0, 0, 10), threePages, neverbounce.DefaultPageSize, []int{1, 2, 3}},
		{"items per page 1000", hpPagesRequest(0, 1000, 10), threePages, 1000, []int{1, 2, 3}},
		{"single page", hpPagesRequest(0, 0, 10), []testutil.Step{hpResults(1, 1, 1), hpResults(1, 1, 2)}, 100, []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, tt.steps...)
			ys := hpCollect(c.JobResultPages(t.Context(), tt.req), s)
			require.Len(t, ys, len(tt.wantPages), "stops cleanly after the last page")
			hpAssertPages(t, ys, tt.wantPages...)
			hpAssertRequests(t, s, tt.wantSize, tt.wantPages...)
			last := len(ys) - 1
			for i, y := range ys {
				assert.Equal(t, i != last, y.page.More(), "More on page %d", y.page.Page)
				assert.Equal(t, ys[0].page.TotalResults, y.page.TotalResults, "page %d", y.page.Page)
				assert.Equal(t, int64(tt.wantPages[last]), y.page.TotalPages, "page %d", y.page.Page)
				require.NotNil(t, y.page.ExecutionTime)
			}
			assert.Equal(t, 100, neverbounce.DefaultPageSize)
		})
	}
}

func TestJobResultPagesYieldsTheRowsInOrder(t *testing.T) {
	c, s := newScriptClient(t, hpResults(5, 3, 1, 2), hpResults(5, 3, 3, 4), hpResults(5, 3, 5))
	var ids []string
	for p, err := range c.JobResultPages(t.Context(), hpPagesRequest(0, 2, 3)) {
		require.NoError(t, err)
		for _, row := range p.Results {
			id, ok := row.Column("id")
			require.True(t, ok)
			ids = append(ids, id)
			assert.Equal(t, neverbounce.ResultValid, row.Verification.Result)
			assert.True(t, row.Verification.Flags.Has(neverbounce.FlagHasDNS))
		}
	}
	assert.Equal(t, []string{"1", "2", "3", "4", "5"}, ids)
	assert.Equal(t, 3, s.Calls())
}

func TestJobResultPagesBudget(t *testing.T) {
	tests := []struct {
		name      string
		req       neverbounce.JobResultPagesRequest
		steps     []testutil.Step
		wantPages []int
		wantNext  int // the budget error's Page; 0 = no error
	}{
		{
			name:      "two of five pages",
			req:       hpPagesRequest(0, 1, 2),
			steps:     []testutil.Step{hpResults(5, 5, 1), hpResults(5, 5, 2), hpResults(5, 5, 3)},
			wantPages: []int{1, 2}, wantNext: 3,
		},
		{
			name:      "one page from page 3",
			req:       hpPagesRequest(3, 1, 1),
			steps:     []testutil.Step{hpResults(5, 5, 3), hpResults(5, 5, 4)},
			wantPages: []int{3}, wantNext: 4,
		},
		{
			name:      "last page reached exactly at the budget",
			req:       hpPagesRequest(0, 1, 3),
			steps:     []testutil.Step{hpResults(3, 3, 1), hpResults(3, 3, 2), hpResults(3, 3, 3), hpResults(3, 3, 4)},
			wantPages: []int{1, 2, 3},
		},
		{
			name:      "single page with a budget of one",
			req:       hpPagesRequest(0, 1, 1),
			steps:     []testutil.Step{hpResults(1, 1, 1), hpResults(1, 1, 2)},
			wantPages: []int{1},
		},
		{
			name:      "budget of 100,000 for two pages",
			req:       hpPagesRequest(0, 1, 100_000),
			steps:     []testutil.Step{hpResults(2, 2, 1), hpResults(2, 2, 2), hpResults(2, 2, 3)},
			wantPages: []int{1, 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, tt.steps...)
			ys := hpCollect(c.JobResultPages(t.Context(), tt.req), s)
			hpAssertPages(t, ys, tt.wantPages...)
			hpAssertRequests(t, s, 1, tt.wantPages...)
			if tt.wantNext == 0 {
				assert.Len(t, ys, len(tt.wantPages), "no budget error when the last page is reached")
				return
			}
			require.Len(t, ys, len(tt.wantPages)+1, "one budget error after the pages")
			hpAssertWalkError(t, ys[len(ys)-1], wantCall{
				Kind: neverbounce.FailureBudgetExhausted, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: tt.wantNext,
			})
			assert.Equal(t, len(tt.wantPages), ys[len(ys)-1].calls, "the budget error makes no call")
		})
	}
}

func TestJobResultPagesResumesAtTheBudgetPage(t *testing.T) {
	c, s := newScriptClient(t,
		hpResults(4, 4, 1), hpResults(4, 4, 2),
		hpResults(4, 4, 3), hpResults(4, 4, 4),
	)
	req := hpPagesRequest(0, 1, 2)
	ys := hpCollect(c.JobResultPages(t.Context(), req), s)
	require.Len(t, ys, 3)
	hpAssertWalkError(t, ys[2], wantCall{
		Kind: neverbounce.FailureBudgetExhausted, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: 3,
	})
	ce := callError(t, ys[2].err)

	req.StartPage = ce.Page
	var pages []int
	for p, err := range c.JobResultPages(t.Context(), req) {
		require.NoError(t, err)
		pages = append(pages, p.Page)
	}
	assert.Equal(t, []int{3, 4}, pages, "a new walk from the error's Page reads the rest")
	hpAssertRequests(t, s, 1, 1, 2, 3, 4)
}

func TestJobResultPagesNoPrefetchAndEarlyBreak(t *testing.T) {
	for _, breakAt := range []int{1, 2, 3} {
		t.Run("break after page "+strconv.Itoa(breakAt), func(t *testing.T) {
			c, s := newScriptClient(t,
				hpResults(8, 4, 1, 2), hpResults(8, 4, 3, 4), hpResults(8, 4, 5, 6), hpResults(8, 4, 7, 8),
			)
			yielded := 0
			for p, err := range c.JobResultPages(t.Context(), hpPagesRequest(0, 2, 10)) {
				require.NoError(t, err)
				yielded++
				assert.Equal(t, yielded, s.Calls(), "no prefetch: one call per page yielded so far")
				assert.Equal(t, yielded, p.Page)
				if p.Page == breakAt {
					break
				}
			}
			assert.Equal(t, breakAt, yielded)
			assert.Equal(t, breakAt, s.Calls(), "breaking leaves exactly the calls made")
			pages := make([]int, breakAt)
			for i := range pages {
				pages[i] = i + 1
			}
			hpAssertRequests(t, s, 2, pages...)
		})
	}
}

func TestJobResultPagesContradictions(t *testing.T) {
	tests := []struct {
		name      string
		req       neverbounce.JobResultPagesRequest
		steps     []testutil.Step
		wantPages []int
		wantField string // "" = the walk ends cleanly
		wantPage  int
	}{
		{
			name:      "total_pages changed",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 1, 2), hpResults(5, 4, 3, 4), hpResults(5, 4, 5)},
			wantPages: []int{1}, wantField: "total_pages", wantPage: 2,
		},
		{
			name:      "total_pages shrank on the last page",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 1, 2), hpResults(5, 3, 3, 4), hpResults(5, 2, 5)},
			wantPages: []int{1, 2}, wantField: "total_pages", wantPage: 3,
		},
		{
			name:      "total_results changed",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 1, 2), hpResults(6, 3, 3, 4), hpResults(6, 3, 5)},
			wantPages: []int{1}, wantField: "total_results", wantPage: 2,
		},
		{
			name:      "empty page before the last",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 1, 2), hpResults(5, 3), hpResults(5, 3, 5)},
			wantPages: []int{1}, wantField: "results", wantPage: 2,
		},
		{
			name:      "empty first page before the last",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3), hpResults(5, 3, 3, 4)},
			wantField: "results", wantPage: 1,
		},
		{
			name:      "rows on a page past the end",
			req:       hpPagesRequest(5, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 9), hpResults(5, 3)},
			wantField: "results", wantPage: 5,
		},
		{
			name:      "rows on the page after the last",
			req:       hpPagesRequest(4, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3, 7, 8)},
			wantField: "results", wantPage: 4,
		},
		{
			name:      "rows with total_pages 0",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(0, 0, 1)},
			wantField: "results", wantPage: 1,
		},
		{
			name:      "an empty page past the end is fine",
			req:       hpPagesRequest(5, 2, 10),
			steps:     []testutil.Step{hpResults(5, 3), hpResults(5, 3)},
			wantPages: []int{5},
		},
		{
			name:      "an empty first page with total_pages 0 is fine",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(0, 0), hpResults(0, 0)},
			wantPages: []int{1},
		},
		{
			name:      "an empty last page is fine",
			req:       hpPagesRequest(0, 2, 10),
			steps:     []testutil.Step{hpResults(4, 3, 1, 2), hpResults(4, 3, 3, 4), hpResults(4, 3), hpResults(4, 3)},
			wantPages: []int{1, 2, 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, tt.steps...)
			ys := hpCollect(c.JobResultPages(t.Context(), tt.req), s)
			hpAssertPages(t, ys, tt.wantPages...)
			if tt.wantField == "" {
				require.Len(t, ys, len(tt.wantPages), "the walk ends cleanly")
				assert.Equal(t, len(tt.wantPages), s.Calls())
				return
			}
			require.Len(t, ys, len(tt.wantPages)+1, "the good pages, then one error")
			hpAssertWalkError(t, ys[len(ys)-1], wantCall{
				Kind: neverbounce.FailureUnexpectedResponse, Op: neverbounce.OpJobResults, Status: http.StatusOK,
				JobID: hpJobID, Page: tt.wantPage, Field: tt.wantField,
			})
			assert.Equal(t, len(tt.wantPages)+1, s.Calls(), "no call after the contradiction")
		})
	}
}

func TestJobResultPagesErrorMidWalk(t *testing.T) {
	tests := []struct {
		name      string
		step      testutil.Step
		kind      neverbounce.FailureKind
		status    int
		apiStatus neverbounce.APIStatus
		field     string
	}{
		{
			name: "HTTP 502", kind: neverbounce.FailureUpstream, status: http.StatusBadGateway,
			step: testutil.Step{Status: http.StatusBadGateway, Body: "upstream down"},
		},
		{
			name: "HTTP 503 temp_unavail", kind: neverbounce.FailureUnavailable, status: http.StatusServiceUnavailable,
			apiStatus: neverbounce.APIStatusTempUnavail,
			step:      testutil.Step{Status: http.StatusServiceUnavailable, Body: `{"status":"temp_unavail"}`},
		},
		{
			name: "throttle_triggered envelope", kind: neverbounce.FailureRateLimited, status: http.StatusOK,
			apiStatus: neverbounce.APIStatusThrottleTriggered,
			step:      testutil.Step{Body: `{"status":"throttle_triggered"}`},
		},
		{
			name: "malformed JSON", kind: neverbounce.FailureMalformedResponse, status: http.StatusOK,
			step: testutil.Step{Body: `{"status":"success","total_results":5,"total_pages":3,"results":[`},
		},
		{
			name: "results null", kind: neverbounce.FailureMalformedResponse, status: http.StatusOK, field: "results",
			step: testutil.Step{Body: `{"status":"success","total_results":5,"total_pages":3,"results":null}`},
		},
		{
			name: "more rows than items_per_page", kind: neverbounce.FailureUnexpectedResponse, status: http.StatusOK,
			field: "results", step: hpResults(5, 3, 3, 4, 5),
		},
		{
			name: "invalid result token", kind: neverbounce.FailureUnexpectedResponse, status: http.StatusOK,
			field: "results[1].verification.result",
			step: testutil.Step{Body: `{"status":"success","total_results":5,"total_pages":3,"results":[` + hpRow(3) +
				`,{"data":{"id":"4"},"verification":{"result":"Valid"}}]}`},
		},
		{name: "transport failure", kind: neverbounce.FailureTransport, step: testutil.Step{Err: errHPTransport}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, hpResults(5, 3, 1, 2), tt.step, hpResults(5, 3, 5))
			var first *neverbounce.JobResultsPage
			var snapshot []byte
			var ys []hpYield
			for p, err := range c.JobResultPages(t.Context(), hpPagesRequest(0, 2, 10)) {
				ys = append(ys, hpYield{page: p, err: err, calls: s.Calls()})
				if first == nil && err == nil {
					first = p
					var merr error
					snapshot, merr = json.Marshal(p)
					require.NoError(t, merr)
				}
				// No break: the iterator alone must stop after the error.
			}
			require.Len(t, ys, 2, "page 1, then the error once, then nothing")
			hpAssertPages(t, ys, 1)
			hpAssertWalkError(t, ys[1], wantCall{
				Kind: tt.kind, Op: neverbounce.OpJobResults, Status: tt.status, API: tt.apiStatus, JobID: hpJobID,
				Page: 2, Field: tt.field,
			})
			assert.Equal(t, 2, s.Calls(), "no call after the error")

			require.NotNil(t, first)
			after, err := json.Marshal(first)
			require.NoError(t, err)
			assert.JSONEq(t, string(snapshot), string(after), "the earlier page is intact")
			require.Len(t, first.Results, 2)
			id, ok := first.Results[1].Column("id")
			assert.True(t, ok)
			assert.Equal(t, "2", id)
		})
	}
}

func TestJobResultPagesRangesRestart(t *testing.T) {
	t.Run("full walks", func(t *testing.T) {
		c, s := newScriptClient(t,
			hpResults(5, 3, 3, 4), hpResults(5, 3, 5),
			hpResults(5, 3, 3, 4), hpResults(5, 3, 5),
		)
		seq := c.JobResultPages(t.Context(), hpPagesRequest(2, 2, 10))
		first := hpCollect(seq, s)
		hpAssertPages(t, first, 2, 3)
		require.Len(t, first, 2)
		second := hpCollect(seq, s)
		require.Len(t, second, 2)
		for i, y := range second {
			require.NoError(t, y.err)
			assert.Equal(t, first[i].page.Page, y.page.Page)
			assert.Equal(t, first[i].page.Results, y.page.Results)
			assert.NotSame(t, first[i].page, y.page, "a new walk reads new pages")
			assert.Equal(t, 3+i, y.calls)
		}
		hpAssertRequests(t, s, 2, 2, 3, 2, 3)
	})
	t.Run("after a budget error", func(t *testing.T) {
		c, s := newScriptClient(t, hpResults(3, 3, 1), hpResults(3, 3, 1), hpResults(3, 3, 1))
		seq := c.JobResultPages(t.Context(), hpPagesRequest(0, 1, 1))
		for range 2 {
			ys := hpCollect(seq, s)
			require.Len(t, ys, 2)
			require.NoError(t, ys[0].err)
			assert.Equal(t, 1, ys[0].page.Page)
			hpAssertWalkError(t, ys[1], wantCall{
				Kind: neverbounce.FailureBudgetExhausted, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: 2,
			})
		}
		hpAssertRequests(t, s, 1, 1, 1)
	})
	t.Run("after an error mid-walk", func(t *testing.T) {
		c, s := newScriptClient(t,
			hpResults(4, 2, 1, 2), testutil.Step{Status: http.StatusBadGateway},
			hpResults(4, 2, 1, 2), hpResults(4, 2, 3, 4),
		)
		seq := c.JobResultPages(t.Context(), hpPagesRequest(0, 2, 10))
		ys := hpCollect(seq, s)
		require.Len(t, ys, 2)
		hpAssertWalkError(t, ys[1], wantCall{
			Kind: neverbounce.FailureUpstream, Op: neverbounce.OpJobResults, Status: http.StatusBadGateway,
			JobID: hpJobID, Page: 2,
		})
		ys = hpCollect(seq, s)
		require.Len(t, ys, 2)
		require.NoError(t, ys[1].err)
		assert.Equal(t, 2, ys[1].page.Page)
		hpAssertRequests(t, s, 2, 1, 2, 1, 2)
	})
}

func TestJobResultPagesCallerContext(t *testing.T) {
	t.Run("canceled before the walk", func(t *testing.T) {
		c, s := newScriptClient(t, hpResults(3, 3, 1))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ys := hpCollect(c.JobResultPages(ctx, hpPagesRequest(7, 0, 10)), s)
		require.Len(t, ys, 1)
		hpAssertWalkError(t, ys[0], wantCall{
			Kind: neverbounce.FailureCanceled, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: 7,
		})
		assert.Zero(t, s.Calls(), "a done context sends nothing")
	})
	t.Run("deadline passed before the walk", func(t *testing.T) {
		c, s := newScriptClient(t, hpResults(3, 3, 1))
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
		defer cancel()
		ys := hpCollect(c.JobResultPages(ctx, hpPagesRequest(0, 0, 10)), s)
		require.Len(t, ys, 1)
		hpAssertWalkError(t, ys[0], wantCall{
			Kind: neverbounce.FailureDeadlineExceeded, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: 1,
		})
		assert.Zero(t, s.Calls())
	})
	t.Run("canceled after the first page", func(t *testing.T) {
		c, s := newScriptClient(t, hpResults(6, 3, 1, 2), hpResults(6, 3, 3, 4), hpResults(6, 3, 5, 6))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var ys []hpYield
		for p, err := range c.JobResultPages(ctx, hpPagesRequest(0, 2, 10)) {
			ys = append(ys, hpYield{page: p, err: err, calls: s.Calls()})
			cancel()
		}
		require.Len(t, ys, 2)
		hpAssertPages(t, ys, 1)
		hpAssertWalkError(t, ys[1], wantCall{
			Kind: neverbounce.FailureCanceled, Op: neverbounce.OpJobResults, JobID: hpJobID, Page: 2,
		})
		assert.Equal(t, 1, s.Calls(), "the next page is never requested")
	})
}

// TestJobResultPagesInvalidSendsNothing checks JobResultPages' own rules and
// their order. JobID and the checks every method shares are in client_test.go.
func TestJobResultPagesInvalidSendsNothing(t *testing.T) {
	c, s := newScriptClient(t, hpResults(1, 1, 1))
	maxInt32 := math.MaxInt32
	tests := []struct {
		name   string
		req    neverbounce.JobResultPagesRequest
		field  string
		reason string
	}{
		{"start page -1", hpPagesRequest(-1, 0, 1), "StartPage", reasonOutOfRange},
		{"start page above MaxInt32", hpPagesRequest(maxInt32+1, 0, 1), "StartPage", reasonOutOfRange},
		{"items per page -1", hpPagesRequest(0, -1, 1), "ItemsPerPage", reasonOutOfRange},
		{"items per page 1001", hpPagesRequest(0, 1001, 1), "ItemsPerPage", reasonOutOfRange},
		{"max pages 0", hpPagesRequest(0, 0, 0), "MaxPages", reasonRequired},
		{"max pages -1", hpPagesRequest(0, 0, -1), "MaxPages", reasonOutOfRange},
		{"max pages 100,001", hpPagesRequest(0, 0, 100_001), "MaxPages", reasonOutOfRange},
		{"job id before start page", neverbounce.JobResultPagesRequest{StartPage: -1}, "JobID", reasonRequired},
		{"start page before items per page", hpPagesRequest(-1, -1, 0), "StartPage", reasonOutOfRange},
		{"items per page before max pages", hpPagesRequest(0, 1001, 0), "ItemsPerPage", reasonOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := c.JobResultPages(t.Context(), tt.req)
			for range 2 { // each range validates again
				ys := hpCollect(seq, s)
				require.Len(t, ys, 1, "exactly one yield")
				assert.Nil(t, ys[0].page)
				assertValidation(t, ys[0].err, tt.field, tt.reason)
				_, isCallErr := errors.AsType[*neverbounce.CallError](ys[0].err)
				assert.False(t, isCallErr, "nothing was attempted")
			}
		})
	}
	assert.Zero(t, s.Calls(), "validation sends nothing")
}

func TestJobResultPagesAcceptsBoundaryValues(t *testing.T) {
	maxInt32 := math.MaxInt32
	tests := []struct {
		name     string
		req      neverbounce.JobResultPagesRequest
		step     testutil.Step
		wantPage int
		wantSize int
	}{
		{"start page MaxInt32", hpPagesRequest(maxInt32, 0, 1), hpResults(5, math.MaxInt32, 9), maxInt32, 100},
		{"items per page 1", hpPagesRequest(0, 1, 1), hpResults(1, 1, 1), 1, 1},
		{"items per page 1000", hpPagesRequest(0, 1000, 1), hpResults(1, 1, 1), 1, 1000},
		{"max pages 100,000", hpPagesRequest(0, 0, 100_000), hpResults(1, 1, 1), 1, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newScriptClient(t, tt.step)
			ys := hpCollect(c.JobResultPages(t.Context(), tt.req), s)
			require.Len(t, ys, 1)
			hpAssertPages(t, ys, tt.wantPage)
			hpAssertRequests(t, s, tt.wantSize, tt.wantPage)
		})
	}
}

// TestJobResultPagesNeverOutrunsThePageRange covers a total_pages beyond the
// 32-bit page numbers jobs/results accepts. The walk may not then ask for a
// page it cannot send: every error after a call is a *CallError for the job,
// never a *ValidationError.
func TestJobResultPagesNeverOutrunsThePageRange(t *testing.T) {
	maxInt32 := math.MaxInt32
	c, s := newScriptClient(t,
		hpResults(math.MaxInt32+5, math.MaxInt32+1, 1),
		hpResults(math.MaxInt32+5, math.MaxInt32+1, 2),
	)
	ys := hpCollect(c.JobResultPages(t.Context(), hpPagesRequest(maxInt32, 1, 3)), s)
	require.NotEmpty(t, ys)
	for _, y := range ys[:len(ys)-1] {
		require.NoError(t, y.err)
		assert.Equal(t, maxInt32, y.page.Page)
	}
	last := ys[len(ys)-1]
	_, isValidation := errors.AsType[*neverbounce.ValidationError](last.err)
	require.False(t, isValidation, "a walk never yields a *ValidationError after a call: %v", last.err)
	hpAssertWalkError(t, last, wantCall{
		Kind: neverbounce.FailureUnexpectedResponse, Op: neverbounce.OpJobResults, Status: http.StatusOK,
		JobID: hpJobID, Page: maxInt32, Field: "total_pages",
	})
	assert.Equal(t, 1, s.Calls(), "only page MaxInt32 can be requested")
}

// hpThreePages answers jobs/results with 3 pages of 2 rows, each row carrying
// a 60,000-byte value so that a retained page is noticeable.
func hpThreePages() testutil.RoundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		var req struct {
			Page int `json:"page"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var b strings.Builder
		b.WriteString(`{"status":"success","total_results":6,"total_pages":3,"results":[`)
		for i := range 2 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"data":{"id":"` + strconv.Itoa(req.Page*10+i) + `","blob":"` +
				strings.Repeat("z", 60000) + `"},"verification":{"result":"valid"}}`)
		}
		b.WriteString(`]}`)
		return testutil.Response(r, http.StatusOK, io.NopCloser(strings.NewReader(b.String())), -1), nil
	}
}

// TestJobResultPagesIgnoresChangesToYieldedPages proves that a yielded page
// belongs to the caller: clearing it after use neither ends the walk early nor
// breaks the later contradiction checks.
func TestJobResultPagesIgnoresChangesToYieldedPages(t *testing.T) {
	c := newTransportClient(t, hpThreePages())
	var pages []int
	for p, err := range c.JobResultPages(t.Context(), neverbounce.JobResultPagesRequest{
		JobID: 42, ItemsPerPage: 2, MaxPages: 10,
	}) {
		require.NoError(t, err)
		pages = append(pages, p.Page)
		*p = neverbounce.JobResultsPage{} // the caller releases its page after storing it
	}
	assert.Equal(t, []int{1, 2, 3}, pages)
}

// TestJobResultPagesHoldsNoEarlierPage proves that the walk keeps no
// reference to a page once it has been yielded: the first page can be
// collected while later pages are current.
func TestJobResultPagesHoldsNoEarlierPage(t *testing.T) {
	c := newTransportClient(t, hpThreePages())
	var first weak.Pointer[neverbounce.JobResultsPage]
	var collected []bool
	for p, err := range c.JobResultPages(t.Context(), neverbounce.JobResultPagesRequest{
		JobID: 42, ItemsPerPage: 2, MaxPages: 10,
	}) {
		require.NoError(t, err)
		if p.Page == 1 {
			first = weak.Make(p)
			continue // the caller keeps nothing from page 1
		}
		runtime.GC()
		runtime.GC()
		collected = append(collected, first.Value() == nil)
	}
	assert.Equal(t, []bool{true, true}, collected)
}
