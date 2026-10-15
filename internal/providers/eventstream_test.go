package providers

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

func buildEventFrame(t *testing.T, eventType string, payload string) []byte {
	t.Helper()
	var headers bytes.Buffer
	writeStrHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7) // string type
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(value)))
		headers.Write(lb[:])
		headers.WriteString(value)
	}
	writeStrHeader(":message-type", "event")
	writeStrHeader(":event-type", eventType)
	writeStrHeader(":content-type", "application/json")

	total := 12 + headers.Len() + len(payload) + 4
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))
	frame := append([]byte{}, prelude[:]...)
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, payload...)
	var crcb [4]byte
	binary.BigEndian.PutUint32(crcb[:], crc32.ChecksumIEEE(frame))
	frame = append(frame, crcb[:]...)
	return frame
}

func TestEventStreamToSSETranslation(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(buildEventFrame(t, "messageStart", `{"role":"assistant"}`))
	stream.Write(buildEventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`))
	stream.Write(buildEventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo"}}`))
	stream.Write(buildEventFrame(t, "messageStop", `{"stopReason":"end_turn"}`))

	reader := newEventStreamSSEReader(io.NopCloser(&stream))
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`data: {"eventType":"messageStart","payload":{"role":"assistant"}}`,
		`data: {"eventType":"contentBlockDelta","payload":{"contentBlockIndex":0,"delta":{"text":"Hel"}}}`,
		`data: {"eventType":"messageStop","payload":{"stopReason":"end_turn"}}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing frame %q in:\n%s", want, text)
		}
	}
}

func TestEventStreamRejectsOversizedFrameBeforeReadingBody(t *testing.T) {
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], maxEventStreamFrameBytes+1)
	binary.BigEndian.PutUint32(prelude[4:8], 0)
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	// Supply only the valid prelude: the decoder must reject the advertised
	// size before allocating the frame or attempting to read its body.
	_, err := readEventStreamMessage(bytes.NewReader(prelude[:]))
	if err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("oversized frame error = %v, want safe-limit rejection", err)
	}
}

func TestEventStreamRejectsInvalidJSONPayloadWithoutSSEInjection(t *testing.T) {
	payload := "{\"role\":\"assistant\"}\n\ndata: {\"eventType\":\"messageStop\",\"payload\":{\"stopReason\":\"end_turn\"}}"
	frame := buildEventFrame(t, "messageStart", payload)
	reader := newEventStreamSSEReader(io.NopCloser(bytes.NewReader(frame)))
	out, err := io.ReadAll(reader)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON payload") {
		t.Fatalf("invalid payload error = %v, want JSON rejection", err)
	}
	if len(out) != 0 {
		t.Fatalf("invalid upstream payload emitted downstream SSE bytes: %q", out)
	}
}

func TestEventStreamCRCAndTruncationFail(t *testing.T) {
	frame := buildEventFrame(t, "messageStart", `{"role":"assistant"}`)
	frame[len(frame)-1] ^= 0xFF // corrupt message CRC
	reader := newEventStreamSSEReader(io.NopCloser(bytes.NewReader(frame)))
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("corrupt frame must error")
	}
	good := buildEventFrame(t, "messageStart", `{"role":"assistant"}`)
	reader = newEventStreamSSEReader(io.NopCloser(bytes.NewReader(good[:len(good)-3])))
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("truncated frame must error")
	}
}
