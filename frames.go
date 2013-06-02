package wp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"fmt"
)

func readFrame(reader *bufio.Reader) (frame Frame, err error) {
	start, err := reader.Peek(3)
	if err != nil {
		return nil, err
	}

	switch start[2] {
	case REQUEST:
		frame = new(requestFrame)
	case RESPONSE:
		frame = new(responseFrame)
	case DATA:
		frame = new(dataFrame)
	case ERROR:
		frame = new(errorFrame)
	case HEADERS:
		frame = new(headersFrame)
	case PUSH:
		frame = new(pushFrame)
	case PING:
		frame = new(pingFrame)
	default:
		return nil, errors.New("Error: Failed to parse frame type.")
	}

	_, err = frame.ReadFrom(reader)
	return frame, err
}

// Request
type requestFrame struct {
	Flags     Flags
	Priority  Priority
	StreamID  StreamID
	Header    http.Header
	rawHeader []byte
}

func (frame *requestFrame) Compress(com Compressor) error {
	if frame.rawHeader != nil {
		return nil
	}

	headers, err := com.Compress(frame.Header)
	if err != nil {
		return err
	}

	frame.rawHeader = headers
	return nil
}

func (frame *requestFrame) Decompress(decom Decompressor) error {
	if frame.Header != nil {
		return nil
	}

	headers, err := decom.Decompress(frame.rawHeader)
	if err != nil {
		return err
	}

	frame.Header = headers
	return nil
}

func (frame *requestFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Request.
	if data[2] != REQUEST {
		return 0, &incorrectFrame{int(data[2]), REQUEST}
	}

	// Check length.
	length := int(bytesToUint16(data[0:2]))
	if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	frame.Flags = Flags(data[3] & 0x1f)
	frame.Priority = Priority(data[3]&0xe0) >> 5
	frame.StreamID = StreamID(bytesToUint32(data[4:8]))

	if !frame.StreamID.Valid() {
		return 8, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 8, streamIdIsZero
	}

	if length > 0 {
		// Read in remaining data.
		frame.rawHeader, err = read(reader, length)
		if err != nil {
			return 8, err
		}
	} else {
		frame.rawHeader = []byte{}
	}

	return int64(length + 8), nil
}

func (frame *requestFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.Flags.FINISH() {
		flags = "FLAG_FINISH"
	}

	buf.WriteString("Request {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", len(frame.rawHeader)))
	buf.WriteString(fmt.Sprintf("Type:                 0\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Priority:             %d\n\t", frame.Priority))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Header:              %#v\n}\n", frame.Header))

	return buf.String()
}

func (frame *requestFrame) WriteTo(writer io.Writer) (int64, error) {
	if frame.rawHeader == nil {
		return 0, errors.New("Error: Header not encoded.")
	}
	if !frame.StreamID.Valid() {
		return 0, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 0, streamIdIsZero
	}

	headers := frame.rawHeader
	length := len(headers)
	out := make([]byte, 8)

	flags := byte(frame.Flags) & 0x1f
	priority := byte(frame.Priority&0x7) << 5
	out[0] = byte(length >> 8)   // Length
	out[1] = byte(length)        // Length
	out[2] = REQUEST             // Type
	out[3] = flags | priority    // Flags and Priority
	out[4] = frame.StreamID.b1() // Stream ID
	out[5] = frame.StreamID.b2() // Stream ID
	out[6] = frame.StreamID.b3() // Stream ID
	out[7] = frame.StreamID.b4() // Stream ID

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	err = write(writer, headers)
	if err != nil {
		return 8, err
	}

	return int64(length + 8), err
}

// Response
type responseFrame struct {
	Flags           Flags
	Priority        Priority
	StreamID        StreamID
	ResponseCode    uint8
	ResponseSubcode uint8
	Header          http.Header
	rawHeader       []byte
}

func (frame *responseFrame) Compress(com Compressor) error {
	if frame.rawHeader != nil {
		return nil
	}

	headers, err := com.Compress(frame.Header)
	if err != nil {
		return err
	}

	frame.rawHeader = headers
	return nil
}

func (frame *responseFrame) Decompress(decom Decompressor) error {
	if frame.Header != nil {
		return nil
	}

	headers, err := decom.Decompress(frame.rawHeader)
	if err != nil {
		return err
	}

	frame.Header = headers
	return nil
}

func (frame *responseFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Response.
	if data[2] != RESPONSE {
		return 8, &incorrectFrame{int(data[2]), RESPONSE}
	}

	// Check length.
	length := int(bytesToUint16(data))
	if length < 1 {
		return 8, &incorrectDataLength{length, 1}
	} else if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	// Read in remaining data.
	code, err := read(reader, 1)
	if err != nil {
		return 8, err
	}

	frame.Flags = Flags(data[3] & 0x1f)
	frame.Priority = Priority(data[3]&0xe0) >> 5
	frame.StreamID = StreamID(bytesToUint32(data[4:8]))
	frame.ResponseCode = (code[0] & 0xc0) >> 6
	frame.ResponseSubcode = code[0] & 0x3f

	if !frame.StreamID.Valid() {
		return 9, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 9, streamIdIsZero
	}

	if length > 1 {
		// Read in remaining data.
		frame.rawHeader, err = read(reader, length-1)
		if err != nil {
			return 9, err
		}
	} else {
		frame.rawHeader = []byte{}
	}

	return int64(length + 8), nil
}

func (frame *responseFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.Flags.FINISH() {
		flags = "FLAG_FINISH"
	}

	buf.WriteString("Response {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", len(frame.rawHeader)))
	buf.WriteString(fmt.Sprintf("Type:                 1\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Priority:             %d\n\t", frame.Priority))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Response code:        %d/%d\n\t", frame.ResponseCode, frame.ResponseSubcode))
	buf.WriteString(fmt.Sprintf("Header:              %#v\n}\n", frame.Header))

	return buf.String()
}

func (frame *responseFrame) WriteTo(writer io.Writer) (int64, error) {
	if frame.rawHeader == nil {
		return 0, errors.New("Error: Header not encoded.")
	}
	if !frame.StreamID.Valid() {
		return 0, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 0, streamIdIsZero
	}

	headers := frame.rawHeader
	length := 1 + len(headers)
	out := make([]byte, 9)

	flags := byte(frame.Flags) & 0x1f
	priority := byte(frame.Priority&0x7) << 5
	code := (frame.ResponseCode & 0x3) << 6
	subcode := frame.ResponseSubcode & 0x3f
	out[0] = byte(length >> 8)   // Length
	out[1] = byte(length)        // Length
	out[2] = RESPONSE            // Type
	out[3] = flags | priority    // Flags and Priority
	out[4] = frame.StreamID.b1() // Stream ID
	out[5] = frame.StreamID.b2() // Stream ID
	out[6] = frame.StreamID.b3() // Stream ID
	out[7] = frame.StreamID.b4() // Stream ID
	out[8] = code | subcode      // Response code

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	err = write(writer, headers)
	if err != nil {
		return 9, err
	}

	return int64(length + 8), err
}

// Data
type dataFrame struct {
	Flags    Flags
	StreamID StreamID
	Data     []byte
}

func (frame *dataFrame) Compress(com Compressor) error {
	return nil
}

func (frame *dataFrame) Decompress(decom Decompressor) error {
	return nil
}

func (frame *dataFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Data.
	if data[2] != DATA {
		return 8, &incorrectFrame{int(data[2]), DATA}
	}

	// Check length.
	length := int(bytesToUint16(data))
	if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	// Read in remaining data.
	payload, err := read(reader, length)
	if err != nil {
		return 8, err
	}

	frame.Flags = Flags(data[3])
	frame.StreamID = StreamID(bytesToUint32(data[4:8]))
	frame.Data = payload

	return int64(length + 8), nil
}

func (frame *dataFrame) String() string {
	buf := new(bytes.Buffer)

	flags := ""
	if frame.Flags.FINISH() {
		flags += " FLAG_FINISH"
	}
	if frame.Flags.READY() {
		flags += " FLAG_READY"
	}
	if flags == "" {
		flags = "[NONE]"
	} else {
		flags = flags[1:]
	}

	buf.WriteString("Data {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", len(frame.Data)))
	buf.WriteString(fmt.Sprintf("Type:                 2\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Data:                 %v\n}\n", frame.Data))

	return buf.String()
}

func (frame *dataFrame) WriteTo(writer io.Writer) (int64, error) {
	if !frame.StreamID.Valid() {
		return 0, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 0, streamIdIsZero
	}

	length := len(frame.Data)
	out := make([]byte, 8)

	out[0] = byte(length >> 8)   // Length
	out[1] = byte(length)        // Length
	out[2] = DATA                // Type
	out[3] = byte(frame.Flags)   // Flags
	out[4] = frame.StreamID.b1() // Stream ID
	out[5] = frame.StreamID.b2() // Stream ID
	out[6] = frame.StreamID.b3() // Stream ID
	out[7] = frame.StreamID.b4() // Stream ID

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	err = write(writer, frame.Data)
	if err != nil {
		return 8, err
	}

	return int64(length + 8), err
}

// Error
type errorFrame struct {
	StreamID StreamID
	Status   StatusCode
}

func (frame *errorFrame) Compress(com Compressor) error {
	return nil
}

func (frame *errorFrame) Decompress(decom Decompressor) error {
	return nil
}

func (frame *errorFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's an Error.
	if data[2] != ERROR {
		return 8, &incorrectFrame{int(data[2]), ERROR}
	}

	// Check length.
	length := int(bytesToUint16(data[0:2]))
	if length != 1 {
		return 8, &incorrectDataLength{length, 1}
	}

	code, err := read(reader, 1)
	if err != nil {
		return 8, err
	}

	frame.StreamID = StreamID(bytesToUint32(data[4:8]))
	frame.Status = StatusCode(code[0])

	return 9, nil
}

func (frame *errorFrame) String() string {
	buf := new(bytes.Buffer)

	buf.WriteString("Error {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               1\n\t"))
	buf.WriteString(fmt.Sprintf("Type:                 3\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                [NONE]\n\t"))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Status:               %d\n}\n", frame.Status))

	return buf.String()
}

func (frame *errorFrame) WriteTo(writer io.Writer) (int64, error) {
	out := make([]byte, 9)

	out[0] = 0                   // Length
	out[1] = 1                   // Length
	out[2] = DATA                // Type
	out[3] = 0                   // Flags
	out[4] = frame.StreamID.b1() // Stream ID
	out[5] = frame.StreamID.b2() // Stream ID
	out[6] = frame.StreamID.b3() // Stream ID
	out[7] = frame.StreamID.b4() // Stream ID
	out[8] = byte(frame.Status)

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	return 9, err
}

// Header
type headersFrame struct {
	Flags     Flags
	StreamID  StreamID
	Header    http.Header
	rawHeader []byte
}

func (frame *headersFrame) Compress(com Compressor) error {
	if frame.rawHeader != nil {
		return nil
	}

	headers, err := com.Compress(frame.Header)
	if err != nil {
		return err
	}

	frame.rawHeader = headers
	return nil
}

func (frame *headersFrame) Decompress(decom Decompressor) error {
	if frame.Header != nil {
		return nil
	}

	headers, err := decom.Decompress(frame.rawHeader)
	if err != nil {
		return err
	}

	frame.Header = headers
	return nil
}

func (frame *headersFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Header.
	if data[2] != HEADERS {
		return 8, &incorrectFrame{int(data[2]), HEADERS}
	}

	// Check length.
	length := int(bytesToUint16(data[0:2]))
	if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	frame.Flags = Flags(data[3])
	frame.StreamID = StreamID(bytesToUint32(data[4:8]))

	if !frame.StreamID.Valid() {
		return 8, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 8, streamIdIsZero
	}

	if length > 0 {
		// Read in remaining data.
		frame.rawHeader, err = read(reader, length)
		if err != nil {
			return 8, err
		}
	} else {
		frame.rawHeader = []byte{}
	}

	return int64(length + 8), nil
}

func (frame *headersFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.Flags.FINISH() {
		flags = "FLAG_FINISH"
	}

	buf.WriteString("Header {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", len(frame.rawHeader)))
	buf.WriteString(fmt.Sprintf("Type:                 4\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Header:              %#v\n}\n", frame.Header))

	return buf.String()
}

func (frame *headersFrame) WriteTo(writer io.Writer) (int64, error) {
	if frame.rawHeader == nil {
		return 0, errors.New("Error: Header not encoded.")
	}
	if !frame.StreamID.Valid() {
		return 0, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 0, streamIdIsZero
	}

	headers := frame.rawHeader
	length := len(headers)
	out := make([]byte, 8)

	out[0] = byte(length >> 8)   // Length
	out[1] = byte(length)        // Length
	out[2] = HEADERS             // Type
	out[3] = byte(frame.Flags)   // Flags
	out[4] = frame.StreamID.b1() // Stream ID
	out[5] = frame.StreamID.b2() // Stream ID
	out[6] = frame.StreamID.b3() // Stream ID
	out[7] = frame.StreamID.b4() // Stream ID

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	err = write(writer, headers)
	if err != nil {
		return 8, err
	}

	return int64(length + 8), err
}

// Push
type pushFrame struct {
	Flags              Flags
	Priority           Priority
	StreamID           StreamID
	AssociatedStreamID StreamID
	Header             http.Header
	rawHeader          []byte
}

func (frame *pushFrame) Compress(com Compressor) error {
	if frame.rawHeader != nil {
		return nil
	}

	headers, err := com.Compress(frame.Header)
	if err != nil {
		return err
	}

	frame.rawHeader = headers
	return nil
}

func (frame *pushFrame) Decompress(decom Decompressor) error {
	if frame.Header != nil {
		return nil
	}

	headers, err := decom.Decompress(frame.rawHeader)
	if err != nil {
		return err
	}

	frame.Header = headers
	return nil
}

func (frame *pushFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Push.
	if data[2] != PUSH {
		return 8, &incorrectFrame{int(data[2]), PUSH}
	}

	// Check length.
	length := int(bytesToUint16(data[0:2]))
	if length < 4 {
		return 8, &incorrectDataLength{length, 4}
	} else if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	// Read in remaining data.
	assoc, err := read(reader, 4)
	if err != nil {
		return 8, err
	}

	frame.Flags = Flags(data[3] & 0x1f)
	frame.Priority = Priority(data[3]&0xe0) >> 5
	frame.StreamID = StreamID(bytesToUint32(data[4:8]))
	frame.AssociatedStreamID = StreamID(bytesToUint32(assoc))

	if !frame.StreamID.Valid() {
		return 12, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 12, streamIdIsZero
	}
	if !frame.AssociatedStreamID.Valid() {
		return 12, streamIdTooLarge
	}

	if length > 0 {
		// Read in remaining data.
		frame.rawHeader, err = read(reader, length-4)
		if err != nil {
			return 12, err
		}
	} else {
		frame.rawHeader = []byte{}
	}

	return int64(length + 8), nil
}

func (frame *pushFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.Flags.FINISH() {
		flags = "FLAG_FINISH"
	}

	buf.WriteString("Push {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               %d\n\t", 4+len(frame.rawHeader)))
	buf.WriteString(fmt.Sprintf("Type:                 5\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Stream ID:            %d\n\t", frame.StreamID))
	buf.WriteString(fmt.Sprintf("Associated Stream ID: %d\n\t", frame.AssociatedStreamID))
	buf.WriteString(fmt.Sprintf("Header:              %#v\n}\n", frame.Header))

	return buf.String()
}

func (frame *pushFrame) WriteTo(writer io.Writer) (int64, error) {
	if frame.rawHeader == nil {
		return 0, errors.New("Error: Header not encoded.")
	}
	if !frame.StreamID.Valid() {
		return 0, streamIdTooLarge
	}
	if frame.StreamID.Zero() {
		return 0, streamIdIsZero
	}
	if !frame.AssociatedStreamID.Valid() {
		return 0, streamIdTooLarge
	}

	headers := frame.rawHeader
	length := 4 + len(headers)
	out := make([]byte, 12)

	flags := byte(frame.Flags) & 0x1f
	priority := byte(frame.Priority&0x7) << 5
	out[0] = byte(length >> 8)              // Length
	out[1] = byte(length)                   // Length
	out[2] = PUSH                           // Type
	out[3] = flags | priority               // Flags and Priority
	out[4] = frame.StreamID.b1()            // Stream ID
	out[5] = frame.StreamID.b2()            // Stream ID
	out[6] = frame.StreamID.b3()            // Stream ID
	out[7] = frame.StreamID.b4()            // Stream ID
	out[8] = frame.AssociatedStreamID.b1()  // Associated Stream ID
	out[9] = frame.AssociatedStreamID.b2()  // Associated Stream ID
	out[10] = frame.AssociatedStreamID.b3() // Associated Stream ID
	out[11] = frame.AssociatedStreamID.b4() // Associated Stream ID

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	err = write(writer, headers)
	if err != nil {
		return 12, err
	}

	return int64(length + 8), err
}

// Ping
type pingFrame struct {
	Flags  Flags
	PingID uint32
}

func (frame *pingFrame) Compress(com Compressor) error {
	return nil
}

func (frame *pingFrame) Decompress(decom Decompressor) error {
	return nil
}

func (frame *pingFrame) ReadFrom(reader io.Reader) (int64, error) {
	// Read in frame header.
	data, err := read(reader, 8)
	if err != nil {
		return 0, err
	}

	// Check it's a Ping.
	if data[2] != PING {
		return 8, &incorrectFrame{int(data[2]), PING}
	}

	// Check length.
	length := int(bytesToUint16(data[0:2]))
	if length != 0 {
		return 8, &incorrectDataLength{length, 0}
	} else if length > MAX_DATA_SIZE {
		return 8, frameTooLarge
	}

	frame.Flags = Flags(data[3])
	frame.PingID = bytesToUint32(data[4:8])

	return 8, nil
}

func (frame *pingFrame) String() string {
	buf := new(bytes.Buffer)

	flags := "[NONE]"
	if frame.Flags.FINISH() {
		flags = "FLAG_FINISH"
	}

	buf.WriteString("Ping {\n\t")
	buf.WriteString(fmt.Sprintf("Length:               0\n\t"))
	buf.WriteString(fmt.Sprintf("Type:                 6\n\t"))
	buf.WriteString(fmt.Sprintf("Flags:                %s\n\t", flags))
	buf.WriteString(fmt.Sprintf("Ping ID:              %d\n}\n", frame.PingID))

	return buf.String()
}

func (frame *pingFrame) WriteTo(writer io.Writer) (int64, error) {
	out := make([]byte, 8)

	out[0] = 0                        // Length
	out[1] = 6                        // Length
	out[2] = PING                     // Type
	out[3] = byte(frame.Flags)        // Flags
	out[4] = byte(frame.PingID >> 24) // Ping ID
	out[5] = byte(frame.PingID >> 16) // Ping ID
	out[6] = byte(frame.PingID >> 8)  // Ping ID
	out[7] = byte(frame.PingID)       // Ping ID

	err := write(writer, out)
	if err != nil {
		return 0, err
	}

	return 8, err
}
