package scan

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestDedupScanFindsRecordsAndFieldsAtDifferentPositions(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	repeated := []byte("{\"payload\":{\"output\":\"same-large-value\"}}\n")
	input := append(append(append([]byte{}, repeated...), []byte("{\"kind\":\"middle\"}\n")...), repeated...)
	stats, err := scanDedupStream(bytes.NewReader(input), DedupScanOptions{
		RecordLayer:      true,
		FieldLayer:       true,
		MinFieldBytes:    10,
		MaxJSONLineBytes: 1 << 20,
	}, index)
	if err != nil {
		t.Fatalf("scan dedup stream: %v", err)
	}
	if stats.RecordCount != 3 {
		t.Fatalf("record count = %d, want 3", stats.RecordCount)
	}
	if stats.FieldCount != 2 {
		t.Fatalf("field count = %d, want 2", stats.FieldCount)
	}

	recordStats, err := index.LayerStats(dedupLayerRecord)
	if err != nil {
		t.Fatalf("record layer stats: %v", err)
	}
	if recordStats.DuplicateOccurrences != 1 || recordStats.DuplicateBytes != int64(len(repeated)) {
		t.Fatalf("record duplicates = (%d, %d), want (1, %d)", recordStats.DuplicateOccurrences, recordStats.DuplicateBytes, len(repeated))
	}

	fieldStats, err := index.LayerStats(dedupLayerField)
	if err != nil {
		t.Fatalf("field layer stats: %v", err)
	}
	rawFieldBytes := len(`"same-large-value"`)
	if fieldStats.DuplicateOccurrences != 1 || fieldStats.DuplicateBytes != int64(rawFieldBytes) {
		t.Fatalf("field duplicates = (%d, %d), want (1, %d)", fieldStats.DuplicateOccurrences, fieldStats.DuplicateBytes, rawFieldBytes)
	}
	top, err := index.TopObjects(dedupLayerField, 1)
	if err != nil {
		t.Fatalf("top field objects: %v", err)
	}
	if len(top) != 1 || top[0].SamplePath != "/payload/output" {
		t.Fatalf("top field sample path = %#v, want /payload/output", top)
	}
}

func TestDedupScanKeepsDifferentRawJSONStringEncodingsSeparate(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	input := []byte("{\"value\":\"same\\n\"}\n{\"value\":\"same\\u000a\"}\n")
	stats, err := scanDedupStream(bytes.NewReader(input), DedupScanOptions{
		FieldLayer:       true,
		MinFieldBytes:    1,
		MaxJSONLineBytes: 1 << 20,
	}, index)
	if err != nil {
		t.Fatalf("scan dedup stream: %v", err)
	}
	if stats.FieldCount != 2 {
		t.Fatalf("field count = %d, want 2", stats.FieldCount)
	}
	fieldStats, err := index.LayerStats(dedupLayerField)
	if err != nil {
		t.Fatalf("field layer stats: %v", err)
	}
	if fieldStats.DuplicateOccurrences != 0 {
		t.Fatalf("raw-distinct JSON strings were merged: %#v", fieldStats)
	}
}

func TestDedupScanSkipsOversizedFieldParsingButHashesRecord(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	line := []byte("{\"payload\":{\"output\":\"this-is-too-large-to-parse\"}}\n")
	input := append(append([]byte{}, line...), line...)
	stats, err := scanDedupStream(bytes.NewReader(input), DedupScanOptions{
		RecordLayer:      true,
		FieldLayer:       true,
		MinFieldBytes:    4,
		MaxJSONLineBytes: 16,
	}, index)
	if err != nil {
		t.Fatalf("scan dedup stream: %v", err)
	}
	if stats.RecordCount != 2 {
		t.Fatalf("record count = %d, want 2", stats.RecordCount)
	}
	if stats.OversizedRecordCount != 2 {
		t.Fatalf("oversized record count = %d, want 2", stats.OversizedRecordCount)
	}
	if stats.FieldCount != 0 {
		t.Fatalf("field count = %d, want 0", stats.FieldCount)
	}

	recordStats, err := index.LayerStats(dedupLayerRecord)
	if err != nil {
		t.Fatalf("record layer stats: %v", err)
	}
	if recordStats.DuplicateOccurrences != 1 || recordStats.DuplicateBytes != int64(len(line)) {
		t.Fatalf("record duplicates = (%d, %d), want (1, %d)", recordStats.DuplicateOccurrences, recordStats.DuplicateBytes, len(line))
	}
}

func TestDedupScanFeedsContentDefinedChunkLayerInSamePass(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	data := deterministicBytes(64 * 1024)
	stats, err := scanDedupStream(bytes.NewReader(data), DedupScanOptions{
		CDCLayer: true,
		CDC:      DedupCDCOptions{MinBytes: 64, AverageBytes: 128, MaxBytes: 256},
	}, index)
	if err != nil {
		t.Fatalf("scan dedup stream: %v", err)
	}
	if stats.CDCChunkCount == 0 || stats.CDCBytes != int64(len(data)) {
		t.Fatalf("CDC stats = (%d chunks, %d bytes), want non-zero chunks and %d bytes", stats.CDCChunkCount, stats.CDCBytes, len(data))
	}
	layerStats, err := index.LayerStats(dedupLayerCDC)
	if err != nil {
		t.Fatalf("CDC layer stats: %v", err)
	}
	if layerStats.ObjectCount != stats.CDCChunkCount {
		t.Fatalf("CDC indexed objects = %d, scan chunks = %d", layerStats.ObjectCount, stats.CDCChunkCount)
	}
}

func TestDedupScanCountsInvalidJSONWithoutStoppingOtherLayers(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close dedup index: %v", err)
		}
	})

	input := []byte("{not-json}\n{\"payload\":{\"output\":\"large-valid-value\"}}\n")
	stats, err := scanDedupStream(bytes.NewReader(input), DedupScanOptions{
		RecordLayer:      true,
		FieldLayer:       true,
		MinFieldBytes:    8,
		MaxJSONLineBytes: 1 << 20,
	}, index)
	if err != nil {
		t.Fatalf("scan dedup stream: %v", err)
	}
	if stats.InvalidJSONRecordCount != 1 || stats.ParsedRecordCount != 1 || stats.RecordCount != 2 {
		t.Fatalf("scan stats = %#v, want one invalid, one parsed, and two hashed records", stats)
	}
}

func TestDedupScanDoesNotMisclassifyIndexFailureAsInvalidJSON(t *testing.T) {
	index, err := openDedupIndex(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("open dedup index: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("close dedup index: %v", err)
	}

	input := []byte("{\"payload\":{\"output\":\"large-valid-value\"}}\n")
	_, err = scanDedupStream(bytes.NewReader(input), DedupScanOptions{
		FieldLayer:       true,
		MinFieldBytes:    8,
		MaxJSONLineBytes: 1 << 20,
	}, index)
	if err == nil {
		t.Fatalf("scan should return the closed-index failure")
	}
}
