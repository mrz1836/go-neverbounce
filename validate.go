package neverbounce

import (
	"context"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSmallRequestBytes bounds the encoded body of every operation other than
// jobs/create, the API key included.
const maxSmallRequestBytes = 64 << 10

// hexDigits spells the escapes the JSON writer emits.
const hexDigits = "0123456789abcdef"

// prepare runs the checks every operation starts with: a Client built by
// NewClient, then a non-nil context.
func (c *Client) prepare(ctx context.Context) error {
	if err := c.ready(); err != nil {
		return err
	}
	if ctx == nil {
		return invalidRequest("ctx", reasonNil)
	}
	return nil
}

// body is a request body under construction. Its first bytes are reserved for
// the "key" member, which only the sender writes, so the encoders never see
// the API key, yet len(b) is always the exact size of the body on the wire.
type body []byte

// newRequestBody returns an empty body for c with room for capHint more bytes.
func (c *Client) newRequestBody(capHint int) body { return c.newBody(capHint) }

// name appends the separator and the quoted member name, such as `,"email":`.
// name must need no escaping.
func (b body) name(name string) body {
	b = append(b, ',', '"')
	b = append(b, name...)
	return append(b, '"', ':')
}

// str appends the member name with the JSON string s.
func (b body) str(name, s string) body { return appendString(b.name(name), s) }

// boolean appends the member name with a JSON boolean.
func (b body) boolean(name string, v bool) body { return strconv.AppendBool(b.name(name), v) }

// integer appends the member name with a JSON integer.
func (b body) integer(name string, n int64) body { return strconv.AppendInt(b.name(name), n, 10) }

// finish closes the body's object and checks its exact size against limit
// bytes, the API key included.
func (b body) finish(limit int) ([]byte, error) {
	out := append(b, '}')
	if len(out) > limit {
		return nil, invalidRequest("request", reasonTooLarge)
	}
	return out, nil
}

// appendString appends s as a JSON string. s is valid UTF-8, which validation
// has checked, so it is copied as it is apart from the escapes JSON requires.
func appendString[T ~[]byte](dst T, s string) T {
	dst = append(dst, '"')
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// checkText applies the rules every request text field shares: at most maxBytes
// bytes of valid UTF-8 without control characters, and not only whitespace.
// An empty value is valid unless required; an empty optional value is simply
// not sent.
func checkText(field, s string, maxBytes int, required bool) error {
	if s == "" {
		if required {
			return invalidRequest(field, reasonRequired)
		}
		return nil
	}
	if err := checkValue(field, s, maxBytes); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		return invalidRequest(field, reasonBlank)
	}
	return nil
}

// checkValue applies the text rules without the blank rule, for data values
// that may legitimately be empty or whitespace.
func checkValue(field, s string, maxBytes int) error {
	if reason := valueProblem(s, maxBytes); reason != "" {
		return invalidRequest(field, reason)
	}
	return nil
}

// valueProblem returns the reason s breaks the text rules without the blank
// rule, or "" when it keeps them. It builds nothing, so callers can render a
// field path only on failure.
func valueProblem(s string, maxBytes int) string {
	switch {
	case len(s) > maxBytes:
		return reasonTooLong
	case !utf8.ValidString(s):
		return reasonInvalidUTF8
	case strings.ContainsFunc(s, unicode.IsControl):
		return reasonControlCharacter
	}
	return ""
}

// checkJobID validates a job handle: it must be greater than 0.
func checkJobID(id JobID) error {
	switch {
	case id == 0:
		return invalidRequest("JobID", reasonRequired)
	case id < 0:
		return invalidRequest("JobID", reasonOutOfRange)
	}
	return nil
}
