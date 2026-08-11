// Package sse is a real Server-Sent Events event framer (WHATWG event stream model),
// not an OpenAI-only "data: " line stripper. It accumulates consecutive `data:` field
// values (joined with newlines), dispatches one frame per blank line, carries an
// `event:` name onto the frame, and ignores `:`-comment lines and unknown fields. It
// deliberately does NOT interpret API sentinels such as `[DONE]`: that is a semantic
// StreamDecoder's job, so `[DONE]` is emitted as an ordinary frame's Data. It is
// byte-level wire framing only and knows nothing about LLM semantics.
package sse

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
)

// maxLineBytes bounds a single SSE line so a hostile or buggy server cannot force
// unbounded buffering. 1 MiB is far above any real streaming delta line.
const maxLineBytes = 1 << 20

// fieldData / fieldEvent are the two SSE field names this framer acts on; every other
// field (id, retry, unknown) is ignored per the design.
const (
	fieldData  = "data"
	fieldEvent = "event"
)

// FramerError wraps a stream read failure surfaced while framing. Typed per the repo
// rule so callers can errors.As it and Unwrap the underlying I/O cause.
type FramerError struct {
	Reason string
	Err    error
}

func (e *FramerError) Error() string {
	if e.Err != nil {
		return "sse: " + e.Reason + ": " + e.Err.Error()
	}
	return "sse: " + e.Reason
}

func (e *FramerError) Unwrap() error { return e.Err }

// Compile-time proof that the package-level DecodeStreamFrames satisfies the framer
// contract via the framer adapter below.
var _ codec.StreamFramer = framer{}

// framer is a value adapter so a caller can hold an codec.StreamFramer; the
// package-level DecodeStreamFrames is the ordinary entry point.
type framer struct{}

func (framer) DecodeStreamFrames(body io.ReadCloser) (*stream.StreamReader[stream.StreamFrame], error) {
	return DecodeStreamFrames(body)
}

// Framer returns the package's StreamFramer as an interface value, for callers that
// inject an codec.StreamFramer.
func Framer() codec.StreamFramer { return framer{} }

// DecodeStreamFrames frames an SSE body into raw stream.StreamFrames. It owns body:
// the returned reader's Close closes body; on an early error (nil body) there is no
// body to close. Framing itself is lazy — each Next() reads lines until it completes a
// frame (blank line) or reaches EOF; a trailing unterminated event with buffered data
// is flushed once at EOF rather than discarded.
func DecodeStreamFrames(body io.ReadCloser) (*stream.StreamReader[stream.StreamFrame], error) {
	if body == nil {
		return nil, &FramerError{Reason: "nil body"}
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLineBytes)
	scanner.Split(scanSSELines)

	next := func() (stream.StreamFrame, error) {
		var data bytes.Buffer
		var event string
		haveData := false
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				// Blank line: dispatch the accumulated event. An event with no data
				// field dispatches nothing (per spec); just reset the event name.
				if !haveData {
					event = ""
					continue
				}
				return makeFrame(event, &data), nil
			}
			if strings.HasPrefix(line, ":") {
				continue // comment line
			}
			field, value := splitField(line)
			switch field {
			case fieldEvent:
				event = value
			case fieldData:
				data.WriteString(value)
				data.WriteByte('\n')
				haveData = true
			default:
				// id, retry, and any unknown field: ignored.
			}
		}
		if err := scanner.Err(); err != nil {
			return stream.StreamFrame{}, &FramerError{Reason: "read stream", Err: err}
		}
		// End of stream: flush a trailing event whose blank-line terminator never
		// arrived, so a server that closes without a final "\n\n" still delivers its
		// last frame. The fresh per-call buffers guarantee this fires at most once.
		if haveData {
			return makeFrame(event, &data), nil
		}
		return stream.StreamFrame{}, io.EOF
	}

	return stream.NewStreamReader(next, body.Close), nil
}

// makeFrame builds a StreamFrame from the accumulated event name and data buffer,
// stripping the single trailing newline the accumulation loop appended and copying the
// bytes so the frame never aliases the reusable buffer.
func makeFrame(event string, data *bytes.Buffer) stream.StreamFrame {
	b := bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
	out := make([]byte, len(b))
	copy(out, b)
	return stream.StreamFrame{Name: event, Data: out}
}

// scanSSELines is a bufio.SplitFunc that splits a stream on any of the three SSE line
// terminators — CR, LF, or CRLF — per the WHATWG event-stream line-splitting rules. The
// stdlib bufio.ScanLines only recognizes LF and CRLF, silently merging bare-CR-terminated
// lines into one; this splitter treats a lone CR as a terminator too. The terminator is
// stripped from the returned token, and a trailing non-terminated line is returned at EOF.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			// A CR followed by LF is one CRLF terminator (consume both). A CR that is the
			// last byte and not yet at EOF might be the first half of a CRLF straddling a
			// read boundary, so ask for more data before deciding.
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if !atEOF {
				return 0, nil, nil
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// splitField parses one SSE line into a field name and value. Per the spec, the value
// is everything after the first colon with a single leading space removed; a line with
// no colon is a field name with an empty value.
func splitField(line string) (field, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimPrefix(line[i+1:], " ")
}
