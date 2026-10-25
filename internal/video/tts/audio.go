package tts

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// WAVDuration reads PCM/standard RIFF WAV chunk metadata without loading audio into memory.
func WAVDuration(path string) (time.Duration, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, errors.New("could not open narration WAV")
	}
	defer file.Close()

	var header [12]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return 0, errors.New("narration output is not a complete WAV file")
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, errors.New("narration output is not a RIFF/WAVE file")
	}

	var byteRate uint32
	var dataSize uint64
	for {
		var chunk [8]byte
		_, err := io.ReadFull(file, chunk[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return 0, errors.New("could not read narration WAV chunks")
		}
		size := binary.LittleEndian.Uint32(chunk[4:8])
		switch string(chunk[0:4]) {
		case "fmt ":
			if size < 16 {
				return 0, errors.New("narration WAV format chunk is too small")
			}
			var format [16]byte
			if _, err := io.ReadFull(file, format[:]); err != nil {
				return 0, errors.New("narration WAV format chunk is incomplete")
			}
			byteRate = binary.LittleEndian.Uint32(format[8:12])
			if size > 16 {
				if _, err := file.Seek(int64(size-16), io.SeekCurrent); err != nil {
					return 0, errors.New("could not skip narration WAV format extensions")
				}
			}
		case "data":
			dataSize = uint64(size)
			if byteRate > 0 {
				if dataSize == 0 {
					return 0, errors.New("narration WAV has no audio data")
				}
				// Seek does not fail when it moves beyond EOF. Check the declared
				// payload (and RIFF's required odd-byte padding) against the opened
				// file size before treating metadata as proof of a complete WAV.
				dataOffset, err := file.Seek(0, io.SeekCurrent)
				if err != nil {
					return 0, errors.New("could not inspect narration WAV data offset")
				}
				info, err := file.Stat()
				if err != nil {
					return 0, errors.New("could not inspect narration WAV size")
				}
				required := int64(size)
				if size%2 == 1 {
					required++
				}
				if dataOffset > info.Size() || required > info.Size()-dataOffset {
					return 0, errors.New("narration WAV data chunk is incomplete")
				}
				return time.Duration(dataSize) * time.Second / time.Duration(byteRate), nil // #nosec G115 -- dataSize comes from a uint32 RIFF field and cannot overflow time.Duration at nanosecond scale.
			}
			if _, err := file.Seek(int64(size), io.SeekCurrent); err != nil {
				return 0, errors.New("could not skip narration WAV data")
			}
		default:
			if _, err := file.Seek(int64(size), io.SeekCurrent); err != nil {
				return 0, errors.New("could not skip an unknown narration WAV chunk")
			}
		}
		if size%2 == 1 {
			if _, err := file.Seek(1, io.SeekCurrent); err != nil {
				return 0, errors.New("could not read narration WAV chunk padding")
			}
		}
		if byteRate > 0 && dataSize > 0 {
			return time.Duration(dataSize) * time.Second / time.Duration(byteRate), nil // #nosec G115 -- dataSize comes from a uint32 RIFF field and cannot overflow time.Duration at nanosecond scale.
		}
	}
	return 0, errors.New("narration WAV is missing its format or audio data")
}

// BuildSRT creates simple, deterministic timed captions from narration text.
// It does not translate the text or claim word-level speech alignment.
func BuildSRT(text string, duration time.Duration) []byte {
	chunks := subtitleChunks(text, 42)
	if len(chunks) == 0 || duration <= 0 {
		return nil
	}
	weights := make([]int, len(chunks))
	totalWeight := 0
	for i, chunk := range chunks {
		weights[i] = utf8.RuneCountInString(chunk)
		if weights[i] < 1 {
			weights[i] = 1
		}
		totalWeight += weights[i]
	}
	if totalWeight == 0 {
		return nil
	}

	var out strings.Builder
	var elapsedWeight int
	durationMS := duration.Milliseconds()
	for i, chunk := range chunks {
		startMS := durationMS * int64(elapsedWeight) / int64(totalWeight)
		elapsedWeight += weights[i]
		endMS := durationMS * int64(elapsedWeight) / int64(totalWeight)
		if i == len(chunks)-1 {
			endMS = durationMS
		}
		if endMS <= startMS {
			endMS = startMS + 1
		}
		fmt.Fprintf(&out, "%d\n%s --> %s\n%s\n\n", i+1, srtTimestamp(startMS), srtTimestamp(endMS), chunk)
	}
	return []byte(out.String())
}

func subtitleChunks(text string, maxRunes int) []string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return nil
	}
	if maxRunes < 8 {
		maxRunes = 8
	}
	words := strings.Fields(text)
	chunks := make([]string, 0, len(words)/6+1)
	var current []string
	currentRunes := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		chunks = append(chunks, strings.Join(current, " "))
		current = nil
		currentRunes = 0
	}
	for _, word := range words {
		wordRunes := utf8.RuneCountInString(word)
		if len(current) > 0 && currentRunes+1+wordRunes > maxRunes {
			flush()
		}
		current = append(current, word)
		currentRunes += wordRunes
		if len(current) > 1 {
			currentRunes++
		}
		if endsSentence(word) {
			flush()
		}
	}
	flush()
	return chunks
}

func endsSentence(word string) bool {
	r, _ := utf8.DecodeLastRuneInString(word)
	return unicode.IsPunct(r) || r == '؟' || r == '۔'
}

func srtTimestamp(milliseconds int64) string {
	if milliseconds < 0 {
		milliseconds = 0
	}
	hours := milliseconds / 3_600_000
	minutes := (milliseconds % 3_600_000) / 60_000
	seconds := (milliseconds % 60_000) / 1_000
	millis := milliseconds % 1_000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, seconds, millis)
}
