package wp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// Response represents the response from a SPDY/HTTP request.
type Response struct {
	Status          string // e.g. "200 OK"
	StatusCode      int    // e.g. 200
	WpStatus        string // e.g. "0/1 Cached"
	WpStatusCode    int    // e.g. 0
	WpStatusSubcode int    // e.g. 1
	Proto           string // e.g. "HTTP/1.0"
	ProtoMajor      int    // e.g. 1
	ProtoMinor      int    // e.g. 0
	WpProto         int    // WP version. Where WP was not used, this will be -1.

	// SentOverWp indicates whether the request was served over WP.
	SentOverWp bool

	// Headers maps header keys to values.  If the response had multiple
	// headers with the same key, they will be concatenated, with comma
	// delimiters.  (Section 4.2 of RFC 2616 requires that multiple headers
	// be semantically equivalent to a comma-delimited sequence.) Values
	// duplicated by other fields in this struct (e.g., ContentLength) are
	// omitted from Header.
	//
	// Keys in the map are canonicalized (see CanonicalHeaderKey).
	Header http.Header

	// Body represents the response body.
	//
	// The http Client and Transport guarantee that Body is always
	// non-nil, even on responses without a body or responses with
	// a zero-lengthed body.
	//
	// The Body is automatically dechunked if the server replied
	// with a "chunked" Transfer-Encoding.
	Body io.ReadCloser

	// ContentLength records the length of the associated content.  The
	// value -1 indicates that the length is unknown.  Unless Request.Method
	// is "HEAD", values >= 0 indicate that the given number of bytes may
	// be read from Body.
	ContentLength int64

	// Contains transfer encodings from outer-most to inner-most. Value is
	// nil, means that "itentity" encoding is used. If SendOverWp is
	// true, then TransferEncoding will always be nil.
	TransferEncoding []string

	// Close records whether the header directed that the connection be
	// closed after reading Body. The value is advice for clients: neither
	// ReadResponse nor Response.Write ever closes a connection. If
	// SentOverWp is true, then Close will always be false.
	Close bool

	// Trailer maps trailer keys to values, in the same
	// format as the header.
	Trailer http.Header

	// The Request that was sent to obtain this Response.
	// Request's Body is nil (having already been consumed).
	// This is only populated for Client requests.
	Request *http.Request
}

// Cookies parses and returns the cookies set in the Set-Cookie headers.
func (r *Response) Cookies() []*http.Cookie {
	return wpToHttpResponse(r, r.Request).Cookies()
}

// Location returns the URL of the response's "Location" header,
// if present.  Relative redirects are resolved relative to
// the Response's Request.  ErrNoLocation is returned if no
// Location header is present.
func (r *Response) Location() (*url.URL, error) {
	lv := r.Header.Get("Location")
	if lv == "" {
		return nil, http.ErrNoLocation
	}
	if r.Request != nil && r.Request.URL != nil {
		return r.Request.URL.Parse(lv)
	}
	return url.Parse(lv)
}

// ProtoAtLeast returns whether the HTTP protocol used
// in the response is at least major.minor.
func (r *Response) ProtoAtLeast(major, minor int) bool {
	return r.ProtoMajor > major ||
		r.ProtoMajor == major && r.ProtoMinor >= minor
}

type response struct {
	StatusCode    int
	StatusSubcode int
	Header        http.Header
	Data          *bytes.Buffer
	Request       *http.Request
	Receiver      Receiver
}

func (r *response) ReceiveData(req *http.Request, data []byte, finished bool) {
	r.Data.Write(data)
	if r.Receiver != nil {
		r.Receiver.ReceiveData(req, data, finished)
	}
}

var statusRegex = regexp.MustCompile(`\A\s*(?P<code>\d+)`)

func (r *response) ReceiveHeaders(req *http.Request, headers http.Header) {
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	updateHeaders(r.Header, headers)
	if r.Receiver != nil {
		r.Receiver.ReceiveHeaders(req, headers)
	}
}

func (r *response) ReceiveRequest(req *http.Request) bool {
	if r.Receiver != nil {
		return r.Receiver.ReceiveRequest(req)
	}
	return false
}

func (r *response) ReceiveResponse(req *http.Request, code, subcode uint8) {
	r.StatusCode = int(code)
	r.StatusSubcode = int(subcode)
	if r.Receiver != nil {
		r.Receiver.ReceiveResponse(req, code, subcode)
	}
}

func (r *response) Response() *Response {
	if r.Data == nil {
		r.Data = new(bytes.Buffer)
	}
	code := wpToHttpResponseCode(r.StatusCode, r.StatusSubcode)
	out := new(Response)
	out.Status = fmt.Sprintf("%d %s", code, http.StatusText(code))
	out.StatusCode = code
	out.WpStatus = fmt.Sprintf("%d/%d %s", r.StatusCode, r.StatusSubcode, StatusText(r.StatusCode, r.StatusSubcode))
	out.WpStatusCode = r.StatusCode
	out.WpStatusSubcode = r.StatusSubcode
	out.Proto = "HTTP/1.1"
	out.ProtoMajor = 1
	out.ProtoMinor = 1
	out.WpProto = 2
	out.SentOverWp = true
	out.Header = r.Header
	out.Body = &readCloserBuffer{r.Data}
	out.ContentLength = int64(r.Data.Len())
	out.TransferEncoding = nil
	out.Close = false
	out.Trailer = make(http.Header)
	out.Request = r.Request
	return out
}

func wpToHttpResponse(res *Response, req *http.Request) *http.Response {
	out := new(http.Response)
	out.Status = res.Status
	out.StatusCode = res.StatusCode
	out.Proto = res.Proto
	out.ProtoMajor = res.ProtoMajor
	out.ProtoMinor = res.ProtoMinor
	out.Header = res.Header
	out.Body = res.Body
	out.ContentLength = res.ContentLength
	out.TransferEncoding = res.TransferEncoding
	out.Close = res.Close
	out.Trailer = res.Trailer
	out.Request = req
	return out
}

func httpToWpResponse(res *http.Response, req *http.Request) *Response {
	out := new(Response)
	out.Status = res.Status
	out.StatusCode = res.StatusCode
	out.Proto = res.Proto
	out.ProtoMajor = res.ProtoMajor
	out.ProtoMinor = res.ProtoMinor
	out.WpProto = -1
	out.SentOverWp = false
	out.Header = res.Header
	out.Body = res.Body
	out.ContentLength = res.ContentLength
	out.TransferEncoding = res.TransferEncoding
	out.Close = res.Close
	out.Trailer = res.Trailer
	out.Request = req
	return out
}
