package neverbounce

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// Field bounds. A field over its bound fails the call as response_too_large:
// the SDK never truncates provider data it returns.
const (
	maxTextBytes       = 4 << 10  // provider text fields
	maxTokenBytes      = 64       // result, flag, job state and failure reason tokens
	maxFlags           = 64       // flags per verification
	maxDataBytes       = 64 << 10 // one results row's data, once normalized
	maxQueueIDBytes    = 128
	maxRetryTokenBytes = 1 << 10

	maxDiagnosticMessageBytes = 1 << 10
)

// maxFields is the most members any decoded object has (the jobs/status
// envelope).
const maxFields = 13

// decodeError is a decoding failure at a JSON path, such as
// "results[3].verification.result". It becomes a CallError once the operation
// is known.
type decodeError struct {
	kind  FailureKind
	cause error
	field string
}

// callError returns the CallError for e on an HTTP 200 response.
func (e *decodeError) callError(op Operation) *CallError {
	ce := newCallError(op, e.kind, http.StatusOK, e.cause)
	ce.Field = e.field
	return ce
}

func missingField(field string) *decodeError {
	return &decodeError{kind: FailureMalformedResponse, cause: errMissingField, field: field}
}

func wrongType(field string) *decodeError {
	return &decodeError{kind: FailureMalformedResponse, cause: errWrongType, field: field}
}

func outOfRange(field string) *decodeError {
	return &decodeError{kind: FailureMalformedResponse, cause: errOutOfRange, field: field}
}

func badToken(field string) *decodeError {
	return &decodeError{kind: FailureUnexpectedResponse, cause: errInvalidToken, field: field}
}

func fieldTooLarge(field string) *decodeError {
	return &decodeError{kind: FailureResponseTooLarge, cause: errFieldTooLarge, field: field}
}

func contradicts(field string) *decodeError {
	return &decodeError{kind: FailureUnexpectedResponse, cause: errContradiction, field: field}
}

func idMismatch(field string) *decodeError {
	return &decodeError{kind: FailureUnexpectedResponse, cause: errIDMismatch, field: field}
}

// path names a JSON location for error messages, rendered only when an error
// needs it: a base such as "results", an optional index, and an optional
// member path inside the indexed element, such as "verification".
type path struct {
	base  string
	index int
	inner string
}

// at returns the path of member inner of the element at index i of the list
// at base, such as results[3].verification.
func at(base string, i int, inner string) path { return path{base: base, index: i, inner: inner} }

// String renders the path, such as "results[3].verification".
func (p path) String() string {
	s := p.base
	if p.index >= 0 {
		s += "[" + strconv.Itoa(p.index) + "]"
	}
	if p.inner != "" {
		s += "." + p.inner
	}
	return s
}

// member renders the path of the member name inside p. A zero path renders
// name alone.
func (p path) member(name string) string {
	if p.base == "" {
		return name
	}
	return p.String() + "." + name
}

// element renders the path of element i of the list member name inside p.
func (p path) element(name string, i int) string {
	return p.member(name) + "[" + strconv.Itoa(i) + "]"
}

// object is the recognized members of one JSON object, by index into the
// names it was read with. A member that is absent is nil.
type object struct {
	fields [maxFields][]byte
}

// readObject reads the JSON object raw against names, matching keys exactly:
// unlike encoding/json, "Status" is not "status", so no case-folded key can
// shadow a documented one. Other members are ignored. checkStrict has
// already rejected repeated keys, escaped spellings included.
func readObject(raw []byte, names []string) object {
	var o object
	for key, value := range members(raw) {
		k := keyBytes(key)
		for i, name := range names {
			if string(k) == name {
				o.fields[i] = value
				break
			}
		}
	}
	return o
}

// slot is one member of a flat object: its name, and where its decoded value
// goes.
type slot[T any] struct {
	name string
	dst  *T
}

// decodeSlots reads the JSON object raw against the slots' names and decodes
// each member into its slot with dec. On failure it returns the index of the
// slot concerned, so that the caller renders the field path only then. The
// names never reach dec or the error: escape analysis does not tell a slot's
// name from its destination, so either would move the caller's result to the
// heap.
func decodeSlots[T any](raw []byte, slots []slot[T], dec func(raw []byte, field string) (T, *decodeError)) (int, *decodeError) {
	var names [maxFields]string
	for i, s := range slots {
		names[i] = s.name
	}
	o := readObject(raw, names[:len(slots)])
	for i, s := range slots {
		v, derr := dec(o.fields[i], "")
		if derr != nil {
			return i, derr
		}
		*s.dst = v
	}
	return 0, nil
}

// decodeText decodes optional provider text: a JSON string, or null or absent
// as "". A string over maxTextBytes fails as response_too_large.
func decodeText(raw []byte, field string) (string, *decodeError) {
	switch {
	case isNull(raw):
		return "", nil
	case raw[0] != '"':
		return "", wrongType(field)
	}
	s := decodeKey(raw)
	if len(s) > maxTextBytes {
		return "", fieldTooLarge(field)
	}
	return s, nil
}

// validToken reports whether s matches ^[a-z0-9_]{1,64}$, the shape of every
// documented result, flag, job state and failure reason.
func validToken(s string) bool {
	if s == "" || len(s) > maxTokenBytes {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// decodeToken decodes a token member. A required one must be present and a
// string; an optional one may be null or absent, and "" means unset. A
// string that is not a valid token fails as unexpected_response: the value
// is kept only when it has the documented shape.
func decodeToken(raw []byte, field string, required bool) (string, *decodeError) {
	switch {
	case isNull(raw):
		if required {
			return "", missingField(field)
		}
		return "", nil
	case raw[0] != '"':
		return "", wrongType(field)
	}
	s := decodeKey(raw)
	if s == "" && !required {
		return "", nil
	}
	if !validToken(s) {
		return "", badToken(field)
	}
	return s, nil
}

// decodeFlags decodes an optional list of flag tokens, in provider order with
// duplicates kept. An empty list is nil.
func decodeFlags(raw []byte, p path) (Flags, *decodeError) {
	switch {
	case isNull(raw):
		return nil, nil
	case raw[0] != '[':
		return nil, wrongType(p.member("flags"))
	}
	var out Flags
	for i, el := range elements(raw) {
		if i == maxFlags {
			return nil, fieldTooLarge(p.member("flags"))
		}
		if !isString(el) {
			return nil, wrongType(p.element("flags", i))
		}
		s := decodeKey(el)
		if !validToken(s) {
			return nil, badToken(p.element("flags", i))
		}
		out = append(out, Flag(s))
	}
	return out, nil
}

// parseInt parses the JSON number lit as an int64 with strconv.ParseInt,
// never through float64: a fraction, an exponent or an out-of-range value is
// not an integer. ok is false when lit is not an integer at all, and
// inRange is false when it is one that does not fit an int64.
func parseInt(lit []byte) (n int64, ok, inRange bool) {
	if len(lit) == 0 || (lit[0] != '-' && (lit[0] < '0' || lit[0] > '9')) {
		return 0, false, false
	}
	n, err := strconv.ParseInt(string(lit), 10, 64)
	if err == nil {
		return n, true, true
	}
	if bytes.ContainsAny(lit, ".eE") {
		return 0, false, false
	}
	return 0, true, false
}

// decodeCount decodes an optional count: a JSON integer from 0 to MaxInt64, or
// null or absent as nil.
func decodeCount(raw []byte, field string) (*int64, *decodeError) {
	if isNull(raw) {
		return nil, nil
	}
	n, ok, inRange := parseInt(raw)
	switch {
	case !ok:
		return nil, wrongType(field)
	case !inRange || n < 0:
		return nil, outOfRange(field)
	}
	return &n, nil
}

// decodeRequiredCount decodes a required count: a JSON integer from 0 to
// MaxInt64.
func decodeRequiredCount(raw []byte, field string) (int64, *decodeError) {
	if isNull(raw) {
		return 0, missingField(field)
	}
	n, derr := decodeCount(raw, field)
	if derr != nil {
		return 0, derr
	}
	return *n, nil
}

// decodeJobID decodes a required job handle: a JSON integer greater than 0.
// A quoted number is the wrong type.
func decodeJobID(raw []byte, field string) (JobID, *decodeError) {
	n, derr := decodeRequiredCount(raw, field)
	switch {
	case derr != nil:
		return 0, derr
	case n == 0:
		return 0, outOfRange(field)
	}
	return JobID(n), nil
}

// decodePercent decodes an optional percentage: a JSON number, integer or
// not, from 0 to 100, or null or absent as nil.
func decodePercent(raw []byte, field string) (*float64, *decodeError) {
	switch {
	case isNull(raw):
		return nil, nil
	case raw[0] != '-' && (raw[0] < '0' || raw[0] > '9'):
		return nil, wrongType(field)
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > 100 {
		return nil, outOfRange(field)
	}
	f += 0 // -0 becomes +0
	return &f, nil
}

// decodeData normalizes an optional results row "data" member: a JSON object
// or array, compacted and escaped exactly as encoding/json marshals a
// json.RawMessage, so that the row survives a JSON round trip unchanged. The
// copy never aliases the response body. Null or absent is nil.
func decodeData(raw []byte, field string) (json.RawMessage, *decodeError) {
	switch {
	case isNull(raw):
		return nil, nil
	case raw[0] != '{' && raw[0] != '[':
		return nil, wrongType(field)
	case len(raw) > 4*maxDataBytes:
		// Normalizing never shrinks a value by more than whitespace, and a
		// body this padded is not a row anyone needs.
		return nil, fieldTooLarge(field)
	}
	norm, err := json.Marshal(json.RawMessage(replaceLoneSurrogates(raw)))
	if err != nil {
		return nil, wrongType(field)
	}
	if len(norm) > maxDataBytes {
		return nil, fieldTooLarge(field)
	}
	return norm, nil
}

// executionTime decodes the metadata member execution_time: a JSON integer of
// at least 0, kept as the provider sent it. Anything else is dropped; it never
// fails a call.
func executionTime(raw []byte) *int64 {
	n, derr := decodeCount(raw, "execution_time")
	if derr != nil {
		return nil
	}
	return n
}

// queueID decodes the metadata member queue_id: a JSON string of 1 to 128
// visible ASCII bytes. Anything else is dropped; it never fails a call.
func queueID(raw []byte) string {
	if !isString(raw) {
		return ""
	}
	s := decodeKey(raw)
	if s == "" || len(s) > maxQueueIDBytes || !asciiRange(s, 0x21, 0x7e) {
		return ""
	}
	return s
}

// retryToken decodes the metadata member retry_token: a JSON string of at
// most 1 KiB. Anything else is dropped; it never fails a call.
func retryToken(raw []byte) string {
	if !isString(raw) {
		return ""
	}
	s := decodeKey(raw)
	if len(s) > maxRetryTokenBytes {
		return ""
	}
	return s
}

// asciiRange reports whether every byte of s is from lo to hi.
func asciiRange(s string, lo, hi byte) bool {
	for i := range len(s) {
		if c := s[i]; c < lo || c > hi {
			return false
		}
	}
	return true
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
