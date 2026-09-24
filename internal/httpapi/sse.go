package httpapi

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// sseEvent is one decoded Server-Sent Event frame.
type sseEvent struct {
	name string
	data string
}

// sseReader decodes an SSE stream according to the framing rules of the
// text/event-stream media type: fields accumulate until a blank line, a data
// field may span multiple data: lines (joined with \n), CRLF and comments are
// tolerated, and unknown fields (id, retry, event) never terminate a frame.
// Naive line-by-line "data:" scanning drops multi-line payloads, which real
// providers and proxy layers occasionally emit.
type sseReader struct {
	sc         *bufio.Scanner
	event      string
	data       strings.Builder
	hasAny     bool
	eventBytes int
}

const maxLegacySSEEventBytes = 8 << 20

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	return &sseReader{sc: sc}
}

// Next returns the next complete event. done is true at clean EOF. A scanner
// error is returned as err.
func (s *sseReader) Next() (ev sseEvent, done bool, err error) {
	for s.sc.Scan() {
		line := strings.TrimRight(s.sc.Text(), "\r")
		// Scanner bounds individual lines, not the aggregate payload formed
		// by an unlimited number of data: fields in a single SSE event.
		if len(line)+1 > maxLegacySSEEventBytes-s.eventBytes {
			return sseEvent{}, false, fmt.Errorf("upstream SSE event exceeds %d bytes", maxLegacySSEEventBytes)
		}
		s.eventBytes += len(line) + 1
		switch {
		case line == "":
			s.eventBytes = 0
			if !s.hasAny {
				continue
			}
			ev := sseEvent{name: s.event, data: s.data.String()}
			s.event = ""
			s.data.Reset()
			s.hasAny = false
			return ev, false, nil
		case strings.HasPrefix(line, ":"):
			continue
		default:
			field, value, found := strings.Cut(line, ":")
			if !found {
				field, value = line, ""
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				s.event = value
				s.hasAny = true
			case "data":
				if s.data.Len() > 0 {
					s.data.WriteByte('\n')
				}
				s.data.WriteString(value)
				s.hasAny = true
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		return sseEvent{}, false, err
	}
	// Tolerate a final frame that was flushed without a trailing blank line.
	if s.hasAny {
		ev := sseEvent{name: s.event, data: s.data.String()}
		s.event = ""
		s.data.Reset()
		s.hasAny = false
		return ev, false, nil
	}
	return sseEvent{}, true, nil
}
