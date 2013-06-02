package wp

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// clientStream is a structure that implements
// the Stream and ResponseWriter interfaces. This
// is used for responding to client requests.
type clientStream struct {
	sync.Mutex
	conn            Conn
	streamID        StreamID
	state           *StreamState
	output          chan<- Frame
	request         *http.Request
	receiver        Receiver
	header          http.Header
	responseCode    int
	responseSubcode int
	stop            <-chan struct{}
	finished        chan struct{}
}

/***********************
 * http.ResponseWriter *
 ***********************/

func (s *clientStream) Header() http.Header {
	return s.header
}

// Write is one method with which request data is sent.
func (s *clientStream) Write(inputData []byte) (int, error) {
	if s.closed() || s.state.ClosedHere() {
		return 0, errors.New("Error: Stream already closed.")
	}

	// Copy the data locally to avoid any pointer issues.
	data := make([]byte, len(inputData))
	copy(data, inputData)

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
func (s *clientStream) WriteHeader(int) {
	s.writeHeader()
}

/*****************
 * io.ReadCloser *
 *****************/

// Close is used to stop the stream safely.
func (s *clientStream) Close() error {
	s.Lock()
	defer s.Unlock()
	s.writeHeader()
	if s.state != nil {
		s.state.Close()
		s.state = nil
	}
	s.output = nil
	s.request = nil
	s.receiver = nil
	s.header = nil
	s.stop = nil
	return nil
}

func (s *clientStream) Read(out []byte) (int, error) {
	// TODO
	return 0, nil
}

/**********
 * Stream *
 **********/

func (s *clientStream) Conn() Conn {
	return s.conn
}

func (s *clientStream) ReceiveFrame(frame Frame) error {
	s.Lock()
	defer s.Unlock()

	if frame == nil {
		return errors.New("Nil frame received in receiveFrame.")
	}

	// Process the frame depending on its type.
	switch frame := frame.(type) {
	case *dataFrame:

		// Extract the data.
		data := frame.Data
		if data == nil {
			data = []byte{}
		}

		// Give to the client.
		if s.receiver != nil {
			s.receiver.ReceiveData(s.request, data, frame.Flags.FINISH())
		}

	case *responseFrame:
		if s.receiver != nil {
			s.receiver.ReceiveHeader(s.request, frame.Header)
		}

	case *headersFrame:
		if s.receiver != nil {
			s.receiver.ReceiveHeader(s.request, frame.Header)
		}

	default:
		return errors.New(fmt.Sprintf("Received unknown frame of type %T.", frame))
	}

	return nil
}

// run is the main control path of
// the stream. Data is recieved,
// processed, and then the stream
// is cleaned up and closed.
func (s *clientStream) Run() error {
	// Receive and process inbound frames.
	<-s.finished

	// Clean up state.
	s.state.CloseHere()
	return nil
}

func (s *clientStream) State() *StreamState {
	return s.state
}

func (s *clientStream) StreamID() StreamID {
	return s.streamID
}

// WriteHeader is used to set the WP status code.
func (s *clientStream) WriteResponse(int, int) {
	panic("Error: Cannot write status code on request.")
}

func (s *clientStream) closed() bool {
	if s.conn == nil || s.state == nil || s.receiver == nil {
		return true
	}
	select {
	case _ = <-s.stop:
		return true
	default:
		return false
	}
}

// WriteHeader is used to flush HTTP headers.
func (s *clientStream) writeHeader() {
	if len(s.header) == 0 {
		return
	}

	// Create the HEADERS frame.
	headers := new(headersFrame)
	headers.StreamID = s.streamID
	headers.Header = cloneHeader(s.header)

	// Clear the headers that have been sent.
	for name := range headers.Header {
		s.header.Del(name)
	}

	s.output <- headers
}
