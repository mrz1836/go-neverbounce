package neverbounce

import (
	"context"
	"encoding/json"
	"math"
)

// maxPage is the largest page number jobs/results is asked for: the provider
// types it as a 32-bit integer.
const maxPage = math.MaxInt32

// JobResultsRequest is one jobs/results call.
type JobResultsRequest struct {
	// JobID is the job whose results to read.
	JobID JobID

	// Page is the page to read, from 1. 0 means not sent (the provider's
	// default is 1).
	Page int

	// ItemsPerPage is the page size, from 1 to MaxItemsPerPage. 0 means not
	// sent (the provider's default is 10).
	ItemsPerPage int
}

// JobResultsPage is one page of a job's results.
//
// It survives a JSON round trip through encoding/json unchanged, under the
// provider's snake_case names. Unmarshaling a raw provider body into it skips
// every check the SDK makes.
type JobResultsPage struct {
	// Page is the page the SDK requested, or 1 (the provider's default) when
	// JobResultsRequest.Page was 0. The provider's own echo of the request
	// ("query", whose documented example says page 0) is never decoded. This
	// field is the SDK's: the provider's answer has no top-level "page".
	Page int `json:"page"`

	// TotalResults is the provider's total_results.
	TotalResults int64 `json:"total_results"`

	// TotalPages is the provider's total_pages.
	TotalPages int64 `json:"total_pages"`

	// Results are the page's rows in the provider's order: none dropped,
	// reordered or deduplicated. The provider may return a list's header row
	// as a record, and duplicates, so correlate rows by a column of your own,
	// never by position. nil when the page is empty.
	Results []ResultRow `json:"results,omitempty"`

	// ExecutionTime is the provider's execution_time, when it is an integer of
	// at least 0. Its units are undocumented.
	ExecutionTime *int64 `json:"execution_time,omitempty"`
}

// More reports whether a later page exists: int64(p.Page) < p.TotalPages. It
// is false for a nil page.
func (p *JobResultsPage) More() bool { return p != nil && int64(p.Page) < p.TotalPages }

// ResultRow is one record of a job's results.
type ResultRow struct {
	// Data is the provider's "data": the record as you submitted it, as a JSON
	// object (keyed by your column names) or array. It is compacted and
	// escaped as encoding/json marshals a json.RawMessage, at most 64 KiB,
	// and never aliases the response. Values may come back as strings even
	// when submitted as numbers. nil when absent or null.
	Data json.RawMessage `json:"data,omitempty"`

	// Verification is the provider's verdict on the record.
	Verification RowVerification `json:"verification"`
}

// Column returns the value of the member name in r.Data, matched exactly: a
// JSON string decoded, or a JSON number as its literal text. ok is false when
// Data is not an object, has no such member, or holds another type there.
func (r ResultRow) Column(name string) (value string, ok bool) {
	if len(r.Data) == 0 || r.Data[0] != '{' {
		return "", false
	}
	for key, v := range members(r.Data) {
		if string(keyBytes(key)) != name || len(v) == 0 {
			continue
		}
		switch c := v[0]; {
		case c == '"':
			return decodeKey(v), true
		case c == '-' || (c >= '0' && c <= '9'):
			return string(v), true
		}
		return "", false
	}
	return "", false
}

// RowVerification is a record's "verification": the same verdict single/check
// returns, without the credit counters.
type RowVerification struct {
	// Result is the verdict. ResultUnknown is a successful answer.
	Result Result `json:"result"`

	// Flags are the provider's observations, in its order. nil when none.
	Flags Flags `json:"flags,omitempty"`

	// SuggestedCorrection is the provider's soft suggestion for a likely typo,
	// or "". The SDK never applies it.
	SuggestedCorrection string `json:"suggested_correction,omitempty"`

	// AddressInfo is the provider's parse of the address, when reported.
	AddressInfo *AddressInfo `json:"address_info,omitempty"`
}

// Members of a jobs/results answer after the envelope.
const (
	resultsTotalResults = envFirst + iota
	resultsTotalPages
	resultsResults
)

// JobResults reads one page of a job's results: POST
// https://api.neverbounce.com/v4.2/jobs/results.
//
// Free and read-only. It makes exactly one attempt and never retries, and it
// never reads another page on its own: see JobResultPages for a bounded walk.
// The page may hold at most ItemsPerPage rows (1000 when not sent);
// more fails as unexpected_response.
//
// Errors are a *ValidationError (nothing sent) or a *CallError whose JobID
// and Page are the request's.
func (c *Client) JobResults(ctx context.Context, req JobResultsRequest) (*JobResultsPage, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, err
	}
	b, err := req.encode(c)
	if err != nil {
		return nil, err
	}
	page := max(req.Page, 1)
	obj, cerr := c.call(ctx, OpJobResults, b, envelopeNames("total_results", "total_pages", "results"))
	if cerr != nil {
		return nil, cerr.forJob(req.JobID, page)
	}
	p, derr := decodeResultsPage(&obj, page, req.rowLimit())
	if derr != nil {
		return nil, derr.callError(OpJobResults).forJob(req.JobID, page)
	}
	return p, nil
}

// encode validates the request and returns its JSON body.
func (r *JobResultsRequest) encode(c *Client) ([]byte, error) {
	if err := checkJobID(r.JobID); err != nil {
		return nil, err
	}
	if r.Page < 0 || r.Page > maxPage {
		return nil, invalidRequest("Page", reasonOutOfRange)
	}
	if r.ItemsPerPage < 0 || r.ItemsPerPage > MaxItemsPerPage {
		return nil, invalidRequest("ItemsPerPage", reasonOutOfRange)
	}
	b := c.newRequestBody(80).integer("job_id", int64(r.JobID))
	if r.Page != 0 {
		b = b.integer("page", int64(r.Page))
	}
	if r.ItemsPerPage != 0 {
		b = b.integer("items_per_page", int64(r.ItemsPerPage))
	}
	return b.finish(maxSmallRequestBytes)
}

// rowLimit is the most rows a page may hold: the page size asked for, or the
// provider's maximum when none was sent.
func (r *JobResultsRequest) rowLimit() int {
	if r.ItemsPerPage != 0 {
		return r.ItemsPerPage
	}
	return MaxItemsPerPage
}

// decodeResultsPage decodes a jobs/results success envelope for page, with at
// most limit rows.
func decodeResultsPage(o *object, page, limit int) (*JobResultsPage, *decodeError) {
	total, derr := decodeRequiredCount(o.fields[resultsTotalResults], "total_results")
	if derr != nil {
		return nil, derr
	}
	pages, derr := decodeRequiredCount(o.fields[resultsTotalPages], "total_pages")
	if derr != nil {
		return nil, derr
	}
	rows, derr := decodeRows(o.fields[resultsResults], limit)
	if derr != nil {
		return nil, derr
	}
	return &JobResultsPage{
		Page:          page,
		TotalResults:  total,
		TotalPages:    pages,
		Results:       rows,
		ExecutionTime: executionTime(o.fields[envExecutionTime]),
	}, nil
}

// decodeRows decodes the required "results" array, with at most limit rows.
// An empty array is nil.
func decodeRows(raw []byte, limit int) ([]ResultRow, *decodeError) {
	const field = "results"
	switch {
	case isNull(raw):
		return nil, missingField(field)
	case raw[0] != '[':
		return nil, wrongType(field)
	}
	var rows []ResultRow
	for i, el := range elements(raw) {
		if i == limit {
			return nil, contradicts(field)
		}
		row, derr := decodeRow(el, i)
		if derr != nil {
			return nil, derr
		}
		if rows == nil {
			rows = make([]ResultRow, 0, min(limit, len(raw)/32+1))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// decodeRow decodes the row at index i.
func decodeRow(raw []byte, i int) (ResultRow, *decodeError) {
	if len(raw) == 0 || raw[0] != '{' {
		return ResultRow{}, wrongType(at("results", i, "").String())
	}
	o := readObject(raw, []string{"data", "verification"})
	data, derr := decodeData(o.fields[0], "")
	if derr != nil {
		derr.field = at("results", i, "data").String()
		return ResultRow{}, derr
	}
	vp := at("results", i, "verification")
	switch v := o.fields[1]; {
	case isNull(v):
		return ResultRow{}, missingField(vp.String())
	case v[0] != '{':
		return ResultRow{}, wrongType(vp.String())
	}
	names := verificationNames()
	vo := readObject(o.fields[1], names[:])
	ver, derr := decodeVerification([verCount][]byte(vo.fields[:verCount]), vp)
	if derr != nil {
		return ResultRow{}, derr
	}
	return ResultRow{Data: data, Verification: RowVerification{
		Result:              ver.result,
		Flags:               ver.flags,
		SuggestedCorrection: ver.suggestedCorrection,
		AddressInfo:         ver.addressInfo,
	}}, nil
}
