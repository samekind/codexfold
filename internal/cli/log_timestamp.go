package cli

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

// timestampedWriter prefixes every complete line with a UTC RFC3339Nano stamp.
// The resident services write plain lines to a launchd-managed stderr file that
// carries no time information of its own, which made a client-visible I/O
// error impossible to correlate with any service event after the fact.
type timestampedWriter struct {
	mu      sync.Mutex
	writer  io.Writer
	partial []byte
	now     func() time.Time
}

func newTimestampedWriter(writer io.Writer) *timestampedWriter {
	return &timestampedWriter{writer: writer, now: time.Now}
}

// Write buffers an incomplete trailing line so a stamp is never inserted into
// the middle of one message.
func (w *timestampedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, data...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			return len(data), nil
		}
		line := w.partial[:index+1]
		w.partial = append([]byte(nil), w.partial[index+1:]...)
		if _, err := fmt.Fprintf(w.writer, "%s %s", w.now().UTC().Format(time.RFC3339Nano), line); err != nil {
			return len(data), err
		}
	}
}
