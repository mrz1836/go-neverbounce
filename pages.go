package neverbounce

import (
	"context"
	"iter"
)

// maxPagesLimit bounds JobResultPagesRequest.MaxPages.
const maxPagesLimit = 100_000

// JobResultPagesRequest is one JobResultPages walk.
type JobResultPagesRequest struct {
	// JobID is the job whose results to read.
	JobID JobID

	// StartPage is the first page to read: 0 means 1.
	StartPage int

	// ItemsPerPage is the page size, from 1 to MaxItemsPerPage: 0 means
	// DefaultPageSize. It is always sent.
	ItemsPerPage int

	// MaxPages is the required page budget: the most jobs/results calls one
	// walk makes, from 1 to 100,000.
	MaxPages int
}

// JobResultPages returns an iterator over a job's result pages, from
// StartPage on.
//
// Free and read-only: it only ever calls jobs/results, one call per page,
// each made only after the previous page was yielded and the loop continued.
// Nothing is prefetched, nothing runs in the background, and breaking out of
// the loop stops it at once. Each range starts again at StartPage with new
// calls. The page counter is the SDK's own, so the walk can never loop.
//
// The first page is yielded even when it is empty because the job has no
// results (TotalPages 0) or StartPage is past the end; the walk then ends after
// the page whose number reaches TotalPages. An error is yielded once, as
// (nil, err), and ends the walk; pages already yielded stay valid:
//   - a *ValidationError, before any call;
//   - a page's *CallError, with JobID and Page set;
//   - unexpected_response when a page contradicts the walk: TotalPages or
//     TotalResults changed since the first page, an empty page before the
//     last, rows past the last page, or more pages than jobs/results can
//     number (it takes a 32-bit page number);
//   - budget_exhausted when MaxPages pages were read and more remain. Its Page
//     is the next page: a new walk with that StartPage resumes there.
func (c *Client) JobResultPages(ctx context.Context, req JobResultPagesRequest) iter.Seq2[*JobResultsPage, error] {
	return func(yield func(*JobResultsPage, error) bool) {
		start, size, err := c.walkStart(ctx, &req)
		if err != nil {
			yield(nil, err)
			return
		}
		// The walk keeps its own copy of the totals, read before each page is
		// handed over: a yielded page belongs to the caller, who may change or
		// drop it, and the walk never holds an earlier page.
		var first *walkTotals
		for page, fetched := start, 0; ; page, fetched = page+1, fetched+1 {
			p, err := c.walkPage(ctx, &req, page, size, fetched, first)
			if err != nil {
				yield(nil, err)
				return
			}
			totals := walkTotals{pages: p.TotalPages, results: p.TotalResults}
			if first == nil {
				first = &totals
			}
			if !yield(p, nil) || int64(page) >= totals.pages {
				return
			}
			if page == maxPage {
				yield(nil, pastLastPage(req.JobID, page))
				return
			}
		}
	}
}

// walkTotals are the totals of a walk's first page.
type walkTotals struct {
	pages, results int64
}

// walkStart checks the Client, the context and the request, and returns the
// first page and the page size.
func (c *Client) walkStart(ctx context.Context, req *JobResultPagesRequest) (start, size int, err error) {
	if err := c.prepare(ctx); err != nil {
		return 0, 0, err
	}
	return req.validate()
}

// walkPage reads one page of a walk, after fetched pages, and checks it
// against the walk's first page. With the budget used up it fails as
// budget_exhausted instead, without a call.
func (c *Client) walkPage(ctx context.Context, req *JobResultPagesRequest, page, size, fetched int,
	first *walkTotals,
) (*JobResultsPage, error) {
	if fetched == req.MaxPages {
		return nil, newCallError(OpJobResults, FailureBudgetExhausted, 0, errPageBudget).forJob(req.JobID, page)
	}
	p, err := c.JobResults(ctx, JobResultsRequest{JobID: req.JobID, Page: page, ItemsPerPage: size})
	if err != nil {
		return nil, err
	}
	if cerr := contradiction(first, p); cerr != nil {
		return nil, cerr.forJob(req.JobID, page)
	}
	return p, nil
}

// pastLastPage returns the failure for a walk whose next page cannot be
// numbered: TotalPages claims more pages than jobs/results accepts.
func pastLastPage(id JobID, page int) *CallError {
	return contradicts("total_pages").callError(OpJobResults).forJob(id, page)
}

// validate checks the request and returns the first page and the page size.
func (r *JobResultPagesRequest) validate() (start, size int, err error) {
	if err := checkJobID(r.JobID); err != nil {
		return 0, 0, err
	}
	if r.StartPage < 0 || r.StartPage > maxPage {
		return 0, 0, invalidRequest("StartPage", reasonOutOfRange)
	}
	if r.ItemsPerPage < 0 || r.ItemsPerPage > MaxItemsPerPage {
		return 0, 0, invalidRequest("ItemsPerPage", reasonOutOfRange)
	}
	switch {
	case r.MaxPages == 0:
		return 0, 0, invalidRequest("MaxPages", reasonRequired)
	case r.MaxPages < 0 || r.MaxPages > maxPagesLimit:
		return 0, 0, invalidRequest("MaxPages", reasonOutOfRange)
	}
	size = r.ItemsPerPage
	if size == 0 {
		size = DefaultPageSize
	}
	return max(r.StartPage, 1), size, nil
}

// contradiction checks page p against the totals of the walk's first page,
// and returns the failure it reveals, or nil. Rows beyond the page size are
// already refused when the page is decoded.
func contradiction(first *walkTotals, p *JobResultsPage) *CallError {
	fail := func(field string) *CallError { return contradicts(field).callError(OpJobResults) }
	switch {
	case first != nil && p.TotalPages != first.pages:
		return fail("total_pages")
	case first != nil && p.TotalResults != first.results:
		return fail("total_results")
	case len(p.Results) == 0 && int64(p.Page) < p.TotalPages:
		return fail("results")
	case len(p.Results) > 0 && int64(p.Page) > p.TotalPages:
		return fail("results")
	}
	return nil
}
