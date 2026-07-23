//go:build !darwin

package mountfs

import (
	"errors"
	"net"
	"os"
)

type nativeSharedReadWindow struct {
	file     *os.File
	mapping  []byte
	capacity int
}

func nativeFileStreamingAvailable() bool {
	return false
}

func nativeSharedReadFDAvailable() bool {
	return false
}

func nativeSharedFileWindowAvailable() bool {
	return false
}

func sendNativeFile(_ net.Conn, _ *os.File, _ int64, _ int) (int, error) {
	return 0, errors.New("native sendfile is unavailable on this platform")
}

func sendNativeReadFD(_ net.Conn, _ *os.File) error {
	return errors.New("native descriptor transfer is unavailable on this platform")
}

func sendSharedReadFD(_ net.Conn, _ *os.File) error {
	return errors.New("shared descriptor transfer is unavailable on this platform")
}

func sendSharedWindowFD(_ net.Conn, _ *os.File) error {
	return errors.New("shared window descriptor transfer is unavailable on this platform")
}

func sendSharedFileWindowFD(_ net.Conn, _ *os.File) error {
	return errors.New("shared file window descriptor transfer is unavailable on this platform")
}

func prepareNativeSharedReadFD(_ int, _ func([]byte) (int, error)) (*os.File, int, error) {
	return nil, 0, errors.New("shared descriptor transfer is unavailable on this platform")
}

func newNativeSharedReadWindow(_ int) (*nativeSharedReadWindow, error) {
	return nil, errors.New("shared read windows are unavailable on this platform")
}

func newNativeSharedFileWindow(_ int) (*nativeSharedReadWindow, error) {
	return nil, errors.New("shared file windows are unavailable on this platform")
}

func (w *nativeSharedReadWindow) Close() error { return nil }
