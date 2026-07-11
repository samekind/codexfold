package contain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFindsExactBodyDespiteDifferentSessionMetadata(t *testing.T) {
	root := t.TempDir()
	containedPath := filepath.Join(root, "contained.jsonl")
	containerPath := filepath.Join(root, "container.jsonl")
	contained := "{\"type\":\"session_meta\",\"id\":\"child\"}\n" +
		"{\"type\":\"message\",\"text\":\"first\"}\n" +
		"{\"type\":\"message\",\"text\":\"second\"}\n"
	container := "{\"type\":\"session_meta\",\"id\":\"parent\"}\n" +
		"{\"type\":\"message\",\"text\":\"before\"}\n" +
		"{\"type\":\"message\",\"text\":\"first\"}\n" +
		"{\"type\":\"message\",\"text\":\"second\"}\n" +
		"{\"type\":\"message\",\"text\":\"after\"}\n"
	writeFixture(t, containedPath, contained)
	writeFixture(t, containerPath, container)

	result, err := Check(context.Background(), Input{ID: "child", Path: containedPath}, Input{ID: "parent", Path: containerPath}, Options{IgnoreSessionMeta: true})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.Contained || !result.VerifiedExact || result.ContainedRecords != 2 {
		t.Fatalf("unexpected containment result: %#v", result)
	}
	if result.ContainerStartRecord != 2 || result.ContainerEndRecord != 3 {
		t.Fatalf("container record evidence = %d..%d, want 2..3", result.ContainerStartRecord, result.ContainerEndRecord)
	}
	wantStart := int64(strings.Index(container, "{\"type\":\"message\",\"text\":\"first\"}"))
	if result.ContainerStartByte != wantStart || result.ContainerEndByte-result.ContainerStartByte != result.ContainedBytes {
		t.Fatalf("container byte evidence is wrong: %#v", result)
	}
}

func TestCheckCanIncludeSessionMetadata(t *testing.T) {
	root := t.TempDir()
	containedPath := filepath.Join(root, "contained.jsonl")
	containerPath := filepath.Join(root, "container.jsonl")
	writeFixture(t, containedPath, "{\"type\":\"session_meta\",\"id\":\"child\"}\n{\"v\":1}\n")
	writeFixture(t, containerPath, "{\"type\":\"session_meta\",\"id\":\"parent\"}\n{\"v\":1}\n")

	result, err := Check(context.Background(), Input{Path: containedPath}, Input{Path: containerPath}, Options{})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if result.Contained {
		t.Fatalf("different session metadata must prevent strict containment: %#v", result)
	}
}

func TestCheckRejectsNonContiguousAndDifferentlyEscapedRecords(t *testing.T) {
	for _, test := range []struct {
		name      string
		contained string
		container string
	}{
		{
			name:      "non-contiguous",
			contained: "{\"v\":1}\n{\"v\":2}\n",
			container: "{\"v\":1}\n{\"gap\":true}\n{\"v\":2}\n",
		},
		{
			name:      "different-raw-escape",
			contained: "{\"v\":\"\\n\"}\n",
			container: "{\"v\":\"\\u000a\"}\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			containedPath := filepath.Join(root, "contained.jsonl")
			containerPath := filepath.Join(root, "container.jsonl")
			writeFixture(t, containedPath, test.contained)
			writeFixture(t, containerPath, test.container)
			result, err := Check(context.Background(), Input{Path: containedPath}, Input{Path: containerPath}, Options{})
			if err != nil {
				t.Fatalf("Check returned error: %v", err)
			}
			if result.Contained {
				t.Fatalf("false positive containment: %#v", result)
			}
		})
	}
}

func TestCheckHandlesRecordLargerThanReaderBuffer(t *testing.T) {
	root := t.TempDir()
	containedPath := filepath.Join(root, "contained.jsonl")
	containerPath := filepath.Join(root, "container.jsonl")
	large := "{\"value\":\"" + strings.Repeat("x", 2*1024*1024) + "\"}\n"
	writeFixture(t, containedPath, large)
	writeFixture(t, containerPath, "{\"before\":true}\n"+large+"{\"after\":true}\n")
	result, err := Check(context.Background(), Input{Path: containedPath}, Input{Path: containerPath}, Options{})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.Contained || !result.VerifiedExact {
		t.Fatalf("large record was not found exactly: %#v", result)
	}
}

func writeFixture(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
