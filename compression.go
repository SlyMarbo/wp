package wp

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Decompressor is used to decompress name/value header blocks.
// Decompressors retain their state, so a single Decompressor
// should be used for each direction of a particular connection.
type decompressor struct {
	m   sync.Mutex
	in  *bytes.Buffer
	out io.ReadCloser
}

// Decompress uses zlib decompression to decompress the provided
// data, according to the SPDY specification of the given version.
func (d *decompressor) Decompress(data []byte) (headers http.Header, err error) {
	d.m.Lock()
	defer d.m.Unlock()

	if d.in == nil {
		d.in = bytes.NewBuffer(data)
	} else {
		d.in.Reset()
		d.in.Write(data)
	}

	// Initialise the decompressor with the appropriate
	// dictionary, depending on SPDY version.
	if d.out == nil {
		d.out, err = zlib.NewReaderDict(d.in, headerDictionary)
		if err != nil {
			return nil, err
		}
	}

	chunk := make([]byte, 4)

	// Read in the number of name/value pairs.
	if _, err = d.out.Read(chunk); err != nil {
		return nil, err
	}
	numNameValuePairs := int(bytesToUint32(chunk))

	headers = make(http.Header)
	length := 0
	bounds := MAX_FRAME_SIZE - 12 // Maximum frame size minus maximum non-headers data (Push)
	for i := 0; i < numNameValuePairs; i++ {
		var nameLength, valueLength int

		// Get the name.
		if _, err = d.out.Read(chunk); err != nil {
			return nil, err
		}
		nameLength = int(bytesToUint32(chunk))

		if nameLength > bounds {
			return nil, errors.New("Error: Incorrect header name length.")
		}
		bounds -= nameLength

		name := make([]byte, nameLength)
		if _, err = d.out.Read(name); err != nil {
			panic(err)
			return nil, err
		}

		// Get the value.
		if _, err = d.out.Read(chunk); err != nil {
			panic(err)
			return nil, err
		}
		valueLength = int(bytesToUint32(chunk))

		if valueLength > bounds {
			return nil, errors.New("Error: Incorrect header values length.")
		}
		bounds -= valueLength

		values := make([]byte, valueLength)
		if _, err = d.out.Read(values); err != nil {
			return nil, err
		}

		// Count name and ': '.
		length += nameLength + 2

		// Split the value on null boundaries.
		for _, value := range bytes.Split(values, []byte{'\x00'}) {
			headers.Add(string(name), string(value))
			length += len(value) + 2 // count value and ', ' or '\n\r'.
		}
	}

	return headers, nil
}

// Compressor is used to compress name/value header blocks.
// Compressors retain their state, so a single Compressor
// should be used for each direction of a particular
// connection.
type compressor struct {
	m   sync.Mutex
	buf *bytes.Buffer
	w   *zlib.Writer
}

// Compress uses zlib compression to compress the provided
// data, according to the SPDY/3 specification.
func (c *compressor) Compress(h http.Header) ([]byte, error) {
	c.m.Lock()
	defer c.m.Unlock()

	var err error
	if c.buf == nil {
		c.buf = new(bytes.Buffer)

		c.w, err = zlib.NewWriterLevelDict(c.buf, zlib.BestCompression, headerDictionary)
		if err != nil {
			return nil, err
		}
	} else {
		c.buf.Reset()
	}

	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Connection")
	h.Del("Transfer-Encoding")

	length := 4
	num := len(h)
	lens := make(map[string]int)
	for name, values := range h {
		length += len(name) + 8
		lens[name] = len(values) - 1
		for _, value := range values {
			length += len(value)
			lens[name] += len(value)
		}
	}

	out := make([]byte, length)
	out[0] = byte(num >> 24)
	out[1] = byte(num >> 16)
	out[2] = byte(num >> 8)
	out[3] = byte(num)

	offset := 4

	for name, values := range h {
		nLen := len(name)
		out[offset+0] = byte(nLen >> 24)
		out[offset+1] = byte(nLen >> 16)
		out[offset+2] = byte(nLen >> 8)
		out[offset+3] = byte(nLen)
		offset += 4

		for i, b := range []byte(strings.ToLower(name)) {
			out[offset+i] = b
		}

		offset += nLen

		vLen := lens[name]
		out[offset+0] = byte(vLen >> 24)
		out[offset+1] = byte(vLen >> 16)
		out[offset+2] = byte(vLen >> 8)
		out[offset+3] = byte(vLen)
		offset += 4

		for n, value := range values {
			for i, b := range []byte(value) {
				out[offset+i] = b
			}
			offset += len(value)
			if n < len(values)-1 {
				out[offset] = '\x00'
				offset += 1
			}
		}
	}

	_, err = c.w.Write(out)
	if err != nil {
		return nil, err
	}

	c.w.Flush()
	return c.buf.Bytes(), nil
}

func (c *compressor) Close() error {
	if c.w == nil {
		return nil
	}
	err := c.w.Close()
	if err != nil {
		return err
	}
	c.w = nil
	return nil
}
