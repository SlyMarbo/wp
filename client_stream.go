package wp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// clientStream is a structure that implements
// the Stream and ResponseWriter interfaces. This
// is used for responding to client requests.
type clientStream struct {
	sync.RWMutex
	conn            *clientConnection
	content         *bytes.Buffer
	streamID        uint32
	state           *StreamState
	output          chan<- Frame
	request         *http.Request
	receiver        Receiver
	headers         http.Header
	responseCode    int
	responseSubcode int
	stop            bool
	version         uint8
	done            chan struct{}
}

func (s *clientStream) Connection() Connection {
	return s.conn
}

func (s *clientStream) Header() http.Header {
	return s.headers
}

func (s *clientStream) Ping() <-chan bool {
	return s.conn.Ping()
}

func (s *clientStream) Push(string) (PushWriter, error) {
	panic("Error: Request stream cannot push.")
}

func (s *clientStream) Read(out []byte) (int, error) {
	n, err := s.content.Read(out)
	if err != io.EOF || s.state.ClosedThere() {
		return n, err
	}
	return n, nil
}

func (s *clientStream) ReceiveFrame(frame Frame) {
	s.Lock()
	s.receiveFrame(frame)
	s.Unlock()
}

func (s *clientStream) State() *StreamState {
	return s.state
}

func (s *clientStream) Stop() {
	s.stop = true
	if s.state.OpenHere() {
		rst := new(ErrorFrame)
		rst.streamID = s.streamID
		rst.Status = FINISH_STREAM
		s.output <- rst
	}
	s.done <- struct{}{}
}

func (s *clientStream) StreamID() uint32 {
	return s.streamID
}

// Write is one method with which request data is sent.
func (s *clientStream) Write(inputData []byte) (int, error) {
	if s.state.ClosedHere() {
		return 0, errors.New("Error: Stream already closed.")
	}

	if s.stop {
		return 0, ErrCancelled
	}

	// Copy the data locally to avoid any pointer issues.
	data := make([]byte, len(inputData))
	copy(data, inputData)

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
func (s *clientStream) WriteHeaders() {
	if len(s.headers) == 0 {
		return
	}

	// Create the HEADERS frame.
	headers := new(HeadersFrame)
	headers.streamID = s.streamID
	headers.Headers = cloneHeaders(s.headers)

	// Clear the headers that have been sent.
	for name := range headers.Headers {
		s.headers.Del(name)
	}

	s.output <- headers
}

// WriteHeader is used to set the WP status code.
func (s *clientStream) WriteResponse(int, int) {
	panic("Error: Cannot write status code on request.")
}

func (s *clientStream) Version() uint8 {
	return s.version
}

// receiveFrame is used to process an inbound frame.
func (s *clientStream) receiveFrame(frame Frame) {
	if frame == nil {
		panic("Nil frame received in receiveFrame.")
	}

	// Process the frame depending on its type.
	switch frame := frame.(type) {
	case *DataFrame:

		// Extract the data.
		data := frame.Data
		if data == nil {
			data = []byte{}
		}

		// Check whether this is the last frame.
		finish := frame.flags&FLAG_FIN != 0

		// Give to the client.
		if s.receiver != nil {
			s.receiver.ReceiveData(s.request, data, finish)
		} else {
			err := Write(s.content, data)
			if err != nil {
				panic(err)
			}
		}

	case *ResponseFrame:
		if s.receiver != nil {
			s.receiver.ReceiveHeaders(s.request, frame.Headers)
			s.receiver.ReceiveResponse(s.request, frame.ResponseCode, frame.ResponseSubcode)
		}

	case *HeadersFrame:
		if s.receiver != nil {
			s.receiver.ReceiveHeaders(s.request, frame.Headers)
		}

	default:
		panic(fmt.Sprintf("Received unknown frame of type %T.", frame))
	}
}

// Run is the main control path of
// the stream. Data is recieved,
// processed, and then the stream
// is cleaned up and closed.
func (s *clientStream) Run() {
	s.conn.done.Add(1)

	// Receive and process inbound frames.
	s.Wait()

	// Clean up state.
	s.state.CloseHere()
	s.conn.done.Done()
}

// Wait will block until the stream
// ends.
func (s *clientStream) Wait() {
	<-s.done
}
