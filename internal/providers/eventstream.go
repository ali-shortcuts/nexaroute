package providers

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
)

// AWS event stream decoding (M5-1b) for Bedrock ConverseStream
// responses, plus a translator that re-emits each event as an SSE
// frame carrying a {"eventType","payload"} JSON envelope — the shape
// canonical.DecodeBedrockStreamChunk consumes. Frame CRCs are
// verified; a corrupt stream fails loudly instead of decoding
// garbage into the client stream.
//
// Frame layout: total_length(4) headers_length(4) prelude_crc(4)
// headers payload message_crc(4). Header value types follow the
// AWS event stream spec.

// maxEventStreamFrameBytes bounds allocation from the peer-controlled total_length
// field. It leaves room for the 24 MiB EventStream payload limit plus headers,
// while preventing a malformed length from requesting gigabytes of memory.
const maxEventStreamFrameBytes uint32 = 32 << 20

type eventStreamMessage struct {
	EventType string
	Payload   []byte
}

func readEventStreamMessage(r io.Reader) (*eventStreamMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return nil, err // io.EOF / ErrUnexpectedEOF propagate
	}
	totalLength := binary.BigEndian.Uint32(prelude[0:4])
	headersLength := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	if crc32.ChecksumIEEE(prelude[0:8]) != preludeCRC {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch")
	}
	if totalLength < 16 || headersLength > totalLength-16 {
		return nil, fmt.Errorf("eventstream: invalid frame lengths (total=%d headers=%d)", totalLength, headersLength)
	}
	if totalLength > maxEventStreamFrameBytes {
		return nil, fmt.Errorf("eventstream: frame length %d exceeds safe limit %d", totalLength, maxEventStreamFrameBytes)
	}
	rest := make([]byte, totalLength-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("eventstream: truncated frame: %w", err)
	}
	messageCRC := binary.BigEndian.Uint32(rest[len(rest)-4:])
	full := append(append([]byte{}, prelude[:12]...), rest[:len(rest)-4]...)
	if crc32.ChecksumIEEE(full) != messageCRC {
		return nil, fmt.Errorf("eventstream: message CRC mismatch")
	}
	headers, err := parseEventStreamHeaders(rest[:headersLength])
	if err != nil {
		return nil, err
	}
	payload := rest[headersLength : len(rest)-4]
	if headers[":message-type"] == "exception" || headers[":message-type"] == "error" {
		excType := headers[":exception-type"]
		if excType == "" {
			excType = headers[":error-code"]
		}
		if excType == "" {
			excType = "UnknownException"
		}
		return &eventStreamMessage{EventType: excType, Payload: payload}, nil
	}
	return &eventStreamMessage{EventType: headers[":event-type"], Payload: payload}, nil
}

func parseEventStreamHeaders(b []byte) (map[string]string, error) {
	headers := map[string]string{}
	for len(b) > 0 {
		nameLen := int(b[0])
		b = b[1:]
		if len(b) < nameLen+1 {
			return nil, fmt.Errorf("eventstream: truncated header")
		}
		name := string(b[:nameLen])
		b = b[nameLen:]
		valueType := b[0]
		b = b[1:]
		var value string
		switch valueType {
		case 0, 1: // bool true/false: no payload bytes
			if valueType == 0 {
				value = "true"
			} else {
				value = "false"
			}
		case 2: // int8
			if len(b) < 1 {
				return nil, fmt.Errorf("eventstream: truncated int8 header")
			}
			value = fmt.Sprintf("%d", int8(b[0]))
			b = b[1:]
		case 3: // int16
			if len(b) < 2 {
				return nil, fmt.Errorf("eventstream: truncated int16 header")
			}
			value = fmt.Sprintf("%d", int16(binary.BigEndian.Uint16(b)))
			b = b[2:]
		case 4: // int32
			if len(b) < 4 {
				return nil, fmt.Errorf("eventstream: truncated int32 header")
			}
			value = fmt.Sprintf("%d", int32(binary.BigEndian.Uint32(b)))
			b = b[4:]
		case 5, 8: // int64 / timestamp
			if len(b) < 8 {
				return nil, fmt.Errorf("eventstream: truncated int64 header")
			}
			value = fmt.Sprintf("%d", int64(binary.BigEndian.Uint64(b)))
			b = b[8:]
		case 6, 7: // byte array / string: uint16 length-prefixed
			if len(b) < 2 {
				return nil, fmt.Errorf("eventstream: truncated bytes header")
			}
			n := int(binary.BigEndian.Uint16(b))
			b = b[2:]
			if len(b) < n {
				return nil, fmt.Errorf("eventstream: truncated bytes header value")
			}
			value = string(b[:n])
			b = b[n:]
		case 9: // UUID
			if len(b) < 16 {
				return nil, fmt.Errorf("eventstream: truncated uuid header")
			}
			value = fmt.Sprintf("%x", b[:16])
			b = b[16:]
		default:
			return nil, fmt.Errorf("eventstream: unknown header value type %d", valueType)
		}
		headers[name] = value
	}
	return headers, nil
}

// eventStreamSSEReader adapts an event stream body into SSE frames.
type eventStreamSSEReader struct {
	src     io.ReadCloser
	br      *bufio.Reader
	pending bytes.Buffer
	err     error // sticky terminal error (after pending drains)
	done    bool
}

func newEventStreamSSEReader(src io.ReadCloser) *eventStreamSSEReader {
	return &eventStreamSSEReader{src: src, br: bufio.NewReader(src)}
}

func (r *eventStreamSSEReader) fill() {
	if r.done {
		return
	}
	msg, err := readEventStreamMessage(r.br)
	if err != nil {
		r.done = true
		if err != io.EOF {
			r.err = err
		}
		return
	}
	// Validate and compact the payload before emitting SSE. The upstream body is
	// untrusted network input; writing it verbatim would allow embedded newlines
	// and trailing bytes to inject additional SSE events into the client stream.
	payload := msg.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if !json.Valid(payload) {
		r.done = true
		r.err = fmt.Errorf("eventstream: invalid JSON payload for %q", msg.EventType)
		return
	}
	frame, err := json.Marshal(struct {
		EventType string          `json:"eventType"`
		Payload   json.RawMessage `json:"payload"`
	}{EventType: msg.EventType, Payload: payload})
	if err != nil {
		r.done = true
		r.err = fmt.Errorf("eventstream: encode SSE envelope: %w", err)
		return
	}
	r.pending.WriteString("data: ")
	r.pending.Write(frame)
	r.pending.WriteString("\n\n")
}

func (r *eventStreamSSEReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 {
		if r.done {
			if r.err != nil {
				return 0, r.err
			}
			return 0, io.EOF
		}
		r.fill()
	}
	return r.pending.Read(p)
}

func (r *eventStreamSSEReader) Close() error { return r.src.Close() }
