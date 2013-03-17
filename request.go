// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// WP Request reading and parsing

package wp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"strings"
)

type badStringError struct {
	what string
	str  string
}

func (e *badStringError) Error() string { return fmt.Sprintf("%s %q", e.what, e.str) }

// A Request represents a WP request received by a server
// or to be sent by a client.
type Request struct {
	Header Header
	
	Type int // Packet type (see WP packet types).
	
	StreamID int // Stream ID or Ping ID
	
	URL *url.URL
	
	Protocol int // Protocol version.
	
	Timestamp uint64 // Time of last cache or 0 if not cached.
	
	// Close indicates whether to close the connection after
	// replying to this request.
	Close bool
	
	Host string
	
	// RemoteAddr allows WP servers and other software to record
	// the network address that sent the request, usually for
	// logging. This field is not filled in by ReadRequest and
	// has no defined format. The WP server in this package
	// sets RemoteAddr to an "IP:port" address before invoking a
	// handler.
	// This field is ignored by the WP client.
	RemoteAddr string
	
	// RequestURI is the unmodified Request-URI of the
	// Request-Line (RFC 2616, Section 5.1) as sent by the client
	// to a server. Usually the URL field should be used instead.
	// It is an error to set this field in an HTTP client request.
	RequestURI string
	
	// TLS allows HTTP servers and other software to record
	// information about the TLS connection on which the request
	// was received. This field is not filled in by ReadRequest.
	// The HTTP server in this package sets the field for
	// TLS-enabled connections before invoking a handler;
	// otherwise it leaves the field nil.
	// This field is ignored by the HTTP client.
	TLS *tls.ConnectionState
}

// UserAgent returns the client's User-Agent, if sent in the request.
func (r *Request) UserAgent() string {
	return r.Header.Get("User-Agent")
}

// Cookies parses and returns the HTTP cookies sent with the request.
func (r *Request) Cookies() []*Cookie {
	return readCookies(r.Header, "")
}

var ErrNoCookie = errors.New("wp: named cookie not present")

// Cookie returns the named cookie provided in the request or
// ErrNoCookie if not found.
func (r *Request) Cookie(name string) (*Cookie, error) {
	for _, c := range readCookies(r.Header, name) {
		return c, nil
	}
	return nil, ErrNoCookie
}

// Error types used in ReadRequest
type gotStreamError struct {
	streamID int // Source stream ID
	status   int // Error code
	data     int // Accompanying data
}

func (e gotStreamError) Error() string {
	return fmt.Sprintf("wp: Received stream error %d for stream %d, with data %d.",
		e.status, e.streamID, e.data)
}

type gotDataResponse struct {
	streamID int // Source stream ID
	code     int // Response code
	subcode  int // Response subcode
}

func (e gotDataResponse) Error() string {
	return fmt.Sprintf("wp: Received data response for stream %d, with response %d/%d.",
		e.streamID, e.code, e.subcode)
}

type gotDataPush struct {
	streamID int    // Source stream ID
	sourceID int    // Original request stream ID
	res      string // Resource being sent
}

func (e gotDataPush) Error() string {
	return fmt.Sprintf("wp: Received data push on stream %d (from stream %d), with resource %s.",
		e.streamID, e.sourceID, e.res)
}

type gotDataContent struct {
	streamID int    // Source stream ID
	first15  []byte // First 15 bytes of data
}

func (e gotDataContent) Error() string {
	return fmt.Sprintf("wp: Received data content on stream %d, containing: %v",
		e.streamID, e.first15)
}

// ReadRequest reads and parses a request from b.
func ReadRequest(b *bufio.Reader) (req *Request, err error) {

	req = new(Request)
	req.Header = make(map[string][]string)

	data := NewDataStream(b)
	var firstByte FirstByte
	if f, err := data.FirstByte(); err != nil {
		return nil, err
	} else {
		firstByte = f
	}
	
	switch firstByte.PacketType() {
		
	/* Hello */
	case IsHello:
		var hello Hello
		if h, err := data.Hello(); err != nil {
			fmt.Println("Error:", err)
			return nil, err
		} else {
			hello = h
		}
		
		//fmt.Println(hello)
		//fmt.Println("--Hello--")
		req.Type = IsHello
		req.Host = hello.Hostname()
		req.Protocol = hello.Version()
		req.Header.Add("Language", hello.LanguageString())
		req.Header.Add("Useragent", hello.Useragent())
		req.Header.Add("Referrer", hello.Referrer())
		
		return req, nil
		
	/* Stream Error */
	case IsStreamError:
		var streamError StreamError
		if s, err := data.StreamError(); err != nil {
			return nil, err
		} else {
			streamError = s
		}
		
		//fmt.Println(streamError)
		//fmt.Println("--StreamError--")
		e := gotStreamError{
			streamError.StreamID(),
			streamError.StatusCode(),
			streamError.StatusData(),
		}
		
		return nil, e
		
	/* Data Request */
	case IsDataRequest:
		var dataRequest DataRequest
		if d, err := data.DataRequest(); err != nil {
			return nil, err
		} else {
			dataRequest = d
		}
		
		//fmt.Println(dataRequest)
		//fmt.Println("--DataRequest--", dataRequest.Resource())
		req.Type = IsDataRequest
		req.StreamID = dataRequest.StreamID()
		req.RequestURI = dataRequest.Resource()
		if u, err := url.Parse(req.RequestURI); err != nil {
			return nil, err
		} else {
			req.URL = u
		}
		req.Timestamp = dataRequest.Timestamp()
		if dataRequest.Flags() & DataRequestFlagFinish > 0 {
			req.Close = true
		} else {
			req.Close = false
		}
		
		// Read in headers.
		tp := textproto.NewReader(bufio.NewReader(strings.NewReader(dataRequest.Header())))
		if mimeHeader, err := tp.ReadMIMEHeader(); err != nil && err != io.EOF {
			return nil, err
		} else {
			req.Header = Header(mimeHeader)
		}
		
		// TODO: Handle upload requests
		
		return req, nil
		
	/* Data Response */
	case IsDataResponse:
		var dataResponse DataResponse
		if d, err := data.DataResponse(); err != nil {
			return nil, err
		} else {
			dataResponse = d
		}
		
		//fmt.Println(dataResponse)
		//fmt.Println("--DataResponse--")
		e := gotDataResponse{
			dataResponse.StreamID(),
			dataResponse.ResponseCode(),
			dataResponse.ResponseSubcode(),
		}
		
		return nil, e
		
	/* Data Push */
	case IsDataPush:
		var dataPush DataPush
		if d, err := data.DataPush(); err != nil {
			return nil, err
		} else {
			dataPush = d
		}
		
		//fmt.Println(dataPush)
		//fmt.Println("--DataPush--")
		e := gotDataPush{
			dataPush.StreamID(),
			dataPush.SourceStreamID(),
			dataPush.Resource(),
		}
		
		return nil, e
		
	/* Data Content */
	case IsDataContent:
		var dataContent DataContent
		if d, err := data.DataContent(); err != nil {
			return nil, err
		} else {
			dataContent = d
		}
		
		//fmt.Println(dataContent)
		//fmt.Println("--DataContent--")
		// TODO: Handle upload requests
		l := 15
		content := dataContent.Data()
		if len(content) < 15 {
			l = len(content)
		}
		e := gotDataContent{
			dataContent.StreamID(),
			make([]byte, l),
		}
		copy(e.first15, content[:l])
		
		return nil, e
		
	/* Ping Request */
	case IsPingRequest:
		var pingRequest PingRequest
		if p, err := data.PingRequest(); err != nil {
			return nil, err
		} else {
			pingRequest = p
		}
		
		//fmt.Println(pingRequest)
		//fmt.Println("--PingRequest--")
		req.Type = IsPingRequest
		req.StreamID = pingRequest.PingID()
		
		return req, nil
		
	/* Ping Reqsponse */
	case IsPingResponse:
		var pingResponse PingResponse
		if p, err := data.PingResponse(); err != nil {
			return nil, err
		} else {
			pingResponse = p
		}
		
		//fmt.Println(pingResponse)
		//fmt.Println("--PingResponse--")
		req.Type = IsPingResponse
		req.StreamID = pingResponse.PingID()
		
		return req, nil
		
	/* Unknown packet type */
	default:
		return nil, fmt.Errorf("wp: Received unknown packet ID %d.", firstByte.PacketType())
	}
	
	return nil, errors.New("wp: not reached")
}

func (r *Request) wantsClose() bool {
	return hasToken(r.Header.get("Connection"), "close")
}
