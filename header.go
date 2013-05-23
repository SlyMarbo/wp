package wp

import (
	"bytes"
	"fmt"
	"io"
	"net/textproto"
	"sort"
	"strings"
)

// A Headers represents the key-value pairs in an HTTP header.
type Headers map[string][]string

// String pretty-prints the Headers content.
func (h Headers) String() string {
	buf := new(bytes.Buffer)
	buf.WriteString(fmt.Sprintf("Headers {"))
	for name, values := range h {
		buf.WriteString(fmt.Sprintf("\n\t\t%-16s", name))
		l := len(values) - 1
		for i, value := range values {
			buf.WriteString(fmt.Sprintf("%s", value))
			if i < l {
				for j := 0; j < 16; j++ {
					buf.WriteString("\n\t\t ")
				}
			}
		}
	}
	buf.WriteString(fmt.Sprintf("\n\t}\n"))
	return buf.String()
}

// Parse decompresses the data and parses the resulting
// data into the wp.Headers.
func (h Headers) Parse(data []byte, dec *Decompressor) error {
	header, err := dec.Decompress(data)
	if err != nil {
		return err
	}

	for name, values := range header {
		for _, value := range values {
			h.Add(name, value)
		}
	}
	return nil
}

// Bytes returns the headers in
// SPDY name/value header block
// format.
func (h Headers) Bytes() []byte {
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Connection")
	h.Del("Transfer-Encoding")

	length := 4
	num := len(h)
	lens := make(map[string]int)
	for name, values := range h {
		length += len(name) + 8
		lens[name] = len(values) - 1
		for _, value := range values {
			length += len(value)
			lens[name] += len(value)
		}
	}

	out := make([]byte, length)
	out[0] = byte(num >> 24)
	out[1] = byte(num >> 16)
	out[2] = byte(num >> 8)
	out[3] = byte(num)

	offset := 4
	for name, values := range h {
		nLen := len(name)
		out[offset+0] = byte(nLen >> 24)
		out[offset+1] = byte(nLen >> 16)
		out[offset+2] = byte(nLen >> 8)
		out[offset+3] = byte(nLen)
		offset += 4

		for i, b := range []byte(strings.ToLower(name)) {
			out[offset+i] = b
		}

		offset += nLen

		vLen := lens[name]
		out[offset+0] = byte(vLen >> 24)
		out[offset+1] = byte(vLen >> 16)
		out[offset+2] = byte(vLen >> 8)
		out[offset+3] = byte(vLen)
		offset += 4

		for n, value := range values {
			for i, b := range []byte(value) {
				out[offset+i] = b
			}
			offset += len(value)
			if n < len(values)-1 {
				out[offset] = '\x00'
				offset += 1
			}
		}
	}

	return out
}

// Compressed returns the binary data of the headers
// once they have been compressed.
func (h Headers) Compressed(com *Compressor) ([]byte, error) {
	return com.Compress(h.Bytes())
}

// Add adds the key, value pair to the header.
// It appends to any existing values associated with key.
func (h Headers) Add(key, value string) {
	textproto.MIMEHeader(h).Add(key, value)
}

// Set sets the header entries associated with key to
// the single element value.  It replaces any existing
// values associated with key.
func (h Headers) Set(key, value string) {
	textproto.MIMEHeader(h).Set(key, value)
}

// Get gets the first value associated with the given key.
// If there are no values associated with the key, Get returns "".
// To access multiple values of a key, access the map directly
// with CanonicalHeaderKey.
func (h Headers) Get(key string) string {
	return textproto.MIMEHeader(h).Get(key)
}

// Update uses a second set of headers to update the previous.
// New headers are added, and old headers are replaced. Headers
// in the old set but not the new are left unmodified.
func (old Headers) Update(update Headers) {
	if old == nil {
		old = update.clone()
		return
	}
	for name, values := range update {
		for i, value := range values {
			if i == 0 {
				old.Set(name, value)
			} else {
				old.Add(name, value)
			}
		}
	}
}

// get is like Get, but key must already be in CanonicalHeaderKey form.
func (h Headers) get(key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Del deletes the values associated with key.
func (h Headers) Del(key string) {
	textproto.MIMEHeader(h).Del(key)
}

// Write writes a header in wire format.
func (h Headers) Write(w io.Writer) error {
	return h.WriteSubset(w, nil)
}

func (h Headers) clone() Headers {
	h2 := make(Headers, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

var headerNewlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")

type writeStringer interface {
	WriteString(string) (int, error)
}

// stringWriter implements WriteString on a Writer.
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (n int, err error) {
	return w.w.Write([]byte(s))
}

type keyValues struct {
	key    string
	values []string
}

// A headerSorter implements sort.Interface by sorting a []keyValues
// by key. It's used as a pointer, so it can fit in a sort.Interface
// interface value without allocation.
type headerSorter struct {
	kvs []keyValues
}

func (s *headerSorter) Len() int           { return len(s.kvs) }
func (s *headerSorter) Swap(i, j int)      { s.kvs[i], s.kvs[j] = s.kvs[j], s.kvs[i] }
func (s *headerSorter) Less(i, j int) bool { return s.kvs[i].key < s.kvs[j].key }

// TODO: convert this to a sync.Cache (issue 4720)
var headerSorterCache = make(chan *headerSorter, 8)

// sortedKeyValues returns h's keys sorted in the returned kvs
// slice. The headerSorter used to sort is also returned, for possible
// return to headerSorterCache.
func (h Headers) sortedKeyValues(exclude map[string]bool) (kvs []keyValues, hs *headerSorter) {
	select {
	case hs = <-headerSorterCache:
	default:
		hs = new(headerSorter)
	}
	if cap(hs.kvs) < len(h) {
		hs.kvs = make([]keyValues, 0, len(h))
	}
	kvs = hs.kvs[:0]
	for k, vv := range h {
		if !exclude[k] {
			kvs = append(kvs, keyValues{k, vv})
		}
	}
	hs.kvs = kvs
	sort.Sort(hs)
	return kvs, hs
}

// WriteSubset writes a header in wire format.
// If exclude is not nil, keys where exclude[key] == true are not written.
func (h Headers) WriteSubset(w io.Writer, exclude map[string]bool) error {
	ws, ok := w.(writeStringer)
	if !ok {
		ws = stringWriter{w}
	}
	kvs, sorter := h.sortedKeyValues(exclude)
	for _, kv := range kvs {
		for _, v := range kv.values {
			v = headerNewlineToSpace.Replace(v)
			v = textproto.TrimString(v)
			for _, s := range []string{kv.key, ": ", v, "\r\n"} {
				if _, err := ws.WriteString(s); err != nil {
					return err
				}
			}
		}
	}
	select {
	case headerSorterCache <- sorter:
	default:
	}
	return nil
}

// hasToken returns whether token appears with v, ASCII
// case-insensitive, with space or comma boundaries.
// token must be all lowercase.
// v may contain mixed cased.
func hasToken(v, token string) bool {
	if len(token) > len(v) || token == "" {
		return false
	}
	if v == token {
		return true
	}
	for sp := 0; sp <= len(v)-len(token); sp++ {
		// Check that first character is good.
		// The token is ASCII, so checking only a single byte
		// is sufficient.  We skip this potential starting
		// position if both the first byte and its potential
		// ASCII uppercase equivalent (b|0x20) don't match.
		// False positives ('^' => '~') are caught by EqualFold.
		if b := v[sp]; b != token[0] && b|0x20 != token[0] {
			continue
		}
		// Check that start pos is on a valid token boundary.
		if sp > 0 && !isTokenBoundary(v[sp-1]) {
			continue
		}
		// Check that end pos is on a valid token boundary.
		if endPos := sp + len(token); endPos != len(v) && !isTokenBoundary(v[endPos]) {
			continue
		}
		if strings.EqualFold(v[sp:sp+len(token)], token) {
			return true
		}
	}
	return false
}

func isTokenBoundary(b byte) bool {
	return b == ' ' || b == ',' || b == '\t'
}
