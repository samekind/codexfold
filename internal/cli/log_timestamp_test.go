package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func fixedTimeWriter(sink *bytes.Buffer) *timestampedWriter {
	writer := newTimestampedWriter(sink)
	stamp := time.Date(2026, 8, 19, 1, 2, 3, 0, time.UTC)
	writer.now = func() time.Time { return stamp }
	return writer
}

func TestTimestampedWriterStampsEveryCompleteLine(t *testing.T) {
	var sink bytes.Buffer
	writer := fixedTimeWriter(&sink)
	if _, err := writer.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatal(err)
	}
	want := "2026-08-19T01:02:03Z first\n2026-08-19T01:02:03Z second\n"
	if sink.String() != want {
		t.Fatalf("output = %q, want %q", sink.String(), want)
	}
}

// A stamp must never land inside one message, so an unterminated tail waits for
// its newline instead of being emitted as its own line.
func TestTimestampedWriterBuffersAnUnterminatedTail(t *testing.T) {
	var sink bytes.Buffer
	writer := fixedTimeWriter(&sink)
	if _, err := writer.Write([]byte("split ")); err != nil {
		t.Fatal(err)
	}
	if sink.Len() != 0 {
		t.Fatalf("partial line was emitted early: %q", sink.String())
	}
	if _, err := writer.Write([]byte("message\n")); err != nil {
		t.Fatal(err)
	}
	want := "2026-08-19T01:02:03Z split message\n"
	if sink.String() != want {
		t.Fatalf("output = %q, want %q", sink.String(), want)
	}
}

// The io.Writer contract requires the full input length on success, even though
// a trailing partial line is held back.
func TestTimestampedWriterReportsTheFullInputLength(t *testing.T) {
	var sink bytes.Buffer
	writer := fixedTimeWriter(&sink)
	input := []byte("done\nheld")
	n, err := writer.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("n = %d, want %d", n, len(input))
	}
	if strings.Count(sink.String(), "\n") != 1 {
		t.Fatalf("output = %q, want exactly one completed line", sink.String())
	}
}

func TestTimestampedWriterKeepsEmptyLines(t *testing.T) {
	var sink bytes.Buffer
	writer := fixedTimeWriter(&sink)
	if _, err := writer.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if sink.String() != "2026-08-19T01:02:03Z \n" {
		t.Fatalf("output = %q", sink.String())
	}
}
