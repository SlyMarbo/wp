package wp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"
)

// conn is a wp.Conn implementing WP/2. This is used in both
// servers and clients, and is created with either NewServerConn,
// or NewClientConn.
type conn struct {
	sync.Mutex
	remoteAddr          string
	server              *http.Server
	conn                net.Conn
	buf                 *bufio.Reader
	tlsState            *tls.ConnectionState
	streams             map[StreamID]Stream        // map of active streams.
	output              [8]chan Frame              // one output channel per priority level.
	pings               map[uint32]chan<- Ping     // response channel for pings.
	nextPingID          uint32                     // next outbound ping ID.
	compressor          Compressor                 // outbound compression state.
	decompressor        Decompressor               // inbound decompression state.
	lastPushStreamID    StreamID                   // last push stream ID. (even)
	lastRequestStreamID StreamID                   // last request stream ID. (odd)
	oddity              StreamID                   // whether locally-sent streams are odd or even.
	numBenignErrors     int                        // number of non-serious errors encountered.
	pushRequests        map[StreamID]*http.Request // map of requests sent in server pushes.
	pushReceiver        Receiver                   // Receiver to call for server Pushes.
	stop                chan struct{}              // this channel is closed when the connection closes.
	sending             chan struct{}              // this channel is used to ensure pending frames are sent.
	init                func()                     // this function is called before the connection begins.
}

// Close ends the connection, cleaning up relevant resources.
// Close can be called multiple times safely.
func (conn *conn) Close() (err error) {
	conn.Lock()
	defer conn.Unlock()

	if conn.closed() {
		return nil
	}

	// Ensure any pending frames are sent.
	if conn.sending == nil {
		conn.sending = make(chan struct{})
		<-conn.sending
	}

	select {
	case _ = <-conn.stop:
	default:
		close(conn.stop)
	}

	err = conn.conn.Close()
	if err != nil {
		return err
	}
	conn.conn = nil

	for _, stream := range conn.streams {
		err = stream.Close()
		if err != nil {
			return err
		}
	}
	conn.streams = nil

	err = conn.compressor.Close()
	if err != nil {
		return err
	}
	conn.compressor = nil
	conn.decompressor = nil

	for _, stream := range conn.output {
		select {
		case _, ok := <-stream:
			if ok {
				close(stream)
			}
		default:
			close(stream)
		}
	}

	runtime.Goexit()
	return nil
}

// Ping is used by wp.PingServer and wp.PingClient to send
// WP PINGs.
func (conn *conn) Ping() (<-chan Ping, error) {
	conn.Lock()
	defer conn.Unlock()

	if conn.closed() {
		return nil, errors.New("Error: Conn has been closed.")
	}

	ping := new(pingFrame)
	pid := conn.nextPingID
	if pid+2 < pid {
		if pid&1 == 0 {
			conn.nextPingID = 2
		} else {
			conn.nextPingID = 1
		}
	} else {
		conn.nextPingID += 2
	}
	ping.PingID = pid
	conn.output[0] <- ping
	c := make(chan Ping, 1)
	conn.pings[pid] = c

	return c, nil
}

// Push is used to issue a server push to the client. Note that this cannot be performed
// by clients.
func (conn *conn) Push(resource string, origin Stream) (http.ResponseWriter, error) {
	if conn.server == nil {
		return nil, errors.New("Error: Only servers can send pushes.")
	}

	// Parse and check URL.
	url, err := url.Parse(resource)
	if err != nil {
		return nil, err
	}
	if url.Scheme == "" || url.Host == "" || url.Path == "" {
		return nil, errors.New("Error: Incomplete path provided to resource.")
	}

	// Prepare the Push.
	push := new(pushFrame)
	push.AssociatedStreamID = origin.StreamID()
	push.Priority = 7
	push.Header = make(http.Header)
	push.Header.Set(":scheme", url.Scheme)
	push.Header.Set(":host", url.Host)
	push.Header.Set(":path", url.Path)
	push.Header.Set(":version", "HTTP/1.1")
	push.Header.Set(":status", "200 OK")

	// Send.
	conn.Lock()
	conn.lastPushStreamID += 2
	if conn.lastPushStreamID > MAX_STREAM_ID {
		conn.Unlock()
		return nil, errors.New("Error: All server streams exhausted.")
	}
	newID := conn.lastPushStreamID
	push.StreamID = newID
	conn.output[0] <- push
	conn.Unlock()

	// Create the pushStream.
	out := new(pushStream)
	out.conn = conn
	out.streamID = newID
	out.origin = origin
	out.state = new(StreamState)
	out.output = conn.output[7]
	out.header = make(http.Header)
	out.stop = conn.stop

	// Store in the connection map.
	conn.streams[newID] = out

	return out, nil
}

// Request is used to make a client request.
func (conn *conn) Request(request *http.Request, receiver Receiver, priority Priority) (Stream, error) {
	if conn.server != nil {
		return nil, errors.New("Error: Only clients can send requests.")
	}

	if !priority.Valid() {
		return nil, errors.New("Error: Priority must be in the range 0 - 7.")
	}

	url := request.URL
	if url == nil || url.Scheme == "" || url.Host == "" || url.Path == "" {
		return nil, errors.New("Error: Incomplete path provided to resource.")
	}

	// Prepare the Request.
	syn := new(requestFrame)
	syn.Priority = priority
	syn.Header = request.Header
	syn.Header.Set(":method", request.Method)
	syn.Header.Set(":path", url.Path)
	syn.Header.Set(":version", "HTTP/1.1")
	syn.Header.Set(":host", url.Host)
	syn.Header.Set(":scheme", url.Scheme)

	// Prepare the request body, if any.
	body := make([]*dataFrame, 0, 1)
	if request.Body != nil {
		buf := make([]byte, 32*1024)
		n, err := request.Body.Read(buf)
		if err != nil {
			return nil, err
		}
		total := n
		for n > 0 {
			data := new(dataFrame)
			data.Data = make([]byte, n)
			copy(data.Data, buf[:n])
			body = append(body, data)
			n, err = request.Body.Read(buf)
			if err != nil && err != io.EOF {
				return nil, err
			}
			total += n
		}

		// Half-close the stream.
		if len(body) == 0 {
			syn.Flags = FLAG_FINISH
		} else {
			syn.Header.Set("Content-Length", fmt.Sprint(total))
			body[len(body)-1].Flags = FLAG_FINISH
		}
		request.Body.Close()
	} else {
		syn.Flags = FLAG_FINISH
	}

	// Send.
	conn.Lock()
	if conn.lastRequestStreamID == 0 {
		conn.lastRequestStreamID = 1
	} else {
		conn.lastRequestStreamID += 2
	}
	if conn.lastRequestStreamID > MAX_STREAM_ID {
		conn.Unlock()
		return nil, errors.New("Error: All client streams exhausted.")
	}
	syn.StreamID = conn.lastRequestStreamID
	conn.output[0] <- syn
	for _, frame := range body {
		frame.StreamID = syn.StreamID
		conn.output[0] <- frame
	}
	conn.Unlock()

	// // Create the request stream.
	out := new(clientStream)
	out.conn = conn
	out.streamID = syn.StreamID
	out.state = new(StreamState)
	out.state.CloseHere()
	out.output = conn.output[0]
	out.request = request
	out.receiver = receiver
	out.header = make(http.Header)
	out.stop = conn.stop
	out.finished = make(chan struct{})

	// Store in the connection map.
	conn.streams[syn.StreamID] = out

	return out, nil
}

func (conn *conn) Run() error {
	// Start the send loop.
	go conn.send()

	// Prepare any initialisation frames.
	if conn.init != nil {
		conn.init()
	}

	// Enter the main loop.
	conn.readFrames()

	// Cleanup before the connection closes.
	return conn.Close()
}

// closed indicates whether the connection has
// been closed.
func (conn *conn) closed() bool {
	select {
	case _ = <-conn.stop:
		return true
	default:
		return false
	}
}

// handleClientData performs the processing of Data frames sent by the client.
func (conn *conn) handleClientData(frame *dataFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.StreamID

	if conn.server == nil {
		log.Println("Error: Requests can only be received by the server.")
		conn.numBenignErrors++
		return
	}

	// Handle push data.
	if sid&1 != 0 {
		log.Printf("Error: Received Data with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check stream ID is valid.
	if !sid.Valid() {
		log.Printf("Error: Received Data with Stream ID %d, which exceeds the limit.\n", sid)
		conn.protocolError(sid)
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
}

// handleHeaders performs the processing of Headers frames.
func (conn *conn) handleHeaders(frame *headersFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.StreamID

	// Handle push headers.
	if sid&1 == 0 && conn.server == nil {
		// Ignore refused push headers.
		if req := conn.pushRequests[sid]; req != nil && conn.pushReceiver != nil {
			conn.pushReceiver.ReceiveHeader(req, frame.Header)
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
}

// handlePush performs the processing of Push frames.
func (conn *conn) handlePush(frame *pushFrame) {
	conn.Lock()
	defer conn.Unlock()

	// Check stream creation is allowed.
	if conn.closed() {
		return
	}

	sid := frame.StreamID

	// Push.
	if conn.server != nil {
		log.Println("Error: Only clients can receive server pushes.")
		return
	}

	// Check Stream ID is even.
	if sid&1 != 0 {
		log.Printf("Error: Received Push with Stream ID %d, which should be even.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is the right number.
	lsid := conn.lastPushStreamID
	if sid <= lsid {
		log.Printf("Error: Received Push with Stream ID %d, which should be greater than %d.\n", sid, lsid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is not out of bounds.
	if !sid.Valid() {
		log.Printf("Error: Received Push with Stream ID %d, which exceeds the limit.\n", sid)
		conn.protocolError(sid)
		return
	}

	// Stream ID is fine.

	if !frame.Priority.Valid() {
		log.Printf("Error: Received Push with invalid priority %d.\n", frame.Priority)
		conn.protocolError(sid)
		return
	}

	// Parse the request.
	header := frame.Header
	rawUrl := header.Get(":scheme") + "://" + header.Get(":host") + header.Get(":path")
	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Println("Error: Received Push with invalid request URL: ", err)
		return
	}

	vers := header.Get(":version")
	major, minor, ok := http.ParseHTTPVersion(vers)
	if !ok {
		log.Println("Error: Invalid HTTP version: " + vers)
		return
	}

	method := header.Get(":method")

	request := &http.Request{
		Method:     method,
		URL:        url,
		Proto:      vers,
		ProtoMajor: major,
		ProtoMinor: minor,
		RemoteAddr: conn.remoteAddr,
		Header:     header,
		Host:       url.Host,
		RequestURI: url.Path,
		TLS:        conn.tlsState,
	}

	// Check whether the receiver wants this resource.
	if conn.pushReceiver != nil && !conn.pushReceiver.ReceiveRequest(request) {
		rst := new(errorFrame)
		rst.StreamID = sid
		rst.Status = REFUSED_STREAM
		conn.output[0] <- rst
		return
	}

	// Create and start new stream.
	if conn.pushReceiver != nil {
		conn.pushReceiver.ReceiveHeader(request, frame.Header)
		conn.pushRequests[sid] = request
		conn.lastPushStreamID = sid
	}
}

// handleRequest performs the processing of Request frames.
func (conn *conn) handleRequest(frame *requestFrame) {
	conn.Lock()
	defer conn.Unlock()

	// Check stream creation is allowed.
	if conn.closed() {
		return
	}

	sid := frame.StreamID

	if conn.server == nil {
		log.Println("Error: Only servers can receive requests.")
		return
	}

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received Request with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is the right number.
	lsid := conn.lastRequestStreamID
	if sid <= lsid && lsid != 0 {
		log.Printf("Error: Received Request with Stream ID %d, which should be greater than %d.\n", sid, lsid)
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is not out of bounds.
	if !sid.Valid() {
		log.Printf("Error: Received Request with Stream ID %d, which exceeds the limit.\n", sid)
		conn.protocolError(sid)
		return
	}

	// Stream ID is fine.

	// Check request priority.
	if !frame.Priority.Valid() {
		log.Printf("Error: Received Request with invalid priority %d.\n", frame.Priority)
		conn.protocolError(sid)
		return
	}

	// Create and start new stream.
	nextStream := conn.newStream(frame, conn.output[frame.Priority])
	// Make sure an error didn't occur when making the stream.
	if nextStream == nil {
		return
	}

	// Determine which handler to use.
	nextStream.handler = conn.server.Handler
	if nextStream.handler == nil {
		nextStream.handler = http.DefaultServeMux
	}

	// Set and prepare.
	conn.streams[sid] = nextStream
	conn.lastRequestStreamID = sid

	// Start the stream.
	go nextStream.Run()
}

// handleError performs the processing of Error frames.
func (conn *conn) handleError(frame *errorFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.StreamID

	// Determine the status code and react accordingly.
	switch frame.Status {
	case INVALID_STREAM:
		log.Printf("Error: Received INVALID_STREAM for stream ID %d.\n", sid)
		if stream, ok := conn.streams[sid]; ok {
			stream.Close()
		}
		conn.numBenignErrors++

	case REFUSED_STREAM:
		if stream, ok := conn.streams[sid]; ok {
			stream.Close()
		}

	case FINISH_STREAM:
		if stream, ok := conn.streams[sid]; ok {
			stream.Close()
		}

	case STREAM_CLOSED:
		log.Printf("Error: Received STREAM_CLOSED for stream ID %d.\n", sid)
		if stream, ok := conn.streams[sid]; ok {
			stream.Close()
		}
		conn.numBenignErrors++

	default:
		log.Printf("Error: Received unknown RST_STREAM status code %d.\n", frame.Status)
		conn.protocolError(sid)
	}
}

// handleServerData performs the processing of Data frames sent by the server.
func (conn *conn) handleServerData(frame *dataFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.StreamID

	// Handle push data.
	if sid&1 == 0 {
		// Ignore refused push data.
		if req := conn.pushRequests[sid]; req != nil && conn.pushReceiver != nil {
			conn.pushReceiver.ReceiveData(req, frame.Data, frame.Flags.FINISH())
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
}

// handleResponse performs the processing of Response frames.
func (conn *conn) handleResponse(frame *responseFrame) {
	conn.Lock()
	defer conn.Unlock()

	sid := frame.StreamID

	if conn.server != nil {
		log.Println("Error: Only clients can receive Response frames.")
		conn.numBenignErrors++
		return
	}

	// Check Stream ID is odd.
	if sid&1 == 0 {
		log.Printf("Error: Received Response with Stream ID %d, which should be odd.\n", sid)
		conn.numBenignErrors++
		return
	}

	if !sid.Valid() {
		log.Printf("Error: Received Response with Stream ID %d, which exceeds the limit.\n", sid)
		conn.protocolError(sid)
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
	conn.streams[sid].ReceiveFrame(frame)
}

// newStream is used to create a new serverStream from a SYN_STREAM frame.
func (conn *conn) newStream(frame *requestFrame, output chan<- Frame) *serverStream {
	stream := new(serverStream)
	stream.conn = conn
	stream.streamID = frame.StreamID
	stream.state = new(StreamState)
	stream.output = output
	stream.header = make(http.Header)
	stream.stop = conn.stop

	if frame.Flags.FINISH() {
		stream.state.CloseThere()
	}

	header := frame.Header
	rawUrl := header.Get(":scheme") + "://" + header.Get(":host") + header.Get(":path")

	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Println("Error: Received SYN_STREAM with invalid request URL: ", err)
		return nil
	}

	vers := header.Get(":version")
	major, minor, ok := http.ParseHTTPVersion(vers)
	if !ok {
		log.Println("Error: Invalid HTTP version: " + vers)
		return nil
	}

	method := header.Get(":method")

	// Build this into a request to present to the Handler.
	stream.request = &http.Request{
		Method:     method,
		URL:        url,
		Proto:      vers,
		ProtoMajor: major,
		ProtoMinor: minor,
		RemoteAddr: conn.remoteAddr,
		Header:     header,
		Host:       url.Host,
		RequestURI: url.Path,
		TLS:        conn.tlsState,
	}

	return stream
}

// protocolError informs the other endpoint that a protocol error has
// occurred, stops all running streams, and ends the connection.
func (conn *conn) protocolError(streamID StreamID) {
	reply := new(errorFrame)
	reply.StreamID = streamID
	reply.Status = PROTOCOL_ERROR
	conn.output[0] <- reply

	conn.Close()
}

// readFrames is the main processing loop, where frames
// are read from the connection and processed individually.
// Returning from readFrames begins the cleanup and exit
// process for this connection.
func (conn *conn) readFrames() {
	// Main loop.
Loop:
	for {

		// This is the mechanism for handling too many benign errors.
		// Default MaxBenignErrors is 10.
		if conn.numBenignErrors > MaxBenignErrors {
			log.Println("Error: Too many invalid stream IDs received. Ending connection.")
			conn.protocolError(0)
		}

		// ReadFrame takes care of the frame parsing for us.
		frame, err := readFrame(conn.buf)
		conn.refreshReadTimeout()
		if err != nil {
			if err == io.EOF {
				// Client has closed the TCP connection.
				debug.Println("Note: Endpoint has disconnected.")
				conn.Close()
				return
			}

			log.Printf("Error: Encountered read error: %q\n", err.Error())
			conn.Close()
			return
		}

		// Decompress the frame's headers, if there are any.
		err = frame.Decompress(conn.decompressor)
		if err != nil {
			log.Println("Error in decompression: ", err)
			conn.protocolError(0)
		}

		debug.Println("Received Frame:")
		debug.Println(frame)

		// This is the main frame handling section.
		switch frame := frame.(type) {

		case *requestFrame:
			conn.handleRequest(frame)

		case *responseFrame:
			conn.handleResponse(frame)

		case *dataFrame:
			if conn.server == nil {
				conn.handleServerData(frame)
			} else {
				conn.handleClientData(frame)
			}

		case *errorFrame:
			if statusCodeIsFatal(frame.Status) {
				code := statusCodeText[frame.Status]
				log.Printf("Warning: Received %s on stream %d. Closing connection.\n", code, frame.StreamID)
				conn.Close()
				return
			}
			conn.handleError(frame)

		case *headersFrame:
			conn.handleHeaders(frame)

		case *pushFrame:
			conn.handlePush(frame)

		case *pingFrame:
			// Check whether Ping ID is a response.
			if frame.PingID&1 == conn.nextPingID&1 {
				if conn.pings[frame.PingID] == nil {
					log.Printf("Warning: Ignored Ping with Ping ID %d, which hasn't been requested.\n",
						frame.PingID)
					conn.numBenignErrors++
					continue Loop
				}
				conn.pings[frame.PingID] <- Ping{}
				close(conn.pings[frame.PingID])
				delete(conn.pings, frame.PingID)
			} else if !frame.Flags.FINISH() {
				debug.Println("Received Ping. Replying...")
				frame.Flags = FLAG_FINISH
				conn.output[0] <- frame
			}

		default:
			log.Println(fmt.Sprintf("Ignored unexpected frame type %T", frame))
		}
	}
}

// Add timeouts if requested by the server.
func (conn *conn) refreshTimeouts() {
	if conn.server == nil {
		return
	}
	if d := conn.server.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
	if d := conn.server.WriteTimeout; d != 0 {
		conn.conn.SetWriteDeadline(time.Now().Add(d))
	}
}

// Add timeouts if requested by the server.
func (conn *conn) refreshReadTimeout() {
	if conn.server == nil {
		return
	}
	if d := conn.server.ReadTimeout; d != 0 {
		conn.conn.SetReadDeadline(time.Now().Add(d))
	}
}

// Add timeouts if requested by the server.
func (conn *conn) refreshWriteTimeout() {
	if conn.server == nil {
		return
	}
	if d := conn.server.WriteTimeout; d != 0 {
		conn.conn.SetWriteDeadline(time.Now().Add(d))
	}
}

// selectFrameToSend follows the specification's guidance
// on frame priority, sending frames with higher priority
// (a smaller number) first.
func (conn *conn) selectFrameToSend() (frame Frame) {
	if conn.closed() {
		return nil
	}

	// Try in priority order first.
	for i := 0; i < 8; i++ {
		select {
		case frame = <-conn.output[i]:
			return frame
		default:
		}
	}

	// No frames are immediately pending, so if the
	// connection is being closed, cease sending
	// safely.
	if conn.sending != nil {
		close(conn.sending)
		runtime.Goexit()
	}

	// Wait for any frame.
	select {
	case frame = <-conn.output[0]:
		return frame
	case frame = <-conn.output[1]:
		return frame
	case frame = <-conn.output[2]:
		return frame
	case frame = <-conn.output[3]:
		return frame
	case frame = <-conn.output[4]:
		return frame
	case frame = <-conn.output[5]:
		return frame
	case frame = <-conn.output[6]:
		return frame
	case frame = <-conn.output[7]:
		return frame
	case _ = <-conn.stop:
		return nil
	}
}

// send is run in a separate goroutine. It's used
// to ensure clear interleaving of frames and to
// provide assurances of priority and structure.
func (conn *conn) send() {
	// Enter the processing loop.
	for {
		frame := conn.selectFrameToSend()

		if frame == nil {
			conn.Close()
			return
		}

		// Compress any name/value header blocks.
		err := frame.Compress(conn.compressor)
		if err != nil {
			log.Println(err)
			continue
		}

		debug.Println("Sending Frame:")
		debug.Println(frame)

		// Leave the specifics of writing to the
		// connection up to the frame.
		_, err = frame.WriteTo(conn.conn)
		conn.refreshWriteTimeout()
		if err != nil {
			if err == io.EOF {
				// Server has closed the TCP connection.
				debug.Println("Note: Endpoint has disconnected.")
				conn.Close()
				return
			}

			// Unexpected error which prevented a write.
			log.Printf("Error: Encountered write error: %q\n", err.Error())
			conn.Close()
			return
		}
	}
}
