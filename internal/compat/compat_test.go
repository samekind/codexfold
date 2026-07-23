package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFSUsageProducesSanitizedOperationContract(t *testing.T) {
	trace := strings.Join([]string{
		"12:00:00.000 open F=3 (R__________X___) /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.001 read F=3 B=4096 /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.002 fcntl F=3 <SETLK> codex.123",
		"12:00:00.003 fsync F=3 /Users/example/.codex/sessions/private.jsonl codex.123",
		"12:00:00.004 read F=3 B=4096 /Users/example/.codex/sessions/private.jsonl codex.123",
	}, "\n")
	contract, err := ParseFSUsage(strings.NewReader(trace), ContractOptions{Platform: "darwin", ClientKind: "cli", ClientVersion: "0.1.0"})
	if err != nil {
		t.Fatalf("ParseFSUsage returned error: %v", err)
	}
	if contract.TraceSHA256 == "" || len(contract.Operations) != 4 {
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
	if got := contract.Operations[0].Signatures; len(got) != 1 || got[0].Value != "(R__________X___)" {
		t.Fatalf("open signatures should contain only stable flags: %#v", got)
	}
	if got := contract.Operations[1].Signatures; len(got) != 0 {
		t.Fatalf("read signatures leaked volatile descriptor or byte counts: %#v", got)
	}
	if got := contract.Operations[2].Signatures; len(got) != 1 || got[0].Value != "<SETLK>" {
		t.Fatalf("fcntl signatures should preserve the stable command: %#v", got)
	}
}

func TestParseFSUsageRecognizesSanitizedFuseAdapterOperations(t *testing.T) {
	trace := "1 getattr\n2 readdir\n3 open\n4 read\n5 release\n6 rename\n7 fsync\n"
	contract, err := ParseFSUsage(strings.NewReader(trace), ContractOptions{Platform: "darwin", ClientKind: "cli", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.Operations) != 7 {
		t.Fatalf("adapter operations = %#v", contract.Operations)
	}
}

func TestParseFSUsageRecognizesNativeFSKitOperationsWithoutDoubleCountingIO(t *testing.T) {
	trace := strings.Join([]string{
		"1 operation=getattr request=1 status=0 payload=18",
		"2 operation=read request=2 status=0 payload=20",
		"3 io=read handle=1 offset=0 bytes=4096",
		"4 operation=write request=3 status=0 payload=64",
		"5 io=write handle=1 offset=4096 bytes=64",
		"6 operation=sync request=4 status=0 payload=0",
		"7 operation=statfs request=5 status=0 payload=0",
		"8 operation=release request=6 status=0 payload=8",
		"9 operation=hello request=1 status=0 payload=36",
		"10 operation=namespace_version request=7 status=0 payload=0",
	}, "\n")
	contract, err := ParseFSUsage(strings.NewReader(trace), ContractOptions{Platform: "darwin", ClientKind: "desktop", ClientVersion: "26.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Operation{
		{Name: "getattr", Count: 1},
		{Name: "read", Count: 1},
		{Name: "write", Count: 1},
		{Name: "fsync", Count: 1},
		{Name: "statfs", Count: 1},
		{Name: "release", Count: 1},
	}
	if !reflect.DeepEqual(contract.Operations, want) {
		t.Fatalf("native FSKit operations = %#v, want %#v", contract.Operations, want)
	}
}

func TestParseFSUsageCanonicalizesDarwinKernelAliases(t *testing.T) {
	trace := strings.Join([]string{
		"12:00:00.000 RdData[S] D=1 B=0x1000 /tmp/session.jsonl codex.1",
		"12:00:00.001 WrData[A] D=1 B=0x1000 /tmp/session.jsonl codex.1",
		"12:00:00.002 statfs64 /tmp/session.jsonl codex.1",
		"12:00:00.003 fstatfs64 F=4 codex.1",
		"12:00:00.004 fstatat64 /tmp/session.jsonl codex.1",
		"12:00:00.005 getdirentries64 F=4 codex.1",
		"12:00:00.006 open_dprotected F=4 /tmp/session.jsonl codex.1",
	}, "\n")
	contract, err := ParseFSUsage(strings.NewReader(trace), ContractOptions{Platform: "darwin", ClientKind: "desktop", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Operation{
		{Name: "read", Count: 1},
		{Name: "write", Count: 1},
		{Name: "statfs", Count: 2},
		{Name: "fstat", Count: 1},
		{Name: "readdir", Count: 1},
		{Name: "open", Count: 1},
	}
	if !reflect.DeepEqual(contract.Operations, want) {
		t.Fatalf("Darwin operations = %#v, want %#v", contract.Operations, want)
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
