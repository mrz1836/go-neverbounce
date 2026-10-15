package neverbounce_test

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/mrz1836/go-neverbounce"
	"github.com/mrz1836/go-neverbounce/internal/testutil"
)

// These benchmarks measure local CPU and allocations through in-memory
// transports. They are not evidence of network or provider performance.

// bzDownloadBytes is the size of the benchmarked download.
const bzDownloadBytes = 64 << 20

// bzCSV returns size bytes of CSV: a header row, then records with CRLF line
// endings, the last one cut wherever size falls.
func bzCSV(size int) []byte {
	b := make([]byte, 0, size+64)
	b = append(b, "email,email_status\r\n"...)
	for i := 1; len(b) < size; i++ {
		b = append(b, "user."...)
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, "@example.com,valid\r\n"...)
	}
	return b[:size]
}

// bzFileTransport answers every request in memory with data as a 200
// application/octet-stream body of declared length, after draining the
// request.
func bzFileTransport(data []byte) testutil.RoundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		testutil.Drain(r)
		resp := testutil.Response(r, http.StatusOK, io.NopCloser(bytes.NewReader(data)), int64(len(data)))
		resp.Header = http.Header{"Content-Type": {mediaOctet}}
		return resp, nil
	}
}

func BenchmarkCheckEmailEndToEnd(b *testing.B) {
	body := string(testutil.Fixture(b, "synthetic/check_full.json"))
	client := newTransportClient(b, testutil.StaticTransport(http.StatusOK, body))
	req := neverbounce.CheckEmailRequest{Email: testEmail, AddressInfo: true, CreditsInfo: true}
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		res, err := client.CheckEmail(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		if res.Result != neverbounce.ResultValid {
			b.Fatalf("result %q, want valid", res.Result)
		}
	}
}

func BenchmarkDownloadCopy(b *testing.B) {
	data := bzCSV(bzDownloadBytes)
	client := newTransportClient(b, bzFileTransport(data))
	req := neverbounce.DownloadJobRequest{JobID: 1001, LineFeed: neverbounce.LineFeedCRLF}
	ctx := b.Context()
	b.SetBytes(bzDownloadBytes)
	b.ReportAllocs()
	for b.Loop() {
		n, err := client.DownloadJob(ctx, req, io.Discard)
		if err != nil {
			b.Fatal(err)
		}
		if n != bzDownloadBytes {
			b.Fatalf("%d bytes written, want %d", n, bzDownloadBytes)
		}
	}
}
