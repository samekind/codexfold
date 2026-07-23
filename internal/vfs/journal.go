package vfs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type JournalRecord struct {
	OperationID string       `json:"operation_id"`
	SessionID   string       `json:"session_id"`
	Kind        string       `json:"kind"`
	Phase       string       `json:"phase"`
	At          string       `json:"at"`
	TempPath    string       `json:"temp_path,omitempty"`
	FinalPath   string       `json:"final_path,omitempty"`
	Candidate   SessionState `json:"candidate"`
	Native      NativeFile   `json:"native,omitempty"`
}

func journalPath(directory string) string { return filepath.Join(directory, "journal.jsonl") }

func appendJournal(directory string, record JournalRecord) error {
	record.At = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode journal record: %w", err)
	}
	file, err := os.OpenFile(journalPath(directory), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open session journal: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write session journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync session journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close session journal: %w", err)
	}
	return nil
}

func readJournal(directory string) ([]JournalRecord, error) {
	file, err := os.Open(journalPath(directory))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var records []JournalRecord
	for scanner.Scan() {
		var record JournalRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode session journal: %w", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session journal: %w", err)
	}
	return records, nil
}
