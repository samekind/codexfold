package reconcile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"regexp"
)

var conversationRecordStartPattern = regexp.MustCompile(`\{\s*"timestamp"\s*:`)

type conversationChain [sha256.Size]byte

func (chain conversationChain) append(record []byte) conversationChain {
	recordDigest := sha256.Sum256(record)
	var input [sha256.Size * 2]byte
	copy(input[:sha256.Size], chain[:])
	copy(input[sha256.Size:], recordDigest[:])
	return sha256.Sum256(input[:])
}

func conversationRecordKind(record []byte) string {
	if !bytes.Contains(record, []byte(`"type"`)) || !bytes.Contains(record, []byte(`"payload"`)) {
		return ""
	}
	var envelope struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(record, &envelope); err != nil || len(envelope.Timestamp) == 0 {
		return ""
	}
	var payload struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return ""
	}
	switch envelope.Type {
	case "response_item":
		if payload.Type == "message" && (payload.Role == "user" || payload.Role == "assistant") {
			return envelope.Type + ":" + payload.Role
		}
	case "event_msg":
		if payload.Type == "user_message" || payload.Type == "agent_message" {
			return envelope.Type + ":" + payload.Type
		}
	}
	return ""
}

func containsCompleteConversationRecord(fragment []byte) bool {
	for _, match := range conversationRecordStartPattern.FindAllIndex(fragment, -1) {
		decoder := json.NewDecoder(bytes.NewReader(fragment[match[0]:]))
		var record json.RawMessage
		if err := decoder.Decode(&record); err == nil && conversationRecordKind(record) != "" {
			return true
		}
	}
	return false
}
