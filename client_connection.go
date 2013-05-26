package wp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"
)

// clientConnection represents a WP session at the client
// end. This performs the overall connection management and
// co-ordination between streams.
type clientConnection struct {
	sync.RWMutex
	remoteAddr         string
	client             *Client
	conn               *tls.Conn
	buf                *bufio.Reader // buffered reader for the connection.
	tlsState           *tls.ConnectionState
	streams            map[uint32]Stream
	dataOutput         chan Frame               // receiving frames from streams to send.
	pings              map[uint32]chan<- bool   // response channel for pings.
	pingID             uint32                   // next outbound ping ID.
	compressor         *Compressor              // outbound compression state.
	decompressor       *Decompressor            // inbound decompression state.
	nextServerStreamID uint32                   // next inbound stream ID. (even)
	nextClientStreamID uint32                   // next outbound stream ID. (odd)
	version            uint8                    // WP version.
	numBenignErrors    int                      // number of non-serious errors encountered.
	done               *sync.WaitGroup          // WaitGroup for active streams.
	pushReceiver       Receiver                 // Receiver used to process server pushes.
	pushRequests       map[uint32]*http.Request // map of requests sent in server pushes.
}

// readFrames is the main processing loop, where frames
// are read from the connection and processed individually.
// Returning from readFrames begins the cleanup and exit
// process for this connection.
func (conn *clientConnection) readFrames() {

	// Add timeouts if requested by the server.
	conn.refreshTimeouts()

	// Main loop.
	for {

		// This is the mechanism for handling too many benign errors.
		// Default MaxBenignErrors is 10.
		if conn.numBenignErrors > MaxBenignErrors {
			log.Println("Error: Too many invalid stream IDs received. Ending connection.")
			conn.PROTOCOL_ERROR(0)
		}

		// ReadFrame takes care of the frame parsing for us.
		frame, err := ReadFrame(conn.buf)
		conn.refreshTimeouts()
		if err != nil {
			if err == io.EOF {
				// Server has closed the TCP connection.
				debug.Println("Note: Server has disconnected.")
				return
			}

			log.Printf("Error: Client encountered read error: %q\n", err.Error())
			return
		}

		// Decompress the frame's headers, if there are any.
		err = frame.DecodeHeaders(conn.decompressor)
		if err != nil {
			panic(err)
		}

		debug.Println("Received Frame:")
		debug.Println(frame)

	FrameHandling:
		// This is the main frame handling section.
		switch frame := frame.(type) {

		case *HeadersFrame:
			conn.handleHeaders(frame)

		case *ErrorFrame:
			if StatusCodeIsFatal(int(frame.Status)) {
				code := StatusCodeText(int(frame.Status))
				log.Printf("Warning: Received %s on stream %d. Closing connection.\n", code, frame.StreamID())
				return
			}
			conn.handleError(frame)

		case *RequestFrame:
			log.Println("Warning: Ignoring Request.")
			conn.numBenignErrors++

		case *ResponseFrame:
			conn.handleResponse(frame)

		case *PushFrame:
			conn.handlePush(frame)

		case *DataFrame:
			conn.handleData(frame)

		case *PingFrame:
			// Check whether Ping ID is client-sent.
			if frame.PingID&1 != 0 {
				if conn.pings[frame.PingID] == nil {
					log.Printf("Warning: Ignored PING with Ping ID %d, which hasn't been requested.\n", frame.PingID)
					conn.numBenignErrors++
					break FrameHandling
				}
				conn.pings[frame.PingID] <- true
				close(conn.pings[frame.PingID])
				delete(conn.pings, frame.PingID)
			} else {
				debug.Println("Received PING. Replying...")
				frame.flags = FLAG_FIN
				conn.WriteFrame(frame)
			}

		default:
			log.Println(fmt.Sprintf("unexpected frame type %T", frame))
		}
	}
}

// send is run in a separate goroutine. It's used
// to ensure clear interleaving of frames and to
// provide assurances of priority and structure.
func (conn *clientConnection) send() {
	for {

		// Select the next frame to send.
		frame := <-conn.dataOutput

		// Compress any name/value header blocks.
		err := frame.EncodeHeaders(conn.compressor)
		if err != nil {
			panic(err)
		}

		debug.Println("Sending Frame:")
		debug.Println(frame)

		// Leave the specifics of writing to the
		// connection up to the frame.
		err = frame.WriteTo(conn.conn)
		conn.refreshTimeouts()
		if err != nil {
			if err == io.EOF {
				// Server has closed the TCP connection.
				debug.Println("Note: Server has disconnected.")
				return
			}

			// Unexpected error which prevented a write, so
			// it's best to panic.
			panic(err)
		}
	}
}

// Ping is used to send a WP ping to the client.
// A channel is returned immediately, and 'true'
// sent when the ping reply is received. If there
// is a fault in the connection, the channel is
// closed.
func (conn *clientConnection) Ping() <-chan bool {
	ping := new(PingFrame)

	conn.Lock()

	pid := conn.pingID
	conn.pingID += 2
	ping.PingID = pid
	conn.dataOutput <- ping

	conn.Unlock()

	c := make(chan bool, 1)
	conn.pings[pid] = c

	return c
}

// Push is a method stub required to satisfy the Connection
// interface. It must not be used by clients.
func (conn *clientConnection) Push(resource string, origin Stream) (PushWriter, error) {
	return nil, errors.New("Error: Clients cannot send pushes.")
}

// Request is used to create and send a request to the server.
// The presented Receiver will be updated with the response
// data received from the server. The returned Stream can be
// used to manipulate the underlying stream, provided the
// request was started successfully.
func (conn *clientConnection) Request(req *http.Request, priority int, res Receiver) (Stream, error) {

	// Prepare the Request.
	syn := new(RequestFrame)
	syn.Priority = uint8(priority)
	url := req.URL
	if url == nil || url.Scheme == "" || url.Host == "" || url.Path == "" {
		return nil, errors.New("Error: Incomplete path provided to resource.")
	}

	headers := req.Header
	headers.Set(":path", url.Path)
	headers.Set(":host", url.Host)
	headers.Set(":scheme", url.Scheme)
	syn.Headers = headers

	// Prepare the request body, if any.
	body := make([]*DataFrame, 0, 10)
	if req.Body != nil {
		buf := make([]byte, 32*1024)
		n, err := req.Body.Read(buf)
		if err != nil {
			return nil, err
		}
		total := n
		for n > 0 {
			data := new(DataFrame)
			data.streamID = syn.streamID
			data.Data = make([]byte, n)
			copy(data.Data, buf[:n])
			body = append(body, data)
			n, err = req.Body.Read(buf)
			if err != nil && err != io.EOF {
				return nil, err
			}
			total += n
		}

		// Half-close the stream.
		if len(body) != 0 {
			syn.Headers.Set("Content-Length", fmt.Sprint(total))
		}
		req.Body.Close()
	}

	// Send.
	conn.Lock()
	syn.streamID = conn.nextClientStreamID
	conn.nextClientStreamID += 2
	conn.WriteFrame(syn)
	for _, frame := range body {
		conn.WriteFrame(frame)
	}
	conn.Unlock()

	// Create the request stream.
	out := new(clientStream)
	out.conn = conn
	out.content = new(bytes.Buffer)
	out.streamID = syn.streamID
	out.state = new(StreamState)
	out.state.CloseHere()
	out.output = conn.dataOutput
	out.request = req
	out.receiver = res
	out.headers = make(http.Header)
	out.stop = false
	out.version = conn.version
	out.done = make(chan struct{}, 1)

	// Store in the connection map.
	conn.streams[syn.streamID] = out

	return out, nil
}

func (conn *clientConnection) WriteFrame(frame Frame) {
	conn.dataOutput <- frame
}

func (conn *clientConnection) Version() uint8 {
	return conn.version
}

// handleResponse performs the processing of Response frames.
func (conn *clientConnection) handleResponse(frame *ResponseFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received Response with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	stream, ok := conn.streams[sid]
	if !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received Response with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send headers to stream.
	stream.ReceiveFrame(frame)

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		stream.State().CloseThere()
		stream.Stop()
	}
}

// handlePush performs the processing of Push frames.
func (conn *clientConnection) handlePush(frame *PushFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.streamID

	// Check Stream ID is even.
	if sid&1 != 0 {
		log.Printf("Error: Received Request with Stream ID %d, which should be even.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is the right number.
	nsid := conn.nextServerStreamID
	if sid != nsid {
		log.Printf("Error: Received Request with Stream ID %d, which should be %d.\n", sid, nsid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is not out of bounds.
	if sid > MAX_STREAM_ID {
		log.Printf("Error: Received Request with Stream ID %d, which exceeds the limit.\n", sid)
		conn.PROTOCOL_ERROR(sid)
	}

	// Stream ID is fine.

	// Parse the request.
	headers := frame.Headers
	rawUrl := headers.Get(":scheme") + "://" + headers.Get(":host") + headers.Get(":path")
	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Println("Error: Received Requst with invalid request URL: ", err)
		return
	}
	vers := headers.Get(":version")
	major, minor, ok := http.ParseHTTPVersion(vers)
	if !ok {
		log.Println("Error: Invalid HTTP version: " + headers.Get(":version"))
		return
	}
	method := headers.Get(":method")
	request := &http.Request{
		Method:     method,
		URL:        url,
		Proto:      vers,
		ProtoMajor: major,
		ProtoMinor: minor,
		RemoteAddr: conn.remoteAddr,
		Header:     headers,
		Host:       url.Host,
		RequestURI: url.Path,
		TLS:        conn.tlsState,
	}

	// Check whether the receiver wants this resource.
	if !conn.pushReceiver.ReceiveRequest(request) {
		rst := new(ErrorFrame)
		rst.streamID = sid
		rst.Status = REFUSED_STREAM
		conn.WriteFrame(rst)
		return
	}

	// Create and start new stream.
	conn.pushReceiver.ReceiveHeaders(request, frame.Headers)
	conn.pushRequests[sid] = request
	conn.nextServerStreamID = sid + 2

	//go nextStream.run()
	conn.done.Add(1)

	return
}

// handleError performs the processing of Error frames.
func (conn *clientConnection) handleError(frame *ErrorFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Determine the status code and react accordingly.
	switch frame.Status {
	case INVALID_STREAM:
		log.Printf("Error: Received INVALID_STREAM for stream %d.\n", sid)
		conn.numBenignErrors++
		return

	case REFUSED_STREAM:
		conn.closeStream(sid)
		return

	case STREAM_CLOSED:
		log.Printf("Error: Received STREAM_CLOSED for stream %d.\n", sid)
		conn.numBenignErrors++
		return

	case STREAM_ID_MISSING:
		log.Printf("Error: Received STREAM_ID_MISSING.\n", sid)
		conn.numBenignErrors++
		return

	case FINISH_STREAM:
		conn.closeStream(sid)
		return

	default:
		log.Printf("Error: Received unknown Error status code %d.\n", frame.Status)
		conn.PROTOCOL_ERROR(sid)
	}
}

// handleData performs the processing of DATA frames.
func (conn *clientConnection) handleData(frame *DataFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Handle push data.
	if sid&1 == 0 {
		req := conn.pushRequests[sid]

		// Ignore refused push data.
		if req != nil {
			conn.pushReceiver.ReceiveData(req, frame.Data, frame.flags&FLAG_FIN != 0)
		}

		return
	}

	// Check stream is open.
	stream, ok := conn.streams[sid]
	if !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received Data with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send data to stream.
	stream.ReceiveFrame(frame)

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		stream.State().CloseThere()
		stream.Stop()
	}
}

// handleHeaders performs the processing of HEADERS frames.
func (conn *clientConnection) handleHeaders(frame *HeadersFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Handle push headers.
	if sid&1 == 0 {
		req := conn.pushRequests[sid]

		// Ignore refused push headers.
		if req != nil {
			conn.pushReceiver.ReceiveHeaders(req, frame.Headers)
		}

		return
	}

	// Check stream is open.
	stream, ok := conn.streams[sid]
	if !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received Headers with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send headers to stream.
	stream.ReceiveFrame(frame)

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		stream.State().CloseThere()
		stream.Stop()
	}
}

// Add timeouts if requested by the server.
func (conn *clientConnection) refreshTimeouts() {
	if d := conn.client.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
	if d := conn.client.WriteTimeout; d != 0 {
		conn.conn.SetWriteDeadline(time.Now().Add(d))
	}
}

// closeStream closes the provided stream safely.
func (conn *clientConnection) closeStream(streamID uint32) {
	if streamID == 0 {
		log.Println("Error: Tried to close stream 0.")
		return
	}

	stream, ok := conn.streams[streamID]
	if !ok {
		log.Printf("Error: Tried to close closed stream %d.\n", streamID)
		return
	}

	stream.State().Close()
	stream.Stop()
	delete(conn.streams, streamID)
}

// PROTOCOL_ERROR informs the client that a protocol error has
// occurred, stops all running streams, and ends the connection.
func (conn *clientConnection) PROTOCOL_ERROR(streamID uint32) {
	reply := new(ErrorFrame)
	reply.streamID = streamID
	reply.Status = PROTOCOL_ERROR
	conn.WriteFrame(reply)

	// Leave time for the message to be sent and received.
	time.Sleep(100 * time.Millisecond)
	conn.cleanup()
	runtime.Goexit()
}

// cleanup is used to end any running streams and
// aid garbage collection before the connection
// is closed.
func (conn *clientConnection) cleanup() {
	for _, stream := range conn.streams {
		stream.Stop()
	}
	conn.streams = nil
	close(conn.dataOutput)
	conn.compressor.Close()
	conn.compressor = nil
	conn.decompressor = nil
}

// run prepares and executes the frame reading
// loop of the connection. At this point, any
// global settings set by the client are sent to
// the new client.
func (conn *clientConnection) run() {
	defer func() {
		if err := recover(); err != nil {
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("panic serving %v: %v\n%s", conn.remoteAddr, err, buf)
		}
	}()

	// Start the send loop.
	go conn.send()

	// Enter the main loop.
	conn.readFrames()

	// Cleanup before the connection closes.
	conn.cleanup()
}

// newConn is used to create and initialise a server connection.
func newClientConn(tlsConn *tls.Conn) *clientConnection {
	conn := new(clientConnection)
	conn.remoteAddr = tlsConn.RemoteAddr().String()
	conn.conn = tlsConn
	conn.buf = bufio.NewReader(tlsConn)
	conn.tlsState = new(tls.ConnectionState)
	*conn.tlsState = tlsConn.ConnectionState()
	conn.streams = make(map[uint32]Stream)
	conn.dataOutput = make(chan Frame)
	conn.pings = make(map[uint32]chan<- bool)
	conn.pingID = 1
	conn.nextClientStreamID = 1
	conn.compressor = new(Compressor)
	conn.decompressor = new(Decompressor)
	conn.done = new(sync.WaitGroup)
	conn.pushRequests = make(map[uint32]*http.Request)

	return conn
}
