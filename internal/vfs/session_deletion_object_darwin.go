//go:build darwin

package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func sessionDeletionObjectFromFile(_ *os.File, info os.FileInfo) (SessionDeletionPurgeObject, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return SessionDeletionPurgeObject{}, errors.New("session deletion object has no Darwin identity")
	}
	return SessionDeletionPurgeObject{
		IdentityProof:     sessionDeletionObjectProofDarwin,
		GenerationSource:  sessionDeletionGenerationSourceDarwin,
		BirthtimeSource:   sessionDeletionBirthtimeSourceDarwin,
		Device:            uint64(stat.Dev),
		Inode:             stat.Ino,
		Generation:        uint64(stat.Gen),
		BirthtimeUnixNano: stat.Birthtimespec.Sec*1e9 + int64(stat.Birthtimespec.Nsec),
		UID:               stat.Uid,
		GID:               stat.Gid,
	}, nil
}

func captureSessionDeletionXattrs(file *os.File, info os.FileInfo, path string) (SessionDeletionPurgeXattrs, error) {
	first, err := captureSessionDeletionXattrsOnce(file, info, path)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	second, err := captureSessionDeletionXattrsOnce(file, info, path)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	if first != second {
		return SessionDeletionPurgeXattrs{}, errors.New("extended attributes changed while exact purge metadata was captured")
	}
	return first, nil
}

func captureSessionDeletionXattrsOnce(file *os.File, info os.FileInfo, path string) (SessionDeletionPurgeXattrs, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return SessionDeletionPurgeXattrs{}, errors.New("session deletion object has no Darwin metadata")
	}
	if stat.Flags != 0 {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("BSD flags prevent exact session deletion purge: %s", path)
	}
	hasACL, err := sessionDeletionFileHasACL(file)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("inspect ACL before session deletion purge: %w", err)
	}
	if hasACL {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("ACL prevents exact session deletion purge: %s", path)
	}
	names, err := sessionDeletionXattrNames(file)
	if err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	for _, name := range names {
		if name != "com.apple.provenance" {
			return SessionDeletionPurgeXattrs{}, fmt.Errorf("unproved extended attribute prevents exact session deletion purge: %s: %s", path, name)
		}
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
			return SessionDeletionPurgeXattrs{}, fmt.Errorf("read extended attribute %q: %w", name, err)
		}
		total += int64(read)
		valueSHA := sha256.Sum256(value)
		_, _ = io.WriteString(hasher, name+"\x00"+strconv.Itoa(read)+"\x00"+hex.EncodeToString(valueSHA[:])+"\n")
	}
	afterNames, err := sessionDeletionXattrNames(file)
	if err != nil || !sameDeletionPurgeStrings(names, afterNames) {
		if err == nil {
			err = errors.New("extended attribute names changed while reading")
		}
		return SessionDeletionPurgeXattrs{}, err
	}
	return SessionDeletionPurgeXattrs{Count: len(names), Bytes: total, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func sessionDeletionFileHasACL(file *os.File) (bool, error) {
	attributes := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	buffer := make([]byte, 32)
	_, _, errno := unix.Syscall6(
		unix.SYS_FGETATTRLIST,
		file.Fd(),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		uintptr(unix.FSOPT_REPORT_FULLSIZE),
		0,
	)
	runtime.KeepAlive(file)
	runtime.KeepAlive(&attributes)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return false, errno
	}
	if len(buffer) < 24 {
		return false, errors.New("Darwin extended-security response is truncated")
	}
	total := binary.LittleEndian.Uint32(buffer[:4])
	if total < 24 {
		return false, errors.New("Darwin extended-security response has an invalid length")
	}
	returnedCommon := binary.LittleEndian.Uint32(buffer[4:8])
	if returnedCommon&unix.ATTR_CMN_RETURNED_ATTRS == 0 {
		return false, errors.New("Darwin extended-security response omitted returned attributes")
	}
	return returnedCommon&unix.ATTR_CMN_EXTENDED_SECURITY != 0, nil
}

func sessionDeletionXattrNames(file *os.File) ([]string, error) {
	size, err := unix.Flistxattr(int(file.Fd()), nil)
	if err != nil {
		return nil, fmt.Errorf("size extended attribute list: %w", err)
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
	for index := 1; index < len(names); index++ {
		if names[index] == names[index-1] {
			return nil, errors.New("extended attribute list contains a duplicate name")
		}
	}
	return names, nil
}
