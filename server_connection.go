package wp

import (
	"bufio"
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

// serverConnection represents a WP session at the server
// end. This performs the overall connection management and
// co-ordination between streams.
type serverConnection struct {
	sync.RWMutex
	remoteAddr         string
	server             *Server
	conn               *tls.Conn
	buf                *bufio.Reader // buffered reader for the connection.
	tlsState           *tls.ConnectionState
	streams            map[uint32]Stream
	streamInputs       map[uint32]chan<- Frame // sending frames to streams.
	dataPriority       [8]chan Frame           // one output channel per priority level.
	pings              map[uint32]chan<- bool  // response channel for pings.
	pingID             uint32                  // next outbound ping ID.
	compressor         *Compressor             // outbound compression state.
	decompressor       *Decompressor           // inbound decompression state.
	nextServerStreamID uint32                  // next outbound stream ID. (even)
	nextClientStreamID uint32                  // next inbound stream ID. (odd)
	version            uint8                   // WP version.
	numBenignErrors    int                     // number of non-serious errors encountered.
	done               *sync.WaitGroup         // WaitGroup for active streams.
}

// readFrames is the main processing loop, where frames
// are read from the connection and processed individually.
// Returning from readFrames begins the cleanup and exit
// process for this connection.
func (conn *serverConnection) readFrames() {

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
				// Client has closed the TCP connection.
				log.Println("Note: Client has disconnected.")
				return
			}

			log.Printf("Error: Server encountered read error: %q\n", err.Error())
			return
		}

		// Decompress the frame's headers, if there are any.
		err = frame.DecodeHeaders(conn.decompressor)
		if err != nil {
			log.Println("Error in decompression: ", err)
			conn.PROTOCOL_ERROR(frame.StreamID())
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
				log.Printf("Warning: Received %s on stream %d. Closing stream.\n", code, frame.StreamID())
				return
			}
			conn.handleError(frame)

		case *RequestFrame:
			conn.handleRequest(frame)

		case *ResponseFrame:
			conn.handleResponse(frame)

		case *PushFrame:
			log.Println("Warning: Ignoring Push frame.")

		case *DataFrame:
			conn.handleData(frame)

		case *PingFrame:
			// Check whether Ping ID is server-sent.
			if frame.PingID&1 == 0 {
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
func (conn *serverConnection) send() {
	for {

		// Select the next frame to send.
		frame := conn.selectFrameToSend()

		// Compress any name/value header blocks.
		err := frame.EncodeHeaders(conn.compressor)
		if err != nil {
			log.Println(err)
			continue
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
				log.Println("Note: Server has disconnected.")
				return
			}

			// Unexpected error which prevented a write.
			return
		}
	}
}

// selectFrameToSend follows the specification's guidance
// on frame priority, sending frames with higher priority
// (a smaller number) first.
func (conn *serverConnection) selectFrameToSend() (frame Frame) {
	// Try in priority order first.
	for i := 0; i < 8; i++ {
		select {
		case frame = <-conn.dataPriority[i]:
			return frame
		default:
		}
	}

	// Wait for any frame.
	select {
	case frame = <-conn.dataPriority[0]:
		return frame
	case frame = <-conn.dataPriority[1]:
		return frame
	case frame = <-conn.dataPriority[2]:
		return frame
	case frame = <-conn.dataPriority[3]:
		return frame
	case frame = <-conn.dataPriority[4]:
		return frame
	case frame = <-conn.dataPriority[5]:
		return frame
	case frame = <-conn.dataPriority[6]:
		return frame
	case frame = <-conn.dataPriority[7]:
		return frame
	}
}

// newStream is used to create a new responseStream from a Request frame.
func (conn *serverConnection) newStream(frame *RequestFrame, input <-chan Frame, output chan<- Frame) *serverStream {
	stream := new(serverStream)
	stream.conn = conn
	stream.streamID = frame.streamID
	stream.state = new(StreamState)
	stream.input = input
	stream.output = output
	stream.headers = make(Headers)
	stream.version = conn.version

	if frame.flags&FLAG_FIN != 0 {
		stream.state.CloseThere()
	}

	headers := frame.Headers
	rawUrl := headers.Get(":scheme") + "://" + headers.Get(":host") + headers.Get(":path")
	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Println("Error: Received Request with invalid request URL: ", err)
		return nil
	}

	// Build this into a request to present to the Handler.
	stream.request = &Request{
		URL:        url,
		Proto:      fmt.Sprintf("WP/%d", conn.version),
		Protocol:   int(conn.version),
		Priority:   int(frame.Priority),
		RemoteAddr: conn.remoteAddr,
		Headers:    headers,
		Host:       url.Host,
		RequestURI: url.Path,
		TLS:        conn.tlsState,
	}

	return stream
}

// Internally-sent frames have high priority.
func (conn *serverConnection) WriteFrame(frame Frame) {
	conn.dataPriority[0] <- frame
}

// Ping is used to send a WP ping to the client.
// A channel is returned immediately, and 'true'
// sent when the ping reply is received. If there
// is a fault in the connection, the channel is
// closed.
func (conn *serverConnection) Ping() <-chan bool {
	ping := new(PingFrame)

	conn.Lock()

	pid := conn.pingID
	conn.pingID += 2
	ping.PingID = pid
	conn.dataPriority[0] <- ping

	conn.Unlock()

	c := make(chan bool, 1)
	conn.pings[pid] = c

	return c
}

// Push is used to create a server push. A Push frame is created and sent,
// opening the stream. Push then creates and initialises a PushWriter and
// returns it.
//
// According to the specification, the establishment of the push is very
// high-priority, to mitigate the race condition of the client receiving
// enough information to request the resource being pushed before the
// push SYN_STREAM arrives. However, the actual push data is fairly low
// priority, since it's probably being sent at the same time as the data
// for a resource which may result in further requests. As a result of
// these two factors, the Push is sent at priority 0 (max), but its data
// is sent at priority 7 (min).
func (conn *serverConnection) Push(resource string, origin Stream) (PushWriter, error) {

	// Prepare the Push.
	push := new(PushFrame)
	push.AssociatedStreamID = origin.StreamID()
	url, err := url.Parse(resource)
	if err != nil {
		return nil, err
	}
	if url.Scheme == "" || url.Host == "" || url.Path == "" {
		return nil, errors.New("Error: Incomplete path provided to resource.")
	}

	headers := make(Headers)
	headers.Set(":host", url.Host)
	headers.Set(":path", url.Path)
	push.Headers = headers

	// Send.
	conn.Lock()
	conn.nextServerStreamID += 2
	newID := conn.nextServerStreamID
	push.streamID = newID
	conn.WriteFrame(push)
	conn.Unlock()

	// Create the pushStream.
	out := new(pushStream)
	out.conn = conn
	out.streamID = newID
	out.origin = origin
	out.state = new(StreamState)
	out.output = conn.dataPriority[7]
	out.headers = make(Headers)
	out.stop = false
	out.version = conn.version

	// Store in the connection map.
	conn.streams[newID] = out

	return out, nil
}

// Request is a method stub required to satisfy the Connection
// interface. It must not be used by servers.
func (conn *serverConnection) Request(req *Request, res Receiver) (Stream, error) {
	return nil, errors.New("Error: Servers cannot make requests.")
}

func (conn *serverConnection) Version() uint8 {
	return conn.version
}

// handleSynStream performs the processing of Request frames.
func (conn *serverConnection) handleRequest(frame *RequestFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received Request with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is the right number.
	nsid := conn.nextClientStreamID
	if sid != nsid && sid != 1 && conn.nextClientStreamID != 0 {
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

	// Create and start new stream.
	input := make(chan Frame)
	nextStream := conn.newStream(frame, input, conn.dataPriority[frame.Priority])
	if nextStream == nil { // Make sure an error didn't occur when making the stream.
		return
	}

	// Determine which handler to use.
	nextStream.handler = conn.server.Handler
	if nextStream.handler == nil {
		nextStream.handler = DefaultServeMux
	}
	nextStream.httpHandler = conn.server.httpHandler
	if nextStream.httpHandler == nil {
		nextStream.httpHandler = http.DefaultServeMux
	}

	// Set and prepare.
	conn.streamInputs[sid] = input
	conn.streams[sid] = nextStream
	conn.nextClientStreamID = sid + 2

	// Start the stream.
	go nextStream.Run()
}

// handleResponse performs the processing of Response frames.
func (conn *serverConnection) handleResponse(frame *ResponseFrame) {
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
	if stream, ok := conn.streams[sid]; !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received Response with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send headers to stream.
	conn.streamInputs[sid] <- frame

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		conn.streams[sid].State().CloseThere()
	}
}

// handleError performs the processing of Error frames.
func (conn *serverConnection) handleError(frame *ErrorFrame) {
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
		log.Printf("Error: Received unknown RST_STREAM status code %d.\n", frame.Status)
		conn.PROTOCOL_ERROR(sid)
	}
}

// handleData performs the processing of Data frames.
func (conn *serverConnection) handleData(frame *DataFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received DATA with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	if stream, ok := conn.streams[sid]; !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received DATA with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send data to stream.
	conn.streamInputs[sid] <- frame

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		conn.streams[sid].State().CloseThere()
	}
}

// handleHeaders performs the processing of HEADERS frames.
func (conn *serverConnection) handleHeaders(frame *HeadersFrame) {
	conn.RLock()
	defer conn.RUnlock()

	sid := frame.streamID

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received HEADERS with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream is open.
	if stream, ok := conn.streams[sid]; !ok || stream == nil || stream.State().ClosedThere() {
		log.Printf("Error: Received HEADERS with Stream ID %d, which is closed or unopened.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Stream ID is fine.

	// Send headers to stream.
	conn.streamInputs[sid] <- frame

	// Handle flags.
	if frame.flags&FLAG_FIN != 0 {
		conn.streams[sid].State().CloseThere()
	}
}

// Add timeouts if requested by the server.
// TODO: this could be improved.
func (conn *serverConnection) refreshTimeouts() {
	if d := conn.server.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
	if d := conn.server.WriteTimeout; d != 0 {
		conn.conn.SetWriteDeadline(time.Now().Add(d))
	}
}

// closeStream closes the provided stream safely.
func (conn *serverConnection) closeStream(streamID uint32) {
	if streamID == 0 {
		log.Println("Error: Tried to close stream 0.")
		return
	}

	conn.streams[streamID].Stop()
	conn.streams[streamID].State().Close()
	close(conn.streamInputs[streamID])
	delete(conn.streams, streamID)
}

// PROTOCOL_ERROR informs the client that a protocol error has
// occurred, stops all running streams, and ends the connection.
func (conn *serverConnection) PROTOCOL_ERROR(streamID uint32) {
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
func (conn *serverConnection) cleanup() {
	for streamID, c := range conn.streamInputs {
		close(c)
		conn.streams[streamID].Stop()
	}
	conn.streamInputs = nil
	conn.streams = nil
}

// serve prepares and executes the frame reading
// loop of the connection. At this point, any
// global settings set by the server are sent to
// the new client.
func (conn *serverConnection) serve() {
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

// acceptDefaultWPv2 is used in starting a WP/2 connection from
// an HTTP server supporting NPN.
func acceptDefaultWPv2(srv *http.Server, tlsConn *tls.Conn, _ http.Handler) {
	server := new(Server)
	server.TLSConfig = srv.TLSConfig
	acceptWPv2(server, tlsConn, nil)
}

// acceptWPv2 is used in starting a WP/2 connection from an HTTP
// server supporting NPN. This is called manually from within a
// closure which stores the SPDY server.
func acceptWPv2(server *Server, tlsConn *tls.Conn, _ http.Handler) {
	conn := newConn(tlsConn)
	conn.server = server
	conn.version = 2

	conn.serve()
}

// newConn is used to create and initialise a server connection.
func newConn(tlsConn *tls.Conn) *serverConnection {
	conn := new(serverConnection)
	conn.remoteAddr = tlsConn.RemoteAddr().String()
	conn.conn = tlsConn
	conn.buf = bufio.NewReader(tlsConn)
	conn.tlsState = new(tls.ConnectionState)
	*conn.tlsState = tlsConn.ConnectionState()
	conn.streams = make(map[uint32]Stream)
	conn.streamInputs = make(map[uint32]chan<- Frame)
	conn.dataPriority = [8]chan Frame{}
	conn.dataPriority[0] = make(chan Frame)
	conn.dataPriority[1] = make(chan Frame)
	conn.dataPriority[2] = make(chan Frame)
	conn.dataPriority[3] = make(chan Frame)
	conn.dataPriority[4] = make(chan Frame)
	conn.dataPriority[5] = make(chan Frame)
	conn.dataPriority[6] = make(chan Frame)
	conn.dataPriority[7] = make(chan Frame)
	conn.compressor = new(Compressor)
	conn.decompressor = new(Decompressor)
	conn.pings = make(map[uint32]chan<- bool)
	conn.done = new(sync.WaitGroup)

	return conn
}
