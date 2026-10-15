package neverbounce

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// TestSmallRequestSizeBound proves that every fixed-shape request is still
// held to maxSmallRequestBytes. No public input can reach the bound (the key
// is at most 256 bytes), so the body's reserved key prefix is stubbed to
// almost fill it.
func TestSmallRequestSizeBound(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"CheckEmail": func(c *Client) error {
			_, err := c.CheckEmail(t.Context(), CheckEmailRequest{Email: "avery.example@example.com"})
			return err
		},
		"ParseJob": func(c *Client) error {
			_, err := c.ParseJob(t.Context(), ParseJobRequest{JobID: 1})
			return err
		},
		"StartJob": func(c *Client) error {
			_, err := c.StartJob(t.Context(), StartJobRequest{JobID: 1})
			return err
		},
		"JobStatus": func(c *Client) error {
			_, err := c.JobStatus(t.Context(), JobStatusRequest{JobID: 1})
			return err
		},
		"JobResults": func(c *Client) error {
			_, err := c.JobResults(t.Context(), JobResultsRequest{JobID: 1})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ct := &testutil.CountingTransport{Next: testutil.StaticTransport(http.StatusOK, `{}`)}
			c := newInternalClient(t, WithTestBaseURL("https://127.0.0.1"), WithHTTPClient(&http.Client{Transport: ct}))
			c.newBody = func(capHint int) []byte {
				return make([]byte, maxSmallRequestBytes-8, maxSmallRequestBytes-8+capHint)
			}
			err := call(c)
			require.ErrorIs(t, err, ErrInvalidRequest)
			ve, ok := err.(*ValidationError) //nolint:errorlint // the SDK returns the *ValidationError itself
			require.True(t, ok)
			assert.Equal(t, "request", ve.Field)
			assert.Equal(t, reasonTooLarge, ve.Reason)
			assert.Zero(t, ct.Attempts())
		})
	}
}

// TestAppendStringEscapesControlBytes covers the JSON writer's escapes for
// bytes validation never lets through, so that the writer stays correct on
// its own.
func TestAppendStringEscapesControlBytes(t *testing.T) {
	got := appendString([]byte(nil), "a\x00b\x1fc\"d\\eé")
	want := []byte{
		'"', 'a',
		'\\', 'u', '0', '0', '0', '0', 'b',
		'\\', 'u', '0', '0', '1', 'f', 'c',
		'\\', '"', 'd', '\\', '\\', 'e', 0xc3, 0xa9, '"',
	}
	assert.Equal(t, want, got)
}
