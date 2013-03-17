// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// WP file system request handler

package wp

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A Dir implements wp.FileSystem using the native file
// system restricted to a specific directory tree.
//
// An empty Dir is treated as ".".
type Dir string

func (d Dir) Open(name string) (File, error) {
	if filepath.Separator != '/' && strings.IndexRune(name, filepath.Separator) >= 0 ||
		strings.Contains(name, "\x00") {
			return nil, errors.New("wp: invalid character in file path")
	}
	dir := string(d)
	if dir == "" {
		dir = "."
	}
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(path.Clean("/"+name))))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// A FileSystem implements access to a collection of named files.
// The elements in a file path are separated by slash ('/', U+002F)
// characters, regardless of host operating system convention.
type FileSystem interface {
	Open(name string) (File, error)
}

// A File is returned by a FileSystem's Open method and can be
// served by the FileServer implementation.
type File interface {
	Close() error
	Stat() (os.FileInfo, error)
	Readdir(count int) ([]os.FileInfo, error)
	Read([]byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
}

func dirList(w ResponseWriter, f File) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "<pre>\n")
	for {
		dirs, err := f.Readdir(100)
		if err != nil || len(dirs) == 0 {
			break
		}
		for _, d := range dirs {
			name := d.Name()
			if d.IsDir() {
				name += "/"
			}
			// TODO htmlescape
			fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", name, name)
		}
	}
	fmt.Fprintf(w, "</pre>\n")
}

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
// The content's Seek method must work: ServeContent uses
// a seek to the end of the content to determine its size.
//
// Note that *os.File implements the io.ReadSeeker interface.
func ServeContent(w ResponseWriter, req *Request, name string, modtime time.Time, content io.ReadSeeker) {
	size, err := content.Seek(0, os.SEEK_END)
	if err != nil {
		Error(w, "seeker can't seek", StatusServerError, StatusServerError)
		return
	}
	_, err = content.Seek(0, os.SEEK_SET)
	if err != nil {
		Error(w, "seeker can't seek", StatusServerError, StatusServerError)
		return
	}
	serveContent(w, req, name, modtime, size, content)
}

// if name is empty, filename is unknown. (used for mime type, before sniffing)
// if modtime.IsZero(), modtime is unknown.
// content must be seeked to the beginning of the file.
func serveContent(w ResponseWriter, r *Request, name string, modtime time.Time, size int64, content io.ReadSeeker) {

	// If Content-Type isn't set, use the file's extension to find it.
	ctype := w.Header().Get("Content-Type")
	if ctype == "" {
		ctype = mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			// read a chunk to decide between utf-8 text and binary
			var buf [1024]byte
			n, _ := io.ReadFull(content, buf[:])
			b := buf[:n]
			ctype = DetectContentType(b)
			_, err := content.Seek(0, os.SEEK_SET) // rewind to output whole file
			if err != nil {
				Error(w, "seeker can't seek", StatusServerError, StatusServerError)
				return
			}
		}
		w.Header().Set("Content-Type", ctype)
	}

	w.WriteHeader(StatusSuccess, StatusSuccess)
	
	io.Copy(w, content)
}

// name is '/'-separated, not filepath.Separator.
func serveFile(w ResponseWriter, r *Request, fs FileSystem, name string, redirect bool) {
	const indexPage = "/index.html"

	f, err := fs.Open(name)
	if err != nil {
		// TODO expose actual error?
		fmt.Println("2/2: ", name)
		NotFound(w, r)
		return
	}
	defer f.Close()

	d, err1 := f.Stat()
	if err1 != nil {
		// TODO expose actual error?
		NotFound(w, r)
		return
	}

	if redirect {
		// redirect to canonical path: / at end of directory url
		// r.URL.Path always begins with /
		url := r.URL.Path
		if d.IsDir() {
			if url[len(url)-1] != '/' {
				localRedirect(w, r, path.Base(url)+"/")
				return
			}
		} else {
			if url[len(url)-1] == '/' {
				localRedirect(w, r, "../"+path.Base(url))
				return
			}
		}
	}

	// use contents of index.html for directory, if present
	if d.IsDir() {
		index := name + indexPage
		ff, err := fs.Open(index)
		if err == nil {
			defer ff.Close()
			dd, err := ff.Stat()
			if err == nil {
				name = index
				d = dd
				f = ff
			}
		}
	}

	// Still a directory? (we didn't find an index.html file)
	if d.IsDir() {
		dirList(w, f)
		return
	}

	// serverContent will check modification time
	serveContent(w, r, d.Name(), d.ModTime(), d.Size(), f)
}

// localRedirect gives a Moved Permanently response.
// It does not convert relative paths to absolute paths like Redirect does.
func localRedirect(w ResponseWriter, r *Request, newPath string) {
	if q := r.URL.RawQuery; q != "" {
		newPath += "?" + q
	}
	w.Header().Set("Location", newPath)
	w.WriteHeader(StatusRedirection, StatusMovedPermanently)
}

// ServeFile replies to the request with the contents of the named file or directory.
func ServeFile(w ResponseWriter, r *Request, name string) {
	dir, file := filepath.Split(name)
	serveFile(w, r, Dir(dir), file, false)
}

type fileHandler struct {
	root FileSystem
}

// FileServer returns a handler that serves WP requests
// with the contents of the file system rooted at root.
//
// To use the operating system's file system implementation,
// use wp.Dir:
//
//     wp.Handle("/", wp.FileServer(wp.Dir("/tmp")))
func FileServer(root FileSystem) Handler {
	return &fileHandler{root}
}

func (f *fileHandler) ServeWP(w ResponseWriter, r *Request) {
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	serveFile(w, r, f.root, path.Clean(upath), true)
}

// countingWriter counts how many bytes have been written to it.
type countingWriter int64

func (w *countingWriter) Write(p []byte) (n int, err error) {
	*w += countingWriter(len(p))
	return len(p), nil
}
