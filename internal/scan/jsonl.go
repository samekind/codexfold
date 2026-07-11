package scan

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
)

const (
	dedupLayerRecord             = "record"
	dedupLayerField              = "field"
	defaultDedupMinFieldBytes    = int64(4 * 1024)
	defaultDedupMaxJSONLineBytes = int64(32 * 1024 * 1024)
)

type DedupScanOptions struct {
	RecordLayer      bool
	FieldLayer       bool
	CDCLayer         bool
	MinFieldBytes    int64
	MaxJSONLineBytes int64
	CDC              DedupCDCOptions
}

type DedupFileStats struct {
	ScannedBytes           int64 `json:"scanned_bytes"`
	RecordCount            int64 `json:"record_count"`
	ParsedRecordCount      int64 `json:"parsed_record_count"`
	OversizedRecordCount   int64 `json:"oversized_record_count"`
	InvalidJSONRecordCount int64 `json:"invalid_json_record_count"`
	FieldCount             int64 `json:"field_count"`
	FieldBytes             int64 `json:"field_bytes"`
	CDCChunkCount          int64 `json:"cdc_chunk_count"`
	CDCBytes               int64 `json:"cdc_bytes"`
}

func scanDedupStream(r io.Reader, options DedupScanOptions, index *dedupIndex) (DedupFileStats, error) {
	if options.MinFieldBytes <= 0 {
		options.MinFieldBytes = defaultDedupMinFieldBytes
	}
	if options.MaxJSONLineBytes <= 0 {
		options.MaxJSONLineBytes = defaultDedupMaxJSONLineBytes
	}

	var stats DedupFileStats
	var cdc *dedupCDCChunker
	if options.CDCLayer {
		var err error
		cdc, err = newDedupCDCChunker(options.CDC, func(digest [sha256.Size]byte, size int64) error {
			if err := index.Observe(dedupLayerCDC, digest, size); err != nil {
				return err
			}
			stats.CDCChunkCount++
			stats.CDCBytes += size
			return nil
		})
		if err != nil {
			return DedupFileStats{}, err
		}
	}

	reader := bufio.NewReaderSize(r, 1024*1024)
	for {
		var recordHasher hash.Hash
		if options.RecordLayer {
			recordHasher = sha256.New()
		}
		var captured []byte
		captureEnabled := options.FieldLayer
		oversized := false
		var recordBytes int64
		hasData := false
		reachedEOF := false

		for {
			fragment, err := reader.ReadSlice('\n')
			if len(fragment) > 0 {
				hasData = true
				if cdc != nil {
					if err := cdc.Write(fragment); err != nil {
						return DedupFileStats{}, err
					}
				}
				if recordHasher != nil {
					_, _ = recordHasher.Write(fragment)
				}
				recordBytes += int64(len(fragment))
				stats.ScannedBytes += int64(len(fragment))
				if captureEnabled {
					if recordBytes <= options.MaxJSONLineBytes {
						captured = append(captured, fragment...)
					} else {
						captured = nil
						captureEnabled = false
						oversized = true
					}
				}
			}

			switch {
			case err == nil:
				goto recordComplete
			case errors.Is(err, bufio.ErrBufferFull):
				continue
			case errors.Is(err, io.EOF):
				reachedEOF = true
				goto recordComplete
			default:
				return DedupFileStats{}, fmt.Errorf("read dedup JSONL record: %w", err)
			}
		}

	recordComplete:
		if !hasData {
			break
		}
		stats.RecordCount++
		if options.RecordLayer {
			var digest [sha256.Size]byte
			copy(digest[:], recordHasher.Sum(nil))
			if err := index.Observe(dedupLayerRecord, digest, recordBytes); err != nil {
				return DedupFileStats{}, err
			}
		}

		if options.FieldLayer {
			if oversized {
				stats.OversizedRecordCount++
			} else {
				if err := observeLargeJSONStrings(captured, options.MinFieldBytes, index, &stats); err != nil {
					if isDedupJSONParseError(err) {
						stats.InvalidJSONRecordCount++
					} else {
						return DedupFileStats{}, err
					}
				} else {
					stats.ParsedRecordCount++
				}
			}
		}
		if reachedEOF {
			break
		}
	}
	if cdc != nil {
		if err := cdc.Finish(); err != nil {
			return DedupFileStats{}, err
		}
	}
	return stats, nil
}

func isDedupJSONParseError(err error) bool {
	var syntaxError *json.SyntaxError
	return errors.As(err, &syntaxError) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func observeLargeJSONStrings(data []byte, minBytes int64, index *dedupIndex, stats *DedupFileStats) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanDedupJSONValue(decoder, data, "", minBytes, index, stats); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values in one JSONL record")
		}
		return err
	}
	return nil
}

func scanDedupJSONValue(decoder *json.Decoder, rawJSON []byte, path string, minBytes int64, index *dedupIndex, stats *DedupFileStats) error {
	startOffset := decoder.InputOffset()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch typed := token.(type) {
	case string:
		rawToken, ok := rawJSONStringToken(rawJSON, startOffset, decoder.InputOffset())
		if !ok {
			return fmt.Errorf("locate raw JSON string token at offset %d", startOffset)
		}
		if int64(len(rawToken)) < minBytes {
			return nil
		}
		digest := sha256.Sum256(rawToken)
		if err := index.ObserveAt(dedupLayerField, digest, int64(len(rawToken)), path); err != nil {
			return err
		}
		stats.FieldCount++
		stats.FieldBytes += int64(len(rawToken))
	case json.Delim:
		switch typed {
		case '{':
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("JSON object key is %T, want string", keyToken)
				}
				if err := scanDedupJSONValue(decoder, rawJSON, path+"/"+escapeJSONPointerToken(key), minBytes, index, stats); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("JSON object ended with %v", end)
			}
		case '[':
			for indexValue := 0; decoder.More(); indexValue++ {
				if err := scanDedupJSONValue(decoder, rawJSON, path+"/"+strconv.Itoa(indexValue), minBytes, index, stats); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("JSON array ended with %v", end)
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	}
	return nil
}

func rawJSONStringToken(data []byte, startOffset int64, endOffset int64) ([]byte, bool) {
	if startOffset < 0 || endOffset < startOffset || endOffset > int64(len(data)) {
		return nil, false
	}
	window := data[startOffset:endOffset]
	relativeStart := bytes.IndexByte(window, '"')
	if relativeStart < 0 {
		return nil, false
	}
	absoluteStart := int(startOffset) + relativeStart
	absoluteEnd, ok := scanJSONStringToken(data, absoluteStart)
	if !ok || int64(absoluteEnd) > endOffset {
		return nil, false
	}
	return data[absoluteStart:absoluteEnd], true
}

func scanJSONStringToken(data []byte, start int) (int, bool) {
	if start >= len(data) || data[start] != '"' {
		return 0, false
	}
	escaped := false
	for cursor := start + 1; cursor < len(data); cursor++ {
		switch {
		case escaped:
			escaped = false
		case data[cursor] == '\\':
			escaped = true
		case data[cursor] == '"':
			return cursor + 1, true
		}
	}
	return 0, false
}

func escapeJSONPointerToken(value string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(value)
}
