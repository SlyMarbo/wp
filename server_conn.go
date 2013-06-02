package wp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
)

// NewServerConn is used to create a WP connection, using the given
// net.Conn for the underlying connection, and the given http.Server to
// configure the request serving.
func NewServerConn(tcpConn net.Conn, server *http.Server) (wpConn Conn, err error) {
	if tcpConn == nil {
		return nil, errors.New("Error: Connection initialised with nil net.conn.")
	}
	if server == nil {
		return nil, errors.New("Error: Connection initialised with nil server.")
	}

	out := new(conn)
	out.remoteAddr = tcpConn.RemoteAddr().String()
	out.server = server
	out.conn = tcpConn
	out.buf = bufio.NewReader(tcpConn)
	if tlsConn, ok := tcpConn.(*tls.Conn); ok {
		out.tlsState = new(tls.ConnectionState)
		*out.tlsState = tlsConn.ConnectionState()
	}
	out.streams = make(map[StreamID]Stream)
	out.output = [8]chan Frame{}
	out.output[0] = make(chan Frame)
	out.output[1] = make(chan Frame)
	out.output[2] = make(chan Frame)
	out.output[3] = make(chan Frame)
	out.output[4] = make(chan Frame)
	out.output[5] = make(chan Frame)
	out.output[6] = make(chan Frame)
	out.output[7] = make(chan Frame)
	out.pings = make(map[uint32]chan<- Ping)
	out.nextPingID = 2
	out.compressor = new(compressor)
	out.decompressor = new(decompressor)
	out.lastPushStreamID = 0
	out.lastRequestStreamID = 0
	out.oddity = 0
	out.stop = make(chan struct{})

	return out, nil
}

// AddWP adds WP support to srv, and must be called before srv begins serving.
func AddWP(srv *http.Server) {
	npnStrings := npn()
	if len(npnStrings) <= 1 {
		return
	}
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
		srv.TLSConfig.NextProtos = make([]string, 0, len(others)+len(npnStrings))
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
				conn, err := NewServerConn(tlsConn, s)
				if err != nil {
					log.Println(err)
					return
				}
				conn.Run()
				conn = nil
				runtime.GC()
			}
		}
	}
}

// ErrNotWP indicates that a WP-specific feature was attempted
// with a ResponseWriter using a non-WP connection.
var ErrNotWP = errors.New("Error: Not a WP connection.")

// ErrNotConnected indicates that a WP-specific feature was
// attempted with a Client not connected to the given server.
var ErrNotConnected = errors.New("Error: Not connected to given server.")

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
//      func main() {
//              http.HandleFunc("/", httpHandler)
//              log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
//
// One can use generate_cert.go in crypto/tls to generate cert.pem and key.pem.
func ListenAndServeTLS(addr string, certFile string, keyFile string, handler http.Handler) error {
	npnStrings := npn()
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
		TLSConfig: &tls.Config{
			NextProtos: npnStrings,
		},
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	for _, str := range npnStrings {
		switch str {
		case "wp/2":
			server.TLSNextProto[str] = func(s *http.Server, tlsConn *tls.Conn, handler http.Handler) {
				conn, err := NewServerConn(tlsConn, s)
				if err != nil {
					log.Println(err)
					return
				}
				conn.Run()
				conn = nil
				runtime.GC()
			}
		}
	}

	return server.ListenAndServeTLS(certFile, keyFile)
}

// PingClient is used to send PINGs with WP servers.
// PingClient takes a ResponseWriter and returns a channel on
// which a wp.Ping will be sent when the PING response is
// received. If the channel is closed before a wp.Ping has
// been sent, this indicates that the PING was unsuccessful.
//
// If the underlying connection is using HTTP, and not WP,
// PingClient will return the ErrNotWP error.
//
// A simple example of sending a ping is:
//
//      import (
//              "github.com/SlyMarbo/wp"
//              "log"
//              "net/http"
//      )
//
//      func httpHandler(w http.ResponseWriter, req *http.Request) {
//              ping, err := wp.PingClient(w)
//              if err != nil {
//                      // Non-WP connection.
//              } else {
//                      resp, ok <- ping
//                      if ok {
//                              // Ping was successful.
//                      }
//              }
//              
//      }
//
//      func main() {
//              http.HandleFunc("/", httpHandler)
//              log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
func PingClient(w http.ResponseWriter) (<-chan Ping, error) {
	if stream, ok := w.(Stream); !ok {
		return nil, ErrNotWP
	} else {
		return stream.Conn().Ping()
	}
}

// PingServer is used to send PINGs with http.Clients using.
// WP. PingServer takes a ResponseWriter and returns a
// channel onwhich a wp.Ping will be sent when the PING
// response is received. If the channel is closed before a
// wp.Ping has been sent, this indicates that the PING was
// unsuccessful.
//
// If the underlying connection is using HTTP, and not WP,
// PingServer will return the ErrNotWP error.
//
// If an underlying connection has not been made to the given
// server, PingServer will return the ErrNotConnected error.
//
// A simple example of sending a ping is:
//
//      import (
//              "github.com/SlyMarbo/wp"
//              "net/http"
//      )
//
//      func main() {
//              resp, err := http.Get("https://example.com/")
//              
//              // ...
//              
//              ping, err := wp.PingServer(http.DefaultClient, "https://example.com")
//              if err != nil {
//                      // No WP connection.
//              } else {
//                      resp, ok <- ping
//                      if ok {
//                              // Ping was successful.
//                      }
//              }
//      }
func PingServer(c http.Client, server string) (<-chan Ping, error) {
	if transport, ok := c.Transport.(*Transport); !ok {
		return nil, ErrNotWP
	} else {
		u, err := url.Parse(server)
		if err != nil {
			return nil, err
		}
		// Make sure the URL host contains the port.
		if !strings.Contains(u.Host, ":") {
			switch u.Scheme {
			case "http":
				u.Host += ":80"

			case "https":
				u.Host += ":443"
			}
		}
		conn, ok := transport.wpConns[u.Host]
		if !ok || conn == nil {
			return nil, ErrNotConnected
		}
		return conn.Ping()
	}
}

// Push is used to send server pushes with WP servers.
// Push takes a ResponseWriter and the url of the resource
// being pushed, and returns a ResponseWriter to which the
// push should be written.
//
// If the underlying connection is using HTTP, and not WP,
// Push will return the ErrNotWP error.
//
// A simple example of pushing a file is:
//
//      import (
//              "github.com/SlyMarbo/wp"
//              "log"
//              "net/http"
//      )
//
//      func httpHandler(w http.ResponseWriter, req *http.Request) {
//              push, err := wp.Push(w, "/javascript.js")
//              if err != nil {
//                      // Non-WP connection.
//              } else {
//                      http.ServeFile(push, req, "./javascript.js") // Push the given file.
//              }
//              
//      }
//
//      func main() {
//              http.HandleFunc("/", httpHandler)
//              log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
//              err := wp.ListenAndServeTLS(":10443", "cert.pem", "key.pem", nil)
//              if err != nil {
//                      log.Fatal(err)
//              }
//      }
func Push(w http.ResponseWriter, url string) (http.ResponseWriter, error) {
	if stream, ok := w.(Stream); !ok {
		return nil, ErrNotWP
	} else {
		return stream.Conn().Push(url, stream)
	}
}

// WPversion returns the WP version being used in the underlying
// connection used by the given http.ResponseWriter. This is 0 for
// connections not using WP.
func WPversion(w http.ResponseWriter) uint16 {
	if stream, ok := w.(Stream); ok {
		switch stream.Conn().(type) {
		case *conn:
			return 2

		default:
			return 0
		}
	}
	return 0
}

// UsingWP indicates whether a given ResponseWriter is using WP.
func UsingWP(w http.ResponseWriter) bool {
	_, ok := w.(Stream)
	return ok
}
