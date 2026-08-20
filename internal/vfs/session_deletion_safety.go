package vfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type sessionDeletionStoreRoot struct {
	path   string
	root   *os.Root
	device uint64
}

func openSessionDeletionStoreRoot(path string) (*sessionDeletionStoreRoot, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute session deletion store root is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session deletion store root is not a plain directory")
	}
	device, err := sessionDeletionFileDevice(info)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		_ = root.Close()
		if err == nil {
			err = errors.New("session deletion store root changed while opening")
		}
		return nil, err
	}
	if openedDevice, deviceErr := sessionDeletionFileDevice(opened); deviceErr != nil || device != 0 && openedDevice != device {
		_ = root.Close()
		if deviceErr != nil {
			return nil, deviceErr
		}
		return nil, errors.New("session deletion store root changed filesystem while opening")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		_ = root.Close()
		if err == nil {
			err = errors.New("session deletion store root changed while opening")
		}
		return nil, err
	}
	if currentDevice, deviceErr := sessionDeletionFileDevice(current); deviceErr != nil || device != 0 && currentDevice != device {
		_ = root.Close()
		if deviceErr != nil {
			return nil, deviceErr
		}
		return nil, errors.New("session deletion store root changed filesystem while opening")
	}
	return &sessionDeletionStoreRoot{path: path, root: root, device: device}, nil
}

func (s *sessionDeletionStoreRoot) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}

func (s *sessionDeletionStoreRoot) relative(absolute string) (string, error) {
	absolute = filepath.Clean(absolute)
	if !filepath.IsAbs(absolute) {
		return "", errors.New("session deletion path is not absolute")
	}
	relative, err := filepath.Rel(s.path, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("session deletion path escapes the store root")
	}
	return relative, nil
}

func (s *sessionDeletionStoreRoot) validatePlainAncestors(absolute string) error {
	return s.validatePlainAncestorChain(absolute, false)
}

func (s *sessionDeletionStoreRoot) validateExistingPlainAncestors(absolute string) error {
	return s.validatePlainAncestorChain(absolute, true)
}

func (s *sessionDeletionStoreRoot) validatePlainAncestorChain(absolute string, allowMissing bool) error {
	relative, err := s.relative(absolute)
	if err != nil {
		return err
	}
	parent := filepath.Dir(relative)
	if parent == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := s.root.Lstat(current)
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session deletion path contains an unsafe ancestor: %s", filepath.Join(s.path, current))
		}
		if err := s.requireSameDevice(info, filepath.Join(s.path, current)); err != nil {
			return err
		}
	}
	return nil
}

func (s *sessionDeletionStoreRoot) pathPair(source string, target string, directory bool) (bool, bool, error) {
	if err := s.validatePlainAncestors(source); err != nil {
		return false, false, err
	}
	if err := s.validateExistingPlainAncestors(target); err != nil {
		return false, false, err
	}
	sourceRelative, err := s.relative(source)
	if err != nil {
		return false, false, err
	}
	targetRelative, err := s.relative(target)
	if err != nil {
		return false, false, err
	}
	sourceInfo, sourceErr := s.root.Lstat(sourceRelative)
	targetInfo, targetErr := s.root.Lstat(targetRelative)
	sourceExists := sourceErr == nil
	targetExists := targetErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return false, false, sourceErr
	}
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return false, false, targetErr
	}
	if sourceExists && targetExists {
		return false, false, errors.New("session deletion source and quarantine target both exist")
	}
	for _, item := range []struct {
		exists bool
		info   os.FileInfo
		path   string
	}{
		{sourceExists, sourceInfo, source}, {targetExists, targetInfo, target},
	} {
		if !item.exists {
			continue
		}
		if item.info.Mode()&os.ModeSymlink != 0 || item.info.IsDir() != directory || (!directory && !item.info.Mode().IsRegular()) {
			return false, false, errors.New("session deletion source or quarantine target has an unsafe type")
		}
		if err := s.requireSameDevice(item.info, item.path); err != nil {
			return false, false, err
		}
	}
	return sourceExists, targetExists, nil
}

func (s *sessionDeletionStoreRoot) lstat(absolute string) (os.FileInfo, error) {
	if err := s.validatePlainAncestors(absolute); err != nil {
		return nil, err
	}
	relative, err := s.relative(absolute)
	if err != nil {
		return nil, err
	}
	info, err := s.root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if err := s.requireSameDevice(info, absolute); err != nil {
		return nil, err
	}
	return info, nil
}

func (s *sessionDeletionStoreRoot) requireSameDevice(info os.FileInfo, path string) error {
	device, err := sessionDeletionFileDevice(info)
	if err != nil {
		return err
	}
	if s.device != 0 && device != s.device {
		return fmt.Errorf("session deletion path crosses a filesystem boundary: %s", path)
	}
	return nil
}

func (s *sessionDeletionStoreRoot) acquireWriterLease(path string) (*os.File, error) {
	if err := s.validatePlainAncestors(path); err != nil {
		return nil, err
	}
	relative, err := s.relative(path)
	if err != nil {
		return nil, err
	}
	before, err := s.root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("session deletion writer lease is not a regular file")
	}
	if err := s.requireSameDevice(before, path); err != nil {
		return nil, err
	}
	file, err := s.root.OpenFile(relative, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion writer lease changed while opening")
		}
		return nil, err
	}
	if err := s.requireSameDevice(opened, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	locked, err := tryLockWriterFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, ErrWriterBusy
	}
	after, err := s.root.Lstat(relative)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		closeErr := errors.Join(unlockWriterFile(file), file.Close())
		if err == nil {
			err = errors.New("session deletion writer lease changed after locking")
		}
		return nil, errors.Join(err, closeErr)
	}
	if err := s.requireSameDevice(after, path); err != nil {
		closeErr := errors.Join(unlockWriterFile(file), file.Close())
		return nil, errors.Join(err, closeErr)
	}
	return file, nil
}

func (s *sessionDeletionStoreRoot) rename(source string, target string, directory bool) error {
	if err := s.validatePlainAncestors(source); err != nil {
		return err
	}
	if err := s.validatePlainAncestors(target); err != nil {
		return err
	}
	targetRelative, err := s.relative(target)
	if err != nil {
		return err
	}
	sourceInfo, err := s.lstat(source)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || sourceInfo.IsDir() != directory || (!directory && !sourceInfo.Mode().IsRegular()) {
		return errors.New("session deletion rename source has an unsafe type")
	}
	if _, err := s.root.Lstat(targetRelative); err == nil {
		return errors.New("session deletion rename target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := sessionDeletionRenameNoReplace(s, source, target); err != nil {
		return err
	}
	if err := s.validatePlainAncestors(target); err != nil {
		return err
	}
	targetInfo, err := s.lstat(target)
	if err != nil {
		return err
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || targetInfo.IsDir() != directory || (!directory && !targetInfo.Mode().IsRegular()) || !os.SameFile(sourceInfo, targetInfo) {
		return errors.New("session deletion rename target differs from the verified source")
	}
	return syncDeletionRenameParents(source, target)
}

func (s *sessionDeletionStoreRoot) commitRegularFile(path string, temporaryPrefix string, data []byte, replace bool) error {
	if err := s.validatePlainAncestors(path); err != nil {
		return err
	}
	relative, err := s.relative(path)
	if err != nil {
		return err
	}
	directory := filepath.Dir(relative)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporaryRelative := filepath.Join(directory, temporaryPrefix+hex.EncodeToString(random)+".tmp")
	temporary, err := s.root.OpenFile(temporaryRelative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.root.Remove(temporaryRelative)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if replace {
		info, err := s.root.Lstat(relative)
		if err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = errors.New("session deletion metadata replacement target is not a regular file")
			}
			return err
		}
		if err := s.requireSameDevice(info, path); err != nil {
			return err
		}
		if err := s.root.Rename(temporaryRelative, relative); err != nil {
			return err
		}
		cleanup = false
	} else {
		if err := s.root.Link(temporaryRelative, relative); err != nil {
			return err
		}
		if err := s.root.Remove(temporaryRelative); err != nil {
			return err
		}
		cleanup = false
	}
	if err := syncStateDirectory(filepath.Join(s.path, directory)); err != nil {
		return err
	}
	persisted, _, err := s.readStableRegularFile(path, int64(len(data))+1)
	if err != nil {
		return err
	}
	if string(persisted) != string(data) {
		return errors.New("published session deletion metadata differs from its intended bytes")
	}
	return nil
}

func captureStableDeletionFileWithinStore(storeRoot string, path string) (NativeFile, error) {
	store, err := openSessionDeletionStoreRoot(storeRoot)
	if err != nil {
		return NativeFile{}, err
	}
	defer store.Close()
	return store.captureStableRegularFile(path)
}

func (s *sessionDeletionStoreRoot) captureStableRegularFile(path string) (NativeFile, error) {
	first, err := s.captureRegularFile(path)
	if err != nil {
		return NativeFile{}, err
	}
	second, err := s.captureRegularFile(path)
	if err != nil {
		return NativeFile{}, err
	}
	if first != second {
		return NativeFile{}, errors.New("file changed while its store-confined deletion proof was captured")
	}
	return first, nil
}

func (s *sessionDeletionStoreRoot) readStableRegularFile(path string, maximumBytes int64) ([]byte, NativeFile, error) {
	before, err := s.captureStableRegularFile(path)
	if err != nil {
		return nil, NativeFile{}, err
	}
	if maximumBytes > 0 && before.Bytes > maximumBytes {
		return nil, NativeFile{}, errors.New("session deletion metadata exceeds its size limit")
	}
	relative, err := s.relative(path)
	if err != nil {
		return nil, NativeFile{}, err
	}
	data, err := s.root.ReadFile(relative)
	if err != nil {
		return nil, NativeFile{}, err
	}
	after, err := s.captureStableRegularFile(path)
	if err != nil {
		return nil, NativeFile{}, err
	}
	if before != after || int64(len(data)) != before.Bytes || digestDeletionBytes(data) != before.SHA256 {
		return nil, NativeFile{}, errors.New("session deletion metadata changed while reading")
	}
	return data, before, nil
}

func (s *sessionDeletionStoreRoot) captureRegularFile(path string) (NativeFile, error) {
	if err := s.validatePlainAncestors(path); err != nil {
		return NativeFile{}, err
	}
	relative, err := s.relative(path)
	if err != nil {
		return NativeFile{}, err
	}
	before, err := s.root.Lstat(relative)
	if err != nil {
		return NativeFile{}, err
	}
	if !before.Mode().IsRegular() {
		return NativeFile{}, errors.New("session deletion proof target is not a regular file")
	}
	if err := s.requireSameDevice(before, path); err != nil {
		return NativeFile{}, err
	}
	file, err := s.root.Open(relative)
	if err != nil {
		return NativeFile{}, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		if err == nil {
			err = errors.New("session deletion proof target changed while opening")
		}
		return NativeFile{}, err
	}
	if err := s.requireSameDevice(opened, path); err != nil {
		_ = file.Close()
		return NativeFile{}, err
	}
	hasher := sha256.New()
	bytes, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	after, statErr := s.root.Lstat(relative)
	if err := errors.Join(copyErr, closeErr, statErr); err != nil {
		return NativeFile{}, err
	}
	if err := s.requireSameDevice(after, path); err != nil {
		return NativeFile{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || bytes != after.Size() {
		return NativeFile{}, errors.New("session deletion proof target changed while hashing")
	}
	return NativeFile{Path: filepath.Clean(path), Bytes: bytes, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}
