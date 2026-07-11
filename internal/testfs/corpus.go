package testfs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Options struct {
	LargeFieldBytes int
	RepeatedRecords int
}

type Session struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Corpus struct {
	Root     string    `json:"root"`
	Sessions []Session `json:"sessions"`
}

func Generate(root string, options Options) (Corpus, error) {
	if root == "" {
		return Corpus{}, errors.New("corpus root is required")
	}
	if options.LargeFieldBytes <= 0 {
		options.LargeFieldBytes = 768 << 10
	}
	if options.RepeatedRecords <= 0 {
		options.RepeatedRecords = 64
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Corpus{}, err
	}
	large := make([]byte, options.LargeFieldBytes)
	for index := range large {
		large[index] = byte('a' + index%23)
	}
	repeated := []byte("{\"type\":\"event\",\"payload\":\"exact-repeated-record\"}\n")
	prefixPath := filepath.Join(root, "shared-prefix.bin")
	prefix, err := os.Create(prefixPath)
	if err != nil {
		return Corpus{}, err
	}
	writer := bufio.NewWriterSize(prefix, 1<<20)
	_, _ = writer.WriteString("{\"type\":\"session_meta\",\"id\":\"synthetic\"}\n")
	_, _ = writer.WriteString("{\"type\":\"large\",\"payload\":\"")
	_, _ = writer.Write(large)
	_, _ = writer.WriteString("\"}\n")
	for index := 0; index < options.RepeatedRecords; index++ {
		_, _ = writer.Write(repeated)
	}
	_, _ = writer.WriteString("not-json-but-valid-rollout-bytes\n")
	if err := writer.Flush(); err != nil {
		_ = prefix.Close()
		return Corpus{}, err
	}
	if err := prefix.Sync(); err != nil {
		_ = prefix.Close()
		return Corpus{}, err
	}
	if err := prefix.Close(); err != nil {
		return Corpus{}, err
	}
	prefixData, err := os.ReadFile(prefixPath)
	if err != nil {
		return Corpus{}, err
	}
	definitions := []struct {
		id   string
		body []byte
	}{
		{id: "fork-a", body: append(append([]byte(nil), prefixData...), []byte("{\"tail\":\"a\"}\n")...)},
		{id: "fork-b", body: append(append([]byte(nil), prefixData...), []byte("{\"tail\":\"b\"}\n")...)},
		{id: "reordered", body: append(append([]byte(nil), repeated...), append(prefixData, repeated...)...)},
		{id: "empty", body: []byte{}},
	}
	corpus := Corpus{Root: root, Sessions: make([]Session, 0, len(definitions))}
	for _, definition := range definitions {
		path := filepath.Join(root, definition.id+".jsonl")
		if err := os.WriteFile(path, definition.body, 0o600); err != nil {
			return Corpus{}, err
		}
		digest := sha256.Sum256(definition.body)
		corpus.Sessions = append(corpus.Sessions, Session{ID: definition.id, Path: path, Bytes: int64(len(definition.body)), SHA256: hex.EncodeToString(digest[:])})
	}
	_ = os.Remove(prefixPath)
	return corpus, nil
}

func GenerateRollout(path string, targetBytes int64) (Session, error) {
	if path == "" || targetBytes < 0 {
		return Session{}, errors.New("rollout path and non-negative target size are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Session{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Session{}, err
	}
	hasher := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(file, hasher), 4<<20)
	record := []byte("{\"type\":\"event\",\"payload\":\"synthetic-repeat-0123456789abcdefghijklmnopqrstuvwxyz\"}\n")
	var written int64
	for targetBytes-written >= int64(len(record)) {
		n, writeErr := writer.Write(record)
		written += int64(n)
		if writeErr != nil {
			_ = file.Close()
			return Session{}, writeErr
		}
	}
	if remaining := targetBytes - written; remaining > 0 {
		padding := make([]byte, remaining)
		for index := range padding {
			padding[index] = byte('A' + index%26)
		}
		n, writeErr := writer.Write(padding)
		written += int64(n)
		if writeErr != nil {
			_ = file.Close()
			return Session{}, writeErr
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return Session{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Session{}, err
	}
	if err := file.Close(); err != nil {
		return Session{}, err
	}
	if written != targetBytes {
		return Session{}, fmt.Errorf("generated %d bytes, want %d", written, targetBytes)
	}
	return Session{ID: "large", Path: path, Bytes: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}
