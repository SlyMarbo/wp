package wp

import (
	"io"
	"net/http"
	"net/url"
	"time"
)

// ServeContent replies to the request using the content in the
// provided ReadSeeker.  The main benefit of ServeContent over io.Copy
// is that it handles Range requests properly, sets the MIME type, and
// handles If-Modified-Since requests.
//
// If the response's Content-Type header is not set, ServeContent
// first tries to deduce the type from name's file extension and,
// if that fails, falls back to reading the first block of the content
// and passing it to DetectContentType.
// The name is otherwise unused; in particular it can be empty and is
// never sent in the response.
//
// If modtime is not the zero time, ServeContent includes it in a
// Last-Modified header in the response.  If the request includes an
// If-Modified-Since header, ServeContent uses modtime to decide
// whether the content needs to be sent at all.
//
// The content's Seek method must work: ServeContent uses
// a seek to the end of the content to determine its size.
//
// If the caller has set w's ETag header, ServeContent uses it to
// handle requests using If-Range and If-None-Match.
//
// Note that *os.File implements the io.ReadSeeker interface.
func ServeContent(wrt ResponseWriter, req *Request, name string, modtime time.Time, content io.ReadSeeker) {
	r := wpToHttpRequest(req)
	w := &_httpResponseWriter{wrt}
	http.ServeContent(w, r, name, modtime, content)
}

// ServeFile replies to the request with the contents of
// the named file or directory.
func ServeFile(wrt ResponseWriter, req *Request, name string) {
	r := wpToHttpRequest(req)
	w := &_httpResponseWriter{wrt}
	http.ServeFile(w, r, name)
}

// PushFile uses a server push to send the contents of
// the named file or directory directly to the client.
//
//		func ServeWP(w wp.ResponseWriter, r *wp.Request) {
//			
//			wp.PushFile(w, r, "/", "./index.html")
//			
//      // ...
//		}
func PushFile(wrt ResponseWriter, req *Request, name, path string) error {
	url := new(url.URL)
	*url = *req.URL
	url.Path = name

	push, err := wrt.Push(url.String())
	if err != nil {
		return err
	}
	r := wpToHttpRequest(req)
	w := &_httpPushWriter{push}
	http.ServeFile(w, r, path)
	push.Close()
	return nil
}
