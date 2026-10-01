//go:build ignore

// Generate a large, valid JSONL performance fixture in a disposable isolated
// Codex home. This is synthetic workload data, not a Codex Desktop acceptance
// session and never a replacement for the byte-exact real-session tests.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func main() {
	output := flag.String("output", "", "new JSONL file under /private/tmp/cfd-*")
	sessionID := flag.String("session-id", "", "synthetic session ID")
	targetBytes := flag.Int64("bytes", 256<<20, "minimum fixture size (64..512 MiB)")
	flag.Parse()
	if err := run(*output, *sessionID, *targetBytes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output, sessionID string, targetBytes int64) (resultErr error) {
	if !safeID.MatchString(sessionID) || targetBytes < 64<<20 || targetBytes > 512<<20 {
		return errors.New("safe session ID and 64..512 MiB target are required")
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil || !strings.HasPrefix(parent, "/private/tmp/cfd-") {
		return errors.New("output parent must be an existing disposable /private/tmp/cfd-* directory")
	}
	file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			_ = os.Remove(abs)
		}
	}()
	hash := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(file, hash), 1<<20)
	const timestamp = "2026-09-24T00:00:00Z"
	if _, err := fmt.Fprintf(writer, `{"timestamp":"%s","type":"session_meta","payload":{"id":"%s","timestamp":"%s","originator":"codexfold-performance-fixture"}}`+"\n", timestamp, sessionID, timestamp); err != nil {
		return err
	}
	// One MiB of non-repeating JSON-safe data per record avoids a single tiny
	// deduplicated object masquerading as a large packed-read workload.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	const payloadBytes = 1 << 20
	payload := make([]byte, payloadBytes)
	seed := fnv.New64a()
	_, _ = seed.Write([]byte(sessionID))
	rng := rand.New(rand.NewSource(int64(seed.Sum64())))
	written := int64(len(fmt.Sprintf(`{"timestamp":"%s","type":"session_meta","payload":{"id":"%s","timestamp":"%s","originator":"codexfold-performance-fixture"}}`+"\n", timestamp, sessionID, timestamp)))
	for sequence := 0; written < targetBytes; sequence++ {
		for offset := 0; offset < len(payload); {
			bits := rng.Uint64()
			for j := 0; j < 10 && offset < len(payload); j++ {
				payload[offset] = alphabet[bits&63]
				bits >>= 6
				offset++
			}
		}
		prefix := fmt.Sprintf(`{"timestamp":"%s","type":"event_msg","payload":{"type":"codexfold_perf_fixture","seq":%d,"text":"`, timestamp, sequence)
		for _, segment := range [][]byte{[]byte(prefix), payload, []byte("\"}}\n")} {
			count, err := writer.Write(segment)
			written += int64(count)
			if err != nil {
				return err
			}
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	fmt.Printf("bytes=%d sha256=%s\n", written, hex.EncodeToString(hash.Sum(nil)))
	return nil
}
