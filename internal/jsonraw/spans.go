package jsonraw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

var ErrMultipleValues = errors.New("multiple JSON values in one record")

type StringSpan struct {
	Path  string
	Start int
	End   int
}

func FindStringSpans(data []byte, minRawBytes int64) ([]StringSpan, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	spans := make([]StringSpan, 0)
	if err := scanValue(decoder, data, "", minRawBytes, &spans); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, ErrMultipleValues
		}
		return nil, err
	}
	return spans, nil
}

func scanValue(decoder *json.Decoder, raw []byte, path string, minRawBytes int64, spans *[]StringSpan) error {
	startOffset := decoder.InputOffset()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch typed := token.(type) {
	case string:
		start, end, ok := rawStringBounds(raw, startOffset, decoder.InputOffset())
		if !ok {
			return fmt.Errorf("locate raw JSON string at offset %d", startOffset)
		}
		if int64(end-start) >= minRawBytes {
			*spans = append(*spans, StringSpan{Path: path, Start: start, End: end})
		}
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
				if err := scanValue(decoder, raw, path+"/"+escapePointerToken(key), minRawBytes, spans); err != nil {
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
			for index := 0; decoder.More(); index++ {
				if err := scanValue(decoder, raw, path+"/"+strconv.Itoa(index), minRawBytes, spans); err != nil {
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

func rawStringBounds(data []byte, startOffset int64, endOffset int64) (int, int, bool) {
	if startOffset < 0 || endOffset < startOffset || endOffset > int64(len(data)) {
		return 0, 0, false
	}
	window := data[startOffset:endOffset]
	relativeStart := bytes.IndexByte(window, '"')
	if relativeStart < 0 {
		return 0, 0, false
	}
	absoluteStart := int(startOffset) + relativeStart
	absoluteEnd, ok := scanStringToken(data, absoluteStart)
	if !ok || int64(absoluteEnd) > endOffset {
		return 0, 0, false
	}
	return absoluteStart, absoluteEnd, true
}

func scanStringToken(data []byte, start int) (int, bool) {
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

func escapePointerToken(value string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(value)
}
