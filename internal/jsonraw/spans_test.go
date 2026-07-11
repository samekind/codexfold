package jsonraw

import "testing"

func TestFindStringSpansReturnsExactNestedRawToken(t *testing.T) {
	line := []byte(`{"payload":{"items":[{"text":"small"},{"text":"large\\nvalue"}]}}`)
	spans, err := FindStringSpans(line, int64(len(`"large\\nvalue"`)))
	if err != nil {
		t.Fatalf("FindStringSpans returned error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1: %#v", len(spans), spans)
	}
	span := spans[0]
	if span.Path != "/payload/items/1/text" {
		t.Fatalf("path = %q", span.Path)
	}
	if got := string(line[span.Start:span.End]); got != `"large\\nvalue"` {
		t.Fatalf("raw token = %q", got)
	}
}

func TestFindStringSpansPreservesSourceOrder(t *testing.T) {
	line := []byte(`{"a":"first-large","b":["second-large"]}`)
	spans, err := FindStringSpans(line, 8)
	if err != nil {
		t.Fatalf("FindStringSpans returned error: %v", err)
	}
	if len(spans) != 2 || spans[0].Path != "/a" || spans[1].Path != "/b/0" {
		t.Fatalf("unexpected spans: %#v", spans)
	}
	if spans[0].End > spans[1].Start {
		t.Fatalf("spans overlap or are out of order: %#v", spans)
	}
}
