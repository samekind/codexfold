package codexzstd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// goldenSHA256 is `zstd -3 --single-thread --no-check` (CLI v1.5.7) of
// rolloutFixture(). A different libzstd version or parameter changes it, and
// a build that changes it can no longer serve folded Codex .zst files.
const goldenSHA256 = "22984b02b80a7949f1d916063dbfd16303449a8d218c07a0e14250840ff23dec"

func rolloutFixture() []byte {
	random := rand.New(rand.NewSource(20261102))
	var out bytes.Buffer
	for line := 0; out.Len() < 900<<10; line++ {
		payload := make([]byte, 40+random.Intn(400))
		for i := range payload {
			payload[i] = "abcdefghijklmnopqrstuvwxyz0123456789 "[random.Intn(37)]
		}
		fmt.Fprintf(&out, `{"timestamp":"2026-10-%02dT08:%02d:%02d.000Z","type":"response_item","payload":{"type":"message","text":%q}}`+"\n",
			1+line%28, line%60, (line*7)%60, payload)
	}
	return out.Bytes()
}

func encodeBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Encode(context.Background(), &out, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestEncodeMatchesPinnedCodexFrame(t *testing.T) {
	if !Supported() {
		t.Skip("exact encoder needs cgo")
	}
	content := rolloutFixture()
	encoded := encodeBytes(t, content)
	digest := sha256.Sum256(encoded)
	if got := hex.EncodeToString(digest[:]); got != goldenSHA256 {
		t.Fatalf("encoded SHA-256 = %s, want %s", got, goldenSHA256)
	}
	decoded, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	roundTrip, err := decoded.DecodeAll(encoded, nil)
	if err != nil || !bytes.Equal(roundTrip, content) {
		t.Fatalf("round trip failed: %v", err)
	}
	if size, err := ContentSize(bytes.NewReader(encoded)); err != nil || size != int64(len(content)) {
		t.Fatalf("ContentSize = %d, %v", size, err)
	}
	if cli, err := exec.LookPath("zstd"); err == nil {
		if !strings.HasPrefix(strings.TrimSpace(mustVersion(t, cli)), "*** Zstandard CLI (64-bit) v1.5.7") {
			t.Skip("installed zstd CLI is not 1.5.7")
		}
		// The CLI only pledges the size when it reads a regular file.
		input := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(input, content, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(cli, "-3", "--single-thread", "--no-check", "-q", "-c", input).Output()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(output, encoded) {
			t.Fatal("encoder output differs from the zstd 1.5.7 CLI")
		}
	}
}

func mustVersion(t *testing.T, cli string) string {
	t.Helper()
	output, err := exec.Command(cli, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestReaderStreamsTheSameFrame(t *testing.T) {
	if !Supported() {
		t.Skip("exact encoder needs cgo")
	}
	content := rolloutFixture()
	reader := NewReader(context.Background(), bytes.NewReader(content), int64(len(content)))
	streamed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if !bytes.Equal(streamed, encodeBytes(t, content)) {
		t.Fatal("streamed frame differs")
	}
	early := NewReader(context.Background(), bytes.NewReader(content), int64(len(content)))
	if _, err := io.ReadFull(early, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if err := early.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeRejectsWrongContentSize(t *testing.T) {
	if !Supported() {
		t.Skip("exact encoder needs cgo")
	}
	content := []byte("{\"a\":1}\n")
	for _, size := range []int64{int64(len(content)) - 1, int64(len(content)) + 1} {
		if err := Encode(context.Background(), io.Discard, bytes.NewReader(content), size); err == nil {
			t.Fatalf("size %d accepted for %d content bytes", size, len(content))
		}
	}
}

func TestVerify(t *testing.T) {
	if !Supported() {
		t.Skip("exact encoder needs cgo")
	}
	content := rolloutFixture()
	dir := t.TempDir()
	exact := filepath.Join(dir, "exact.jsonl.zst")
	if err := os.WriteFile(exact, encodeBytes(t, content), 0o600); err != nil {
		t.Fatal(err)
	}
	proof, err := Verify(context.Background(), exact)
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := sha256.Sum256(content)
	if proof.ContentBytes != int64(len(content)) || proof.ContentSHA256 != hex.EncodeToString(contentDigest[:]) || proof.Profile != Profile {
		t.Fatalf("proof = %#v", proof)
	}

	other, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign.jsonl.zst")
	if err := os.WriteFile(foreign, other.EncodeAll(content, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), foreign); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("foreign encoder verify error = %v", err)
	}
	truncated := filepath.Join(dir, "truncated.jsonl.zst")
	data := encodeBytes(t, content)
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), truncated); err == nil {
		t.Fatal("truncated frame verified")
	}
	trailing := filepath.Join(dir, "trailing.jsonl.zst")
	if err := os.WriteFile(trailing, append(append([]byte(nil), data...), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), trailing); err == nil {
		t.Fatal("two concatenated frames verified")
	}
}

// TestVerifyExternalCodexFiles checks real Codex output. Point
// CODEXFOLD_CODEX_ZST_ROOT at a directory such as ~/.codex/fold-native.
func TestVerifyExternalCodexFiles(t *testing.T) {
	root := os.Getenv("CODEXFOLD_CODEX_ZST_ROOT")
	if root == "" {
		t.Skip("set CODEXFOLD_CODEX_ZST_ROOT to verify real Codex .zst files")
	}
	var verified, failed int
	var bytesVerified int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl.zst") || strings.HasPrefix(entry.Name(), "._") {
			return err
		}
		proof, verifyErr := Verify(context.Background(), path)
		if verifyErr != nil {
			failed++
			t.Errorf("%s: %v", path, verifyErr)
			return nil
		}
		verified++
		bytesVerified += proof.EncodedBytes
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verified %d Codex .zst files (%d bytes), %d failed", verified, bytesVerified, failed)
}
