package wp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"fmt"
)

type Connection interface {
	Ping() <-chan bool
	Push(string, Stream) (PushWriter, error)
	Request(*Request, Receiver) (Stream, error)
	WriteFrame(Frame)
	Version() uint8
}

type Stream interface {
	Connection() Connection
	Headers() Headers
	Run()
	State() *StreamState
	Stop()
	StreamID() uint32
	Write([]byte) (int, error)
	WriteHeaders()
	WriteResponse(int, int)
	Version() uint8
	Wait()
}

type Frame interface {
	Bytes() ([]byte, error)
	DecodeHeaders(*Decompressor) error
	EncodeHeaders(*Compressor) error
	Flags() byte
	Parse(*bufio.Reader) error
	StreamID() uint32
	String() string
	WriteTo(io.Writer) error
}

// Objects implementing the Receiver interface can be
// registered to a specific request on the Client.
//
// ReceiveData is passed the original request, the data
// to receive and a bool indicating whether this is the
// final batch of data. If the bool is set to true, the
// data may be empty, but should not be nil.
//
// ReceiveHeaders is passed the request and any sent
// text headers. This may be called multiple times.
//
// ReceiveRequest is used when server pushes are sent.
// The returned bool should inticate whether to accept
// the push. The provided Request will be that sent by
// the server with the push.
type Receiver interface {
	ReceiveData(request *Request, data []byte, final bool)
	ReceiveHeaders(request *Request, headers Headers)
	ReceiveRequest(request *Request) bool
	ReceiveResponse(request *Request, statusCode int, statusSubcode int)
}

func ReadFrame(reader *bufio.Reader) (frame Frame, err error) {
	start, err := reader.Peek(3)
	if err != nil {
		return nil, err
	}

	switch start[2] {
	case HEADERS:
		frame = new(HeadersFrame)
	case ERROR:
		frame = new(ErrorFrame)
	case REQUEST:
		frame = new(RequestFrame)
	case RESPONSE:
		frame = new(ResponseFrame)
	case PUSH:
		frame = new(PushFrame)
	case DATA:
		frame = new(DataFrame)
	case PING:
		frame = new(PingFrame)
	default:
		return nil, errors.New("Error: Failed to parse frame type.")
	}

	err = frame.Parse(reader)
	return frame, err
}

// Headers
type HeadersFrame struct {
	flags          byte
	streamID       uint32
	Headers        Headers
	rawHeaders     []byte
	headersDecoded bool
	headersEncoded bool
}

func (frame *HeadersFrame) Bytes() ([]byte, error) {
	if !frame.headersDecoded {
		return nil, errors.New("Error: Headers not decoded.")
	}

	headers := frame.rawHeaders
	length := 6 + len(headers)
	out := make([]byte, 8, length)

	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = HEADERS                    // Type
	out[3] = frame.flags                // Flags
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out = append(out, headers...)       // Headers

	return out, nil
}

func (frame *HeadersFrame) DecodeHeaders(decom *Decompressor) error {
	if frame.headersDecoded {
		return nil
	}

	headers := make(Headers)
	err := headers.Parse(frame.rawHeaders, decom)
	if err != nil {
		return err
	}

	frame.Headers = headers
	frame.headersDecoded = true
	return nil
}

func (frame *HeadersFrame) EncodeHeaders(com *Compressor) error {
	headers, err := frame.Headers.Compressed(com)
	if err != nil {
		return err
	}

	frame.rawHeaders = headers
	frame.headersEncoded = true
	return nil
}

func (frame *HeadersFrame) Flags() byte {
	return frame.flags
}

func (frame *HeadersFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 2)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data))
	if l < 6 {
		return &IncorrectDataLength{l, 6}
	}

	// Read in remaining data.
	data, err = Read(reader, l)
	if err != nil {
		return err
	}

	// Check it's a Headers.
	if data[0] != HEADERS {
		return &IncorrectFrame{int(data[0]), HEADERS}
	}

	frame.flags = data[1]
	frame.streamID = bytesToUint32(data[2:6])
	if l > 6 {
		frame.rawHeaders = data[6:]
	} else {
		frame.rawHeaders = []byte{}
	}
	frame.headersDecoded = false
	frame.headersEncoded = false

	return nil
}

func (frame *HeadersFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *HeadersFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.flags&FLAG_FIN != 0 {
		flags = "FLAG_FIN"
	}

	buf.WriteString("Headers frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 6+len(frame.rawHeaders)))
	buf.WriteString(fmt.Sprintf("Type:                 0\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Headers:              %v\n}\n", frame.Headers))

	return buf.String()
}

func (frame *HeadersFrame) WriteTo(writer io.Writer) error {
	if !frame.headersEncoded {
		return errors.New("Error: Headers not encoded.")
	}

	headers := frame.rawHeaders
	length := 6 + len(headers)
	out := make([]byte, 8)

	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = HEADERS                    // Type
	out[3] = frame.flags                // Flags
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID

	err := Write(writer, out)
	if err != nil {
		return err
	}

	err = Write(writer, headers)
	return err
}

// Error
type ErrorFrame struct {
	streamID uint32
	Status   uint8
}

func (frame *ErrorFrame) Bytes() ([]byte, error) {
	out := make([]byte, 7)

	out[0] = 0                          // Length
	out[1] = 7                          // Length
	out[2] = 1                          // Type
	out[3] = ERROR                      // Flags
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out[8] = frame.Status               // Status

	return out, nil
}

func (frame *ErrorFrame) DecodeHeaders(decom *Decompressor) error {
	return nil
}

func (frame *ErrorFrame) EncodeHeaders(com *Compressor) error {
	return nil
}

func (frame *ErrorFrame) Flags() byte {
	return 0
}

func (frame *ErrorFrame) Parse(reader *bufio.Reader) error {
	// Read in frame.
	data, err := Read(reader, 9)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data[:2]))
	if l != 7 {
		return &IncorrectDataLength{l, 7}
	}

	// Check it's an Error.
	if data[2] != ERROR {
		return &IncorrectFrame{int(data[2]), ERROR}
	}

	frame.streamID = bytesToUint32(data[4:8])
	frame.Status = data[8]

	return nil
}

func (frame *ErrorFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *ErrorFrame) String() string {
	buf := new(bytes.Buffer)

	buf.WriteString("Error frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               7\n\t"))
	buf.WriteString(fmt.Sprintf("Type:                 1\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                [NONE]\n\t"))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Status:               %d\n}\n", frame.Status))

	return buf.String()
}

func (frame *ErrorFrame) WriteTo(writer io.Writer) error {
	out, err := frame.Bytes()
	if err != nil {
		return err
	}

	err = Write(writer, out)
	return err
}

// Request
type RequestFrame struct {
	flags          uint8
	Priority       uint8
	streamID       uint32
	Headers        Headers
	rawHeaders     []byte
	headersDecoded bool
	headersEncoded bool
}

func (frame *RequestFrame) Bytes() ([]byte, error) {
	if !frame.headersDecoded {
		return nil, errors.New("Error: Headers not decoded.")
	}

	headers := frame.rawHeaders
	length := 6 + len(headers)
	out := make([]byte, 8, length)

	flags := frame.flags & 0x1f
	priority := (frame.Priority & 0x7) << 5
	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = REQUEST                    // Type
	out[3] = flags | priority           // Flags and Priority
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out = append(out, headers...)       // Headers

	return out, nil
}

func (frame *RequestFrame) DecodeHeaders(decom *Decompressor) error {
	if frame.headersDecoded {
		return nil
	}

	headers := make(Headers)
	err := headers.Parse(frame.rawHeaders, decom)
	if err != nil {
		return err
	}

	frame.Headers = headers
	frame.headersDecoded = true
	return nil
}

func (frame *RequestFrame) EncodeHeaders(com *Compressor) error {
	headers, err := frame.Headers.Compressed(com)
	if err != nil {
		return err
	}

	frame.rawHeaders = headers
	frame.headersEncoded = true
	return nil
}

func (frame *RequestFrame) Flags() byte {
	return frame.flags
}

func (frame *RequestFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 2)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data))
	if l < 6 {
		return &IncorrectDataLength{l, 6}
	}

	// Read in remaining data.
	data, err = Read(reader, l)
	if err != nil {
		return err
	}

	// Check it's a Request.
	if data[0] != REQUEST {
		return &IncorrectFrame{int(data[0]), REQUEST}
	}

	frame.flags = data[1] & 0x1f
	frame.Priority = (data[1] & 0xe0) >> 5
	frame.streamID = bytesToUint32(data[2:6])
	frame.rawHeaders = data[6:]
	frame.headersDecoded = false
	frame.headersEncoded = false

	return nil
}

func (frame *RequestFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *RequestFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.flags&FLAG_FIN != 0 {
		flags = "FLAG_FIN"
	}

	buf.WriteString("Request frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 6+len(frame.rawHeaders)))
	buf.WriteString(fmt.Sprintf("Type:                 2\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Priority:             %d\n\t", frame.Priority))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Headers:              %v\n}\n", frame.Headers))

	return buf.String()
}

func (frame *RequestFrame) WriteTo(writer io.Writer) error {
	if !frame.headersEncoded {
		return errors.New("Error: Headers not encoded.")
	}

	headers := frame.rawHeaders
	length := 6 + len(headers)
	out := make([]byte, 8)

	flags := frame.flags & 0x1f
	priority := (frame.Priority & 0x7) << 5
	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = REQUEST                    // Type
	out[3] = flags | priority           // Flags and Priority
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID

	err := Write(writer, out)
	if err != nil {
		return err
	}

	err = Write(writer, headers)
	return err
}

// Response
type ResponseFrame struct {
	flags           uint8
	Priority        uint8
	streamID        uint32
	ResponseCode    uint8
	ResponseSubcode uint8
	Headers         Headers
	rawHeaders      []byte
	headersDecoded  bool
	headersEncoded  bool
}

func (frame *ResponseFrame) Bytes() ([]byte, error) {
	if !frame.headersDecoded {
		return nil, errors.New("Error: Headers not decoded.")
	}

	headers := frame.rawHeaders
	length := 7 + len(headers)
	out := make([]byte, 9, length)

	flags := frame.flags & 0x1f
	priority := (frame.Priority & 0x7) << 5
	code := (frame.ResponseCode & 0x3) << 6
	subcode := frame.ResponseSubcode & 0x3f
	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = RESPONSE                   // Type
	out[3] = flags | priority           // Flags and Priority
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out[8] = code | subcode             // Response code
	out = append(out, headers...)       // Headers

	return out, nil
}

func (frame *ResponseFrame) DecodeHeaders(decom *Decompressor) error {
	if frame.headersDecoded {
		return nil
	}

	headers := make(Headers)
	err := headers.Parse(frame.rawHeaders, decom)
	if err != nil {
		return err
	}

	frame.Headers = headers
	frame.headersDecoded = true
	return nil
}

func (frame *ResponseFrame) EncodeHeaders(com *Compressor) error {
	headers, err := frame.Headers.Compressed(com)
	if err != nil {
		return err
	}

	frame.rawHeaders = headers
	frame.headersEncoded = true
	return nil
}

func (frame *ResponseFrame) Flags() byte {
	return frame.flags
}

func (frame *ResponseFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 2)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data))
	if l < 7 {
		return &IncorrectDataLength{l, 6}
	}

	// Read in remaining data.
	data, err = Read(reader, l)
	if err != nil {
		return err
	}

	// Check it's a Response.
	if data[0] != RESPONSE {
		return &IncorrectFrame{int(data[0]), RESPONSE}
	}

	frame.flags = data[1] & 0x1f
	frame.Priority = (data[1] & 0xe0) >> 5
	frame.streamID = bytesToUint32(data[2:6])
	frame.ResponseCode = (data[6] & 0xc0) >> 6
	frame.ResponseSubcode = data[6] & 0x3f
	frame.rawHeaders = data[7:]
	frame.headersDecoded = false
	frame.headersEncoded = false

	return nil
}

func (frame *ResponseFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *ResponseFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.flags&FLAG_FIN != 0 {
		flags = "FLAG_FIN"
	}

	buf.WriteString("Headers {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 6+len(frame.rawHeaders)))
	buf.WriteString(fmt.Sprintf("Type:                 0\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Priority:             %d\n\t", frame.Priority))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Response code:        %d/%d\n\t", frame.ResponseCode, frame.ResponseSubcode))
	buf.WriteString(fmt.Sprintf("Headers:              %v\n}\n", frame.Headers))

	return buf.String()
}

func (frame *ResponseFrame) WriteTo(writer io.Writer) error {
	if !frame.headersEncoded {
		return errors.New("Error: Headers not encoded.")
	}

	headers := frame.rawHeaders
	length := 7 + len(headers)
	out := make([]byte, 9)

	flags := frame.flags & 0x1f
	priority := (frame.Priority & 0x7) << 5
	code := (frame.ResponseCode & 0x3) << 6
	subcode := frame.ResponseSubcode & 0x3f
	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = RESPONSE                   // Type
	out[3] = flags | priority           // Flags and Priority
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out[8] = code | subcode             // Response code

	err := Write(writer, out)
	if err != nil {
		return err
	}

	err = Write(writer, headers)
	return err
}

// Push
type PushFrame struct {
	flags              byte
	streamID           uint32
	AssociatedStreamID uint32
	Headers            Headers
	rawHeaders         []byte
	headersDecoded     bool
	headersEncoded     bool
}

func (frame *PushFrame) Bytes() ([]byte, error) {
	if !frame.headersDecoded {
		return nil, errors.New("Error: Headers not decoded.")
	}

	headers := frame.rawHeaders
	length := 10 + len(headers)
	out := make([]byte, 12, length)

	out[0] = byte(length >> 8)                    // Length
	out[1] = byte(length)                         // Length
	out[2] = PUSH                                 // Type
	out[3] = frame.flags                          // Flags
	out[4] = byte(frame.streamID >> 24)           // Stream ID
	out[5] = byte(frame.streamID >> 16)           // Stream ID
	out[6] = byte(frame.streamID >> 8)            // Stream ID
	out[7] = byte(frame.streamID)                 // Stream ID
	out[8] = byte(frame.AssociatedStreamID >> 24) // Associated Stream ID
	out[9] = byte(frame.AssociatedStreamID >> 16) // Associated Stream ID
	out[10] = byte(frame.AssociatedStreamID >> 8) // Associated Stream ID
	out[11] = byte(frame.AssociatedStreamID)      // Associated Stream ID
	out = append(out, headers...)                 // Headers

	return out, nil
}

func (frame *PushFrame) DecodeHeaders(decom *Decompressor) error {
	if frame.headersDecoded {
		return nil
	}

	headers := make(Headers)
	err := headers.Parse(frame.rawHeaders, decom)
	if err != nil {
		return err
	}

	frame.Headers = headers
	frame.headersDecoded = true
	return nil
}

func (frame *PushFrame) EncodeHeaders(com *Compressor) error {
	headers, err := frame.Headers.Compressed(com)
	if err != nil {
		return err
	}

	frame.rawHeaders = headers
	frame.headersEncoded = true
	return nil
}

func (frame *PushFrame) Flags() byte {
	return frame.flags
}

func (frame *PushFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 2)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data))
	if l < 10 {
		return &IncorrectDataLength{l, 10}
	}

	// Read in remaining data.
	data, err = Read(reader, l)
	if err != nil {
		return err
	}

	// Check it's a Push.
	if data[0] != PUSH {
		return &IncorrectFrame{int(data[0]), PUSH}
	}

	frame.flags = data[1]
	frame.streamID = bytesToUint32(data[2:6])
	frame.AssociatedStreamID = bytesToUint32(data[6:10])
	frame.rawHeaders = data[10:]
	frame.headersDecoded = false
	frame.headersEncoded = false

	return nil
}

func (frame *PushFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *PushFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.flags&FLAG_FIN != 0 {
		flags = "FLAG_FIN"
	}

	buf.WriteString("Push frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 10+len(frame.rawHeaders)))
	buf.WriteString(fmt.Sprintf("Type:                 4\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Associated Stream ID: %d\n\t", frame.AssociatedStreamID))
	buf.WriteString(fmt.Sprintf("Headers:              %v\n}\n", frame.Headers))

	return buf.String()
}

func (frame *PushFrame) WriteTo(writer io.Writer) error {
	if !frame.headersEncoded {
		return errors.New("Error: Headers not encoded.")
	}

	headers := frame.rawHeaders
	length := 10 + len(headers)
	out := make([]byte, 12)

	out[0] = byte(length >> 8)                    // Length
	out[1] = byte(length)                         // Length
	out[2] = PUSH                                 // Type
	out[3] = frame.flags                          // Flags
	out[4] = byte(frame.streamID >> 24)           // Stream ID
	out[5] = byte(frame.streamID >> 16)           // Stream ID
	out[6] = byte(frame.streamID >> 8)            // Stream ID
	out[7] = byte(frame.streamID)                 // Stream ID
	out[8] = byte(frame.AssociatedStreamID >> 24) // Associated Stream ID
	out[9] = byte(frame.AssociatedStreamID >> 16) // Associated Stream ID
	out[10] = byte(frame.AssociatedStreamID >> 8) // Associated Stream ID
	out[11] = byte(frame.AssociatedStreamID)      // Associated Stream ID

	err := Write(writer, out)
	if err != nil {
		return err
	}

	err = Write(writer, headers)
	return err
}

// Data
type DataFrame struct {
	flags    byte
	streamID uint32
	Data     []byte
}

func (frame *DataFrame) Bytes() ([]byte, error) {

	length := 6 + len(frame.Data)
	out := make([]byte, 8, length)

	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = DATA                       // Type
	out[3] = frame.flags                // Flags
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID
	out = append(out, frame.Data...)    // Data

	return out, nil
}

func (frame *DataFrame) DecodeHeaders(decom *Decompressor) error {
	return nil
}

func (frame *DataFrame) EncodeHeaders(com *Compressor) error {
	return nil
}

func (frame *DataFrame) Flags() byte {
	return frame.flags
}

func (frame *DataFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 2)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data))
	if l < 6 {
		return &IncorrectDataLength{l, 6}
	}

	// Read in remaining data.
	data, err = Read(reader, l)
	if err != nil {
		return err
	}

	// Check it's a Data.
	if data[0] != DATA {
		return &IncorrectFrame{int(data[0]), DATA}
	}

	frame.flags = data[1]
	frame.streamID = bytesToUint32(data[2:6])
	frame.Data = data[6:]

	return nil
}

func (frame *DataFrame) StreamID() uint32 {
	return frame.streamID
}

func (frame *DataFrame) String() string {
	buf := new(bytes.Buffer)

	flags := ""
	if frame.flags&FLAG_FIN != 0 {
		flags += "FLAG_FIN "
	}
	if frame.flags&FLAG_READY != 0 {
		flags += "FLAG_READY "
	}
	if flags == "" {
		flags = "[NONE]"
	}

	buf.WriteString("Data frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 6+len(frame.Data)))
	buf.WriteString(fmt.Sprintf("Type:                 5\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.streamID))
	buf.WriteString(fmt.Sprintf("Data:                 %v\n}\n", frame.Data))

	return buf.String()
}

func (frame *DataFrame) WriteTo(writer io.Writer) error {

	length := 6 + len(frame.Data)
	out := make([]byte, 8)

	out[0] = byte(length >> 8)          // Length
	out[1] = byte(length)               // Length
	out[2] = DATA                       // Type
	out[3] = frame.flags                // Flags
	out[4] = byte(frame.streamID >> 24) // Stream ID
	out[5] = byte(frame.streamID >> 16) // Stream ID
	out[6] = byte(frame.streamID >> 8)  // Stream ID
	out[7] = byte(frame.streamID)       // Stream ID

	err := Write(writer, out)
	if err != nil {
		return err
	}

	err = Write(writer, frame.Data)
	return err
}

// Ping
type PingFrame struct {
	flags  byte
	PingID uint32
}

func (frame *PingFrame) Bytes() ([]byte, error) {

	out := make([]byte, 8)

	out[0] = 0                        // Length
	out[1] = 6                        // Length
	out[2] = PING                     // Type
	out[3] = frame.flags              // Flags
	out[4] = byte(frame.PingID >> 24) // Ping ID
	out[5] = byte(frame.PingID >> 16) // Ping ID
	out[6] = byte(frame.PingID >> 8)  // Ping ID
	out[7] = byte(frame.PingID)       // Ping ID

	return out, nil
}

func (frame *PingFrame) DecodeHeaders(decom *Decompressor) error {
	return nil
}

func (frame *PingFrame) EncodeHeaders(com *Compressor) error {
	return nil
}

func (frame *PingFrame) Flags() byte {
	return frame.flags
}

func (frame *PingFrame) Parse(reader *bufio.Reader) error {
	// Read in length.
	data, err := Read(reader, 8)
	if err != nil {
		return err
	}

	// Check length.
	l := int(bytesToUint16(data[:2]))
	if l < 6 {
		return &IncorrectDataLength{l, 6}
	}

	// Check it's a Ping.
	if data[2] != PING {
		return &IncorrectFrame{int(data[0]), PING}
	}

	frame.flags = data[3]
	frame.PingID = bytesToUint32(data[4:8])

	return nil
}

func (frame *PingFrame) StreamID() uint32 {
	return 0
}

func (frame *PingFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.flags&FLAG_FIN != 0 {
		flags = "FLAG_FIN"
	}

	buf.WriteString("Ping frame {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               6\n\t"))
	buf.WriteString(fmt.Sprintf("Type:                 6\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Ping ID:              %d\n}\n", frame.PingID))

	return buf.String()
}

func (frame *PingFrame) WriteTo(writer io.Writer) error {
	out := make([]byte, 8)

	out[0] = 0                        // Length
	out[1] = 6                        // Length
	out[2] = PING                     // Type
	out[3] = frame.flags              // Flags
	out[4] = byte(frame.PingID >> 24) // Ping ID
	out[5] = byte(frame.PingID >> 16) // Ping ID
	out[6] = byte(frame.PingID >> 8)  // Ping ID
	out[7] = byte(frame.PingID)       // Ping ID

	err := Write(writer, out)
	return err
}
