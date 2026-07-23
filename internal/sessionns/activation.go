package sessionns

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	actionActivate   = "activate"
	actionDeactivate = "deactivate"
)

var sessionDirectories = []string{"sessions", "archived_sessions"}

type Options struct {
	Home       string
	Mount      string
	NativeRoot string
	MountProbe func(string) error
}

type Result struct {
	Active     bool   `json:"active"`
	Recovered  bool   `json:"recovered"`
	Home       string `json:"home"`
	Mount      string `json:"mount"`
	NativeRoot string `json:"native_root"`
	Journal    string `json:"journal"`
}

type journal struct {
	Version int    `json:"version"`
	Action  string `json:"action"`
}

func Inspect(options Options) (Result, error) {
	options, err := validate(options)
	if err != nil {
		return Result{}, err
	}
	result := resultFor(options)
	activeLinks := 0
	nativeDirectories := 0
	nativeEntries := 0
	for _, name := range sessionDirectories {
		homePath := filepath.Join(options.Home, name)
		info, err := os.Lstat(homePath)
		if err != nil {
			return Result{}, fmt.Errorf("inspect %s: %w", homePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(homePath)
			if err != nil || filepath.Clean(target) != filepath.Join(options.Mount, name) {
				return Result{}, fmt.Errorf("unexpected namespace link %s", homePath)
			}
			activeLinks++
		} else if !info.IsDir() {
			return Result{}, fmt.Errorf("namespace source is not a directory: %s", homePath)
		}
		nativePath := filepath.Join(options.NativeRoot, name)
		if info, err := os.Stat(nativePath); err == nil && info.IsDir() {
			nativeDirectories++
			entries, err := os.ReadDir(nativePath)
			if err != nil {
				return Result{}, err
			}
			nativeEntries += len(entries)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	}
	if activeLinks == len(sessionDirectories) && nativeDirectories == len(sessionDirectories) {
		result.Active = true
		return result, nil
	}
	if activeLinks == 0 && nativeEntries == 0 {
		return result, nil
	}
	return Result{}, errors.New("session namespace is partially activated")
}

func Activate(options Options) (Result, error) {
	options, err := validate(options)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(journalPath(options)); err == nil {
		if _, recoverErr := Recover(options); recoverErr != nil {
			return Result{}, recoverErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if options.MountProbe == nil {
		return Result{}, errors.New("mount identity probe is required for namespace activation")
	}
	if err := options.MountProbe(options.Mount); err != nil {
		return Result{}, fmt.Errorf("canonical mount identity is not healthy: %w", err)
	}
	status, err := Inspect(options)
	if err == nil && status.Active {
		if err := installRouteGuard(options); err != nil {
			return Result{}, err
		}
		return status, nil
	}
	if err != nil {
		return Result{}, err
	}
	for _, name := range sessionDirectories {
		if info, err := os.Stat(filepath.Join(options.Mount, name)); err != nil || !info.IsDir() {
			return Result{}, fmt.Errorf("canonical mount directory is unavailable: %s", filepath.Join(options.Mount, name))
		}
	}
	if err := os.MkdirAll(options.NativeRoot, 0o700); err != nil {
		return Result{}, err
	}
	for _, name := range sessionDirectories {
		if err := removeEmptyDirectory(filepath.Join(options.NativeRoot, name)); err != nil {
			return Result{}, err
		}
	}
	if err := writeJournal(options, journal{Version: 1, Action: actionActivate}); err != nil {
		return Result{}, err
	}
	for _, name := range sessionDirectories {
		homePath := filepath.Join(options.Home, name)
		nativePath := filepath.Join(options.NativeRoot, name)
		if err := os.Rename(homePath, nativePath); err != nil {
			return rollbackAfterError(options, err)
		}
		if err := os.Symlink(filepath.Join(options.Mount, name), homePath); err != nil {
			return rollbackAfterError(options, err)
		}
	}
	if err := installRouteGuard(options); err != nil {
		return rollbackAfterError(options, err)
	}
	if err := removeJournal(options); err != nil {
		return Result{}, err
	}
	return Inspect(options)
}

func Deactivate(options Options) (Result, error) {
	options, err := validate(options)
	if err != nil {
		return Result{}, err
	}
	status, err := Recover(options)
	if err != nil {
		return Result{}, err
	}
	if !status.Active {
		return status, nil
	}
	if err := writeJournal(options, journal{Version: 1, Action: actionDeactivate}); err != nil {
		return Result{}, err
	}
	for _, name := range sessionDirectories {
		homePath := filepath.Join(options.Home, name)
		if err := os.Remove(homePath); err != nil {
			return finishAfterError(options, err)
		}
		if err := os.Rename(filepath.Join(options.NativeRoot, name), homePath); err != nil {
			return finishAfterError(options, err)
		}
	}
	if err := removeRouteGuard(options); err != nil {
		return finishAfterError(options, err)
	}
	if err := removeJournal(options); err != nil {
		return Result{}, err
	}
	return Inspect(options)
}

func Recover(options Options) (Result, error) {
	options, err := validate(options)
	if err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(journalPath(options))
	if errors.Is(err, os.ErrNotExist) {
		return Inspect(options)
	}
	if err != nil {
		return Result{}, err
	}
	var transaction journal
	if err := json.Unmarshal(data, &transaction); err != nil || transaction.Version != 1 {
		return Result{}, errors.New("invalid session namespace journal")
	}
	if transaction.Action != actionActivate && transaction.Action != actionDeactivate {
		return Result{}, errors.New("unknown session namespace journal action")
	}
	for _, name := range sessionDirectories {
		homePath := filepath.Join(options.Home, name)
		nativePath := filepath.Join(options.NativeRoot, name)
		if info, err := os.Lstat(homePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(homePath); err != nil {
				return Result{}, err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		if _, err := os.Lstat(homePath); errors.Is(err, os.ErrNotExist) {
			if info, nativeErr := os.Stat(nativePath); nativeErr == nil && info.IsDir() {
				if err := os.Rename(nativePath, homePath); err != nil {
					return Result{}, err
				}
			} else if nativeErr != nil && !errors.Is(nativeErr, os.ErrNotExist) {
				return Result{}, nativeErr
			}
		}
	}
	if err := removeRouteGuard(options); err != nil {
		return Result{}, err
	}
	if err := removeJournal(options); err != nil {
		return Result{}, err
	}
	result, err := Inspect(options)
	if err != nil {
		return Result{}, err
	}
	result.Recovered = true
	return result, nil
}

func rollbackAfterError(options Options, cause error) (Result, error) {
	_, recoverErr := Recover(options)
	if recoverErr != nil {
		return Result{}, errors.Join(cause, recoverErr)
	}
	return Result{}, cause
}

func finishAfterError(options Options, cause error) (Result, error) {
	_, recoverErr := Recover(options)
	if recoverErr != nil {
		return Result{}, errors.Join(cause, recoverErr)
	}
	return Result{}, cause
}

func validate(options Options) (Options, error) {
	if !filepath.IsAbs(options.Home) || !filepath.IsAbs(options.Mount) || !filepath.IsAbs(options.NativeRoot) {
		return Options{}, errors.New("absolute home, mount, and native root paths are required")
	}
	options.Home = filepath.Clean(options.Home)
	options.Mount = filepath.Clean(options.Mount)
	options.NativeRoot = filepath.Clean(options.NativeRoot)
	if options.Home == options.Mount || options.Home == options.NativeRoot || options.Mount == options.NativeRoot {
		return Options{}, errors.New("home, mount, and native root paths must be distinct")
	}
	return options, nil
}

func resultFor(options Options) Result {
	return Result{Home: options.Home, Mount: options.Mount, NativeRoot: options.NativeRoot, Journal: journalPath(options)}
}

func journalPath(options Options) string {
	return filepath.Join(options.Home, ".codexfold-namespace.json")
}

func writeJournal(options Options, transaction journal) error {
	data, err := json.Marshal(transaction)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(options.Home, ".codexfold-namespace-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
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
	if err := os.Rename(temporaryPath, journalPath(options)); err != nil {
		return err
	}
	return syncDirectory(options.Home)
}

func removeJournal(options Options) error {
	if err := os.Remove(journalPath(options)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(options.Home)
}

func removeEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("native namespace destination is not empty: %s", path)
	}
	return os.Remove(path)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
