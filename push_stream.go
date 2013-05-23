package wp

import (
	"errors"
	"sync"
)

// pushStream is a structure that implements the
// Stream and PushWriter interfaces. this is used
// for performing server pushes.
type pushStream struct {
	sync.RWMutex
	conn        *serverConnection
	streamID    uint32
	origin      Stream
	state       *StreamState
	output      chan<- Frame
	headers     Headers
	headersSent bool
	stop        bool
	cancelled   bool
	version     uint8
}

func (p *pushStream) Cancel() {
	p.Lock()
	p.cancelled = true
	rst := new(ErrorFrame)
	rst.streamID = p.streamID
	rst.Status = FINISH_STREAM
	p.output <- rst
	p.Unlock()
}

func (p *pushStream) Connection() Connection {
	return p.conn
}

// Close is used to complete a server push. This
// closes the underlying stream and signals to
// the recipient that the push is complete. The
// equivalent action in a ResponseWriter is to
// return from the handler. Any calls to Write
// after calling Close will have no effect.
func (p *pushStream) Close() {
	p.stop = true

	stop := new(DataFrame)
	stop.streamID = p.streamID
	stop.flags = FLAG_FIN
	stop.Data = []byte{}

	p.output <- stop

	p.state.CloseHere()
}

func (p *pushStream) Headers() Headers {
	return p.headers
}

func (p *pushStream) State() *StreamState {
	return p.state
}

func (p *pushStream) Stop() {
	p.stop = true
}

func (p *pushStream) StreamID() uint32 {
	return p.streamID
}

// Write is used for sending data in the push.
func (p *pushStream) Write(inputData []byte) (int, error) {
	if p.cancelled {
		return 0, errors.New("Error: Push has been cancelled.")
	}

	if p.state.ClosedHere() {
		return 0, errors.New("Error: Stream already closed.")
	}

	state := p.origin.State()
	if p.origin == nil || state.ClosedHere() {
		return 0, errors.New("Error: Origin stream is closed.")
	}

	if p.stop {
		return 0, ErrCancelled
	}

	p.WriteHeaders()

	// Copy the data locally to avoid any pointer issues.
	data := make([]byte, len(inputData))
	copy(data, inputData)

	// Chunk the response if necessary.
	written := 0
	for len(data) > MAX_DATA_SIZE {
		dataFrame := new(DataFrame)
		dataFrame.streamID = p.streamID
		dataFrame.Data = data[:MAX_DATA_SIZE]
		p.output <- dataFrame

		written += MAX_DATA_SIZE
	}

	n := len(data)
	if n == 0 {
		return written, nil
	}

	dataFrame := new(DataFrame)
	dataFrame.streamID = p.streamID
	dataFrame.Data = data
	p.output <- dataFrame

	return written + n, nil
}

// WriteHeaders is used to send HTTP headers to
// the client.
func (p *pushStream) WriteHeaders() {
	if len(p.headers) == 0 {
		return
	}

	headers := new(HeadersFrame)
	headers.streamID = p.streamID
	headers.Headers = p.headers.clone()
	for name := range headers.Headers {
		p.headers.Del(name)
	}
	p.output <- headers
}

// WriteResponse is provided to satisfy the Stream
// interface, but has no effect.
func (p *pushStream) WriteResponse(int, int) {
	log.Println("Warning: PushWriter.WriteHeader has no effect.")
	p.WriteHeaders()
	return
}

func (p *pushStream) Version() uint8 {
	return p.version
}

func (p *pushStream) Run() {
	panic("Error: Push cannot run.")
}

func (s *pushStream) Wait() {
	panic("Error: Push cannot run.")
}
