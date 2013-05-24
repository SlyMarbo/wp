package wp

import (
	"net/http"
	"net/url"
)

// PushFile uses a server push to send the contents of
// the named file or directory directly to the client.
//
//		func ServeWP(w wp.ResponseWriter, r *http.Request) {
//			
//			wp.PushFile(w, r, "/", "./index.html")
//			
//      // ...
//		}
func PushFile(wrt ResponseWriter, r *http.Request, name, path string) error {
	url := new(url.URL)
	*url = *r.URL
	url.Path = name

	push, err := wrt.Push(url.String())
	if err != nil {
		return err
	}
	w := &httpPushWriter{push}
	http.ServeFile(w, r, path)
	push.Close()
	return nil
}
