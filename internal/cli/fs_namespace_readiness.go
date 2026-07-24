package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/sessionns"
)

type canonicalNamespaceReadiness struct {
	Active bool
	Ready  bool
}

// enrollmentCanonicalNamespaceReadinessProbe is replaceable in tests. A
// restart can briefly expose a healthy mount before native passthrough entries
// have repopulated, so readiness is intentionally distinct from mount health.
var enrollmentCanonicalNamespaceReadinessProbe = probeEnrollmentCanonicalNamespaceReadiness

func probeEnrollmentCanonicalNamespaceReadiness(home string, mount string, nativeRoot string) canonicalNamespaceReadiness {
	status, err := sessionns.Inspect(sessionns.Options{Home: home, Mount: mount, NativeRoot: nativeRoot, MountProbe: mountHealthProbe})
	if err != nil || !status.Active {
		return canonicalNamespaceReadiness{}
	}
	if err := probeCanonicalNativePassthrough(mount, nativeRoot); err != nil {
		return canonicalNamespaceReadiness{Active: true}
	}
	return canonicalNamespaceReadiness{Active: true, Ready: true}
}

// probeCanonicalNativePassthrough verifies that every unmanaged native entry
// currently has a corresponding path in the mounted namespace. It checks
// visibility and kind only, not content, so active appends cannot turn a
// readiness check into a false mismatch.
func probeCanonicalNativePassthrough(mount string, nativeRoot string) error {
	mount = filepath.Clean(mount)
	nativeRoot = filepath.Clean(nativeRoot)
	for _, namespace := range []string{"sessions", "archived_sessions"} {
		nativeBase := filepath.Join(nativeRoot, namespace)
		mountedBase := filepath.Join(mount, namespace)
		if err := requireDirectory(nativeBase); err != nil {
			return fmt.Errorf("native %s namespace: %w", namespace, err)
		}
		if err := requireDirectory(mountedBase); err != nil {
			return fmt.Errorf("mounted %s namespace: %w", namespace, err)
		}
		err := filepath.WalkDir(nativeBase, func(nativePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(nativeBase, nativePath)
			if err != nil || relative == "." {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("native path %s is a symbolic link", nativePath)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			mountedPath := filepath.Join(mountedBase, relative)
			mounted, err := os.Lstat(mountedPath)
			if err != nil {
				return fmt.Errorf("native path %s is not visible at %s: %w", nativePath, mountedPath, err)
			}
			if mounted.IsDir() != info.IsDir() || (!info.IsDir() && !mounted.Mode().IsRegular()) {
				return fmt.Errorf("native path %s and mounted path %s have different kinds", nativePath, mountedPath)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return nil
}
