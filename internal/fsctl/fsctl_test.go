package fsctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusAcceptsOnlyCanonicalCapabilities(t *testing.T) {
	for _, capability := range []Capability{StorageEngine, FSEnginePreview, PlatformCanary, Capability("production-ready:macos"), CrossPlatformReady} {
		if _, err := NewStatus(capability, "darwin"); err != nil {
			t.Fatalf("NewStatus(%q) returned error: %v", capability, err)
		}
	}
	for _, capability := range []Capability{"transparent", "stable", "production-ready"} {
		if _, err := NewStatus(capability, "darwin"); err == nil {
			t.Fatalf("NewStatus(%q) should reject non-canonical capability", capability)
		}
	}
}

func TestShadowComparesCompleteAndRandomBytes(t *testing.T) {
	root := t.TempDir()
	data := bytes.Repeat([]byte("shadow-source-"), 1000)
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(nativePath, data, 0o600); err != nil {
		t.Fatalf("write native: %v", err)
	}
	result, err := Shadow(context.Background(), nativePath, byteReader(data), ShadowOptions{BlockBytes: 257, RandomReads: 10000, Seed: 42})
	if err != nil {
		t.Fatalf("Shadow returned error: %v", err)
	}
	if !result.Verified || result.RandomReads != 10000 || result.ComparedBytes != int64(len(data)) {
		t.Fatalf("unexpected shadow result: %#v", result)
	}

	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)/2] ^= 1
	if result, err := Shadow(context.Background(), nativePath, byteReader(corrupt), ShadowOptions{BlockBytes: 257, RandomReads: 100, Seed: 42}); err == nil || result.Verified {
		t.Fatalf("Shadow should reject one-byte mismatch: result=%#v err=%v", result, err)
	}
}

func TestDoctorRequiresEveryComponentAndSeparatesDaemonFromMount(t *testing.T) {
	checks := make([]Check, 0, len(RequiredComponents))
	for _, component := range RequiredComponents {
		component := component
		checks = append(checks, Check{Component: component, Run: func(context.Context) error {
			if component == ComponentMount {
				return errors.New("mount unavailable")
			}
			return nil
		}})
	}
	report := Doctor(context.Background(), checks)
	if report.Healthy || report.ComponentHealth[ComponentDaemon] != true || report.ComponentHealth[ComponentMount] != false {
		t.Fatalf("doctor did not separate daemon and mount: %#v", report)
	}
	if len(report.Issues) != 1 || report.Issues[0].Component != ComponentMount {
		t.Fatalf("unexpected doctor issues: %#v", report.Issues)
	}

	report = Doctor(context.Background(), checks[:len(checks)-1])
	if report.Healthy || report.IssueCount < 2 {
		t.Fatalf("doctor should report a missing required component: %#v", report)
	}
}

func TestBenchmarkMeasuresNativeAndVirtualReads(t *testing.T) {
	root := t.TempDir()
	data := bytes.Repeat([]byte("benchmark-data-"), 10000)
	nativePath := filepath.Join(root, "native.jsonl")
	if err := os.WriteFile(nativePath, data, 0o600); err != nil {
		t.Fatalf("write native: %v", err)
	}
	report, err := Benchmark(context.Background(), nativePath, byteReader(data), BenchmarkOptions{SequentialBlockBytes: 4096, RandomBlockBytes: 4096, RandomReads: 200, Seed: 9})
	if err != nil {
		t.Fatalf("Benchmark returned error: %v", err)
	}
	if report.Native.Bytes != int64(len(data)) || report.Virtual.Bytes != int64(len(data)) || report.Random.Reads != 200 {
		t.Fatalf("unexpected benchmark report: %#v", report)
	}
	if report.Native.Duration <= 0 || report.Virtual.Duration <= 0 || report.Random.P95 <= 0 {
		t.Fatalf("benchmark durations were not recorded: %#v", report)
	}
}

func TestBenchmarkRecordsRequestedOSCacheBypass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	data := []byte("cache-bypass")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Benchmark(context.Background(), path, byteReader(data), BenchmarkOptions{BypassOSCache: true, RandomReads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OSCacheBypassRequested {
		t.Fatalf("benchmark did not record cache-bypass request: %#v", report)
	}
}

type byteReader []byte

func (r byteReader) Size() int64 { return int64(len(r)) }

func (r byteReader) ReadAt(_ context.Context, destination []byte, offset int64) (int, error) {
	if offset >= int64(len(r)) {
		return 0, io.EOF
	}
	n := copy(destination, r[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}
