//go:build linux

package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sessionDeletionLinuxStatxMask            = unix.STATX_BASIC_STATS | unix.STATX_BTIME
	sessionDeletionNanosecondsPerSecondLinux = int64(1_000_000_000)
)

var (
	sessionDeletionStatxLinux      = unix.Statx
	sessionDeletionGenerationLinux = readSessionDeletionGenerationLinux
	sessionDeletionIoctlLinux      = invokeSessionDeletionIoctlLinux
)

func sessionDeletionObjectFromFile(file *os.File, info os.FileInfo) (SessionDeletionPurgeObject, error) {
	if file == nil || info == nil {
		return SessionDeletionPurgeObject{}, errors.New("session deletion object has no open Linux identity")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return SessionDeletionPurgeObject{}, errors.New("session deletion object has no Linux identity")
	}

	var statx unix.Statx_t
	if err := sessionDeletionStatxLinux(
		int(file.Fd()),
		"",
		unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT,
		sessionDeletionLinuxStatxMask,
		&statx,
	); err != nil {
		return SessionDeletionPurgeObject{}, fmt.Errorf("read Linux session deletion identity with statx: %w", err)
	}
	requiredMask := uint32(sessionDeletionLinuxStatxMask)
	if statx.Mask&requiredMask != requiredMask {
		return SessionDeletionPurgeObject{}, fmt.Errorf("Linux statx identity lacks required mask %#x", requiredMask)
	}
	if statx.Ino == 0 {
		return SessionDeletionPurgeObject{}, errors.New("Linux statx identity has a zero inode")
	}
	device := unix.Mkdev(statx.Dev_major, statx.Dev_minor)
	if device == 0 {
		return SessionDeletionPurgeObject{}, errors.New("Linux statx identity has a zero device")
	}
	if device != uint64(stat.Dev) || statx.Ino != uint64(stat.Ino) || statx.Uid != stat.Uid || statx.Gid != stat.Gid {
		return SessionDeletionPurgeObject{}, errors.New("Linux statx identity differs from the open file descriptor")
	}
	birthtime, err := sessionDeletionBirthtimeUnixNanoLinux(statx.Btime)
	if err != nil {
		return SessionDeletionPurgeObject{}, err
	}
	generation, err := sessionDeletionGenerationLinux(file)
	if err != nil {
		return SessionDeletionPurgeObject{}, err
	}
	if generation == 0 {
		return SessionDeletionPurgeObject{}, errors.New("Linux inode generation is zero")
	}

	return SessionDeletionPurgeObject{
		IdentityProof:     sessionDeletionObjectProofLinux,
		GenerationSource:  sessionDeletionGenerationSourceLinux,
		BirthtimeSource:   sessionDeletionBirthtimeSourceLinux,
		Device:            device,
		Inode:             statx.Ino,
		Generation:        generation,
		BirthtimeUnixNano: birthtime,
		UID:               statx.Uid,
		GID:               statx.Gid,
	}, nil
}

func sessionDeletionBirthtimeUnixNanoLinux(timestamp unix.StatxTimestamp) (int64, error) {
	if timestamp.Sec == 0 && timestamp.Nsec == 0 {
		return 0, errors.New("Linux statx identity has no birth time")
	}
	if timestamp.Nsec >= uint32(sessionDeletionNanosecondsPerSecondLinux) {
		return 0, errors.New("Linux statx identity has an invalid birth-time nanosecond value")
	}
	if timestamp.Sec > math.MaxInt64/sessionDeletionNanosecondsPerSecondLinux || timestamp.Sec < math.MinInt64/sessionDeletionNanosecondsPerSecondLinux {
		return 0, errors.New("Linux statx birth time overflows Unix nanoseconds")
	}
	birthtime := timestamp.Sec * sessionDeletionNanosecondsPerSecondLinux
	if birthtime > math.MaxInt64-int64(timestamp.Nsec) {
		return 0, errors.New("Linux statx birth time overflows Unix nanoseconds")
	}
	birthtime += int64(timestamp.Nsec)
	if birthtime <= 0 {
		return 0, errors.New("Linux statx identity has a nonpositive birth time")
	}
	return birthtime, nil
}

func sessionDeletionFSIOCGetVersionRequestLinux() uintptr {
	const ioctlTypeMask = uintptr(0xff) << 8
	return uintptr(unix.FS_IOC_GETFLAGS)&^ioctlTypeMask | uintptr('v')<<8
}

func readSessionDeletionGenerationLinux(file *os.File) (uint64, error) {
	var version int32
	errno := sessionDeletionIoctlLinux(
		file.Fd(),
		sessionDeletionFSIOCGetVersionRequestLinux(),
		uintptr(unsafe.Pointer(&version)),
	)
	runtime.KeepAlive(file)
	if errno != 0 {
		return 0, fmt.Errorf("read Linux inode generation with FS_IOC_GETVERSION: %w", errno)
	}
	generation := uint64(uint32(version))
	if generation == 0 {
		return 0, errors.New("Linux FS_IOC_GETVERSION returned a zero inode generation")
	}
	return generation, nil
}

func invokeSessionDeletionIoctlLinux(fileDescriptor uintptr, request uintptr, argument uintptr) unix.Errno {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fileDescriptor, request, argument)
	return errno
}

func captureSessionDeletionXattrs(file *os.File, _ os.FileInfo, path string) (SessionDeletionPurgeXattrs, error) {
	first, err := captureSessionDeletionXattrsOnceLinux(file, path)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	second, err := captureSessionDeletionXattrsOnceLinux(file, path)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	if first != second {
		return SessionDeletionPurgeXattrs{}, errors.New("extended attributes changed while exact purge metadata was captured")
	}
	return first, nil
}

func captureSessionDeletionXattrsOnceLinux(file *os.File, path string) (SessionDeletionPurgeXattrs, error) {
	names, err := sessionDeletionXattrNamesLinux(file)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	if len(names) != 0 {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("unproved extended attributes prevent exact session deletion purge: %s", path)
	}
	hasher := sha256.New()
	var total int64
	for _, name := range names {
		size, err := unix.Fgetxattr(int(file.Fd()), name, nil)
		if err != nil {
			return SessionDeletionPurgeXattrs{}, fmt.Errorf("size extended attribute %q: %w", name, err)
		}
		if size < 0 || size > 64<<20 || total > int64(64<<20-size) {
			return SessionDeletionPurgeXattrs{}, fmt.Errorf("extended attributes exceed the exact purge proof limit: %s", path)
		}
		value := make([]byte, size)
		read, err := unix.Fgetxattr(int(file.Fd()), name, value)
		if err != nil || read != size {
			if err == nil {
				err = errors.New("extended attribute changed while reading")
			}
			return SessionDeletionPurgeXattrs{}, err
		}
		total += int64(read)
		valueSHA := sha256.Sum256(value)
		_, _ = io.WriteString(hasher, name+"\x00"+strconv.Itoa(read)+"\x00"+hex.EncodeToString(valueSHA[:])+"\n")
	}
	afterNames, err := sessionDeletionXattrNamesLinux(file)
	if err != nil || !sameDeletionPurgeStrings(names, afterNames) {
		if err == nil {
			err = errors.New("extended attribute names changed while reading")
		}
		return SessionDeletionPurgeXattrs{}, err
	}
	return SessionDeletionPurgeXattrs{Count: len(names), Bytes: total, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func sessionDeletionXattrNamesLinux(file *os.File) ([]string, error) {
	size, err := unix.Flistxattr(int(file.Fd()), nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	if size < 0 || size > 1<<20 {
		return nil, errors.New("extended attribute name list exceeds the exact purge proof limit")
	}
	buffer := make([]byte, size)
	read, err := unix.Flistxattr(int(file.Fd()), buffer)
	if err != nil || read != size {
		if err == nil {
			err = errors.New("extended attribute name list changed while reading")
		}
		return nil, err
	}
	parts := bytes.Split(buffer[:read], []byte{0})
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		name := string(part)
		if strings.ContainsRune(name, '\x00') {
			return nil, errors.New("extended attribute name contains NUL")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
