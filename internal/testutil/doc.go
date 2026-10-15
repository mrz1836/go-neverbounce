// Package testutil holds the test plumbing go-neverbounce's tests share:
// testdata fixtures, HTTP fakes, a recording TLS server for HTTP/1.1 and
// HTTP/2, a scripted transport, writer fakes for streaming downloads,
// formatting-leak assertions, a token-walk JSON oracle, race-aware allocation
// guards, and the value-free shape printer the live probe reports with.
//
// It is imported only by tests. It imports testify and the standard library,
// never the neverbounce package, so that the root package's internal tests can
// use it too.
package testutil
