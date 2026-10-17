//go:build windows

package vfs

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type deletionRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

const deletionFileRenameInformationEx = 65

func sessionDeletionRenameNoReplace(store *sessionDeletionStoreRoot, source, target string) error {
	sourceRelative, err := store.relative(source)
	if err != nil {
		return fmt.Errorf("open deletion rename source path: %w", err)
	}
	targetRelative, err := store.relative(target)
	if err != nil {
		return err
	}
	sourceParent, err := store.root.Open(filepath.Dir(sourceRelative))
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	targetParent, err := store.root.Open(filepath.Dir(targetRelative))
	if err != nil {
		return err
	}
	defer targetParent.Close()
	directoryName, err := windows.NewNTUnicodeString("")
	if err != nil {
		return err
	}
	parentAttributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(targetParent.Fd()), ObjectName: directoryName}
	parentAttributes.Length = uint32(unsafe.Sizeof(parentAttributes))
	var writableParent windows.Handle
	var parentStatus windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&writableParent, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.FILE_TRAVERSE, &parentAttributes, &parentStatus, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return fmt.Errorf("open writable quarantine parent: %w", err)
	}
	defer windows.CloseHandle(writableParent)
	name, err := windows.NewNTUnicodeString(filepath.Base(sourceRelative))
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(sourceParent.Fd()), ObjectName: name}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	newName, err := windows.UTF16FromString(filepath.Base(targetRelative))
	if err != nil {
		return err
	}
	var layout deletionRenameInformation
	header := int(unsafe.Offsetof(layout.FileName))
	length := (len(newName) - 1) * 2
	buffer := make([]byte, header+length)
	information := (*deletionRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.ReplaceIfExists = windows.FILE_RENAME_POSIX_SEMANTICS
	information.RootDirectory = writableParent
	information.FileNameLength = uint32(length)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[header])), len(newName)-1), newName[:len(newName)-1])
	// FILE_RENAME_REPLACE_IF_EXISTS stays clear. Source and destination parents are
	// open handles anchored in os.Root, not freshly resolved absolute paths.
	if err := windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), deletionFileRenameInformationEx); err != nil {
		return fmt.Errorf("rename deletion quarantine: %w", err)
	}
	return nil
}
