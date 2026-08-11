package chutes

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// errSSEDone is the terminal signal returned by sseEventReader.next when it
// reads the literal `data: [DONE]` payload. Callers treat it like a clean end
// of stream (distinct from io.EOF, which means the connection closed without a
// [DONE]; the Chutes capture is cut at max_tokens and ends that way).
var errSSEDone = errors.New("sse: [DONE]")

// sseEventReader is an SSE reader that yields the full accumulated `data:`
// payload of each event (multiple `data:` lines in one event are joined with
// "\n", per the SSE spec). Comment/keepalive lines (starting with ':') and
// other fields (`event:`, `id:`, `retry:`) are skipped.
//
// The Chutes e2e stream does NOT use SSE `event:` names; it encodes the event
// type as a JSON key inside the data payload (`{"e2e_init":...}`,
// `{"e2e":...}`, `{"usage":...}`, `{"e2e_error":...}`). That is confirmed by
// the captured fixture testdata/stream.sse and by the reference transport
// (chutesai/chutes-e2ee-transport src/chutes_e2ee/transport.py, which parses
// `data: ` lines and switches on the decoded JSON key). So this reader only
// needs to surface the data payload; stream.go inspects the JSON key.
type sseEventReader struct {
	r  io.ReadCloser
	sc *bufio.Scanner
}

func newSSEEventReader(r io.ReadCloser) *sseEventReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1 MiB max line
	return &sseEventReader{r: r, sc: sc}
}

// next returns the next event's data payload. It returns errSSEDone for a
// `data: [DONE]` terminal and io.EOF at a clean end of input. A pending partial
// event (data lines with no trailing blank line) is flushed at EOF.
func (s *sseEventReader) next() (string, error) {
	var data strings.Builder
	have := false
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if have {
				return finishEvent(data.String())
			}
			continue // blank line with no accumulated data: skip
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keepalive
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if have {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
			have = true
			continue
		}
		// event:, id:, retry: and any other field are ignored.
	}
	if err := s.sc.Err(); err != nil {
		return "", err
	}
	if have {
		return finishEvent(data.String())
	}
	return "", io.EOF
}

func finishEvent(data string) (string, error) {
	if data == "[DONE]" {
		return "", errSSEDone
	}
	return data, nil
}

func (s *sseEventReader) Close() error {
	return s.r.Close()
}
