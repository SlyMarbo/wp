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
	
	http.HandleFunc("/", ServeHTTP)

	// Use wp's ListenAndServe.
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

func Serve(w http.ResponseWriter, r *http.Request) {
	if wp.UsingWP(w) {
		// Using WP.
	} else {
		// Using HTTP(S).
	}
	http.ServeFile(w, r, "." + r.RequestURI)
}

func main() {
	
	// Register handler.
	http.HandleFunc("/", Serve)

	err := wp.ListenAndServeTLS("localhost:443", "cert.pem", "key.pem", nil)
	if err != nil {
		// handle error.
	}
}
```

The following examples use features specific to WP.

Just the handler is shown.

Use WP's pinging features to test the connection:
```go
package main

import (
	"github.com/SlyMarbo/wp"
	"net/http"
	"time"
)

func Serve(w http.ResponseWriter, r *http.Request) {
	// Ping returns a channel which will send an empty struct.
	ping, err := wp.PingClient(w)
	if err != nil {
		// Not using WP.
	}
	
	select {
	case response := <- ping:
		if response != nil {
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

import (
	"github.com/SlyMarbo/wp"
	"net/http"
)

func Serve(w http.ResponseWriter, r *http.Request) {
	// Push returns a separate http.ResponseWriter and an error.
	push, err := wp.Push("/example.js")
	if err != nil {
		// Not using WP.
	}
	http.ServeFile(push, r, "./content/example.js")
	
	// ...
}
```


Clients
-------

Here's a simple example that will fetch the requested page over HTTP, HTTPS, or WP, as necessary.
```go
package main

import (
	"fmt"
	"github.com/SlyMarbo/wp" // Simply import WP.
	"io/ioutil"
)

func main() {
	res, err := http.Get("https://example.com/") // http.Get (and .Post etc) can now use WP.
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
