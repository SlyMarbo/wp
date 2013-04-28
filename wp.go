package wp

import (
	"bufio"
  "bytes"
	"encoding/hex"
  "fmt"
	"strings"
	"time"
)

// Control types.
const (
  IsHello = iota
	IsStreamError
  IsDataRequest
  IsDataResponse
  IsDataPush
	IsDataContent
  IsPingRequest
  IsPingResponse
)

// Flags
// Hello (4)
const (
  HelloFlagUTF16 = 1
)

// Stream error (4)
const (
	StreamErrorFlagFinish = 1
)

// Data request (4)
const (
  DataRequestFlagFinish = 1
  DataRequestFlagUpload = 2
)

// Data response (4)
const (
  DataResponseFlagFinish = 1
)

// Data push (4)
const (
	DataPushFlagSuggest = 1
)

// Data content (7)
const (
  DataContentFlagFinish    = 1
	DataContentFlagReady     = 2
)

// Hello constants
const (
  V1 = 1
)

// Stream error status codes
const (
  ProtocolError       = 0x1
  UnsupportedVersion  = 0x2
  InvalidStream       = 0x3
  RefusedStream       = 0x4
  StreamClosed        = 0x5
  StreamInUse         = 0x6
  StreamIDMissing     = 0x7
  InternalError       = 0x8
  FinishStream        = 0x9
)

// Stream finish status data
const (
	ErrorDataContinue       = 0
	ErrorDataCannotContinue = 1
	ErrorDataDoNotContinue  = 1
	ErrorDataWPv1           = 1
)

// Data request constants
const (
	RequestNotCached = 0
)

// Data response status types
const (
	StatusSuccess     = 0
	StatusRedirection = 1
	StatusClientError = 2
	StatusServerError = 3
)

// Data response status subtypes
// SUCCESS
const (
	//    Success = 0
	StatusCached  = 1
	StatusPartial = 2
)

// REDIRECTION
const (
	StatusMovedTemporarily = 0
	StatusMovedPermanently = 1
)

// CLIENT ERROR
const (
	StatusBadRequest = 0
	StatusForbidden  = 1
	StatusNotFound   = 2
	StatusRemoved    = 3
)

// SERVER ERROR
const (
	//    ServerError        = 0
	StatusServiceUnavailable = 1
	StatusServiceTimeout     = 2
)

// Data response constants
const (
	ResponseDoNotCache = 0
)

// Data content header constants
const ContentMaxFileSize = 0xffffffff

func upsize(b []byte, addition uint64) []byte {
	if addition == 0 {
		return b
	}
	
	nb := make([]byte, uint64(len(b)) + addition)
	copy(nb, b)
	
	return nb
}

type ServerSession struct {
  IdOdd     uint16 // Client ID
	IdEven    uint16 // Server ID
	Lang      string
  Host      string
	Useragent string
	Ref       string
  Resources map[int]string
}

type ClientSession struct {
  IdOdd     uint16
	IdEven    uint16
	Open      map[int]bool
  Resources map[int]string
	StreamIDs map[int]int
}

// Packet types.
type DataStream struct {
	*bufio.Reader
}
func NewDataStream(r *bufio.Reader) DataStream {
	return DataStream{r}
}
type FirstByte []byte
type Hello []byte
type StreamError []byte
type DataRequest []byte
type DataResponse []byte
type DataPush []byte
type DataContent []byte
type PingRequest []byte
type PingResponse []byte

// Methods

// First byte
func (reader *DataStream) FirstByte() (FirstByte, error) {
	var first FirstByte
	if b, err := reader.Peek(1); err != nil {
		return nil, err
	} else {
		first = FirstByte(b)
	}
	
	return first, nil
}

func (reader *DataStream) getFirstByte(c chan byte) {
	if b, err := reader.Peek(1); err != nil {
		return
	} else {
		c <- b[0]
	}
}

type TimedOutRequest byte
func (t TimedOutRequest) Error() string {
	return "wp: request timed out."
}

func (reader *DataStream) TimedFirstByte(d time.Duration) (FirstByte, error) {
	c := make(chan byte)
	go reader.getFirstByte(c)
	
	select {
	case b := <- c:
		return FirstByte([]byte{b,}), nil
	case <- time.After(d):
		return nil, TimedOutRequest(0)
	}
	
	panic("not reached.")
}

func (reader *DataStream) ReadAll() string {
	buf := make([]byte, reader.Buffered())
	reader.Read(buf)
	return string(buf)
}

func (packet FirstByte) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("First byte:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:  %d\n", packet.PacketType()))
	buffer.WriteString(fmt.Sprintf("Flags:        %d\n", packet.Flags()))

  return buffer.String()
}

func (packet FirstByte) PacketType() int {
  return int((packet[0] & 0xe0) >> 5)
}

func (packet FirstByte) Flags() int {
  return int(packet[0] & 0x1f)
}

func (packet FirstByte) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet FirstByte) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

// Hello
func NewHello(lang []byte, host, usera, ref, head string) Hello {
	out := Hello(make([]byte, HelloSize + len(lang) + len(host) + len(usera) + len(ref) + len(head)))
	out.SetPacketType(IsHello)
	out.SetLanguage(lang)
	out.SetHostname(host)
	out.SetUseragent(usera)
	out.SetReferrer(ref)
	out.SetHeader(head)
	return out
}

func (reader DataStream) Hello() (Hello, error) {
	buffer := make([]byte, HelloSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	// Fetch remaining content.
	start := len(buffer)
	hello := Hello(buffer)
	buffer = upsize(buffer,
		uint64(hello.LanguageSize()) +
		uint64(hello.HostnameSize()) +
		uint64(hello.UseragentSize()) +
		uint64(hello.ReferrerSize()) +
		uint64(hello.HeaderSize()))
	if _, err := reader.Read(buffer[start:]); err != nil {
		return nil, err
	}
	
	return Hello(buffer), nil
}

const HelloSize = 8
func (packet Hello) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Hello:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
	buffer.WriteString(fmt.Sprintf("Packet type:     %d\n", packet.PacketType()))
	buffer.WriteString(fmt.Sprintf("Flags:           %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("WP Version:      %d\n", packet.Version()))
  buffer.WriteString(fmt.Sprintf("Language size:   %d\n", packet.LanguageSize()))
  buffer.WriteString(fmt.Sprintf("Hostname size:   %d\n", packet.HostnameSize()))
  buffer.WriteString(fmt.Sprintf("User-agent size: %d\n", packet.UseragentSize()))
  buffer.WriteString(fmt.Sprintf("Referrer size:   %d\n", packet.ReferrerSize()))
	buffer.WriteString(fmt.Sprintf("Header size:     %d\n", packet.HeaderSize()))

  return buffer.String()
}

func (packet Hello) PacketType() int {
  return int((packet[0] & 0xe0) >> 5)
}

func (packet Hello) Flags() int {
  return int(packet[0] & 0x1f)
}

func (packet Hello) Version() int {
  return int(packet[1])
}

func (packet Hello) LanguageSize() int {
  return int(packet[2])
}

func (packet Hello) HostnameSize() int {
  return int(packet[3])
}

func (packet Hello) UseragentSize() int {
  return int(packet[4])
}

func (packet Hello) ReferrerSize() int {
  return int(packet[5])
}

func (packet Hello) HeaderSize() int {
	return (int(packet[6]) << 8) + int(packet[7])
}

func (packet Hello) Language() []byte {
	size := packet.LanguageSize()
	return packet[8 : 8 + size]
}

func (packet Hello) LanguageString() string {
	bytes := packet.Language()
	buffer := make([]string, len(bytes))
	for i, b := range bytes {
		buffer[i] = Language(int(b))
	}
	return strings.Join(buffer, ", ")
}

func (packet Hello) Hostname() string {
	size := packet.HostnameSize()
	start := 8 + packet.LanguageSize()
	return string(packet[start : start + size])
}

func (packet Hello) Useragent() string {
	size := packet.UseragentSize()
	start := 8 + packet.LanguageSize() +
		packet.HostnameSize()
	return string(packet[start : start + size])
}

func (packet Hello) Referrer() string {
	size := packet.ReferrerSize()
	start := 8 + packet.LanguageSize() +
		packet.HostnameSize() +
		packet.UseragentSize()
	return string(packet[start : start + size])
}

func (packet Hello) Header() string {
	size := packet.HeaderSize()
	start := 8 + packet.LanguageSize() +
		packet.HostnameSize() +
		packet.UseragentSize() +
		packet.ReferrerSize()
	return string(packet[start : start + size])
}

func (packet Hello) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet Hello) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet Hello) SetVersion(val byte) {
  packet[1] = val
}

func (packet Hello) SetLanguageSize(val byte) {
  packet[2] = val
}

func (packet Hello) SetHostnameSize(val byte) {
  packet[3] = val
}

func (packet Hello) SetUseragentSize(val byte) {
  packet[4] = val
}

func (packet Hello) SetReferrerSize(val byte) {
  packet[5] = val
}

func (packet Hello) SetHeaderSize(val uint16) {
	packet[6] = byte(val >> 8)
	packet[7] = byte(val)
}

func (packet Hello) SetLanguage(val []byte) {
	if len(val) == 0 {
		return
	}
	packet.SetLanguageSize(byte(len(val)))
	start := 8
	copy(packet[start:], val)
}

func (packet Hello) SetHostname(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetHostnameSize(byte(len(val)))
	start := 8 + packet.LanguageSize()
	copy(packet[start:], []byte(val))
}

func (packet Hello) SetUseragent(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetUseragentSize(byte(len(val)))
	start := 8 + int(packet.LanguageSize()) +
		int(packet.HostnameSize())
	copy(packet[start:], []byte(val))
}

func (packet Hello) SetReferrer(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetReferrerSize(byte(len(val)))
	start := 8 + int(packet.LanguageSize()) +
		int(packet.HostnameSize()) +
		int(packet.UseragentSize())
	copy(packet[start:], []byte(val))
}

func (packet Hello) SetHeader(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetHeaderSize(uint16(len(val)))
	start := 8 + int(packet.LanguageSize()) +
		int(packet.HostnameSize()) +
		int(packet.UseragentSize()) +
		int(packet.ReferrerSize())
	copy(packet[start:], []byte(val))
}

// Stream Error
func NewStreamError() StreamError {
	out := StreamError(make([]byte, StreamErrorSize))
	out.SetPacketType(IsStreamError)
	return out
}

func (reader DataStream) StreamError() (StreamError, error) {
	buffer := make([]byte, StreamErrorSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	return StreamError(buffer), nil
}

const StreamErrorSize = 6
func (packet StreamError) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Stream Error:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:    %d\n", packet.PacketType()))
  buffer.WriteString(fmt.Sprintf("Flags:          %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("Stream ID:      %d\n", packet.StreamID()))
  buffer.WriteString(fmt.Sprintf("Status code:    %d\n", packet.StatusCode()))
  buffer.WriteString(fmt.Sprintf("Status data:    %d\n", packet.StatusData()))

  return buffer.String()
}

func (packet StreamError) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet StreamError) Flags() byte {
  return packet[0] & 0x1f
}

func (packet StreamError) StreamID() int {
  return (int(packet[1]) << 8) + int(packet[2])
}

func (packet StreamError) StatusCode() int {
  return int(packet[3])
}

func (packet StreamError) StatusData() int {
  return (int(packet[4]) << 8) + int(packet[5])
}

func (packet StreamError) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet StreamError) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet StreamError) SetStreamID(val uint16) {
  packet[1] = byte(val >> 8)
  packet[2] = byte(val)
}

func (packet StreamError) SetStatusCode(val byte) {
  packet[3] = val
}

func (packet StreamError) SetStatusData(val uint16) {
  packet[4] = byte(val >> 8)
	packet[5] = byte(val)
}

// Data Request
func NewDataRequest(res, head string) DataRequest {
	out := DataRequest(make([]byte, DataRequestSize + len(res) + len(head)))
	out.SetPacketType(IsDataRequest)
	out.SetResource(res)
	out.SetHeader(head)
	return out
}

func (reader DataStream) DataRequest() (DataRequest, error) {
	buffer := make([]byte, DataRequestSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	// Fetch remaining content.
	start := len(buffer)
	dataRequest := DataRequest(buffer)
	buffer = upsize(buffer, uint64(dataRequest.ResourceSize()) +
			uint64(dataRequest.HeaderSize()))
	if _, err := reader.Read(buffer[start:]); err != nil {
		return nil, err
	}
	
	return DataRequest(buffer), nil
}

const DataRequestSize = 17
func (packet DataRequest) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Data request:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:    %d\n", packet.PacketType()))
  buffer.WriteString(fmt.Sprintf("Flags:          %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("Stream ID:      %d\n", packet.StreamID()))
  buffer.WriteString(fmt.Sprintf("Resource size:  %d\n", packet.ResourceSize()))
  buffer.WriteString(fmt.Sprintf("Header size:    %d\n", packet.HeaderSize()))
  buffer.WriteString(fmt.Sprintf("Timestamp:      %d\n", packet.Timestamp()))

  return buffer.String()
}

func (packet DataRequest) PacketType() int {
  return (int(packet[0]) & 0xe0) >> 5
}

func (packet DataRequest) Flags() int {
  return int(packet[0]) & 0x1f
}

func (packet DataRequest) StreamID() int {
  return (int(packet[1]) << 8) + int(packet[2])
}

func (packet DataRequest) ResourceSize() int {
  return (int(packet[3]) << 8) + int(packet[4])
}

func (packet DataRequest) HeaderSize() uint32 {
  return (uint32(packet[5]) << 24) + (uint32(packet[6]) << 16) + (uint32(packet[7]) << 8) +
    uint32(packet[8])
}

func (packet DataRequest) Timestamp() uint64 {
  return (uint64(packet[9]) << 56) + (uint64(packet[10]) << 48) + (uint64(packet[11]) << 40) +
		(uint64(packet[12]) << 32) + (uint64(packet[13]) << 24) + (uint64(packet[14]) << 16) +
		(uint64(packet[15]) << 8) + uint64(packet[16])
}

func (packet DataRequest) Resource() string {
	start := 17
	size := packet.ResourceSize()
	return string(packet[start : start + size])
}

func (packet DataRequest) Header() string {
	start := uint32(17 + packet.ResourceSize())
	size := packet.HeaderSize()
	return string(packet[start : start + size])
}

func (packet DataRequest) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet DataRequest) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet DataRequest) SetStreamID(val uint16) {
  packet[1] = byte(val >> 8)
  packet[2] = byte(val)
}

func (packet DataRequest) SetResourceSize(val uint16) {
  packet[3] = byte(val >> 8)
  packet[4] = byte(val)
}

func (packet DataRequest) SetHeaderSize(val uint32) {
  packet[5] = byte(val >> 24)
  packet[6] = byte(val >> 16)
  packet[7] = byte(val >> 8)
  packet[8] = byte(val)
}

func (packet DataRequest) SetTimestamp(val uint64) {
  packet[9] = byte(val >> 56)
  packet[10] = byte(val >> 48)
  packet[11] = byte(val >> 40)
  packet[12] = byte(val >> 32)
  packet[13] = byte(val >> 24)
  packet[14] = byte(val >> 16)
  packet[15] = byte(val >> 8)
  packet[16] = byte(val)
}

func (packet DataRequest) SetResource(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetResourceSize(uint16(len(val)))
	start := 17
	copy(packet[start:], []byte(val))
}

func (packet DataRequest) SetHeader(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetHeaderSize(uint32(len(val)))
	start := 17 + int(packet.ResourceSize())
	copy(packet[start:], []byte(val))
}

// Data Response
func NewDataResponse(head string) DataResponse {
	out := DataResponse(make([]byte, DataResponseSize + len(head)))
	out.SetPacketType(IsDataResponse)
	out.SetHeader(head)
	return out
}

func (reader DataStream) DataResponse() (DataResponse, error) {
	buffer := make([]byte, DataResponseSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	// Fetch remaining content.
	start := len(buffer)
	dataResponse := DataResponse(buffer)
	buffer = upsize(buffer, uint64(dataResponse.HeaderSize()))
	if _, err := reader.Read(buffer[start:]); err != nil {
		return nil, err
	}
	
	return DataResponse(buffer), nil
}

const DataResponseSize = 16
func (packet DataResponse) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Data response:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:      %d\n", packet.PacketType()))
  buffer.WriteString(fmt.Sprintf("Flags:            %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("Stream ID:        %d\n", packet.StreamID()))
	buffer.WriteString(fmt.Sprintf("Response code:    %d\n", packet.ResponseCode()))
	buffer.WriteString(fmt.Sprintf("Response subcode: %d\n", packet.ResponseSubcode()))
  buffer.WriteString(fmt.Sprintf("Header size:      %d\n", packet.HeaderSize()))
  buffer.WriteString(fmt.Sprintf("Cache:            %d\n", packet.Timestamp()))

  return buffer.String()
}

func (packet DataResponse) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet DataResponse) Flags() byte {
  return packet[0] & 0x1f
}

func (pkt *DataResponse) StreamID() int {
  packet := []byte(*pkt)
  return (int(packet[1]) << 8) + int(packet[2])
}

func (packet DataResponse) ResponseCode() int {
  return int((packet[3] & 0xc0) >> 6)
}

func (packet DataResponse) ResponseSubcode() int {
  return int(packet[3] & 0x3f)
}

func (packet DataResponse) HeaderSize() uint32 {
  return (uint32(packet[4]) << 24) + (uint32(packet[5]) << 16) + (uint32(packet[6]) << 8) +
    uint32(packet[7])
}

func (packet DataResponse) Timestamp() uint64 {
  return (uint64(packet[8]) << 56) + (uint64(packet[9]) << 48) + (uint64(packet[10]) << 40) +
		(uint64(packet[11]) << 32) + (uint64(packet[12]) << 24) + (uint64(packet[13]) << 16) +
		(uint64(packet[14]) << 8) + uint64(packet[15])
}

func (packet DataResponse) Header() string {
	start := uint32(16)
	size := packet.HeaderSize()
	return string(packet[start : start + size])
}

func (packet DataResponse) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet DataResponse) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet DataResponse) SetStreamID(val uint16) {
  packet[1] = byte(val >> 8)
  packet[2] = byte(val)
}

func (packet DataResponse) SetResponseCode(val byte) {
  packet[3] = (packet[3] & 0x3f) | ((val & 0x3) << 6)
}

func (packet DataResponse) SetResponseSubcode(val byte) {
  packet[3] = (packet[3] & 0xc0) | (val & 0x3f)
}

func (packet DataResponse) SetHeaderSize(val uint32) {
  packet[4] = byte(val >> 24)
  packet[5] = byte(val >> 16)
  packet[6] = byte(val >> 8)
  packet[7] = byte(val)
}

func (packet DataResponse) SetTimestamp(val uint64) {
  packet[8] = byte(val >> 56)
  packet[9] = byte(val >> 48)
  packet[10] = byte(val >> 40)
  packet[11] = byte(val >> 32)
  packet[12] = byte(val >> 24)
  packet[13] = byte(val >> 16)
  packet[14] = byte(val >> 8)
  packet[15] = byte(val)
}

func (packet DataResponse) SetHeader(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetHeaderSize(uint32(len(val)))
	start := 16
	copy(packet[start:], []byte(val))
}

// Data Push
func NewDataPush(res, head string) DataPush {
	out := DataPush(make([]byte, DataPushSize + len(res) + len(head)))
	out.SetPacketType(IsDataPush)
	out.SetResource(res)
	out.SetHeader(head)
	return out
}

func (reader DataStream) DataPush() (DataPush, error) {
	buffer := make([]byte, DataPushSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	// Fetch remaining content.
	start := len(buffer)
	dataPush := DataPush(buffer)
	buffer = upsize(buffer, uint64(dataPush.ResourceSize()) +
			uint64(dataPush.HeaderSize()))
	if _, err := reader.Read(buffer[start:]); err != nil {
		return nil, err
	}
	
	return DataPush(buffer), nil
}

const DataPushSize = 19
func (packet DataPush) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Data push:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:      %d\n", packet.PacketType()))
  buffer.WriteString(fmt.Sprintf("Flags:            %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("Stream ID:        %d\n", packet.StreamID()))
  buffer.WriteString(fmt.Sprintf("Source stream ID: %d\n", packet.SourceStreamID()))
	buffer.WriteString(fmt.Sprintf("Resource size:    %d\n", packet.ResourceSize()))
  buffer.WriteString(fmt.Sprintf("Header size:      %d\n", packet.HeaderSize()))
	buffer.WriteString(fmt.Sprintf("Cache timestamp:  %d\n", packet.Timestamp()))

  return buffer.String()
}

func (packet DataPush) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet DataPush) Flags() byte {
  return packet[0] & 0x1f
}

func (packet DataPush) StreamID() int {
  return (int(packet[1]) << 8) + int(packet[2])
}

func (packet DataPush) SourceStreamID() int {
  return (int(packet[3]) << 8) + int(packet[4])
}

func (packet DataPush) ResourceSize() int {
  return (int(packet[5]) << 8) + int(packet[6])
}

func (packet DataPush) HeaderSize() uint32 {
  return (uint32(packet[7]) << 24) + (uint32(packet[8]) << 16) + (uint32(packet[9]) << 8) +
    uint32(packet[10])
}

func (packet DataPush) Timestamp() uint64 {
	return (uint64(packet[11]) << 56) + (uint64(packet[12]) << 48) + (uint64(packet[13]) << 40) +
		(uint64(packet[14]) << 32) + (uint64(packet[15]) << 24) + (uint64(packet[16]) << 16) +
		(uint64(packet[17]) << 8) + uint64(packet[18])
}

func (packet DataPush) Resource() string {
	start := 19
	size := packet.ResourceSize()
	return string(packet[start : start + size])
}

func (packet DataPush) Header() string {
	start := uint32(19 + packet.ResourceSize())
	size := packet.HeaderSize()
	return string(packet[start : start + size])
}

func (packet DataPush) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet DataPush) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet DataPush) SetStreamID(val uint16) {
  packet[1] = byte(val >> 8)
  packet[2] = byte(val)
}

func (packet DataPush) SetSourceStreamID(val uint16) {
  packet[3] = byte(val >> 8)
  packet[4] = byte(val)
}

func (packet DataPush) SetResourceSize(val uint16) {
  packet[5] = byte(val >> 8)
  packet[6] = byte(val)
}

func (packet DataPush) SetHeaderSize(val uint32) {
  packet[7] = byte(val >> 24)
  packet[8] = byte(val >> 16)
  packet[9] = byte(val >> 8)
  packet[10] = byte(val)
}

func (packet DataPush) SetTimestamp(val uint64) {
  packet[11] = byte(val >> 56)
  packet[12] = byte(val >> 48)
  packet[13] = byte(val >> 40)
  packet[14] = byte(val >> 32)
  packet[15] = byte(val >> 24)
  packet[16] = byte(val >> 16)
  packet[17] = byte(val >> 8)
  packet[18] = byte(val)
}

func (packet DataPush) SetResource(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetResourceSize(uint16(len(val)))
	start := 19
	copy(packet[start:], []byte(val))
}

func (packet DataPush) SetHeader(val string) {
	if len(val) == 0 {
		return
	}
	packet.SetHeaderSize(uint32(len(val)))
	start := 19 + packet.ResourceSize()
	copy(packet[start:], []byte(val))
}

// Data Content
func NewDataContent(dat []byte) DataContent {
	out := DataContent(make([]byte, DataContentSize + len(dat)))
	out.SetPacketType(IsDataContent)
	out.SetData(dat)
	return out
}

func (reader DataStream) DataContent() (DataContent, error) {
	buffer := make([]byte, DataContentSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	// Fetch remaining content.
	start := len(buffer)
	dataContent := DataContent(buffer)
	buffer = upsize(buffer, uint64(dataContent.Size()))
	m := len(buffer)
	
	// Account for TCP sharding.
	for m > 0 {
		if n, err := reader.Read(buffer[start:]); err != nil {
			return nil, err
		} else if n != m {
			if n == 0 {
				break
			}
			m -= n
			start += n
			continue
		}
		break
	}
	
	return DataContent(buffer), nil
}

const DataContentSize = 7
func (packet DataContent) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Data content:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:      %d\n", packet.PacketType()))
  buffer.WriteString(fmt.Sprintf("Flags:            %d\n", packet.Flags()))
  buffer.WriteString(fmt.Sprintf("Stream ID:        %d\n", packet.StreamID()))
  buffer.WriteString(fmt.Sprintf("Size:             %d\n", packet.Size()))

  return buffer.String()
}

func (packet DataContent) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet DataContent) Flags() byte {
  return packet[0] & 0x1f
}

func (packet DataContent) StreamID() int {
  return (int(packet[1]) << 8) + int(packet[2])
}

func (packet DataContent) Size() uint32 {
  return (uint32(packet[3]) << 24) + (uint32(packet[4]) << 16) + (uint32(packet[5]) << 8) +
    uint32(packet[6])
}

func (packet DataContent) Data() []byte {
	start := uint32(7)
	size := packet.Size()
	return packet[start : start + size]
}

func (packet DataContent) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet DataContent) SetFlags(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

func (packet DataContent) SetStreamID(val uint16) {
  packet[1] = byte(val >> 8)
  packet[2] = byte(val)
}

func (packet DataContent) SetSize(val uint32) {
  packet[3] = byte(val >> 24)
  packet[4] = byte(val >> 16)
  packet[5] = byte(val >> 8)
  packet[6] = byte(val)
}

func (packet DataContent) SetData(val []byte) {
	if len(val) == 0 {
		return
	}
	packet.SetSize(uint32(len(val)))
	start := 7
	copy(packet[start:], val)
}

// Ping Request
func NewPingRequest(id byte) PingRequest {
	out := PingRequest(make([]byte, PingRequestSize))
	out.SetPacketType(IsPingRequest)
	out.SetPingID(id)
	return out
}

func (reader DataStream) PingRequest() (PingRequest, error) {
	buffer := make([]byte, PingRequestSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	return PingRequest(buffer), nil
}

const PingRequestSize = 1
func (packet PingRequest) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Ping request:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:  %d\n", packet.PacketType()))
	buffer.WriteString(fmt.Sprintf("Ping ID:      %d\n", packet.PingID()))

  return buffer.String()
}

func (packet PingRequest) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet PingRequest) PingID() int {
  return int(packet[0] & 0x1f)
}

func (packet PingRequest) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet PingRequest) SetPingID(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

// Ping Response
func NewPingResponse(id byte) PingResponse {
	out := PingResponse(make([]byte, PingResponseSize))
	out.SetPacketType(IsPingResponse)
	out.SetPingID(id)
	return out
}

func (reader DataStream) PingResponse() (PingResponse, error) {
	buffer := make([]byte, PingResponseSize)
	
	// Fetch packet header.
	if _, err := reader.Read(buffer); err != nil {
		return nil, err
	}
	
	return PingResponse(buffer), nil
}

const PingResponseSize = 1
func (packet PingResponse) String() string {
  var buffer bytes.Buffer

  buffer.WriteString("Ping response:\n")
	buffer.WriteString(hex.Dump([]byte(packet)))
  buffer.WriteString(fmt.Sprintf("Packet type:  %d\n", packet.PacketType()))
	buffer.WriteString(fmt.Sprintf("Ping ID:      %d\n", packet.PingID()))

  return buffer.String()
}

func (packet PingResponse) PacketType() byte {
  return (packet[0] & 0xe0) >> 5
}

func (packet PingResponse) PingID() int {
  return int(packet[0] & 0x1f)
}

func (packet PingResponse) SetPacketType(val byte) {
  packet[0] = (packet[0] & 0x1f) | ((val & 0x7) << 5)
}

func (packet PingResponse) SetPingID(val byte) {
  packet[0] = (packet[0] & 0xe0) | (val & 0x1f)
}

// Language options
var languageCodes = map[int]string{
	0x0:  "Afrikaans",                // af
	0x1:  "Luo",                      // ach
	0x2:  "Akan",                     // ak
	0x3:  "Amharic",                  // am
	0x4:  "Arabic",                   // ar
	0x5:  "Azerbaijani",              // az
	0x6:  "Belarusian",               // be
	0x7:  "Bemba",                    // bem
	0x8:  "Bulgarian",                // bg
	0x9:  "Bihari",                   // bh
	0xa:  "Bengali",                  // bn
	0xb:  "Breton",                   // br
	0xc:  "Bosnian",                  // bs
	0xd:  "Catalan",                  // ca
	0xe:  "Cherokee",                 // chr
	0xf:  "Kurdish",                  // ckb
	0x10: "Corsican",                 // co
	0x11: "Seychellois Creole",       // crs
	0x12: "Czech",                    // cs
	0x13: "Welsh",                    // cy
	0x14: "Danish",                   // da
	0x15: "German",                   // de
	0x16: "Ewe",                      // ee
	0x17: "Greek",                    // el
	0x18: "English",                  // en
	0x19: "English (United Kingdom)", // en-gb
	0x1a: "English (United States)",  // en-us
	//...
}

func Language(code int) string {
	return languageCodes[code]
}

func LanguageCode(str string) int {
	for i, s := range languageCodes {
		if s == str {
			return i
		}
	}
	return -1
}

