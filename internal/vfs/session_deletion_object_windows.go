//go:build windows

package vfs

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var deletionVolumeInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetVolumeInformationByHandleW")
var deletionQueryEA = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryEaFile")

func sessionDeletionObjectFromFile(file *os.File, info os.FileInfo) (SessionDeletionPurgeObject, error) {
	if file == nil || info == nil {
		return SessionDeletionPurgeObject{}, errors.New("missing Windows deletion object")
	}
	var filesystem [32]uint16
	result, _, callErr := deletionVolumeInformation.Call(file.Fd(), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&filesystem[0])), uintptr(len(filesystem)))
	if result == 0 {
		return SessionDeletionPurgeObject{}, fmt.Errorf("identify deletion filesystem: %w", callErr)
	}
	if !strings.EqualFold(windows.UTF16ToString(filesystem[:]), "NTFS") {
		return SessionDeletionPurgeObject{}, errors.New("exact Windows session deletion requires NTFS")
	}
	var native windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &native); err != nil {
		return SessionDeletionPurgeObject{}, err
	}
	reference := uint64(native.FileIndexHigh)<<32 | uint64(native.FileIndexLow)
	if native.VolumeSerialNumber == 0 || reference == 0 || reference>>48 == 0 || native.CreationTime.Nanoseconds() <= 0 {
		return SessionDeletionPurgeObject{}, errors.New("NTFS object has no proven sequence and creation time")
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return SessionDeletionPurgeObject{}, fmt.Errorf("capture deletion security descriptor: %w", err)
	}
	security := sha256.Sum256([]byte(descriptor.String()))
	return SessionDeletionPurgeObject{IdentityProof: sessionDeletionObjectProofWindows, GenerationSource: sessionDeletionGenerationSourceWindows, BirthtimeSource: sessionDeletionBirthtimeSourceWindows, Device: uint64(native.VolumeSerialNumber), Inode: reference, Generation: reference >> 48, BirthtimeUnixNano: native.CreationTime.Nanoseconds(), SecuritySHA256: hex.EncodeToString(security[:]), Attributes: native.FileAttributes}, nil
}

func captureSessionDeletionXattrs(file *os.File, info os.FileInfo, path string) (SessionDeletionPurgeXattrs, error) {
	var native windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &native); err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	const ordinaryAttributes = windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED
	if native.FileAttributes&^uint32(ordinaryAttributes) != 0 {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("unproved Windows attributes prevent deletion purge: %s", path)
	}
	if err := requireNoDeletionStreams(file); err != nil {
		return SessionDeletionPurgeXattrs{}, err
	}
	buffer := make([]byte, 65536)
	var status windows.IO_STATUS_BLOCK
	result, _, _ := deletionQueryEA.Call(file.Fd(), uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0, 0, 0, 1)
	runtime.KeepAlive(file)
	runtime.KeepAlive(buffer)
	const statusNoEAsOnFile = uint32(0xc0000052)
	if uint32(result) != statusNoEAsOnFile {
		return SessionDeletionPurgeXattrs{}, fmt.Errorf("unproved Windows extended attributes prevent deletion purge (status=%#x): %s", uint32(result), path)
	}
	empty := sha256.Sum256(nil)
	return SessionDeletionPurgeXattrs{SHA256: hex.EncodeToString(empty[:])}, nil
}

func requireNoDeletionStreams(file *os.File) error {
	buffer := make([]byte, 65536)
	const fileStreamInfo = 7
	if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), fileStreamInfo, &buffer[0], uint32(len(buffer))); err != nil {
		if errors.Is(err, windows.ERROR_HANDLE_EOF) {
			return nil
		}
		return fmt.Errorf("enumerate deletion object streams: %w", err)
	}
	for offset := 0; ; {
		if offset < 0 || offset+24 > len(buffer) {
			return errors.New("invalid deletion stream metadata")
		}
		next := int(binary.LittleEndian.Uint32(buffer[offset:]))
		length := int(binary.LittleEndian.Uint32(buffer[offset+4:]))
		if length == 0 && next == 0 {
			return nil
		}
		if length <= 0 || length%2 != 0 || offset+24+length > len(buffer) {
			return errors.New("invalid deletion stream name")
		}
		name := make([]uint16, length/2)
		for index := range name {
			name[index] = binary.LittleEndian.Uint16(buffer[offset+24+index*2:])
		}
		if windows.UTF16ToString(name) != "::$DATA" {
			return errors.New("alternate streams prevent exact session deletion purge")
		}
		if next == 0 {
			return nil
		}
		if next < 24+length {
			return errors.New("invalid deletion stream offset")
		}
		offset += next
	}
}
