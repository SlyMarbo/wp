package wp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/browser"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// ResponseHandler is used in the new client API.
// It's important to note that Handle may well 
// be called multiple times, particularly for
// large files. The function should check for
// Response.Close to identify the final call.
//
// Init is called once before each request is
// sent, allowing for initialisation.
// Init is provided with the name of the
// resource being requested, and a unique ID.
//
// Suggest is called when a resource Suggest is
// received. It provides the resource's name and
// timestamp, which should be checked against the
// cache. Suggest should return a boolean to
// indicate whether to issue a request. If the
// request is issued, Init will be called as
// normal, so initialisation is not required in
// Suggest.
//
// Handle should return a slice of resources to
// be requested. If no further resources need be
// requested, Handle should return nil.
//
// Close is used to indicate that all responses
// have been received and that the connection is
// about to be closed. Close should perform any
// cleanup that may be required. Close is called
// exactly once. If an error occurs during the
// connection, this error is passed to Close.
// Otherwise, the error will be nil.
type ResponseHandler interface {
	Init(resource string, id int) error
	Suggest(resource string, timestamp uint64) (bool, error)
	Handle(response *Response, id int) ([]string, error)
	Close(error) error
}

type DownloadHandler struct {
	filenames []string
	finished  []bool
	files     []*os.File
	fileroot  string
	follow    bool
	id        int
}

func (d *DownloadHandler) Init(s string, id int) error {
	if d.id != id {
		return errors.New("Error: DownloadHandler ID numbers are not synchronised.")
	}
	d.filenames = append(d.filenames, s)
	d.finished = append(d.finished, false)
	d.files = append(d.files, nil)
	d.id++
	return nil
}

func (d *DownloadHandler) Suggest(s string, t uint64) (bool, error) {
	return d.follow, nil
}

func (d *DownloadHandler) Handle(r *Response, id int) ([]string, error) {
	if id >= d.id {
		return nil, errors.New("Error: Unexpected stream ID.")
	}
	
	// Check we have the details we need.
	if len(d.filenames) <= id ||
			len(d.finished) <= id ||
			len(d.files) <= id {
		return nil, errors.New("Error: Unexpected response.")
	}
	
	// Make sure the request was successful.
	if r.StatusCode != 0 {
		return nil, errors.New("Error: Received status " + r.Status + ".")
	}
	
	// Open the output file if it's not already open.
	if d.files[id] == nil {
		// First, check that the directory exists.
		p := path.Dir(d.fileroot + d.filenames[id])
		if _, err := os.Stat(p); err != nil {
			// Create it.
			err = os.MkdirAll(p, os.ModeDir | os.ModePerm)
			if err != nil {
				return nil, errors.New("Error: Failed to create directory '" + p + "'.")
			}
		}
		
		if f, err := os.Create(d.fileroot + d.filenames[id]); err != nil {
			return nil, errors.New("Error: Failed to create '" + d.fileroot + d.filenames[id] +"'.")
		} else {
			d.files[id] = f
		}
	}
	
	// Write the response data to disk.
	io.Copy(d.files[id], r.Body)
	
	// This was the final packet; the file is now complete.
	if r.Close {
		defer d.files[id].Close()
		
		// If we're not just downloading one file.
		if d.follow {
			
			// Fetch the file's stats.
			s, err := d.files[id].Stat()
			if err != nil {
				fmt.Println(err)
				return nil, errors.New("Error: Failed to identify output file '" + d.files[id].Name() +
					"'s size.")
			}
		
			// Check whether file is small enough to scan for links.
			// This is currently 1MB.
			if s.Size() < (1 << 20) {
				data, err := ioutil.ReadFile(d.fileroot + d.filenames[id])
				if err != nil {
					return nil, errors.New("Error: Failed to open '" +
						d.fileroot + d.filenames[id] + "' for link scanning.")
				}
			
				// Fetch the links.
				links, err := browser.Links(string(data))
				if err != nil {
					return nil, errors.New("Error: Failed to parse '" +
						d.fileroot + d.filenames[id] + "' for link scanning.")
				}
			
				// Prepare the path to make the relative links absolute.
				dir := path.Dir(d.filenames[id])
				if !strings.HasSuffix(dir, "/") {
					dir += "/"
				}
				
				// Bundle the links
				requests := make([]string, 0, len(links))
				for _, link := range links {
					requests = append(requests, dir + link)
				}
				
				return requests, nil
			}
		}
	}
	
	return nil, nil
}

func (d *DownloadHandler) Close(err error) error {
	for id, closed := range d.finished {
		if !closed {
			if d.files[id] != nil {
				d.files[id].Truncate(0)
				d.files[id].Close()
			}
		}
	}
	return err
}

// DownloadTo returns a ResponseHandler which
// will write the response data to disk in
// the directory fileRoot, with the filename
// equal to the requested resource, or index.html
// if the requested resource is a directory.
//
// Note that DownloadTo will only write the
// response to disk if it receives a successful
// response.
func DownloadTo(fileRoot string) *DownloadHandler {
	if strings.HasSuffix(fileRoot, "/") {
		fileRoot = fileRoot[:len(fileRoot)-1]
	}
	return &DownloadHandler{
		filenames: make([]string, 0, 1),
		finished:  make([]bool, 0, 1),
		files:     make([]*os.File, 0, 1),
		fileroot:  fileRoot,
		follow:    false,
	}
}

// DownloadAllTo returns a ResponseHandler which
// will write the response data to disk in
// the directory fileRoot, with the filename
// equal to the requested resource, or index.html
// if the requested resource is a directory.
//
// DownloadAllTo reads the received files and then
// requests any further files linked to via HTML
// or CSS. This is intended to simulate the
// behaviour of a web browser in fetching just
// links required to render the page, rather than
// every valid link found.
//
// Note that DownloadAllTo will only write the
// response to disk if it receives a successful
// response.
func DownloadAllTo(fileRoot string) *DownloadHandler {
	if strings.HasSuffix(fileRoot, "/") {
		fileRoot = fileRoot[:len(fileRoot)-1]
	}
	return &DownloadHandler{
		filenames: make([]string, 0, 1),
		finished:  make([]bool, 0, 1),
		files:     make([]*os.File, 0, 1),
		fileroot:  fileRoot,
		follow:    true,
	}
}

// Fetch is part of the new client API. It is used
// to fetch a request and provide it in Response
// form to a ResponseHandler. If so instructed by
// the ResponseHandler, Fetch will utilise the
// connection to perform subsequent requests in a
// more sophisticated manner than Get.
// This is often used in conjunction with the
// helper functions DownloadTo and DownloadAllTo.
func Fetch(req string, config *tls.Config, handler ResponseHandler) error {
	return fetch(req, config, handler, true)
}

func FetchJust(req string, config *tls.Config, handler ResponseHandler) error {
	return fetch(req, config, handler, false)
}

func fetch(req string, config *tls.Config, handler ResponseHandler, follow bool) error {
	
	hosts := make(map[string]chan string)
	me := make(chan string)
	ids := make(chan int)
	
	// Parse the request.
	url, err := url.Parse(req)
	if err != nil {
		return handler.Close(err)
	} else if url.Path == "" {
		return handler.Close(errors.New("Error: URI must contain a request path."))
	}
	
	open := 1
	
	go func() {
		for i := 0; ; i++ {
			ids <- i
		}
	}()
	
	hosts[url.Host] = make(chan string)
	go fet(config, handler, follow, hosts[url.Host], me, ids)
	hosts[url.Host] <- url.String()
	
	for open > 0 {
		next := <- me
		if strings.HasPrefix(next, "Done") {
			open--
			continue
		}
		if strings.HasPrefix(next, "New") {
			open++
			continue
		}
		url, err = url.Parse(next)
		if err != nil {
			return handler.Close(err)
		}
		open++
		if host, ok := hosts[url.Host]; ok {
			host <- url.String()
		} else {
			hosts[url.Host] = make(chan string)
			go fet(config, handler, follow, hosts[url.Host], me, ids)
			hosts[url.Host] <- url.String()
		}
	}
	
	return nil
}

func fet(config *tls.Config, handler ResponseHandler, follow bool, next chan string, ctrl chan string, ids chan int) {
	
	n := <- next
	
	// Parse the request.
	url, err := url.Parse(n)
	if err != nil {
		log.Fatal(handler.Close(err))
	} else if url.Path == "" {
		log.Fatal(handler.Close(errors.New("Error: URI must contain a request path.")))
	}
	
	state := ClientSession{
		IdOdd:     1,
		Resources: make(map[int]string),
		StreamIDs: make(map[int]int),
	}
	
	id := <- ids
	open := 1
	done := make(map[string]struct{})
	
	// Initialise the handler.
	err = handler.Init(url.Path, id)
	if err != nil {
		log.Fatal(handler.Close(err))
	}
	
	// Open connection
	if (config == nil) {
		config = &tls.Config{}
	}
	conn, err := Connect(url, config)
	if err != nil {
		log.Fatal(handler.Close(err))
	}
	
	// Prepare request
	helloLen := HelloSize + len(url.Host)
	buffer := make([]byte, helloLen + DataRequestSize + len(url.Path))
	
	{
		hello := Hello(buffer)
		hello.SetPacketType(IsHello)
		hello.SetVersion(V1)
		hello.SetHostname(url.Host)
	
		request := DataRequest(buffer[helloLen:])
		request.SetPacketType(IsDataRequest)
		request.SetFlags(DataRequestFlagFinish)
		request.SetStreamID(1)
		request.SetResource(url.Path)
	}
	state.Resources[1] = url.Path
	state.StreamIDs[1] = id
	done[url.Path] = struct{}{}
	
	// Send request
	if _, err := conn.Write(buffer); err != nil {
		log.Fatal(handler.Close(err))
	}
	
	responses := make(map[int]*Response)
	
	responses[1] = &Response{
		Proto: "WP/1",
		ProtoNum: 1,
		Request: &Request{
			Type: IsDataRequest,
			StreamID: 1,
			URL: url,
			Protocol: 1,
			Close: false,
			Host: url.Host,
			RequestURI: url.Path,
		},
		Close: true,
	}
	
	// Receive response.
	data := NewDataStream(bufio.NewReader(conn))
	for open > 0 {
		firstByte, err := data.FirstByte()
		if err != nil {
			log.Fatal(handler.Close(err))
		}
	
		switch firstByte.PacketType() {
		
		case IsHello:
			if hello, err := data.Hello(); err != nil {
				fmt.Println(hello)
				log.Fatal(handler.Close(err))
			}
		
		case IsStreamError:
			if s, err := data.StreamError(); err != nil {
				log.Fatal(handler.Close(err))
			} else {
				fmt.Println(s)
				log.Fatal(handler.Close(fmt.Errorf("wp: Fetch() received error code %d on stream %d.",
					s.StatusCode(), s.StreamID())))
			}
		
		case IsDataRequest:
			if _, err := data.DataRequest(); err != nil {
				log.Fatal(handler.Close(err))
			}
		
		/* Data Response */
		case IsDataResponse:
			dataResponse, err := data.DataResponse()
			if err != nil {
				log.Fatal(handler.Close(err))
			}
		
			sid := dataResponse.StreamID()
			res := responses[sid]
			
			res.StatusCode = dataResponse.ResponseCode()
			res.StatusSubcode = dataResponse.ResponseSubcode()
			res.Status = fmt.Sprintf("%d/%d", res.StatusCode, res.StatusSubcode)
		
		case IsDataPush:
			dataPush, err := data.DataPush()
			if err != nil {
				log.Fatal(handler.Close(err))
			}
			
			if !follow {
				continue
			}
			
			link := dataPush.Resource()
			
			// Handle suggestions.
			if dataPush.Flags() & DataPushFlagSuggest != 0 {
				
				req, err := handler.Suggest(link, dataPush.Timestamp())
				if err != nil {
					log.Fatal(handler.Close(err))
				}
				if req {
					u, err := url.Parse(<- next)
					if err != nil {
						log.Fatal(handler.Close(err))
					}
					if u.Host != url.Host {
						ctrl <- u.String()
						continue
					}
					link = u.Path
					if _, ok := done[link]; ok {
						continue
					} else {
						done[link] = struct{}{}
					}
					ctrl <- "New"
					open++
						
					// Update state
					state.IdOdd += 2
					sid := int(state.IdOdd)
					url.Path = link
						
					responses[sid] = &Response{
						Proto: "WP/1",
						ProtoNum: 1,
						Request: &Request{
							Type: IsDataRequest,
							StreamID: sid,
							URL: url,
							Protocol: 1,
							Close: false,
							Host: url.Host,
							RequestURI: url.String(),
						},
						Close: true,
					}
					
					// Prepare request
					buffer := NewDataRequest(link, "")
					buffer.SetStreamID(uint16(sid))
					id = <- ids
					state.Resources[sid] = link
					state.StreamIDs[sid] = id
					err = handler.Init(link, id)
					if err != nil {
						log.Fatal(handler.Close(err))
					}
					
					// Send request
					if _, err := conn.Write(buffer); err != nil {
						log.Fatal(handler.Close(err))
					}
				}
				
				continue
			}
			
			u, err := url.Parse(<- next)
			if err != nil {
				log.Fatal(handler.Close(err))
			}
			if u.Host != url.Host {
				ctrl <- u.String()
				continue
			}
			
			done[link] = struct{}{}
			ctrl <- "New"
			open++
			
			sid := dataPush.StreamID()
			responses[sid] = &Response{
				Proto: "WP/1",
				ProtoNum: 1,
				Request: &Request{
					Type: IsDataRequest,
					StreamID: sid,
					URL: url,
					Protocol: 1,
					Close: false,
					Host: url.Host,
					RequestURI: url.String(),
				},
				Close: true,
				StatusCode: StatusSuccess,
				StatusSubcode: StatusSuccess,
				Status: fmt.Sprintf("%d/%d", StatusSuccess, StatusSuccess),
			}
			id = <- ids
			state.Resources[sid] = link
			state.StreamIDs[sid] = id
			err = handler.Init(link, id)
			if err != nil {
				log.Fatal(handler.Close(err))
			}
		
		/* Data Content */
		case IsDataContent:
			dataContent, err := data.DataContent()
			if err != nil {
				log.Fatal(handler.Close(err))
			}
			
			sid := dataContent.StreamID()
			
			if !follow && sid & 1 == 0 {
				continue
			}

			res := responses[sid]
			if res == nil {
				fmt.Println(dataContent)
			}
			res.Body = bytes.NewReader(dataContent.Data())
			res.Close = (dataContent.Flags() & DataContentFlagFinish) != 0
			res.Ready = (dataContent.Flags() & DataContentFlagReady) != 0
			
			links, err := handler.Handle(res, state.StreamIDs[sid])
			if err != nil {
				log.Fatal(err)
			}
			
			if res.Close {
				for _, link := range links {
					
					u, err := url.Parse(link)
					if err != nil {
						log.Fatal(handler.Close(err))
					}
					if u.Host != url.Host {
						ctrl <- u.String()
						continue
					}
					link = u.Path
					if _, ok := done[link]; ok {
						continue
					} else {
						done[link] = struct{}{}
					}
					ctrl <- "New"
					open++
						
					// Update state
					state.IdOdd += 2
					sid := int(state.IdOdd)
					url.Path = link
						
					responses[sid] = &Response{
						Proto: "WP/1",
						ProtoNum: 1,
						Request: &Request{
							Type: IsDataRequest,
							StreamID: sid,
							URL: url,
							Protocol: 1,
							Close: false,
							Host: url.Host,
							RequestURI: url.String(),
						},
						Close: true,
					}
					
					// Prepare request
					buffer := NewDataRequest(link, "")
					buffer.SetStreamID(uint16(sid))
					id = <- ids
					state.Resources[sid] = link
					state.StreamIDs[sid] = id
					err = handler.Init(link, id)
					if err != nil {
						log.Fatal(handler.Close(err))
					}
					
					// Send request
					if _, err := conn.Write(buffer); err != nil {
						log.Fatal(handler.Close(err))
					}
				}

				ctrl <- "Done"
				open--
				//fmt.Printf("Finished '%s' (%d).\n", state.Resources[sid], open)
			}
		
		case IsPingRequest:
			if _, err := data.PingRequest(); err != nil {
				log.Fatal(handler.Close(err))
			}
		
		case IsPingResponse:
			if _, err := data.PingResponse(); err != nil {
				log.Fatal(handler.Close(err))
			}
		}
	}
	
	handler.Close(nil)
}

func Ping(target net.Conn, id int) {
	var ID byte
	if id == 0 {
		ID = byte(rand.Intn(255) + 1)
	} else {
		ID = byte(id)
	}
	
	r := NewPingRequest(ID)
	target.Write([]byte(r))
}

func Connect(url *url.URL, config *tls.Config) (net.Conn, error) {
	
	// Parse URI.
	if url.Path == "" {
		return nil, errors.New("wp: URI must contain a request path.")
	} else if url.Host == "" {
		return nil, errors.New("wp: URI must contain a hostname.")
	}
	
	// Connect.
	conn, err := tls.Dial("tcp", url.Host, config)
	if err != nil {
		return nil, err
	}
	
	return conn, nil
}

func Get(target string, config *tls.Config) (*Response, error) {
	
	// Parse URI.
	var url *url.URL
	if u, err := url.Parse(target); err != nil {
		return nil, err
	} else if u.Path == "" {
		return nil, errors.New("URI must contain a request path.")
	} else {
		url = u
	}
	
	// Connect
	if config == nil {
		config = &tls.Config{}
	}
	conn, err := Connect(url, config)
	if err != nil {
		return nil, err
	}
	
	// Prepare request
	helloLen := HelloSize + len(url.Host)
	buffer := make([]byte, 0, helloLen + DataRequestSize + len(url.Path))
	
	{
		hello := Hello(buffer)
		hello.SetPacketType(IsHello)
		hello.SetVersion(V1)
		hello.SetHostname(url.Host)
	
		request := DataRequest(buffer[helloLen:])
		request.SetPacketType(IsDataRequest)
		request.SetFlags(DataRequestFlagFinish)
		request.SetStreamID(1)
		request.SetResource(url.Path)
	}
	
	// Send request
	if _, err := conn.Write(buffer); err != nil {
		return nil, err
	}
	
	out := Response{
		Proto: "WP/1",
		ProtoNum: 1,
		Request: &Request{
			Type: IsDataRequest,
			StreamID: 1,
			URL: url,
			Protocol: 1,
			Close: true,
			Host: url.Host,
			RequestURI: target,
		},
		Close: true,
	}
	
	// Receive response.
	data := NewDataStream(bufio.NewReader(conn))
	for {
		var firstByte FirstByte
		if f, err := data.FirstByte(); err != nil {
			return nil, err
		} else {
			firstByte = f
		}
	
		switch firstByte.PacketType() {
		
		case IsHello:
			if _, err := data.Hello(); err != nil {
				return nil, err
			}
		
		case IsStreamError:
			if s, err := data.StreamError(); err != nil {
				return nil, err
			} else {
				return nil, fmt.Errorf("wp: Get() received error code %d on stream %d.", s.StatusCode(),
					s.StreamID())
			}
		
		case IsDataRequest:
			if _, err := data.DataRequest(); err != nil {
				return nil, err
			}
		
		/* Data Response */
		case IsDataResponse:
			var dataResponse DataResponse
			if d, err := data.DataResponse(); err != nil {
				return nil, err
			} else {
				dataResponse = d
			}
		
			out.StatusCode = dataResponse.ResponseCode()
			out.StatusSubcode = dataResponse.ResponseSubcode()
			out.Status = fmt.Sprintf("%d/%d", out.StatusCode, out.StatusSubcode)
			//out.Header
		
		case IsDataPush:
			// Ignore.
			if _, err := data.DataPush(); err != nil {
				return nil, err
			}
		
		/* Data Content */
		case IsDataContent:
			var dataContent DataContent
			if d, err := data.DataContent(); err != nil {
				return nil, err
			} else {
				dataContent = d
			}
		
			out.Body = bytes.NewReader(dataContent.Data())
			return &out, nil
		
		case IsPingRequest:
			if _, err := data.PingRequest(); err != nil {
				return nil, err
			}
		
		case IsPingResponse:
			if _, err := data.PingResponse(); err != nil {
				return nil, err
			}
		}
	}
	
	return nil, errors.New("Not reached")
}
