package wp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

/**************
 * Interfaces *
 **************/

// Connection represents a WP connection. The connection should
// be started with a call to Run, which will return once the
// connection has been terminated. The connection can be ended
// early by using Close.
type Conn interface {
	io.Closer
	Ping() (<-chan Ping, error)
	Push(url string, origin Stream) (http.ResponseWriter, error)
	Request(request *http.Request, receiver Receiver, priority Priority) (Stream, error)
	Run() error
}

// Stream contains a single WP stream.
type Stream interface {
	http.ResponseWriter
	io.ReadCloser
	Conn() Conn
	ReceiveFrame(Frame) error
	Run() error
	State() *StreamState
	StreamID() StreamID
	WriteResponse(int, int)
}

// Frame represents a single WP frame.
type Frame interface {
	fmt.Stringer
	io.ReaderFrom
	io.WriterTo
	Compress(Compressor) error
	Decompress(Decompressor) error
}

// Compressor is used to compress the text header of a WP frame.
type Compressor interface {
	io.Closer
	Compress(http.Header) ([]byte, error)
}

// Decompressor is used to decompress the text header of a WP frame.
type Decompressor interface {
	Decompress([]byte) (http.Header, error)
}

// Objects implementing the Receiver interface can be
// registered to receive requests on the Client.
//
// ReceiveData is passed the original request, the data
// to receive and a bool indicating whether this is the
// final batch of data. If the bool is set to true, the
// data may be empty, but should not be nil.
//
// ReceiveHeaders is passed the request and any sent
// text headers. This may be called multiple times.
//
// ReceiveRequest is used when server pushes are sent.
// The returned bool should inticate whether to accept
// the push. The provided Request will be that sent by
// the server with the push.
type Receiver interface {
	ReceiveData(request *http.Request, data []byte, final bool)
	ReceiveHeader(request *http.Request, header http.Header)
	ReceiveRequest(request *http.Request) bool
}

/********
 * Ping *
 ********/

// Ping is used in indicating the response from a ping request.
type Ping struct{}

/************
 * StreamID *
 ************/

// StreamID is the unique identifier for a single WP stream.
type StreamID uint32

func (s StreamID) b1() byte {
	return byte(s >> 24)
}

func (s StreamID) b2() byte {
	return byte(s >> 16)
}

func (s StreamID) b3() byte {
	return byte(s >> 8)
}

func (s StreamID) b4() byte {
	return byte(s)
}

// Client indicates whether the ID should belong to a client-sent stream.
func (s StreamID) Client() bool {
	return s != 0 && s&1 != 0
}

// Server indicates whether the ID should belong to a server-sent stream.
func (s StreamID) Server() bool {
	return s != 0 && s&1 == 0
}

// Valid indicates whether the ID is in the range of legal values (including 0).
func (s StreamID) Valid() bool {
	return s <= MAX_STREAM_ID
}

// Zero indicates whether the ID is zero.
func (s StreamID) Zero() bool {
	return s == 0
}

/*********
 * Flags *
 *********/

// Flags represent a frame's Flags.
type Flags byte

// FINISH indicates whether the FINISH flag is set.
func (f Flags) FINISH() bool {
	return f&FLAG_FINISH != 0
}

// READY indicates whether the READY flag is set.
func (f Flags) READY() bool {
	return f&FLAG_READY != 0
}

/************
 * Priority *
 ************/

// Priority represents a stream's priority.
type Priority byte

// Byte returns the priority in binary form.
func (p Priority) Byte() byte {
	return byte((p & 7) << 5)
}

// Valid indicates whether the priority is in the valid
// range.
func (p Priority) Valid() bool {
	return p <= 7
}

/**************
 * StatusCode *
 **************/

// StatusCode represents a status code sent in Error frames.
type StatusCode uint32

func (r StatusCode) b1() byte {
	return byte(r >> 24)
}

func (r StatusCode) b2() byte {
	return byte(r >> 16)
}

func (r StatusCode) b3() byte {
	return byte(r >> 8)
}

func (r StatusCode) b4() byte {
	return byte(r)
}

// String gives the StatusCode in text form.
func (r StatusCode) String() string {
	return statusCodeText[r]
}

/***************
 * StreamState *
 ***************/

// StreamState is used to store and query the stream's state. The active methods
// do not directly affect the stream's state, but it will use that information
// to effect the changes.
type StreamState struct {
	sync.RWMutex
	s uint8
}

// Check whether the stream is open.
func (s *StreamState) Open() bool {
	s.RLock()
	defer s.RUnlock()
	return s.s == stateOpen
}

// Check whether the stream is closed.
func (s *StreamState) Closed() bool {
	s.RLock()
	defer s.RUnlock()
	return s.s == stateClosed
}

// Check whether the stream is half-closed at the other endpoint.
func (s *StreamState) ClosedThere() bool {
	s.RLock()
	defer s.RUnlock()
	return s.s == stateClosed || s.s == stateHalfClosedThere
}

// Check whether the stream is open at the other endpoint.
func (s *StreamState) OpenThere() bool {
	return !s.ClosedThere()
}

// Check whether the stream is half-closed at the other endpoint.
func (s *StreamState) ClosedHere() bool {
	s.RLock()
	defer s.RUnlock()
	return s.s == stateClosed || s.s == stateHalfClosedHere
}

// Check whether the stream is open locally.
func (s *StreamState) OpenHere() bool {
	return !s.ClosedHere()
}

// Closes the stream.
func (s *StreamState) Close() {
	s.Lock()
	s.s = stateClosed
	s.Unlock()
}

// Half-close the stream locally.
func (s *StreamState) CloseHere() {
	s.Lock()
	if s.s == stateOpen {
		s.s = stateHalfClosedHere
	} else if s.s == stateHalfClosedThere {
		s.s = stateClosed
	}
	s.Unlock()
}

// Half-close the stream at the other endpoint.
func (s *StreamState) CloseThere() {
	s.Lock()
	if s.s == stateOpen {
		s.s = stateHalfClosedThere
	} else if s.s == stateHalfClosedHere {
		s.s = stateClosed
	}
	s.Unlock()
}

/********************
 * Helper Functions *
 ********************/

// cloneHeader returns a duplicate of the provided Header.
func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

// updateHeader adds and new name/value pairs and replaces
// those already existing in the older header.
func updateHeader(older, newer http.Header) {
	for name, values := range newer {
		for i, value := range values {
			if i == 0 {
				older.Set(name, value)
			} else {
				older.Add(name, value)
			}
		}
	}
}

// frameNames provides the name for a particular WP
// frame type.
var frameNames = map[int]string{
	HEADERS:  "HEADERS",
	ERROR:    "ERROR",
	REQUEST:  "REQUEST",
	RESPONSE: "RESPONSE",
	PUSH:     "PUSH",
	DATA:     "DATA",
	PING:     "PING",
}

func bytesToUint16(b []byte) uint16 {
	return (uint16(b[0]) << 8) + uint16(b[1])
}

func bytesToUint24(b []byte) uint32 {
	return (uint32(b[0]) << 16) + (uint32(b[1]) << 8) + uint32(b[2])
}

func bytesToUint24Reverse(b []byte) uint32 {
	return (uint32(b[2]) << 16) + (uint32(b[1]) << 8) + uint32(b[0])
}

func bytesToUint32(b []byte) uint32 {
	return (uint32(b[0]) << 24) + (uint32(b[1]) << 16) + (uint32(b[2]) << 8) + uint32(b[3])
}

// read is used to ensure that the given number of bytes
// are read if possible, even if multiple calls to Read
// are required.
func read(r io.Reader, i int) ([]byte, error) {
	out := make([]byte, i)
	in := out[:]
	for i > 0 {
		if n, err := r.Read(in); err != nil {
			return nil, err
		} else {
			in = in[n:]
			i -= n
		}
	}
	return out, nil
}

// write is used to ensure that the given data is written
// if possible, even if multiple calls to Write are
// required.
func write(w io.Writer, data []byte) error {
	i := len(data)
	for i > 0 {
		if n, err := w.Write(data); err != nil {
			return err
		} else {
			data = data[n:]
			i -= n
		}
	}
	return nil
}

// readCloser is a helper structure to allow
// an io.Reader to satisfy the io.ReadCloser
// interface.
type readCloser struct {
	io.Reader
}

func (r *readCloser) Close() error {
	return nil
}

/**********
 * Errors *
 **********/

type incorrectFrame struct {
	got, expected int
}

func (i *incorrectFrame) Error() string {
	return fmt.Sprintf("Error: Frame %s tried to parse data for a %s.", frameNames[i.expected], frameNames[i.got])
}

type incorrectDataLength struct {
	got, expected int
}

func (i *incorrectDataLength) Error() string {
	return fmt.Sprintf("Error: Incorrect amount of data for frame: got %d bytes, expected %d.", i.got, i.expected)
}

var frameTooLarge = errors.New("Error: Frame too large.")

type invalidField struct {
	field         string
	got, expected int
}

func (i *invalidField) Error() string {
	return fmt.Sprintf("Error: Field %q recieved invalid data %d, expecting %d.", i.field, i.got, i.expected)
}

var streamIdTooLarge = errors.New("Error: Stream ID is too large.")

var streamIdIsZero = errors.New("Error: Stream ID is zero.")
