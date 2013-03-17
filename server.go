// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// WP server

package wp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Errors introduced by the WP server.
var (
	ErrWriteAfterFlush = errors.New("Conn.Write called after Flush")
	ErrContentLength   = errors.New("Conn.Write wrote more than the declared Content-Length")
)

// Objects implementing the Handler interface can be
// registered to serve a particular path or subtree
// in the HTTP server.
//
// ServeWP should write reply headers and data to the ResponseWriter
// and then return.  Returning signals that the request is finished
// and that the WP server can move on to the next request on
// the connection.
type Handler interface {
	ServeWP(ResponseWriter, *Request)
}

// A ResponseWriter interface is used by a WP handler to
// construct a WP response.
type ResponseWriter interface {
	// Header returns the header map that will be sent by WriteHeader.
	// Changing the header after a call to WriteHeader (or Write) has
	// no effect.
	Header() Header
	
	// Write writes the data to the connection as part of a WP reply.
	// If WriteHeader has not yet been called, Write calls 
	// WriteHeader(wp.StatusSuccess, wp.StatusSuccess) before writing
	// the data. Write determines the MIME type from the result of
	// passing the initial 512 bytes of written data to
	// DetectContentType.
	Write([]byte) (int, error)
	
	// Send is used after Write for streaming data, where HTTP would use
	// polling or an alternative protocol.
	Send()

	// WriteHeader sends a WP response header with status code and 
	// subcode. If WriteHeader is not called explicitly, the first
	// call to Write will trigger an implicit
	// WriteHeader(wp.StatusSuccess, wp.StatusSuccess). Thus explicit
	// calls to WriteHeader are mainly used to send error codes.
	WriteHeader(int, int)
	
	// Push is used to send other resources to the client immediately.
	// This is intended for dynamic resources, or other resources the
	// client is guaranteed not to have cached. If a resource may have
	// been cached by the client, Suggest is advised instead.
	// Push calls WriteHeader(wp.StatusSuccess, wp.StatusSuccess)
	// before sending any data.
	Push(string, Header, uint64, []byte) (error)
	
	// PushFile is a helper function for Push, which takes a resource
	// name and the filename from which to read and pushes the data
	// to the client automatically.
	PushFile(string, string) (error)
	
	// Suggest is used for recommending resources to the client, which
	// they may have cached. The timestamp for the resource must be sent
	// so that the client can determine whether they have the latest
	// version, if they have something cached. If the resource has no
	// timestamp, then Push is advised instead.
	// Suggest calls WriteHeader(wp.StatusSuccess, wp.StatusSuccess)
	// before sending any data.
	Suggest(string, Header, uint64) (error)
	
	// SuggestFile is a helper function for Suggest, which takes a
	// resource name and the filename from which to read and sends
	// the data to the client automatically.
	SuggestFile(string, string) (error)
}

var errTooLarge = errors.New("wp: request too large")

// A response represents the server side of an WP response.
type response struct {
	conn          *conn
	req           *Request // request for this response
	wroteHeader   bool     // reply header has been written
	responseSent  bool     // Data response packet has been sent
	header        Header   // reply header parameters
	written       int64    // number of bytes written in body
	contentLength int64    // explicitly-declared Content-Length; or -1
	status        int      // status code passed to WriteHeader
	substatus     int      // status subcode passed to WriteHeader
	needSniff     bool     // need to sniff to find Content-Type
	streamID      uint16   // stream ID for this response.

	// close connection after this reply.  set on request and
	// updated after response from handler if there's a
	// "Connection: keep-alive" response header and a
	// Content-Length.
	closeAfterReply bool

	// requestBodyLimitHit is set by requestTooLarge when
	// maxBytesReader hits its max size. It is checked in
	// WriteHeader, to make sure we don't consume the
	// remaining request body to try to advance to the next WP
	// request. Instead, when this is set, we stop reading
	// subsequent requests on this connection and stop reading
	// input from it.
	requestBodyLimitHit bool
	
	lock *sync.Mutex
	
	state *ServerSession
}

func (w *response) Header() Header {
	return w.header
}

type writerOnly struct {
	io.Writer
}

// maxPostHandlerReadBytes is the max number of Request.Body bytes not
// consumed by a handler that the server will read from the client
// in order to keep a connection alive.  If there are more bytes than
// this then the server to be paranoid instead sends a "Connection:
// close" response.
//
// This number is approximately what a typical machine's TCP buffer
// size is anyway.  (if we have the bytes on the machine, we might as
// well read them)
const maxPostHandlerReadBytes = 256 << 10

func (w *response) WriteHeader(code, subcode int) {
	if w.wroteHeader && w.status != code && w.substatus != subcode {
		log.Print("wp: multiple response.WriteHeader calls")
		debug.PrintStack()
		return
	}
	w.status = code
	w.substatus = subcode
	w.wroteHeader = true
}

// sniff uses the first block of written data,
// stored in w.conn.body, to decide the Content-Type
// for the WP body.
// TODO: Do sniffing.
// func (w *response) sniff() {
// 	if !w.needSniff {
// 		return
// 	}
// 	w.needSniff = false
// 
// 	//data := w.conn.body[:sniffLen]
// 	//fmt.Fprintf(w.conn.buf, "Content-Type: %s", DetectContentType(data))
// 	//w.conn.buf.Write(data)
// }

// Write is the main method with which data is sent.
func (w *response) Write(data []byte) (n int, err error) {
	
	w.lock.Lock()
	if !w.wroteHeader {
		w.WriteHeader(StatusSuccess, StatusSuccess)
	}
	
	if len(data) == 0 {
		return 0, nil
	}

	w.written += int64(len(data)) // ignoring errors, for errorKludge
	if w.contentLength != -1 && w.written > w.contentLength {
		return 0, ErrContentLength
	}

	// if w.needSniff {
	// 
	// 		// Filled the buffer; more data remains.
	// 		// Sniff the content (flushes the buffer)
	// 		// and then proceed with the remainder
	// 		// of the data as a normal Write.
	// 		// Calling sniff clears needSniff.
	// 		w.sniff()
	// 	}
	
	// TODO: Actually read and use these.
	rawHeaders := w.req.Header
	rawHeaders.Del("Content-Length")
	rawHeaders.Del("Language")
	rawHeaders.Del("Hostname")
	rawHeaders.Del("Useragent")
	rawHeaders.Del("Referrer")
	headers := rawHeaders.String()
	
	responseSize := DataResponseSize + len(headers)
	if w.responseSent {
		responseSize = 0
	}
	contentSize  := DataContentSize + len(data)
	if len(data) == 0 {
		contentSize = 0
	}
	
	buffer := make([]byte, responseSize + contentSize)
	
	if !w.responseSent {
		response := DataResponse(buffer)
		response.SetPacketType(IsDataResponse)
		response.SetStreamID(w.streamID)
		response.SetResponseCode(byte(w.status))
		response.SetResponseSubcode(byte(w.substatus))
		// TODO: Add timestamps
		// TODO: Add MIME types
		response.SetHeader(headers)
		w.responseSent = true
	}
	
	if len(data) > 0 {
		content := DataContent(buffer[responseSize:])
		content.SetPacketType(IsDataContent)
		content.SetStreamID(w.streamID)
		content.SetData(data)
	}
	w.lock.Unlock()
	
	// Send data down the wire.
	if w == nil {
		log.Fatal("Write w = nil")
	} else if w.conn == nil {
		log.Fatal("Write w.conn = nil")
	} else if w.conn.rwc == nil {
		log.Fatal("Write w.conn.rwc = nil")
	}
	
	w.conn.rwc.Write(buffer)

	return len(data), nil
}

// Send is used to send data immediately. This is normally used for controlled
// streaming data, where HTTP would use polling or an alternative protocol.
func (w *response) Send() {
	w.finishRequest(true)
}

func (w *response) push(res string, rawHeaders Header, time uint64, data []byte, push bool) (uint16, error) {
	w.lock.Lock()
	if !w.wroteHeader {
		w.WriteHeader(StatusSuccess, StatusSuccess)
		w.Write([]byte{})
	}
	
	w.conn.lock.Lock()
	w.state.IdEven += 2
	pushID := uint16(w.state.IdEven)
	w.conn.lock.Unlock()
	
	flags := byte(0)
	if !push {
		flags = DataPushFlagSuggest
	}
	
	rawHeaders.Del("Content-Length")
	rawHeaders.Del("Language")
	rawHeaders.Del("Hostname")
	rawHeaders.Del("Useragent")
	rawHeaders.Del("Referrer")
	headers := rawHeaders.String()
	
	pushSize := DataPushSize + len(headers) + len(res)
	contentSize  := DataContentSize + len(data)
	if len(data) == 0 {
		contentSize = 0
	}
	
	buffer := make([]byte, pushSize + contentSize)
	
	response := DataPush(buffer)
	response.SetPacketType(IsDataPush)
	response.SetFlags(flags)
	response.SetStreamID(pushID)
	response.SetSourceStreamID(w.streamID)
	response.SetTimestamp(time)
	response.SetResource(res)
	response.SetHeader(headers)
	
	if len(data) > 0 {
		content := DataContent(buffer[pushSize:])
		content.SetPacketType(IsDataContent)
		content.SetStreamID(pushID)
		content.SetData(data)
	}
	w.lock.Unlock()
	
	// Send data down the wire.
	if w == nil {
		log.Fatal("Write w = nil")
	} else if w.conn == nil {
		log.Fatal("Write w.conn = nil")
	} else if w.conn.rwc == nil {
		log.Fatal("Write w.conn.rwc = nil")
	}
	
	w.conn.rwc.Write(buffer)

	return pushID, nil
}

func (w *response) Push(resource string, rawHeaders Header, timestamp uint64, data []byte) error {
	_, err := w.push(resource, rawHeaders, timestamp, data, true)
	return err
}

func (w *response) PushFile(resource, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		return err
	}
	
	id, err := w.push(resource, Header{}, uint64(d.ModTime().UnixNano()), []byte{}, true)
	if err != nil {
		return err
	}
	
	r := bufio.NewReader(f)
	buf := make([]byte, 32*1024)
	
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
	
			data := NewDataContent(buf[0:nr])
			data.SetStreamID(id)
	
			// Send data down the wire.
			if w == nil {
				log.Fatal("Write w = nil")
			} else if w.conn == nil {
				log.Fatal("Write w.conn = nil")
			} else if w.conn.rwc == nil {
				log.Fatal("Write w.conn.rwc = nil")
			}
			w.conn.rwc.Write(data)
		}
		if er == io.EOF {
			reply := NewDataContent([]byte{})
			reply.SetFlags(DataContentFlagFinish)
			reply.SetStreamID(id)
			w.conn.rwc.Write(reply)
			return nil
		}
		if er != nil {
			reply := NewStreamError()
			reply.SetStatusCode(InternalError)
			w.conn.rwc.Write(reply)
			return er
		}
	}
	
	reply := NewDataContent([]byte{})
	reply.SetFlags(DataContentFlagFinish)
	
	w.conn.rwc.Write(reply)
	
	return nil
}

func (w *response) Suggest(resource string, rawHeaders Header, timestamp uint64) error {
	_, err := w.push(resource, rawHeaders, timestamp, []byte{}, false)
	return err
}

func (w *response) SuggestFile(resource, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		return err
	}
	
	_, err = w.push(resource, Header{}, uint64(d.ModTime().UnixNano()), []byte{}, false)
	if err != nil {
		return err
	}
	
	return nil
}

func (w *response) finishRequest(streaming bool) {
	w.lock.Lock()
	if !w.wroteHeader {
		w.WriteHeader(StatusSuccess, StatusSuccess)
	}
	
	rawHeaders := w.req.Header
	rawHeaders.Del("Content-Length")
	rawHeaders.Del("Language")
	rawHeaders.Del("Hostname")
	rawHeaders.Del("Useragent")
	rawHeaders.Del("Referrer")
	headers := rawHeaders.String()
	
	var buffer []byte
	
	if !w.responseSent {
		buffer = make([]byte, DataResponseSize + len(headers))
		
		response := DataResponse(buffer)
		response.SetPacketType(IsDataResponse)
		response.SetFlags(DataResponseFlagFinish)
		response.SetStreamID(w.streamID)
		response.SetResponseCode(byte(w.status))
		response.SetResponseSubcode(byte(w.substatus))
		// TODO: Add timestamps
		// TODO: Add MIME types
		response.SetHeader(headers)
		w.responseSent = true
		
	} else {
		buffer = make([]byte, DataContentSize)
		
		content := DataContent(buffer)
		content.SetPacketType(IsDataContent)
		
		if streaming {
			content.SetFlags(DataContentFlagFinish | DataContentFlagStreaming)
		} else {
			content.SetFlags(DataContentFlagFinish)
		}
		
		content.SetStreamID(uint16(w.req.StreamID))
	}
	w.lock.Unlock()
	
	// Send data down the wire.
	if w == nil {
		log.Fatal("finishRequest w = nil")
	} else if w.conn == nil {
		log.Fatal("finishRequest w.conn = nil")
	} else if w.conn.rwc == nil {
		log.Fatal("finishRequest w.conn.rwc = nil")
	}
	w.conn.rwc.Write(buffer)
}

// A conn represents the server side of a WP connection.
type conn struct {
	remoteAddr string                 // network address of remote side
	server     *Server                // the Server on which the connection arrived
	rwc        net.Conn               // i/o connection
	lock       *sync.Mutex            // mutex for writing to the connection
	buf        *bufio.ReadWriter      // buffered(lr,rwc), reading from bufio->limitReader->rwc
	tlsState   *tls.ConnectionState   // or nil when not using TLS
	state      *ServerSession         // Server session state
	stopChans  map[uint16]chan<-stop  // set of channels for stopping individual streams
}

type stop struct{}

// Read next request from connection.
func (c *conn) readRequest() (w *response, err error) {
	
	var req *Request
	if req, err = ReadRequest(c.buf.Reader); err != nil {
		return nil, err
	}

	req.RemoteAddr = c.remoteAddr
	req.TLS = c.tlsState

	w = new(response)
	w.conn = c
	w.req = req
	w.header = make(Header)
	w.contentLength = -1
	w.lock = new(sync.Mutex)
	return w, nil
}

func (c *conn) finalFlush() {
	if c.buf != nil {
		c.buf.Flush()
		c.buf = nil
	}
}

// Close the connection.
func (c *conn) close() {
	c.finalFlush()
	if c.rwc != nil {
		c.rwc.Close()
		c.rwc = nil
	}
}

// rstAvoidanceDelay is the amount of time we sleep after closing the
// write side of a TCP connection before closing the entire socket.
// By sleeping, we increase the chances that the client sees our FIN
// and processes its final data before they process the subsequent RST
// from closing a connection with known unread data.
// This RST seems to occur mostly on BSD systems. (And Windows?)
// This timeout is somewhat arbitrary (~latency around the planet).
const rstAvoidanceDelay = 500 * time.Millisecond

// closeWrite flushes any outstanding data and sends a FIN packet (if
// client is connected via TCP), signalling that we're done.  We then
// pause for a bit, hoping the client processes it before `any
// subsequent RST.
//
// See http://golang.org/issue/3595
func (c *conn) closeWriteAndWait() {
	c.finalFlush()
	if tcp, ok := c.rwc.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
	time.Sleep(rstAvoidanceDelay)
}

// Serve a new connection.
func (c *conn) serve() {
	var finished sync.WaitGroup
	defer func() {
		err := recover()
		if err != nil {
			log.Printf("wp: panic serving %v: %v\n", c.remoteAddr, err)
		}

		//fmt.Print("Waiting...")
		finished.Wait()
		//fmt.Println(" done.")
		if c.rwc != nil {
			c.rwc.Close()
		}
		c.close()
		return
	}()

	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			c.close()
			return
		}
		c.tlsState = new(tls.ConnectionState)
		*c.tlsState = tlsConn.ConnectionState()
	}

	for {
		w, err := c.readRequest() // returns *response, error
		if err != nil {
				
			if e, ok := err.(gotStreamError); ok {
				if e.status == ProtocolError ||
					e.status == UnsupportedVersion ||
					e.status == InternalError ||
					e.status == FinishStream {
					// Session end.
				} else {
					// Non-fatal error. For now, this still ends the session.
					// TODO: handle non-fatal errors correctly.
					reply := NewStreamError()
					reply.SetStatusCode(FinishStream)
					c.rwc.Write(reply)
				}
				return
			}
			if _, ok := err.(gotDataResponse); ok {
				// Not expecting data responses, so end the session.
				reply := NewStreamError()
				reply.SetStatusCode(ProtocolError)
				c.rwc.Write(reply)
				return
			}
			if _, ok := err.(gotDataPush); ok {
				// Not expecting data pushes, so end the session.
				reply := NewStreamError()
				reply.SetStatusCode(ProtocolError)
				c.rwc.Write(reply)
				return
			}
			if _, ok := err.(gotDataContent); ok {
				// Not expecting data.
				reply := NewStreamError()
				reply.SetStatusCode(ProtocolError)
				c.rwc.Write(reply)
				continue
			}
			
			// Unknown error: internal error.
			reply := NewStreamError()
			reply.SetStatusCode(InternalError)
			c.rwc.Write(reply)
			return
		} /* if err != nil */
		
		handler := c.server.Handler
		if handler == nil {
			handler = DefaultServeMux
		}
		
		// Handle packets other than data requests.
		if w.req.Type == IsHello {
			lang := w.req.Header["Language"][0]
			host := w.req.Host
			usra := w.req.Header["Useragent"][0]
			refr := w.req.Header["Referrer"][0]
				
			if lang != "" { c.state.Lang = lang }
			if host != "" { c.state.Host = host }
			if usra != "" { c.state.Useragent = usra }
			if refr != "" { c.state.Ref = refr }
			
			continue
		}
		if w.req.Type == IsPingRequest { // TODO: make ping responses optional.
			id := byte(w.req.StreamID)
			reply := NewPingResponse(id)
			c.rwc.Write(reply)
			continue
		}
		if w.req.Type == IsPingResponse {
			// TODO: inform the user of ping responses.
			continue
		}
		
		if w.req.Type != IsDataRequest {
			// This shouldn't happen.
			reply := NewStreamError()
			reply.SetStatusCode(InternalError)
			c.rwc.Write(reply)
			return
		}
		
		c.lock.Lock()
		if c.state.IdOdd != 0 && uint16(w.req.StreamID) != c.state.IdOdd + 2 {
			// Invalid stream ID
			reply := NewStreamError()
			
			if uint16(w.req.StreamID) > c.state.IdOdd + 2 {
				// Stream ID is too large
				reply.SetStatusCode(RefusedStream)
			} else {
				// Stream ID is too small
				reply.SetStatusCode(StreamInUse)
			}
			reply.SetStatusData(c.state.IdOdd + 2)
			//fmt.Println("Recieved:", w.req.StreamID, "Expected:", c.state.IdOdd + 2)
			c.rwc.Write(reply)
			c.lock.Unlock()
			continue
		}
		c.lock.Unlock()
		
		/*************************/
		/* End of error checking */
		/*************************/
		
		c.lock.Lock()
		c.state.IdOdd = uint16(w.req.StreamID)
		w.streamID = c.state.IdOdd
		w.state = c.state
		c.lock.Unlock()

		// Experimental concurrency support.
		//go func() {
		func() {
			finished.Add(1)
			handler.ServeWP(w, w.req)
			w.finishRequest(false)
			finished.Done()
		}()
	}
}

// noLimit is an effective infinite upper bound for io.LimitedReader
const noLimit int64 = (1 << 63) - 1

// Create new connection from rwc.
func (srv *Server) newConn(rwc net.Conn) (c *conn, err error) {
	c = new(conn)
	c.remoteAddr = rwc.RemoteAddr().String()
	c.server = srv
	c.rwc = rwc
	br := bufio.NewReader(rwc)
	bw := bufio.NewWriter(rwc)
	c.buf = bufio.NewReadWriter(br, bw)
	c.state = new(ServerSession)
	c.lock = new(sync.Mutex)
	c.stopChans = make(map[uint16]chan<-stop)
	return c, nil
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as WP handlers.  If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler object that calls f.
type HandlerFunc func(ResponseWriter, *Request)

// ServeWP calls f(w, r).
func (f HandlerFunc) ServeWP(w ResponseWriter, r *Request) {
	f(w, r)
}

// Error replies to the request with the specified error message and WP code.
func Error(w ResponseWriter, error string, code, subcode int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code, subcode)
	fmt.Fprintln(w, error)
}

// NotFound replies to the request with an WP 2/2 Not Found error.
const notFoundHTML = "<html><head><title>2/2 Not Found</title></head><body><h1>2/2 Not Found</h1></body></html>"
func NotFound(w ResponseWriter, r *Request) { Error(w, notFoundHTML, StatusClientError, StatusNotFound) }

// NotFoundHandler returns a simple request handler
// that replies to each request with a 2/2 Not Found reply.
func NotFoundHandler() Handler { return HandlerFunc(NotFound) }

// StripPrefix returns a handler that serves WP requests
// by removing the given prefix from the request URL's Path
// and invoking the handler h. StripPrefix handles a
// request for a path that doesn't begin with prefix by
// replying with a WP 2/2 Not Found error.
func StripPrefix(prefix string, h Handler) Handler {
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			NotFound(w, r)
			return
		}
		r.URL.Path = r.URL.Path[len(prefix):]
			h.ServeWP(w, r)
	})
}

// Redirect replies to the request with a redirect to url,
// which may be a path relative to the request path.
func Redirect(w ResponseWriter, r *Request, urlStr string, code, subcode int) {
	if u, err := url.Parse(urlStr); err == nil {
		// If url was relative, make absolute by
		// combining with request path.
		// The browser would probably do this for us,
		// but doing it ourselves is more reliable.

		// NOTE(rsc): RFC 2616 says that the Location
		// line must be an absolute URI, like
		// "wp://www.google.com/redirect/",
		// not a path like "/redirect/".
		// Unfortunately, we don't know what to
		// put in the host name section to get the
		// client to connect to us again, so we can't
		// know the right absolute URI to send back.
		// Because of this problem, no one pays attention
		// to the RFC; they all send back just a new path.
		// So do we.
		oldpath := r.URL.Path
		if oldpath == "" { // should not happen, but avoid a crash if it does
			oldpath = "/"
		}
		if u.Scheme == "" {
			// no leading wp://server
			if urlStr == "" || urlStr[0] != '/' {
				// make relative path absolute
				olddir, _ := path.Split(oldpath)
				urlStr = olddir + urlStr
			}

			var query string
			if i := strings.Index(urlStr, "?"); i != -1 {
				urlStr, query = urlStr[:i], urlStr[i:]
			}

			// clean up but preserve trailing slash
			trailing := urlStr[len(urlStr)-1] == '/'
			urlStr = path.Clean(urlStr)
			if trailing && urlStr[len(urlStr)-1] != '/' {
				urlStr += "/"
			}
			urlStr += query
		}
	}

	w.Header().Set("Location", urlStr)
	w.WriteHeader(code, subcode)
}

var htmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	// "&#34;" is shorter than "&quot;".
	`"`, "&#34;",
	// "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
	"'", "&#39;",
)

func htmlEscape(s string) string {
	return htmlReplacer.Replace(s)
}

// Redirect to a fixed URL
type redirectHandler struct {
	url     string
	code    int
	subcode int
}

func (rh *redirectHandler) ServeWP(w ResponseWriter, r *Request) {
	Redirect(w, r, rh.url, rh.code, rh.subcode)
}

// RedirectHandler returns a request handler that redirects
// each request it receives to the given url using the given
// status code.
func RedirectHandler(url string, code, subcode int) Handler {
	return &redirectHandler{url, code, subcode}
}

// ServeMux is an HTTP request multiplexer.
// It matches the URL of each incoming request against a list of registered
// patterns and calls the handler for the pattern that
// most closely matches the URL.
//
// Patterns name fixed, rooted paths, like "/favicon.ico",
// or rooted subtrees, like "/images/" (note the trailing slash).
// Longer patterns take precedence over shorter ones, so that
// if there are handlers registered for both "/images/"
// and "/images/thumbnails/", the latter handler will be
// called for paths beginning "/images/thumbnails/" and the
// former will receive requests for any other paths in the
// "/images/" subtree.
//
// Patterns may optionally begin with a host name, restricting matches to
// URLs on that host only.  Host-specific patterns take precedence over
// general patterns, so that a handler might register for the two patterns
// "/codesearch" and "codesearch.google.com/" without also taking over
// requests for "wp://www.google.com/".
//
// ServeMux also takes care of sanitizing the URL request path,
// redirecting any request containing . or .. elements to an
// equivalent .- and ..-free URL.
type ServeMux struct {
	mu    sync.RWMutex
	m     map[string]muxEntry
	hosts bool // whether any patterns contain hostnames
	ping  bool // whether to respond to ping requests
}

type muxEntry struct {
	explicit bool
	h        Handler
	pattern  string
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux { return &ServeMux{m: make(map[string]muxEntry)} }

// DefaultServeMux is the default ServeMux used by Serve.
var DefaultServeMux = NewServeMux()

// Does path match pattern?
func pathMatch(pattern, path string) bool {
	n := len(pattern)
	if n == 0 {
		// should not happen
		return false
	}
	if pattern[n-1] != '/' {
		return pattern == path
	}
	return len(path) >= n && path[0:n] == pattern
}

// Return the canonical path for p, eliminating . and .. elements.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// put the trailing slash back if necessary.
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// Find a handler on a handler map given a path string
// Most-specific (longest) pattern wins
func (mux *ServeMux) match(path string) (h Handler, pattern string) {
	var n = 0
	for k, v := range mux.m {
		if !pathMatch(k, path) {
			continue
		}
		if h == nil || len(k) > n {
			n = len(k)
			h = v.h
			pattern = v.pattern
		}
	}
	return
}

// Handler returns the handler to use for the given request,
// consulting r.Method, r.Host, and r.URL.Path. It always returns
// a non-nil handler. If the path is not in its canonical form, the
// handler will be an internally-generated handler that redirects
// to the canonical path.
//
// Handler also returns the registered pattern that matches the
// request or, in the case of internally-generated redirects,
// the pattern that will match after following the redirect.
//
// If there is no registered handler that applies to the request,
// Handler returns a ``page not found'' handler and an empty pattern.
func (mux *ServeMux) Handler(r *Request) (h Handler, pattern string) {
	return mux.handler(r.Host, r.URL.Path)
}

// handler is the main implementation of Handler.
// The path is known to be in canonical form, except for CONNECT methods.
func (mux *ServeMux) handler(host, path string) (h Handler, pattern string) {
	mux.mu.RLock()
	defer mux.mu.RUnlock()

	// Host-specific pattern takes precedence over generic ones
	if mux.hosts {
		h, pattern = mux.match(host + path)
	}
	if h == nil {
		h, pattern = mux.match(path)
	}
	if h == nil {
		h, pattern = NotFoundHandler(), ""
	}
	return
}

// ServeWP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (mux *ServeMux) ServeWP(w ResponseWriter, r *Request) {
	h, _ := mux.Handler(r)
	h.ServeWP(w, r)
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, Handle panics.
func (mux *ServeMux) Handle(pattern string, handler Handler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()

	if pattern == "" {
		panic("wp: invalid pattern " + pattern)
	}
	if handler == nil {
		panic("wp: nil handler")
	}
	if mux.m[pattern].explicit {
		panic("wp: multiple registrations for " + pattern)
	}

	mux.m[pattern] = muxEntry{explicit: true, h: handler, pattern: pattern}

	if pattern[0] != '/' {
		mux.hosts = true
	}

	// Helpful behavior:
	// If pattern is /tree/, insert an implicit permanent redirect for /tree.
	// It can be overridden by an explicit registration.
	n := len(pattern)
	if n > 0 && pattern[n-1] == '/' && !mux.m[pattern[0:n-1]].explicit {
		// If pattern contains a host name, strip it and use remaining
		// path for redirect.
		path := pattern
		if pattern[0] != '/' {
			// In pattern, at least the last character is a '/', so
			// strings.Index can't be -1.
			path = pattern[strings.Index(pattern, "/"):]
		}
		mux.m[pattern[0:n-1]] = muxEntry{h: RedirectHandler(path, StatusRedirection, StatusMovedPermanently), pattern: pattern}
	}
}

// HandleFunc registers the handler function for the given pattern.
func (mux *ServeMux) HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	mux.Handle(pattern, HandlerFunc(handler))
}

// Handle registers the handler for the given pattern
// in the DefaultServeMux.
// The documentation for ServeMux explains how patterns are matched.
func Handle(pattern string, handler Handler) { DefaultServeMux.Handle(pattern, handler) }

// HandleFunc registers the handler function for the given pattern
// in the DefaultServeMux.
// The documentation for ServeMux explains how patterns are matched.
func HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	DefaultServeMux.HandleFunc(pattern, handler)
}

// Serve accepts incoming WP connections on the listener l,
// creating a new service thread for each.  The service threads
// read requests and then call handler to reply to them.
// Handler is typically nil, in which case the DefaultServeMux is used.
func Serve(l net.Listener, handler Handler) error {
	srv := &Server{Handler: handler}
	return srv.Serve(l)
}

// A Server defines parameters for running an HTTP server.
type Server struct {
	Addr           string        // TCP address to listen on, ":99" if empty
	Handler        Handler       // handler to invoke, http.DefaultServeMux if nil
	ReadTimeout    time.Duration // maximum duration before timing out read of the request
	WriteTimeout   time.Duration // maximum duration before timing out write of the response
	MaxHeaderBytes int           // maximum size of request headers, DefaultMaxHeaderBytes if 0
	TLSConfig      *tls.Config   // optional TLS config, used by ListenAndServeTLS
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.  If
// srv.Addr is blank, ":http" is used.
// func (srv *Server) ListenAndServe() error {
// 	addr := srv.Addr
// 	if addr == "" {
// 		addr = ":99"
// 	}
// 	l, e := net.Listen("tcp", addr)
// 	if e != nil {
// 		return e
// 	}
// 	return srv.Serve(l)
// }

// Serve accepts incoming connections on the Listener l, creating a
// new service thread for each.  The service threads read requests and
// then call srv.Handler to reply to them.
func (srv *Server) Serve(l net.Listener) error {
	defer l.Close()
	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		rw, e := l.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Printf("wp: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		if srv.ReadTimeout != 0 {
			rw.SetReadDeadline(time.Now().Add(srv.ReadTimeout))
		}
		if srv.WriteTimeout != 0 {
			rw.SetWriteDeadline(time.Now().Add(srv.WriteTimeout))
		}
		c, err := srv.newConn(rw)
		if err != nil {
			continue
		}
		go c.serve()
	}
	panic("not reached")
}

// ListenAndServe listens on the TCP network address addr
// and then calls Serve with handler to handle requests
// on incoming connections.  Handler is typically nil,
// in which case the DefaultServeMux is used.
//
// A trivial example server is:
//
//      package main
//
//      import (
//              "io"
//              "net/wp"
//              "log"
//      )
//
//      // hello world, the web server
//      func HelloServer(w wp.ResponseWriter, req *wp.Request) {
//              io.WriteString(w, "hello, world!\n")
//      }
//
//      func main() {
//              wp.HandleFunc("/hello", HelloServer)
//              err := wp.ListenAndServe(":12345", nil)
//              if err != nil {
//                      log.Fatal("ListenAndServe: ", err)
//              }
//      }
// func ListenAndServe(addr string, handler Handler) error {
// 	server := &Server{Addr: addr, Handler: handler}
// 	return server.ListenAndServe()
// }

// Files containing a certificate and
// matching private key for the server must be provided. If the certificate
// is signed by a certificate authority, the certFile should be the concatenation
// of the server's certificate followed by the CA's certificate.
//
// A trivial example server is:
//
//      import (
//              "log"
//              "net/wp"
//      )
//
//      func handler(w wp.ResponseWriter, req *wp.Request) {
//              w.Header().Set("Content-Type", "text/plain")
//              w.Write([]byte("This is an example server.\n"))
//      }
//
//      func main() {
//              wp.HandleFunc("/", handler)
//              log.Printf("About to listen on 10443. Go to wp://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
//
// One can use generate_cert.go in crypto/tls to generate cert.pem and key.pem.
func ListenAndServe(addr string, certFile string, keyFile string, handler Handler) error {
	server := &Server{Addr: addr, Handler: handler}
	return server.ListenAndServe(certFile, keyFile)
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls Serve to handle requests on incoming TLS connections.
//
// Filenames containing a certificate and matching private key for
// the server must be provided. If the certificate is signed by a
// certificate authority, the certFile should be the concatenation
// of the server's certificate followed by the CA's certificate.
//
// If srv.Addr is blank, ":100" is used.
func (srv *Server) ListenAndServe(certFile, keyFile string) error {
	addr := srv.Addr
	if addr == "" {
		addr = ":99"
	}
	config := &tls.Config{}
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"wp/1"}
	}

	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, config)
	return srv.Serve(tlsListener)
}

// TimeoutHandler returns a Handler that runs h with the given time limit.
//
// The new Handler calls h.ServeWP to handle each request, but if a
// call runs for more than ns nanoseconds, the handler responds with
// a 3/1 Service Unavailable error and the given message in its body.
// (If msg is empty, a suitable default message will be sent.)
// After such a timeout, writes by h to its ResponseWriter will return
// ErrHandlerTimeout.
func TimeoutHandler(h Handler, dt time.Duration, msg string) Handler {
	f := func() <-chan time.Time {
		return time.After(dt)
	}
	return &timeoutHandler{h, f, msg}
}

// ErrHandlerTimeout is returned on ResponseWriter Write calls
// in handlers which have timed out.
var ErrHandlerTimeout = errors.New("wp: Handler timeout")

type timeoutHandler struct {
	handler Handler
	timeout func() <-chan time.Time // returns channel producing a timeout
	body    string
}

func (h *timeoutHandler) errorBody() string {
	if h.body != "" {
		return h.body
	}
	return "<html><head><title>Timeout</title></head><body><h1>Timeout</h1></body></html>"
}

func (h *timeoutHandler) ServeWP(w ResponseWriter, r *Request) {
	done := make(chan bool)
	tw := &timeoutWriter{w: w}
	go func() {
		h.handler.ServeWP(tw, r)
		done <- true
	}()
	select {
	case <-done:
		return
	case <-h.timeout():
		tw.mu.Lock()
		defer tw.mu.Unlock()
		if !tw.wroteHeader {
			tw.w.WriteHeader(StatusServerError, StatusServiceUnavailable)
			tw.w.Write([]byte(h.errorBody()))
		}
		tw.timedOut = true
	}
}

type timeoutWriter struct {
	w ResponseWriter

	mu          sync.Mutex
	timedOut    bool
	wroteHeader bool
}

func (tw *timeoutWriter) Header() Header {
	return tw.w.Header()
}

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	timedOut := tw.timedOut
	tw.mu.Unlock()
	if timedOut {
		return 0, ErrHandlerTimeout
	}
	return tw.w.Write(p)
}

func (tw *timeoutWriter) Send() {
	tw.w.Send()
}

func (tw *timeoutWriter) Push(res string, rawHeaders Header, timestamp uint64, data []byte) error {
	return tw.w.Push(res, rawHeaders, timestamp, data)
}

func (tw *timeoutWriter) PushFile(resource, filename string) error {
	return tw.w.PushFile(resource, filename)
}

func (tw *timeoutWriter) Suggest(resource string, rawHeaders Header, timestamp uint64) error {
	return tw.w.Suggest(resource, rawHeaders, timestamp)
}

func (tw *timeoutWriter) SuggestFile(resource, filename string) error {
	return tw.w.SuggestFile(resource, filename)
}

func (tw *timeoutWriter) WriteHeader(code, subcode int) {
	tw.mu.Lock()
	if tw.timedOut || tw.wroteHeader {
		tw.mu.Unlock()
		return
	}
	tw.wroteHeader = true
	tw.mu.Unlock()
	tw.w.WriteHeader(code, subcode)
}
