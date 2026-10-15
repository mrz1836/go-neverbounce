package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Esc returns s with every ~ replaced by a backslash, so that JSON escapes read
// naturally in raw string literals: Esc(`"~u0061"`) is the JSON string "a"
// spelled with an escape.
func Esc(s string) string { return strings.ReplaceAll(s, "~", `\`) }

// JSONQuote returns s as a JSON string literal, escaped as encoding/json
// escapes it.
func JSONQuote(s string) string {
	b, _ := json.Marshal(s) //nolint:errchkjson // a string always marshals: invalid UTF-8 becomes U+FFFD
	return string(b)
}

// TokenWalk walks data token by token with encoding/json, as an oracle for the
// hand-written scanners. valid reports whether data is one well-formed JSON
// value in valid UTF-8 with nothing after it. dup reports whether an object the
// walk reached repeats a key once decoded; for valid data that is every object.
func TokenWalk(data []byte) (valid, dup bool) {
	type frame struct {
		keys    map[string]struct{}
		wantKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return false, dup
		}
		if n := len(stack); n > 0 && stack[n-1].wantKey {
			if k, ok := tok.(string); ok {
				_, seen := stack[n-1].keys[k]
				dup = dup || seen
				stack[n-1].keys[k] = struct{}{}
				stack[n-1].wantKey = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, frame{keys: map[string]struct{}{}, wantKey: true})
			continue
		case json.Delim('['):
			stack = append(stack, frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			break
		}
		if top := &stack[len(stack)-1]; top.keys != nil {
			top.wantKey = true
		}
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF) && utf8.Valid(data), dup
}
