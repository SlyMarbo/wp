package wp

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Objects implementing the Handler interface can be
// registered to serve a particular path or subtree
// in the WP server.
//
// ServeWP should write reply headers and data to the ResponseWriter
// and then return.  Returning signals that the request is finished
// and that the WP server can move on to other requests on the
// connection.
type Handler interface {
	ServeWP(ResponseWriter, *Request)
}

type ResponseWriter interface {

	// Headers returns the header map that will be sent by WriteResponse
	// and WriteHeaders.
	Headers() Headers

	// Ping immediately returns a channel on which a single boolean will
	// sent when the ping completes, which can be used as some measure of
	// the network's current performance. The boolean will be true if
	// the ping was replied to, and false otherwise.
	Ping() <-chan bool

	// Push returns a PushWriter, which can be used immediately to send
	// server pushes, and takes a string giving the name for the
	// resource being pushed.
	Push(string) (PushWriter, error)

	// Write writes the data to the connection as part of an HTTP/WP
	// reply. If WriteHeader has not yet been called, Write calls
	// WriteResponse(wp.StatusSuccess, wp.StatusSuccess) before writing
	// the data. If the Header does not contain a Content-Type line,
	// Write adds a Content-Type set to the result of passing the
	// initial 512 bytes of written data to DetectContentType.
	Write([]byte) (int, error)

	// WriteHeaders is used to send new changes to the Header. This is
	// called implicitly by WriteResponse and Write, so it's rarely
	// necessary to call manually.
	WriteHeaders()

	// WriteResponse sends a SPDY response with the status code provided.
	// If WriteHeader is not called explicitly, the first call to Write
	// will Trigger an implicit WriteResponse(wp.StatusSuccess,
	// wp.StatusSuccess). Thus explicit calls to WriteResponse are mainly
	// used to send error codes.
	WriteResponse(int, int)
}

// PushWriter is used for server pushes. The methods provided by
// PushWriter are fairly limited compared to a ResponseWriter, but
// a ResponseWriter will always be available in situations where
// a PushWriter will be used.
type PushWriter interface {
	// Close is used to complete a server push. This closes the underlying
	// stream and signals to the recipient that the push is complete. The
	// equivalent action in a ResponseWriter is to return from the handler.
	// Any calls to Write after calling Close will have no effect.
	Close()

	// Headers returns the header map that will be sent with the push.
	Headers() Headers

	// Write writes the data to the connection as part of a SPDY server
	// push. If the Header does not contain a Content-Type line, Write
	// adds a Content-Type set to the result of passing the initial 512
	// bytes of written data to DetectContentType.
	Write([]byte) (int, error)

	// WriteHeaders is used to send new changes to the Header. This is
	// called implicitly by Write, so it's rarely necessary to call
	// manually.
	WriteHeaders()
}

// A Server defines parameters for running an SPDY server.
type Server struct {
	Addr         string        // TCP address to listen on, ":http" if empty
	Handler      Handler       // handler to invoke, spdy.DefaultServeMux if nil
	httpHandler  http.Handler  // handler to invoke if Handler and DefaultServeMux are nil/empty.
	ReadTimeout  time.Duration // maximum duration before timing out read of the request
	WriteTimeout time.Duration // maximum duration before timing out write of the response
	TLSConfig    *tls.Config   // optional TLS config, used by ListenAndServeTLS
}

// The HandlerFunc type is an adapter to allow the use of ordinary
// functions as WP handlers. If f is a function with the appropriate
// signature, HandlerFunc(f) is a Handler object that calls f.
type HandlerFunc func(ResponseWriter, *Request)

// ServeSPDY calls f(w, r).
func (f HandlerFunc) ServeWP(w ResponseWriter, r *Request) {
	f(w, r)
}

// Helper handlers.

// Error replies to the request with the specified error message and
// HTTP code.
func Error(w ResponseWriter, error string, code, subcode int) {
	w.Headers().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteResponse(code, subcode)
	fmt.Fprintln(w, error)
}

// NotFound replies to the request with a WP 2/2 Not Found error.
func NotFound(w ResponseWriter, r *Request) {
	Error(w, "2/2 Not Found", StatusClientError, StatusNotFound)
}

// NotFoundHandler returns a simple request handler that replies to
// each request with a ''2/2 Not Found'' reply.
func NotFoundHandler() Handler {
	return HandlerFunc(NotFound)
}

// StripPrefix returns a handler that serves WP requests by removing
// the given prefix from the request URL's Path and invoking the
// handler h. StripPrefix handles a request for a path that doesn't
// begin with prefix by replying with an WP 2/2 not found error.
func StripPrefix(prefix string, h Handler) Handler {
	if prefix == "" {
		return h
	}
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		if p := strings.TrimPrefix(r.URL.Path, prefix); len(p) < len(r.URL.Path) {
			r.URL.Path = p
			h.ServeWP(w, r)
		} else {
			NotFound(w, r)
		}
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
		// "http://www.google.com/redirect/",
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
			// no leading https://server
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
			trailing := strings.HasSuffix(urlStr, "/")
			urlStr = path.Clean(urlStr)
			if trailing && !strings.HasSuffix(urlStr, "/") {
				urlStr += "/"
			}
			urlStr += query
		}
	}

	w.Headers().Set("Location", urlStr)
	w.WriteResponse(code, subcode)

	// RFC2616 recommends that a short note "SHOULD" be included in the
	// response because older user agents may not understand 301/307.
	note := "<a href=\"" + htmlEscape(urlStr) + "\">" + http.StatusText(code) + "</a>.\n"
	fmt.Fprintln(w, note)
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

// Redirect to a fixed URL.
type redirectHandler struct {
	url     string
	code    int
	subcode int
}

func (rh *redirectHandler) ServeWP(w ResponseWriter, r *Request) {
	Redirect(w, r, rh.url, rh.code, rh.subcode)
}

// RedirectHandler returns a request handler that redirects each
// request it receives to the given URL, using the given status
// code.
func RedirectHandler(url string, code, subcode int) Handler {
	return &redirectHandler{url, code, subcode}
}

// ServeMux is a WP request multiplexer. It matches the URL of each
// incoming request against a list of registered patterns and calls
// the handler for the pattern that most closely matches the URL.
//
// Patterns name fixed, rooted paths, like "/favicon.ico", or rooted
// subtrees, like "/images/" (note the trailing slash). Longer patterns
// take precedence over shorter ones, so that if there are handlers
// registered for both "/images/" and "/images/thumbnails/", the latter
// handler will be called for paths beginning "/images/thumbnails/" and
// the former will receive requests for any other paths in the
// "/images/" subtree.
//
// Patterns may optionally begin with a host name, restricting matches
// to URLs on that host only. Host-specific paterns take precedence
// over general patterns, so that a handler might register for the two
// patterns "/codesearch" and "codesearch.google.com/" without also
// taking over requests for "https://www.google.com".
//
// ServeMux also takes care of sanitising the URL request path,
// redirecting any request containing . or .. elements to an
// equivalent .- and ..-free URL.
type ServeMux struct {
	sync.RWMutex
	m     map[string]muxEntry
	hosts bool // whether any patterns contain hostnames.
}

func (s *ServeMux) Nil() bool {
	return len(s.m) == 0
}

type muxEntry struct {
	explicit bool
	h        Handler
	pattern  string
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux {
	return &ServeMux{m: make(map[string]muxEntry)}
}

// DefaultServeMux is the default ServeMux used by Serve and ServeFunc.
var DefaultServeMux = NewServeMux()

// Does path match pattern?
func pathMatch(pattern, path string) bool {
	n := len(pattern)
	if n == 0 {
		// Should not happen.
		return false
	}
	if pattern[n-1] != '/' {
		return pattern == path
	}
	return len(path) >= n && path[:n] == pattern
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

// Find a handler on a handler map given a path string.
// The most-specific (longest) pattern wins.
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
// consulting r.Method, r.Host, and r.URL.Path. It always
// returns a non-nil handler. If the path is not in its
// canonical form, the handler will be an internally-
// generated handler that redirects to the canonical path.
//
// Handler also returns the registered pattern that matches
// the request or, in the case of internally-generated
// redirects, the pattern that will match after following
// the redirect.
//
// If there is no registered handler that applies to the
// request, Handler returns a ''page not found'' handler
// and an empty pattern.
func (mux *ServeMux) Handler(r *Request) (h Handler, pattern string) {
	if p := cleanPath(r.URL.Path); p != r.URL.Path {
		_, pattern = mux.handler(r.Host, p)
		return RedirectHandler(p, StatusRedirection, StatusMovedPermanently), pattern
	}

	return mux.handler(r.Host, r.URL.Path)
}

// handler is the main implementation of Handler.
// The path is known to be in canonical form.
func (mux *ServeMux) handler(host, path string) (h Handler, pattern string) {
	mux.RLock()
	defer mux.RUnlock()

	// Host-specific pattern takes precedence over generic ones.
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
	if r.RequestURI == "*" {
		w.Headers().Set("Connection", "close")
		w.WriteResponse(StatusClientError, StatusBadRequest)
		return
	}
	h, _ := mux.Handler(r)
	h.ServeWP(w, r)
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, Handle panics.
func (mux *ServeMux) Handle(pattern string, handler Handler) {
	mux.Lock()
	defer mux.Unlock()

	if pattern == "" {
		panic("invalid pattern " + pattern)
	}
	if handler == nil {
		panic("nil handler")
	}
	if mux.m[pattern].explicit {
		panic("multiple registrations for " + pattern)
	}

	mux.m[pattern] = muxEntry{
		explicit: true,
		h:        handler,
		pattern:  pattern,
	}

	if pattern[0] != '/' {
		mux.hosts = true
	}

	// Helpful behaviour:
	// If attern is /tree/, insert an implicit permanent redirect
	// for /tree. It can be overriden by an explicit registration.
	n := len(pattern)
	if n > 0 && pattern[n-1] == '/' && !mux.m[pattern[:n-1]].explicit {
		// If pattern contains a host name, strip it and use remaining
		// path for redirect.
		path := pattern
		if pattern[0] != '/' {
			// In pattern, at least the last character is a '/', so
			// strings.Index can't be -1.
			path = pattern[strings.Index(pattern, "/"):]
		}
		mux.m[pattern[:n-1]] = muxEntry{
			h:       RedirectHandler(path, StatusRedirection, StatusMovedPermanently),
			pattern: pattern,
		}
	}
}

// HandleFunc registers the handler function for the given pattern.
func (mux *ServeMux) HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	mux.Handle(pattern, HandlerFunc(handler))
}

// Handle registers the handler for the given pattern
// in the DefaultServeMux.
// The documentation for ServeMux explains how patterns are matched.
func Handle(pattern string, handler Handler) {
	DefaultServeMux.Handle(pattern, handler)
}

// HandleFunc registers the handler function for the given pattern
// in the DefaultServeMux.
// The documentation for ServeMux explains how patterns are matched.
func HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {
	DefaultServeMux.HandleFunc(pattern, handler)
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls Serve to handle requests on incoming TLS connections.
//
// Filenames containing a certificate and matching private key for
// the server must be provided. If the certificate is signed by a
// certificate authority, the certFile should be the concatenation
// of the server's certificate followed by the CA's certificate.
//
// If srv.Addr is blank, ":https" is used.
//
// A trivial example server is:
//
//      import (
//              "github.com/SlyMarbo/wp"
//              "log"
//              "net/http"
//      )
//
//      func httpHandler(w http.ResponseWriter, req *http.Request) {
//              w.Header().Set("Content-Type", "text/plain")
//              w.Write([]byte("This is an example server.\n"))
//      }
//
//      func wpHandler(w wp.ResponseWriter, req *wp.Request) {
//              w.Header().Set("Content-Type", "text/plain")
//              w.Write([]byte("This is an example server.\n"))
//      }
//
//      func main() {
//              http.HandleFunc("/", handler)
//              wp.HandleFunc("/", handler)
//              log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
//
// One can use generate_cert.go in crypto/tls to generate cert.pem and key.pem.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	npnStrings := NpnStrings()
	server := &http.Server{
		Addr: srv.Addr,
		TLSConfig: &tls.Config{
			NextProtos: npnStrings,
		},
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	for _, str := range npnStrings {
		switch str {
		case "wp/2":
			server.TLSNextProto[str] = func(s *http.Server, tlsConn *tls.Conn, handler http.Handler) {
				srv.httpHandler = handler
				acceptWPv2(srv, tlsConn, nil)
			}
		}
	}

	return server.ListenAndServeTLS(certFile, keyFile)
}

// ListenAndServeTLS listens on the TCP network address addr
// and then calls Serve with handler to handle requests on
// incoming connections.  Handler is typically nil, in which
// case the DefaultServeMux is used. Additionally, files
// containing a certificate and matching private key for the
// server must be provided. If the certificate is signed by
// a certificate authority, the certFile should be the
// concatenation of the server's certificate followed by the
// CA's certificate.
//
// A trivial example server is:
//
//      import (
//              "github.com/SlyMarbo/wp"
//              "log"
//              "net/http"
//      )
//
//      func httpHandler(w http.ResponseWriter, req *http.Request) {
//              w.Header().Set("Content-Type", "text/plain")
//              w.Write([]byte("This is an example server.\n"))
//      }
//
//      func wpHandler(w wp.ResponseWriter, req *wp.Request) {
//              w.Header().Set("Content-Type", "text/plain")
//              w.Write([]byte("This is an example server.\n"))
//      }
//
//      func main() {
//              http.HandleFunc("/", httpHandler)
//              wp.HandleFunc("/", wpHandler)
//              log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
//
// One can use generate_cert.go in crypto/tls to generate cert.pem and key.pem.
func ListenAndServeTLS(addr string, certFile string, keyFile string, handler Handler) error {
	server := &Server{Addr: addr, Handler: handler}
	return server.ListenAndServeTLS(certFile, keyFile)
}

// AddWPServer adds WP support to srv, using server to handle requests. This
// must be called before srv begins serving.
func AddWPServer(srv *http.Server, server *Server) {
	npnStrings := NpnStrings()
	if srv.TLSConfig == nil {
		srv.TLSConfig = new(tls.Config)
	}
	if srv.TLSConfig.NextProtos == nil {
		srv.TLSConfig.NextProtos = npnStrings
	} else {
		// Collect compatible alternative protocols.
		others := make([]string, 0, len(srv.TLSConfig.NextProtos))
		for _, other := range srv.TLSConfig.NextProtos {
			if !strings.Contains(other, "wp/") && !strings.Contains(other, "http/") {
				others = append(others, other)
			}
		}

		// Start with wp.
		srv.TLSConfig.NextProtos = make([]string, 0, len(others)+3)
		srv.TLSConfig.NextProtos = append(srv.TLSConfig.NextProtos, npnStrings[:len(npnStrings)-1]...)

		// Add the others.
		srv.TLSConfig.NextProtos = append(srv.TLSConfig.NextProtos, others...)
		srv.TLSConfig.NextProtos = append(srv.TLSConfig.NextProtos, "http/1.1")
	}
	if srv.TLSNextProto == nil {
		srv.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	}
	for _, str := range npnStrings {
		switch str {
		case "wp/2":
			srv.TLSNextProto[str] = func(s *http.Server, tlsConn *tls.Conn, handler http.Handler) {
				server.httpHandler = handler
				acceptWPv2(server, tlsConn, nil)
			}
		}
	}
}

// AddWP adds WP support to srv, using wp.DefaultServeMux to handle requests.
// This must be called before srv begins serving.
func AddWP(srv *http.Server) {
	server := &Server{Handler: DefaultServeMux}
	AddWPServer(srv, server)
}
