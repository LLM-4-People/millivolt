package format

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// ErrTruncatedStream is returned when an upstream SSE stream ends without its
// protocol's terminal event. The proxy turns it into one in-band error frame
// so a client never waits forever on a half-delivered stream.
var ErrTruncatedStream = errors.New("upstream stream ended before a terminal event")

// StreamDecoder yields canonical events from one upstream SSE stream.
type StreamDecoder interface {
	Next() (*ir.Event, error)
}

// StreamEncoder writes one client-facing SSE stream from canonical events.
type StreamEncoder interface {
	Write(*ir.Event) error
	Close() error
}

// sseEvent is one parsed SSE event.
type sseEvent struct {
	Name string
	Data []byte
}

// sseReader parses SSE frames with a bounded line and event size. It accepts
// comment lines, multi-line data fields, and an optional event name.
type sseReader struct {
	scanner *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, sseLineBufInit), sseLineBufMax)
	return &sseReader{scanner: s}
}

func (r *sseReader) Next() (*sseEvent, error) {
	var (
		ev   sseEvent
		data bytes.Buffer
	)
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			if ev.Name == "" && data.Len() == 0 {
				continue
			}
			ev.Data = data.Bytes()
			return &ev, nil
		}
		if line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			ev.Name = strings.TrimSpace(string(line[len("event:"):]))
			continue
		}
		payload, ok := sse.DataPayload(line)
		if !ok {
			continue
		}
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		if data.Len()+len(payload) > sseLineBufMax {
			return nil, errors.New("sse event exceeds the line limit")
		}
		data.Write(payload)
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if data.Len() > 0 || ev.Name != "" {
		ev.Data = data.Bytes()
		return &ev, nil
	}
	return nil, io.EOF
}

// eventQueue is a small FIFO used by decoders that must emit several canonical
// events for one wire event (for example closing blocks before a terminal).
type eventQueue struct {
	pending []*ir.Event
}

func (q *eventQueue) push(ev *ir.Event) { q.pending = append(q.pending, ev) }

func (q *eventQueue) pop() (*ir.Event, bool) {
	if len(q.pending) == 0 {
		return nil, false
	}
	ev := q.pending[0]
	q.pending = q.pending[1:]
	return ev, true
}
