wp
==

A reference implementation of the Web Protocol.

This library's interface is intended to be very similar to "net/http",
with only minor differences for features new to WP, such as data pushes.

Example server use:
```go
package main

import (
	"github.com/SlyMarbo/wp"
)

func Web(writer wp.ResponseWriter, reader *wp.Request) {
	wp.ServeFile(writer, reader, "." + reader.RequestURI)
}

func main() {
	
	// Register handler.
	wp.HandleFunc("/", Web)
	
  // This actually starts the listening.
  err := wp.ListenAndServe(":99", "cert.pem", "key.pem", nil)
  if err != nil {
    // handle error.
  }
}

```

Example client use:
```go
package main

import (
	"github.com/SlyMarbo/wp"
)

func main() {
	
	resp, err := wp.Get("wp://example.com/", nil)
	if err != nil {
		// handle error.
	}
	
	// use resp.
}

```

The Response structure is similar to net/http.Response:
```go
type Response struct {
	Status        string // e.g. "0/0"
	StatusCode    int    // e.g. 0
	StatusSubcode int    // e.g. 2
	Proto         string // e.g. "WP/1"
	ProtoNum      int    // e.g. 1

	// Header maps header keys to values.  If the response had multiple
	// headers with the same key, they will be concatenated, with comma
	// delimiters.  (Section 4.2 of RFC 2616 requires that multiple headers
	// be semantically equivalent to a comma-delimited sequence.) Values
	// duplicated by other fields in this struct (e.g., ContentLength) are
	// omitted from Header.
	//
	// Keys in the map are canonicalized (see CanonicalHeaderKey).
	Header Header

	// Body represents the response body.
	//
	// The WP Client and Transport guarantee that Body is always
	// non-nil, even on responses without a body or responses with
	// a zero-lengthed body.
	Body io.Reader

	// ContentLength records the length of the associated content.  The
	// value -1 indicates that the length is unknown.  Values >= 0
	// indicate that the given number of bytes may be read from Body.
	ContentLength int64

	// Close records whether the header directed that the connection be
	// closed after reading Body.
	Close bool

	// The Request that was sent to obtain this Response.
	// Request's Body is nil (having already been consumed).
	// This is only populated for Client requests.
	Request *Request
}
```
