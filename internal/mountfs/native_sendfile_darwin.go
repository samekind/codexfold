//go:build darwin

package mountfs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"unsafe"

	"github.com/samekind/codexfold/internal/fskitproto"
	"golang.org/x/sys/unix"
)

type nativeSharedReadObject struct {
	file   *os.File
	unlink func() error
}

type nativeSharedReadWindow struct {
	file     *os.File
	mapping  []byte
	capacity int
}

var createNativeSharedReadObject = newNativePOSIXSharedMemory
var createNativeSharedFileObject = newNativeRegularFile

func nativeFileStreamingAvailable() bool {
	return true
}

func nativeSharedReadFDAvailable() bool {
	return true
}

func nativeSharedFileWindowAvailable() bool {
	return true
}

func sendNativeFile(connection net.Conn, file *os.File, offset int64, count int) (int, error) {
	if file == nil || offset < 0 || count < 0 {
		return 0, errors.New("invalid native sendfile range")
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("FSKit connection is not a Unix socket")
	}
	rawConnection, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, err
	}
	written := 0
	var callbackErr error
	rawErr := rawConnection.Write(func(outputFD uintptr) bool {
		for written < count {
			currentOffset := offset + int64(written)
			n, sendErr := unix.Sendfile(int(outputFD), int(file.Fd()), &currentOffset, count-written)
			if n > 0 {
				written += n
			}
			if sendErr == unix.EINTR {
				continue
			}
			if sendErr == unix.EAGAIN || sendErr == unix.EWOULDBLOCK {
				return false
			}
			if sendErr != nil {
				callbackErr = sendErr
				return true
			}
			if n == 0 {
				callbackErr = io.ErrUnexpectedEOF
				return true
			}
		}
		return true
	})
	if rawErr != nil {
		return written, rawErr
	}
	if callbackErr != nil {
		return written, callbackErr
	}
	return written, nil
}

func sendNativeReadFD(connection net.Conn, file *os.File) error {
	return sendReadFD(connection, file, fskitproto.NativeReadFDMarker)
}

func sendSharedReadFD(connection net.Conn, file *os.File) error {
	return sendReadFD(connection, file, fskitproto.SharedReadFDMarker)
}

func sendSharedWindowFD(connection net.Conn, file *os.File) error {
	return sendReadFD(connection, file, fskitproto.SharedWindowFDMarker)
}

func sendSharedFileWindowFD(connection net.Conn, file *os.File) error {
	return sendReadFD(connection, file, fskitproto.SharedFileWindowFDMarker)
}

func sendReadFD(connection net.Conn, file *os.File, markerByte byte) error {
	if file == nil {
		return errors.New("read descriptor is nil")
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("FSKit connection is not a Unix socket")
	}
	rawConnection, err := unixConnection.SyscallConn()
	if err != nil {
		return err
	}
	oob := unix.UnixRights(int(file.Fd()))
	marker := []byte{markerByte}
	var callbackErr error
	rawErr := rawConnection.Write(func(outputFD uintptr) bool {
		for {
			n, sendErr := unix.SendmsgN(int(outputFD), marker, oob, nil, 0)
			if sendErr == unix.EINTR {
				continue
			}
			if sendErr == unix.EAGAIN || sendErr == unix.EWOULDBLOCK {
				return false
			}
			if sendErr != nil {
				callbackErr = sendErr
				return true
			}
			if n != len(marker) {
				callbackErr = io.ErrShortWrite
			}
			return true
		}
	})
	if rawErr != nil {
		return rawErr
	}
	return callbackErr
}

func prepareNativeSharedReadFD(length int, populate func([]byte) (int, error)) (_ *os.File, populated int, resultErr error) {
	if length <= 0 || populate == nil {
		return nil, 0, errors.New("invalid shared read mapping request")
	}
	window, err := newNativeSharedReadWindow(length)
	if err != nil {
		return nil, 0, err
	}
	populated, populateErr := populate(window.mapping[:length])
	if populateErr != nil {
		return nil, populated, errors.Join(fmt.Errorf("populate shared read file: %w", populateErr), window.Close())
	}
	if populated < 0 || populated > length {
		return nil, populated, errors.Join(
			fmt.Errorf("shared read populated %d bytes into %d-byte mapping", populated, length),
			window.Close(),
		)
	}
	if err := unix.Munmap(window.mapping); err != nil {
		return nil, populated, errors.Join(fmt.Errorf("unmap shared read file: %w", err), window.Close())
	}
	window.mapping = nil
	file := window.file
	window.file = nil
	return file, populated, nil
}

func newNativeSharedReadWindow(length int) (*nativeSharedReadWindow, error) {
	return newNativeSharedReadWindowWith(length, createNativeSharedReadObject)
}

func newNativeSharedFileWindow(length int) (*nativeSharedReadWindow, error) {
	return newNativeSharedReadWindowWith(length, createNativeSharedFileObject)
}

func newNativeSharedReadWindowWith(length int, create func() (nativeSharedReadObject, error)) (*nativeSharedReadWindow, error) {
	if length <= 0 {
		return nil, errors.New("invalid shared read window size")
	}
	if create == nil {
		return nil, errors.New("shared read object creator is nil")
	}
	object, err := create()
	if err != nil {
		return nil, fmt.Errorf("create shared read object: %w", err)
	}
	if object.file == nil || object.unlink == nil {
		if object.file != nil {
			_ = object.file.Close()
		}
		return nil, errors.New("shared read object is incomplete")
	}
	file := object.file
	if err := object.unlink(); err != nil {
		return nil, errors.Join(fmt.Errorf("unlink shared read object: %w", err), file.Close(), object.unlink())
	}
	mappedLength := nativeSharedReadMappedLength(length)
	if err := file.Truncate(int64(mappedLength)); err != nil {
		return nil, errors.Join(fmt.Errorf("size shared read file: %w", err), file.Close())
	}
	mapping, err := unix.Mmap(int(file.Fd()), 0, mappedLength, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("map shared read file: %w", err), file.Close())
	}
	return &nativeSharedReadWindow{file: file, mapping: mapping, capacity: length}, nil
}

func newNativeRegularFile() (nativeSharedReadObject, error) {
	file, err := os.CreateTemp("", ".codexfold-shared-window-*")
	if err != nil {
		return nativeSharedReadObject{}, err
	}
	if err := file.Chmod(0o600); err != nil {
		return nativeSharedReadObject{}, errors.Join(err, file.Close(), os.Remove(file.Name()))
	}
	path := file.Name()
	return nativeSharedReadObject{
		file: file,
		unlink: func() error {
			return os.Remove(path)
		},
	}, nil
}

func (w *nativeSharedReadWindow) Close() error {
	if w == nil {
		return nil
	}
	var result error
	if w.mapping != nil {
		result = errors.Join(result, unix.Munmap(w.mapping))
		w.mapping = nil
	}
	if w.file != nil {
		result = errors.Join(result, w.file.Close())
		w.file = nil
	}
	return result
}

func nativeSharedReadMappedLength(length int) int {
	pageBytes := os.Getpagesize()
	return (length + pageBytes - 1) / pageBytes * pageBytes
}

func newNativePOSIXSharedMemory() (nativeSharedReadObject, error) {
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nativeSharedReadObject{}, err
	}
	name := fmt.Sprintf("/cfs-%x", entropy[:])
	pointer, err := unix.BytePtrFromString(name)
	if err != nil {
		return nativeSharedReadObject{}, err
	}
	descriptor, _, errno := unix.Syscall(
		unix.SYS_SHM_OPEN,
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unix.O_RDWR|unix.O_CREAT|unix.O_EXCL),
		uintptr(0o600),
	)
	if errno != 0 {
		return nativeSharedReadObject{}, errno
	}
	file := os.NewFile(descriptor, name)
	if file == nil {
		_ = unix.Close(int(descriptor))
		return nativeSharedReadObject{}, errors.New("construct POSIX shared memory file")
	}
	return nativeSharedReadObject{
		file: file,
		unlink: func() error {
			namePointer, err := unix.BytePtrFromString(name)
			if err != nil {
				return err
			}
			_, _, errno := unix.Syscall(unix.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(namePointer)), 0, 0)
			if errno != 0 {
				return errno
			}
			return nil
		},
	}, nil
}
