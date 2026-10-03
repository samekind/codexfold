package service

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/buildid"
)

// An offline frontend update must also select its new engine image. Retain the
// previous binding so the existing binary-update rollback can restore both.
// The caller has already stopped the owned Windows service.
func PrepareWindowsCoreOfflineUpdate(binary string) (func() error, error) {
	root := filepath.Join(filepath.Dir(binary), "Core")
	bindingPath := filepath.Join(root, "current.json")
	previous, err := os.ReadFile(bindingPath)
	if errors.Is(err, os.ErrNotExist) {
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := configuredWindowsCoreImage(binary); err != nil {
		return nil, err
	}
	sha, err := buildid.FileSHA256(binary)
	if err != nil {
		return nil, err
	}
	image := filepath.Join(root, sha+".exe")
	if info, err := os.Lstat(image); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("offline engine image is not an ordinary file")
		}
		if actual, err := buildid.FileSHA256(image); err != nil || actual != sha {
			return nil, errors.New("offline engine image failed verification")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		source, err := os.Open(binary)
		if err != nil {
			return nil, err
		}
		defer source.Close()
		target, err := os.CreateTemp(root, ".offline-engine-*.tmp")
		if err != nil {
			return nil, err
		}
		name := target.Name()
		defer os.Remove(name)
		_, copyErr := io.Copy(target, source)
		syncErr := target.Sync()
		closeErr := target.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return nil, err
		}
		if actual, err := buildid.FileSHA256(name); err != nil || actual != sha {
			return nil, errors.New("offline engine source changed")
		}
		if err := os.Rename(name, image); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	data, _ := json.Marshal(struct {
		Version int
		SHA256  string
	}{1, sha})
	if _, err := WriteDefinition(bindingPath, data, true); err != nil {
		return nil, err
	}
	return func() error { _, err := WriteDefinition(bindingPath, previous, true); return err }, nil
}
