wp
==

A reference implementation of the Web Protocol.

This library's interface is intended to be very similar to "net/http",
with only minor differences for features new to WP, such as data pushes.

Servers
-------

Adding WP support to an existing Go server doesn't take much work.

Modifying a simple example server like the following:
```go
package main

import (
	"net/http"
)

func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Hello, HTTP!"))
}

func main() {
	
	// Register handler.
	http.HandleFunc("/", ServeHTTP)

	err := http.ListenAndServeTLS("localhost:443", "cert.pem", "key.pem", nil)
	if err != nil {
		// handle error.
	}
}
```

Simply requires the following changes:
```go
package main

import (
	"github.com/SlyMarbo/wp" // Import WP.
	"net/http"
)

// This handler will now serve HTTP, HTTPS, and WP requests.
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Hello, HTTP!"))
}

func main() {
	
	// Register handler.
	http.HandleFunc("/", ServeHTTP)

	// Use wp's ListenAndServe.
	err := wp.ListenAndServeTLS("localhost:443", "cert.pem", "key.pem", nil)
	if err != nil {
		// handle error.
	}
}
```

WP now supports reuse of HTTP handlers, as demonstrated above. Although this allows you to use just one set of
handlers, it means there is no way to use the WP-specific capabilities provided by `wp.ResponseWriter`, such as
server pushes, or to know which protocol is being used.

Making full use of the Web Protocol simple requires adding an extra handler:
```go
package main

import (
	"github.com/SlyMarbo/wp"
	"net/http"
)

// This now only serves HTTP/HTTPS requests.
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Hello, HTTP!"))
}

// Add a WP handler.
func ServeWP(w wp.ResponseWriter, r *wp.Request) {
	w.Write([]byte("Hello, WP!"))
}

func main() {
	
	// Register handlers.
	http.HandleFunc("/", ServeHTTP)
	wp.HandleFunc("/", ServeWP)

	err := wp.ListenAndServeTLS("localhost:443", "cert.pem", "key.pem", nil)
	if err != nil {
		// handle error.
	}
}
```

A very simple file server for both WP and HTTPS:
```go
package main

import (
	"github.com/SlyMarbo/wp"
	"net/http"
)

func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "." + r.RequestURI)
}

func ServeWP(w wp.ResponseWriter, r *wp.Request) {
	wp.ServeFile(w, r, "." + r.RequestURI)
}

func main() {
	
	// Register handlers.
	http.HandleFunc("/", ServeHTTP)
	wp.HandleFunc("/", ServeWP)

	err := wp.ListenAndServeTLS("localhost:443", "cert.pem", "key.pem", nil)
	if err != nil {
		// handle error.
	}
}
```

The following examples use features specific to WP.

Just the WP handler is shown.

Use WP's pinging features to test the connection:
```go
package main

import (
	"github.com/SlyMarbo/wp"
	"time"
)

func ServeWP(w wp.ResponseWriter, r *wp.Request) {
	// Ping returns a channel which will send a bool.
	ping := w.Ping()
	
	select {
	case success := <- ping:
		if success {
			// Connection is fine.
		} else {
			// Something went wrong.
		}
		
	case <-time.After(timeout):
		// Ping took too long.
		
	}
	
	// ...
}
```

Sending a server push:
```go
package main

import "github.com/SlyMarbo/wp"

func ServeWP(w wp.ResponseWriter, r *wp.Request) {
	
	// Push a whole file automatically.
	wp.PushFile(w, r, otherFile)
	
	// or
	
	// Push returns a PushWriter (similar to a ResponseWriter) and an error.
	push, err := w.Push()
	if err != nil {
		// Handle the error.
	}
	push.Write([]byte("Some stuff."))   // Push data manually.
	
	// ...
}
```

Clients
-------

The basic client API seems to work well in general, but gets a redirect loop when requesting https://twitter.com/, so
I'm not happy with it. Since I can't see Twitter's servers' WP logs, I don't know what's wrong yet, but I'm working
hard at it.

Here's a simple example that will fetch the requested page over HTTP, HTTPS, or WP, as necessary.
```go
package main

import (
	"fmt"
	"github.com/SlyMarbo/wp"
	"io/ioutil"
)

func main() {
	res, err := wp.Get("https://example.com/")
	if err != nil {
		// handle the error.
	}
	
	bytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		// handle the error.
	}
	res.Body.Close()
	
	fmt.Printf("Received: %s\n", bytes)
}
```
