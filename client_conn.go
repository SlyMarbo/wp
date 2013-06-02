package wp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
)

// init modifies http.DefaultClient to use a wp.Transport, enabling
// support for WP in functions like http.Get.
func init() {
	http.DefaultClient = &http.Client{Transport: new(Transport)}
}

// NewClientConn is used to create a WP connection, using the given
// net.Conn for the underlying connection, and the given Receiver to
// receive server pushes.
func NewClientConn(tcpConn net.Conn, push Receiver) (wpConn Conn, err error) {
	if tcpConn == nil {
		return nil, errors.New("Error: Connection initialised with nil net.conn.")
	}

	out := new(conn)
	out.remoteAddr = tcpConn.RemoteAddr().String()
	out.server = nil
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
	out.oddity = 1
	out.pushReceiver = push
	out.pushRequests = make(map[StreamID]*http.Request)
	out.stop = make(chan struct{})

	return out, nil
}
