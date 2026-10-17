package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const readReserveBytes = 256 << 10
const readReserveName = ".codexfold-read-reserve-v1"

var readReserveMagic = []byte("CodexFold read-lease emergency reserve v1\n")
var readReserves = struct {
	sync.Mutex
	roots map[string]time.Time
}{roots: make(map[string]time.Time)}

// PrepareReadReserve keeps only 256 KiB per store, not a session backup. It is
// released solely to establish reader/resolver leases when the disk is full;
// bypassing those leases would make concurrent reclamation unsafe.
func PrepareReadReserve(store string) {
	store, err := filepath.Abs(store)
	if err != nil {
		return
	}
	readReserves.Lock()
	defer readReserves.Unlock()
	if last, ok := readReserves.roots[store]; ok && time.Since(last) < 10*time.Second {
		return
	}
	readReserves.roots[store] = time.Now()
	path := filepath.Join(store, readReserveName)
	if _, err := os.Lstat(path); err == nil {
		return
	}
	available, err := AvailableBytes(store)
	if err != nil || available < 16<<20 {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	data := make([]byte, readReserveBytes)
	copy(data, readReserveMagic)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	created, statErr := f.Stat()
	closeErr := f.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil && statErr == nil {
		if current, err := os.Lstat(path); err == nil && os.SameFile(created, current) {
			_ = os.Remove(path)
		}
	}
}

func releaseReadReserveFor(directory string) bool {
	readReserves.Lock()
	defer readReserves.Unlock()
	for store := range readReserves.roots {
		if !strings.HasPrefix(filepath.Clean(directory), store+string(filepath.Separator)) {
			continue
		}
		path := filepath.Join(store, readReserveName)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != readReserveBytes {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		opened, statErr := f.Stat()
		data := make([]byte, readReserveBytes)
		_, readErr := f.ReadAt(data, 0)
		_ = f.Close()
		current, currentErr := os.Lstat(path)
		if errors.Join(statErr, readErr, currentErr) != nil || !os.SameFile(info, opened) || !os.SameFile(opened, current) || !bytes.Equal(data[:len(readReserveMagic)], readReserveMagic) || !bytes.Equal(data[len(readReserveMagic):], make([]byte, readReserveBytes-len(readReserveMagic))) {
			continue
		}
		return os.Remove(path) == nil
	}
	return false
}
