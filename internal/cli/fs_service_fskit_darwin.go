//go:build darwin

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jstar0/codexfold/internal/service"
)

const launchServicesRegister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

type darwinFSKitAppTransaction struct {
	target       string
	source       string
	stageRoot    string
	backupRoot   string
	backupPath   string
	appGroupPath string
	changed      bool
	hadTarget    bool
}

func prepareFSKitAppPlatform(ctx context.Context, source string, target string) (fsKitAppTransaction, error) {
	if !filepath.IsAbs(target) {
		return nil, errors.New("installed FSKit app path must be absolute")
	}
	target = filepath.Clean(target)
	if source == "" {
		appGroup, err := validateAndEnableFSKitApp(ctx, target)
		if err != nil {
			return nil, err
		}
		return &darwinFSKitAppTransaction{target: target, appGroupPath: appGroup}, nil
	}
	if !filepath.IsAbs(source) {
		return nil, errors.New("FSKit app source path must be absolute")
	}
	source = filepath.Clean(source)
	if source == target {
		appGroup, err := validateAndEnableFSKitApp(ctx, target)
		if err != nil {
			return nil, err
		}
		return &darwinFSKitAppTransaction{target: target, appGroupPath: appGroup}, nil
	}
	if err := validateFSKitApp(ctx, source); err != nil {
		return nil, fmt.Errorf("validate FSKit app source: %w", err)
	}
	sourceDigest, err := hashAppBundle(source)
	if err != nil {
		return nil, err
	}
	if targetDigest, targetErr := hashAppBundle(target); targetErr == nil && targetDigest == sourceDigest {
		unregisterFSKitApp(ctx, source)
		appGroup, err := validateAndEnableFSKitApp(ctx, target)
		if err != nil {
			return nil, err
		}
		return &darwinFSKitAppTransaction{target: target, appGroupPath: appGroup}, nil
	} else if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return nil, targetErr
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return nil, err
	}
	stageRoot, err := os.MkdirTemp(filepath.Dir(target), ".codexfold-fskit-stage-*")
	if err != nil {
		return nil, err
	}
	transaction := &darwinFSKitAppTransaction{target: target, source: source, stageRoot: stageRoot, changed: true}
	stagePath := filepath.Join(stageRoot, filepath.Base(target))
	if output, err := exec.CommandContext(ctx, "/usr/bin/ditto", source, stagePath).CombinedOutput(); err != nil {
		_ = transaction.Commit()
		return nil, commandOutputError("stage FSKit app", output, err)
	}
	if err := validateFSKitApp(ctx, stagePath); err != nil {
		_ = transaction.Commit()
		return nil, fmt.Errorf("validate staged FSKit app: %w", err)
	}
	if stagedDigest, err := hashAppBundle(stagePath); err != nil || stagedDigest != sourceDigest {
		_ = transaction.Commit()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("staged FSKit app does not match the source bundle")
	}
	if _, err := os.Stat(target); err == nil {
		transaction.hadTarget = true
		transaction.backupRoot, err = os.MkdirTemp(filepath.Dir(target), ".codexfold-fskit-backup-*")
		if err != nil {
			_ = transaction.Commit()
			return nil, err
		}
		transaction.backupPath = filepath.Join(transaction.backupRoot, filepath.Base(target))
		if err := os.Rename(target, transaction.backupPath); err != nil {
			_ = transaction.Commit()
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = transaction.Commit()
		return nil, err
	}
	if err := os.Rename(stagePath, target); err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	unregisterFSKitApp(ctx, source)
	appGroup, err := validateAndEnableFSKitApp(ctx, target)
	if err != nil {
		rollbackErr := transaction.Rollback(ctx)
		return nil, errors.Join(err, rollbackErr)
	}
	transaction.appGroupPath = appGroup
	return transaction, nil
}

func unregisterFSKitApp(ctx context.Context, appPath string) {
	if module, err := service.FSKitModulePath(appPath); err == nil {
		_, _ = exec.CommandContext(ctx, "/usr/bin/pluginkit", "-r", module).CombinedOutput()
	}
	_, _ = exec.CommandContext(ctx, launchServicesRegister, "-u", appPath).CombinedOutput()
}

func (t *darwinFSKitAppTransaction) AppGroupPath() string { return t.appGroupPath }
func (t *darwinFSKitAppTransaction) Changed() bool        { return t.changed }

func (t *darwinFSKitAppTransaction) Rollback(ctx context.Context) error {
	if t == nil || !t.changed {
		return nil
	}
	var result error
	if t.source != "" {
		unregisterFSKitApp(ctx, t.source)
	}
	_, _ = exec.CommandContext(ctx, launchServicesRegister, "-u", t.target).CombinedOutput()
	if err := os.RemoveAll(t.target); err != nil {
		result = errors.Join(result, err)
	}
	if t.hadTarget && t.backupPath != "" {
		if err := os.Rename(t.backupPath, t.target); err != nil {
			result = errors.Join(result, err)
		} else if _, err := validateAndEnableFSKitApp(ctx, t.target); err != nil {
			result = errors.Join(result, err)
		}
	}
	result = errors.Join(result, syncDirectory(filepath.Dir(t.target)))
	t.changed = false
	return errors.Join(result, t.Commit())
}

func (t *darwinFSKitAppTransaction) Commit() error {
	if t == nil {
		return nil
	}
	var result error
	for _, path := range []string{t.stageRoot, t.backupRoot} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	t.stageRoot = ""
	t.backupRoot = ""
	t.backupPath = ""
	return errors.Join(result, syncDirectory(filepath.Dir(t.target)))
}

func validateAndEnableFSKitApp(ctx context.Context, appPath string) (string, error) {
	if err := validateFSKitApp(ctx, appPath); err != nil {
		return "", err
	}
	if output, err := exec.CommandContext(ctx, launchServicesRegister, "-f", "-R", "-trusted", appPath).CombinedOutput(); err != nil {
		return "", commandOutputError("register FSKit app", output, err)
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/pluginkit", "-e", "use", "-p", "com.apple.fskit.fsmodule", "-i", service.FSKitModuleIdentifier).CombinedOutput(); err != nil {
		return "", commandOutputError("enable FSKit extension election", output, err)
	}
	if _, err := ensureFSKitModuleEnabled(service.FSKitModuleIdentifier); err != nil {
		return "", err
	}
	// FSKit may retain the previous extension endpoint across a same-bundle-ID
	// app replacement. Stop the agent first so it cannot immediately respawn the
	// stale module while the LaunchServices and preferences caches are refreshed.
	killUserProcess("fskit_agent")
	killUserProcess("CodexFoldFSKitModule")
	killUserProcess("cfprefsd")
	time.Sleep(time.Second)
	launcher, err := service.FSKitHostLauncherPath(appPath)
	if err != nil {
		return "", err
	}
	output, err := exec.CommandContext(ctx, launcher, "--app-group-path").CombinedOutput()
	if err != nil {
		return "", commandOutputError("resolve FSKit App Group path", output, err)
	}
	appGroup := filepath.Clean(strings.TrimSpace(string(output)))
	if !filepath.IsAbs(appGroup) || filepath.Base(appGroup) != service.FSKitAppGroupIdentifier {
		return "", fmt.Errorf("FSKit host returned invalid App Group path %q", appGroup)
	}
	return appGroup, nil
}

func validateFSKitApp(ctx context.Context, appPath string) error {
	launcher, err := service.FSKitHostLauncherPath(appPath)
	if err != nil {
		return err
	}
	module, err := service.FSKitModulePath(appPath)
	if err != nil {
		return err
	}
	if info, err := os.Stat(launcher); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		if err != nil {
			return err
		}
		return errors.New("FSKit host launcher is not executable")
	}
	if info, err := os.Stat(module); err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return errors.New("FSKit module bundle is missing")
	}
	checks := []struct {
		path string
		key  string
		want string
	}{
		{filepath.Join(appPath, "Contents", "Info.plist"), "CFBundleIdentifier", service.FSKitHostBundleIdentifier},
		{filepath.Join(module, "Contents", "Info.plist"), "CFBundleIdentifier", service.FSKitModuleIdentifier},
	}
	for _, check := range checks {
		output, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", check.key, "raw", check.path).CombinedOutput()
		if err != nil {
			return commandOutputError("read FSKit bundle identity", output, err)
		}
		if strings.TrimSpace(string(output)) != check.want {
			return fmt.Errorf("FSKit bundle identifier=%q expected=%q", strings.TrimSpace(string(output)), check.want)
		}
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", appPath).CombinedOutput(); err != nil {
		return commandOutputError("verify FSKit app signature", output, err)
	}
	entitlements, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-d", "--entitlements", ":-", "--xml", module).CombinedOutput()
	if err != nil {
		return commandOutputError("read FSKit module entitlements", entitlements, err)
	}
	for _, required := range []string{"com.apple.developer.fskit.fsmodule", "com.apple.security.app-sandbox", "com.apple.security.application-groups", service.FSKitAppGroupIdentifier} {
		if !strings.Contains(string(entitlements), required) {
			return fmt.Errorf("FSKit module signature is missing %s", required)
		}
	}
	profile := filepath.Join(module, "Contents", "embedded.provisionprofile")
	profileData, err := exec.CommandContext(ctx, "/usr/bin/security", "cms", "-D", "-i", profile).CombinedOutput()
	if err != nil {
		return commandOutputError("read FSKit module provisioning profile", profileData, err)
	}
	if !strings.Contains(string(profileData), service.FSKitAppGroupIdentifier) {
		return errors.New("FSKit module provisioning profile does not authorize the App Group")
	}
	return nil
}

func ensureFSKitModuleEnabled(moduleID string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, "Library", "Group Containers", "group.com.apple.fskit.settings", "enabledModules.plist")
	output, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", path).CombinedOutput()
	if err != nil {
		return false, commandOutputError("read enabled FSKit modules", output, err)
	}
	var modules []string
	if err := json.Unmarshal(output, &modules); err != nil {
		return false, err
	}
	for _, current := range modules {
		if current == moduleID {
			return false, nil
		}
	}
	command := fmt.Sprintf("Add :%d string %s", len(modules), moduleID)
	if output, err := exec.Command("/usr/libexec/PlistBuddy", "-c", command, path).CombinedOutput(); err != nil {
		return false, commandOutputError("enable FSKit module", output, err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		return false, commandOutputError("validate enabled FSKit modules", output, err)
	}
	return true, nil
}

func killUserProcess(name string) {
	output, err := exec.Command("/usr/bin/pgrep", "-u", strconv.Itoa(os.Getuid()), "-x", name).Output()
	if err != nil {
		return
	}
	for _, field := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 1 {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}
}

func hashAppBundle(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("app bundle path must be absolute")
	}
	hash := sha256.New()
	err := filepath.WalkDir(filepath.Clean(root), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = io.WriteString(hash, relative)
		_, _ = io.WriteString(hash, "\x00"+info.Mode().String()+"\x00")
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(hash, target)
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func commandOutputError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
