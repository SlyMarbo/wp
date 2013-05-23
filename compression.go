package wp

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"sync"
)

// Decompressor is used to decompress name/value header blocks.
// Decompressors retain their state, so a single Decompressor
// should be used for each direction of a particular connection.
type Decompressor struct {
	m   sync.Mutex
	in  *bytes.Buffer
	out io.ReadCloser
}

// Decompress uses zlib decompression to decompress the provided
// data, according to the SPDY specification of the given version.
func (d *Decompressor) Decompress(data []byte) (headers Headers, err error) {
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
		d.out, err = zlib.NewReaderDict(d.in, HeaderDictionary)
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

	headers = make(Headers)
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
type Compressor struct {
	m   sync.Mutex
	buf *bytes.Buffer
	w   *zlib.Writer
}

// Compress uses zlib compression to compress the provided
// data, according to the SPDY/3 specification.
func (c *Compressor) Compress(data []byte) ([]byte, error) {
	c.m.Lock()
	defer c.m.Unlock()

	var err error
	if c.buf == nil {
		c.buf = new(bytes.Buffer)

		c.w, err = zlib.NewWriterLevelDict(c.buf, zlib.BestCompression, HeaderDictionary)
		if err != nil {
			return nil, err
		}
	} else {
		c.buf.Reset()
	}

	_, err = c.w.Write(data)
	if err != nil {
		return nil, err
	}

	c.w.Flush()
	return c.buf.Bytes(), nil
}
