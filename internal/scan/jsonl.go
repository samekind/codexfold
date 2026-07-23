package scan

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/samekind/codexfold/internal/cdc"
	"github.com/samekind/codexfold/internal/jsonraw"
)

const (
	dedupLayerRecord             = "record"
	dedupLayerField              = "field"
	dedupLayerCDC                = "cdc"
	defaultDedupMinFieldBytes    = int64(4 * 1024)
	defaultDedupMaxJSONLineBytes = int64(32 * 1024 * 1024)
)

type DedupCDCOptions = cdc.Options

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
	var chunker *cdc.Chunker
	if options.CDCLayer {
		var err error
		chunker, err = cdc.New(options.CDC, func(chunk cdc.Chunk) error {
			size := int64(len(chunk.Data))
			if err := index.Observe(dedupLayerCDC, chunk.Digest, size); err != nil {
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
				if chunker != nil {
					if err := chunker.Write(fragment); err != nil {
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
	if chunker != nil {
		if err := chunker.Finish(); err != nil {
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
	spans, err := jsonraw.FindStringSpans(data, minBytes)
	if err != nil {
		return err
	}
	for _, span := range spans {
		rawToken := data[span.Start:span.End]
		digest := sha256.Sum256(rawToken)
		if err := index.ObserveAt(dedupLayerField, digest, int64(len(rawToken)), span.Path); err != nil {
			return err
		}
		stats.FieldCount++
		stats.FieldBytes += int64(len(rawToken))
	}
	return nil
}
