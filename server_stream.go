package wp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// serverStream is a structure that implements
// the Stream and ResponseWriter interfaces. This
// is used for responding to client requests.
type serverStream struct {
	sync.Mutex
	conn            Conn
	streamID        StreamID
	requestBody     *bytes.Buffer
	state           *StreamState
	output          chan<- Frame
	request         *http.Request
	handler         http.Handler
	header          http.Header
	responseCode    int
	responseSubcode int
	wroteHeader     bool
	stop            <-chan struct{}
}

/***********************
 * http.ResponseWriter *
 ***********************/

func (s *serverStream) Header() http.Header {
	return s.header
} // Write is the main method with which data is sent.
func (s *serverStream) Write(inputData []byte) (int, error) {
	if s.closed() || s.state.ClosedHere() {
		return 0, errors.New("Error: Stream already closed.")
	}

	// Copy the data locally to avoid any pointer issues.
	data := make([]byte, len(inputData))
	copy(data, inputData)

	// Default to 200 response.
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}

	// Send any new headers.
	s.writeHeader()

	// Chunk the response if necessary.
	written := 0
	for len(data) > MAX_DATA_SIZE {
		dataFrame := new(dataFrame)
		dataFrame.StreamID = s.streamID
		dataFrame.Data = data[:MAX_DATA_SIZE]
		s.output <- dataFrame

		written += MAX_DATA_SIZE
	}

	n := len(data)
	if n == 0 {
		return written, nil
	}

	dataFrame := new(dataFrame)
	dataFrame.StreamID = s.streamID
	dataFrame.Data = data
	s.output <- dataFrame

	return written + n, nil
}

// WriteHeader is used to set the HTTP status code.
func (s *serverStream) WriteHeader(code int) {
	if s.wroteHeader {
		log.Println("Error: Multiple calls to ResponseWriter.WriteHeader.")
		return
	}

	s.wroteHeader = true
	s.responseCode = code
	s.header.Set("status", strconv.Itoa(code))
	s.header.Set("version", "HTTP/1.1")

	// Create the Response.
	response := new(responseFrame)
	response.StreamID = s.streamID
	response.Header = cloneHeader(s.header)

	// Clear the headers that have been sent.
	for name := range response.Header {
		s.header.Del(name)
	}

	// These responses have no body, so close the stream now.
	if code == 204 || code == 304 || code/100 == 1 {
		response.Flags = FLAG_FINISH
		s.state.CloseHere()
	}

	s.output <- response

	s.WriteResponse(httpToWpResponseCode(code))
}

/*****************
 * io.ReadCloser *
 *****************/

func (s *serverStream) Close() error {
	s.Lock()
	defer s.Unlock()
	s.writeHeader()
	if s.state != nil {
		s.state.Close()
		s.state = nil
	}
	if s.requestBody != nil {
		s.requestBody.Reset()
		s.requestBody = nil
	}
	s.output = nil
	s.request = nil
	s.handler = nil
	s.header = nil
	s.stop = nil
	return nil
}

func (s *serverStream) Read(out []byte) (int, error) {
	n, err := s.requestBody.Read(out)
	if err == io.EOF && s.state.OpenThere() {
		return n, nil
	}
	return n, err
}

/**********
 * Stream *
 **********/

func (s *serverStream) Conn() Conn {
	return s.conn
}

func (s *serverStream) ReceiveFrame(frame Frame) error {
	s.Lock()
	defer s.Unlock()

	if frame == nil {
		return errors.New("Nil frame received in receiveFrame.")
	}

	// Process the frame depending on its type.
	switch frame := frame.(type) {
	case *dataFrame:
		s.requestBody.Write(frame.Data) // TODO

	case *responseFrame:
		updateHeader(s.header, frame.Header)

	case *headersFrame:
		updateHeader(s.header, frame.Header)

	default:
		return errors.New(fmt.Sprintf("Received unknown frame of type %T.", frame))
	}

	return nil
}

// run is the main control path of
// the stream. It is prepared, the
// registered handler is called,
// and then the stream is cleaned
// up and closed.
func (s *serverStream) Run() error {
	// Make sure Request is prepared.
	s.requestBody = new(bytes.Buffer)
	s.request.Body = &readCloser{s.requestBody}

	/***************
	 *** HANDLER ***
	 ***************/
	s.handler.ServeHTTP(s, s.request)

	// Close the stream with a Response if
	// none has been sent, or an empty Data
	// frame, if a Response has been sent
	// already.
	// If the stream is already closed at
	// this end, then nothing happens.
	if s.state.OpenHere() && !s.wroteHeader {
		s.header.Set("status", "200")
		s.header.Set("version", "HTTP/1.1")

		// Create the Response.
		response := new(responseFrame)
		response.Flags = FLAG_FINISH
		response.StreamID = s.streamID
		response.Header = s.header

		s.output <- response
	} else if s.state.OpenHere() {
		// Create the Data.
		data := new(dataFrame)
		data.StreamID = s.streamID
		data.Flags = FLAG_FINISH
		data.Data = []byte{}

		s.output <- data
	}

	// Clean up state.
	s.state.CloseHere()
	return nil
}

func (s *serverStream) State() *StreamState {
	return s.state
}

func (s *serverStream) StreamID() StreamID {
	return s.streamID
}

// WriteResponse is used to set the WP status code.
func (s *serverStream) WriteResponse(code, subcode int) {
	if s.wroteHeader {
		log.Println("Error: Multiple calls to ResponseWriter.WriteHeader.")
		return
	}

	s.wroteHeader = true
	s.responseCode = code
	s.responseSubcode = subcode

	// Create the response SYN_REPLY.
	reply := new(responseFrame)
	reply.StreamID = s.streamID
	reply.Header = cloneHeader(s.header)

	// Clear the headers that have been sent.
	for name := range reply.Header {
		s.header.Del(name)
	}

	// These responses have no body, so close the stream now.
	if (code == StatusSuccess && subcode == StatusCached) || code == StatusRedirection {
		reply.Flags = FLAG_FINISH
		s.state.CloseHere()
	}

	s.output <- reply
}

func (s *serverStream) closed() bool {
	if s.conn == nil || s.state == nil || s.handler == nil {
		return true
	}
	select {
	case _ = <-s.stop:
		return true
	default:
		return false
	}
}

// writeHeader is used to flush HTTP headers.
func (s *serverStream) writeHeader() {
	if len(s.header) == 0 {
		return
	}

	// Create the HEADERS frame.
	header := new(headersFrame)
	header.StreamID = s.streamID
	header.Header = cloneHeader(s.header)

	// Clear the headers that have been sent.
	for name := range header.Header {
		s.header.Del(name)
	}

	s.output <- header
}
