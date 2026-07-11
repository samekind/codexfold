package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFSUsageProducesSanitizedOperationContract(t *testing.T) {
	trace := strings.Join([]string{
		"12:00:00.000 open F=3 (R_____) /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.001 read F=3 B=4096 /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.002 fsync F=3 /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.003 read F=3 B=4096 /Users/example/.codex/sessions/private.jsonl codex.123",
	}, "\n")
	contract, err := ParseFSUsage(strings.NewReader(trace), ContractOptions{Platform: "darwin", ClientKind: "cli", ClientVersion: "0.1.0"})
	if err != nil {
		t.Fatalf("ParseFSUsage returned error: %v", err)
	}
	if contract.TraceSHA256 == "" || len(contract.Operations) != 3 {
		t.Fatalf("unexpected contract: %#v", contract)
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal contract: %v", err)
	}
	if bytes.Contains(encoded, []byte("/Users/example")) || bytes.Contains(encoded, []byte("private.jsonl")) {
		t.Fatalf("contract leaked trace paths: %s", encoded)
	}
	if contract.Operations[1].Name != "read" || contract.Operations[1].Count != 2 {
		t.Fatalf("operation aggregation differs: %#v", contract.Operations)
	}
}

func TestEvaluateQuarantinesUnknownClientVersion(t *testing.T) {
	contracts := []Contract{{Version: ContractVersion, Platform: "darwin", ClientKind: "cli", ClientVersion: "1.0.0", TraceSHA256: strings.Repeat("a", 64)}}
	approved := Evaluate([]ClientVersion{{Platform: "darwin", Kind: "cli", Version: "1.0.0"}}, contracts)
	if approved.Quarantine || !approved.Approved {
		t.Fatalf("known version should be approved: %#v", approved)
	}
	unknown := Evaluate([]ClientVersion{{Platform: "darwin", Kind: "cli", Version: "1.1.0"}}, contracts)
	if !unknown.Quarantine || unknown.Approved || len(unknown.Unknown) != 1 {
		t.Fatalf("unknown version should quarantine: %#v", unknown)
	}
}

func TestSaveAndLoadContractRoundTrip(t *testing.T) {
	root := t.TempDir()
	contract := Contract{Version: ContractVersion, Platform: "darwin", ClientKind: "desktop", ClientVersion: "26.1", TraceSHA256: strings.Repeat("b", 64), Operations: []Operation{{Name: "open", Count: 1}}}
	path, err := Save(root, contract)
	if err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if loaded.ClientVersion != contract.ClientVersion || loaded.TraceSHA256 != contract.TraceSHA256 {
		t.Fatalf("loaded contract differs: %#v", loaded)
	}
}

func TestDetectCLIVersionParsesCommandOutput(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "codex-test")
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho 'codex-cli 9.8.7'\n"), 0o700); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	version, err := DetectCLIVersion(context.Background(), command)
	if err != nil {
		t.Fatalf("DetectCLIVersion returned error: %v", err)
	}
	if version.Platform == "" || version.Kind != "cli" || version.Version != "9.8.7" {
		t.Fatalf("unexpected CLI version: %#v", version)
	}
}
