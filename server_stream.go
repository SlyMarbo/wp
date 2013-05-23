package wp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// serverStream is a structure that implements
// the Stream and ResponseWriter interfaces. This
// is used for responding to client requests.
type serverStream struct {
	sync.RWMutex
	conn            *serverConnection
	streamID        uint32
	requestBody     *bytes.Buffer
	state           *StreamState
	output          chan<- Frame
	request         *Request
	handler         Handler
	httpHandler     http.Handler
	headers         Headers
	responseCode    int
	responseSubcode int
	stop            bool
	wroteHeader     bool
	version         uint8
	done            chan struct{}
}

func (s *serverStream) Cancel() {
	panic("Error: Client-sent stream cancelled. Use Stop() instead.")
}

func (s *serverStream) Connection() Connection {
	return s.conn
}

func (s *serverStream) Headers() Headers {
	return s.headers
}

func (s *serverStream) Ping() <-chan bool {
	return s.conn.Ping()
}

func (s *serverStream) Push(resource string) (PushWriter, error) {
	return s.conn.Push(resource, s)
}

func (s *serverStream) Read(out []byte) (int, error) {
	n, err := s.requestBody.Read(out)
	if err != io.EOF || s.state.ClosedThere() {
		return n, err
	}
	return n, nil
}

func (s *serverStream) ReceiveFrame(frame Frame) {
	s.Lock()
	s.receiveFrame(frame)
	s.Unlock()
}

func (s *serverStream) State() *StreamState {
	return s.state
}

func (s *serverStream) Stop() {
	s.stop = true
	s.done <- struct{}{}
}

func (s *serverStream) StreamID() uint32 {
	return s.streamID
}

// Write is the main method with which data is sent.
func (s *serverStream) Write(inputData []byte) (int, error) {
	if s.state.ClosedHere() {
		return 0, errors.New("Error: Stream already closed.")
	}

	if s.stop {
		return 0, ErrCancelled
	}

	// Copy the data locally to avoid any pointer issues.
	data := make([]byte, len(inputData))
	copy(data, inputData)

	// Default to 0/0 response.
	if !s.wroteHeader {
		s.WriteResponse(StatusSuccess, StatusSuccess)
	}

	// Send any new headers.
	s.WriteHeaders()

	// Chunk the response if necessary.
	written := 0
	for len(data) > MAX_DATA_SIZE {
		dataFrame := new(DataFrame)
		dataFrame.streamID = s.streamID
		dataFrame.Data = data[:MAX_DATA_SIZE]
		s.output <- dataFrame

		written += MAX_DATA_SIZE
	}

	n := len(data)
	if n == 0 {
		return written, nil
	}

	dataFrame := new(DataFrame)
	dataFrame.streamID = s.streamID
	dataFrame.Data = data
	s.output <- dataFrame

	return written + n, nil
}

// WriteHeaders is used to flush HTTP headers.
func (s *serverStream) WriteHeaders() {
	if len(s.headers) == 0 {
		return
	}

	// Create the HEADERS frame.
	headers := new(HeadersFrame)
	headers.streamID = s.streamID
	headers.Headers = s.headers.clone()

	// Clear the headers that have been sent.
	for name := range headers.Headers {
		s.headers.Del(name)
	}

	s.output <- headers
}

// WriteResponse is used to set the WP status code.
func (s *serverStream) WriteResponse(code, subcode int) {
	if s.wroteHeader {
		log.Println("spdy: Error: Multiple calls to ResponseWriter.WriteHeader.")
		return
	}

	s.wroteHeader = true
	s.responseCode = code
	s.responseSubcode = subcode

	// Create the response SYN_REPLY.
	reply := new(ResponseFrame)
	reply.streamID = s.streamID
	reply.Headers = s.headers.clone()

	// Clear the headers that have been sent.
	for name := range reply.Headers {
		s.headers.Del(name)
	}

	// These responses have no body, so close the stream now.
	if (code == StatusSuccess && subcode == StatusCached) || code == StatusRedirection {
		reply.flags = FLAG_FIN
		s.state.CloseHere()
	}

	s.output <- reply
}

func (s *serverStream) Version() uint8 {
	return s.version
}

// receiveFrame is used to process an inbound frame.
func (s *serverStream) receiveFrame(frame Frame) {
	if frame == nil {
		panic("Nil frame received in receiveFrame.")
	}

	// Process the frame depending on its type.
	switch frame := frame.(type) {
	case *DataFrame:
		s.requestBody.Write(frame.Data) // TODO

	case *ResponseFrame:
		s.headers.Update(frame.Headers)

	case *HeadersFrame:
		s.headers.Update(frame.Headers)

	default:
		panic(fmt.Sprintf("Received unknown frame of type %T.", frame))
	}
}

// run is the main control path of
// the stream. It is prepared, the
// registered handler is called,
// and then the stream is cleaned
// up and closed.
func (s *serverStream) Run() {
	s.conn.done.Add(1)

	// Make sure Request is prepared.
	s.requestBody = new(bytes.Buffer)
	s.request.Body = &readCloserBuffer{s}

	/***************
	 *** HANDLER ***
	 ***************/
	mux, ok := s.handler.(*ServeMux)
	if s.handler == nil || (ok && mux.Nil()) {
		r := wpToHttpRequest(s.request)
		w := &_httpResponseWriter{s}
		s.httpHandler.ServeHTTP(w, r)
	} else {
		s.handler.ServeWP(s, s.request)
	}

	// Close the stream with a Response if
	// none has been sent, or an empty Data
	// frame, if a Response has been sent
	// already.
	// If the stream is already closed at
	// this end, then nothing happens.
	if s.state.OpenHere() && !s.wroteHeader {

		// Create the Response.
		reply := new(ResponseFrame)
		reply.flags = FLAG_FIN
		reply.streamID = s.streamID
		reply.Headers = s.headers

		s.output <- reply
	} else if s.state.OpenHere() {
		// Create the Data.
		data := new(DataFrame)
		data.streamID = s.streamID
		data.flags = FLAG_FIN
		data.Data = []byte{}

		s.output <- data
	}

	s.Wait()

	// Clean up state.
	s.state.CloseHere()
	s.conn.done.Done()
}

// Wait will block until the stream
// ends.
func (s *serverStream) Wait() {
	<-s.done
}
