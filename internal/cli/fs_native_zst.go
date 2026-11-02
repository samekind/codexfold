package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/samekind/codexfold/internal/codex"
	"github.com/samekind/codexfold/internal/codexzstd"
	"github.com/samekind/codexfold/internal/enroll"
	"github.com/samekind/codexfold/internal/mountfs"
	"github.com/samekind/codexfold/internal/nativezst"
	"github.com/samekind/codexfold/internal/pack"
	"github.com/samekind/codexfold/internal/storage"
	"github.com/spf13/cobra"
)

// storeMaintenanceMu serializes the service enrollment cycle with folding of
// Codex .zst files: a manifest that appears between the cycle's pack build
// and its loose retirement would fail the pack-only proof.
var storeMaintenanceMu sync.Mutex

const (
	nativeZstPassInterval = 30 * time.Second
	nativeZstPassBudget   = 2 * time.Minute
	// Codex renames a finished .zst into place; leave it alone for a while
	// after its last metadata change.
	nativeZstSettle       = 10 * time.Minute
	nativeZstMinimumBytes = 16 << 10
	nativeZstSweepEvery   = 10 * time.Minute
)

type nativeZstFolder struct {
	filesystem  *mountfs.Filesystem
	source      *nativezst.Source
	store       string
	diagnostics io.Writer
	refresh     func()
	skipped     map[string]nativeZstIdentity
	lastSweep   time.Time
	lastError   string
}

type nativeZstIdentity struct {
	size    int64
	modTime time.Time
}

func startNativeZstFolding(ctx context.Context, filesystem *mountfs.Filesystem, source *nativezst.Source, store string, enabled func() bool, refresh func(), diagnostics io.Writer) <-chan struct{} {
	done := make(chan struct{})
	folder := &nativeZstFolder{
		filesystem: filesystem, source: source, store: store, diagnostics: diagnostics, refresh: refresh,
		skipped: make(map[string]nativeZstIdentity),
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(nativeZstPassInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !enabled() || !storeMaintenanceMu.TryLock() {
				continue
			}
			err := folder.pass(ctx)
			storeMaintenanceMu.Unlock()
			folder.report(err)
		}
	}()
	return done
}

func (z *nativeZstFolder) report(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrOperationLockHeld) {
		z.lastError = ""
		return
	}
	if err.Error() != z.lastError {
		_, _ = fmt.Fprintf(z.diagnostics, "fold Codex .zst files: %v\n", err)
		z.lastError = err.Error()
	}
}

func (z *nativeZstFolder) pass(ctx context.Context) error {
	scan, err := z.filesystem.ScanNativeZst(ctx)
	if err != nil {
		return err
	}
	maintenance, err := storage.BeginMaintenance(ctx, z.store)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	work := maintenance.Context()

	var converted, finished, waiting int
	var freed, stored int64
	for _, file := range scan.Folded {
		identity := nativeZstIdentity{size: file.Info.Size(), modTime: file.Info.ModTime()}
		if nativezst.AllocatedBytes(file.Info) == 0 || z.skipped[file.Path] == identity {
			continue
		}
		err := z.filesystem.FinishNativeZst(work, z.source, file)
		if err == nil {
			finished++
			freed += nativezst.AllocatedBytes(file.Info)
			continue
		}
		if work.Err() != nil {
			return maintenance.YieldError(work.Err())
		}
		if !errors.Is(err, mountfs.ErrNativeZstBusy) {
			z.skipped[file.Path] = identity
			_, _ = fmt.Fprintf(z.diagnostics, "finish folding Codex .zst %s: %v\n", file.Path, err)
		}
	}
	candidates := make([]nativezst.NativeFile, 0, len(scan.Candidates))
	settledBefore := time.Now().Add(-nativeZstSettle)
	for _, file := range scan.Candidates {
		identity := nativeZstIdentity{size: file.Info.Size(), modTime: file.Info.ModTime()}
		if skipped, ok := z.skipped[file.Path]; ok && skipped == identity {
			continue
		}
		if file.Info.Size() < nativeZstMinimumBytes || nativezst.ChangedAfter(file.Info, settledBefore) {
			continue
		}
		candidates = append(candidates, file)
	}
	// The largest files free the most space per pass.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Info.Size() > candidates[j].Info.Size() })
	deadline := time.Now().Add(nativeZstPassBudget)
	var passErr error
	for _, file := range candidates {
		if time.Now().After(deadline) {
			break
		}
		prepared, err := nativezst.Prepare(work, z.source, file.Path)
		if err != nil {
			if work.Err() != nil {
				passErr = maintenance.YieldError(work.Err())
				break
			}
			if errors.Is(err, storage.ErrOperationLockHeld) {
				break
			}
			if errors.Is(err, nativezst.ErrChanged) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			// A later, cheaper file may still fit; this one is retried once
			// folding has freed some space.
			if errors.Is(err, storage.ErrBudgetExceeded) {
				waiting++
				continue
			}
			z.skipped[file.Path] = nativeZstIdentity{size: file.Info.Size(), modTime: file.Info.ModTime()}
			if !errors.Is(err, codexzstd.ErrNotReproducible) && !errors.Is(err, nativezst.ErrNotWorthwhile) {
				_, _ = fmt.Fprintf(z.diagnostics, "fold Codex .zst %s: %v\n", file.Path, err)
			}
			continue
		}
		if err := z.filesystem.ConvertNativeZst(z.store, prepared); err != nil {
			if !errors.Is(err, nativezst.ErrMarkerKept) {
				nativezst.Discard(prepared)
			}
			if !errors.Is(err, mountfs.ErrNativeZstBusy) && !errors.Is(err, nativezst.ErrChanged) {
				_, _ = fmt.Fprintf(z.diagnostics, "convert Codex .zst %s: %v\n", file.Path, err)
			}
			continue
		}
		converted++
		freed += nativezst.AllocatedBytes(file.Info)
		stored += prepared.NewStoredBytes
	}
	if converted+finished > 0 || waiting > 0 {
		_, _ = fmt.Fprintf(z.diagnostics, "%s folded %d Codex .zst file(s): freed %s, stored %s new; %d waiting for disk space\n",
			time.Now().UTC().Format(time.RFC3339Nano), converted+finished, formatBytes(freed), formatBytes(stored), waiting)
	}
	if converted+finished > 0 {
		if z.refresh != nil {
			z.refresh()
		}
	}
	if passErr == nil && time.Since(z.lastSweep) >= nativeZstSweepEvery {
		result, err := nativezst.Sweep(work, z.store, scan, time.Now())
		if err != nil {
			return errors.Join(passErr, err)
		}
		z.lastSweep = time.Now()
		if result.Released+result.Repaired > 0 {
			_, _ = fmt.Fprintf(z.diagnostics, "%s Codex .zst sweep: released %d manifest(s), repaired %d marker(s)\n",
				time.Now().UTC().Format(time.RFC3339Nano), result.Released, result.Repaired)
		}
	}
	return passErr
}

// withNativeFootprint adds the native session tree to the storage figures, so
// the saved percentage compares all session content with everything on disk.
func withNativeFootprint(nativeRoot string, scan func(context.Context, storage.Options) (storage.Inventory, error)) func(context.Context, storage.Options) (storage.Inventory, error) {
	return func(ctx context.Context, options storage.Options) (storage.Inventory, error) {
		inventory, err := scan(ctx, options)
		if err != nil || nativeRoot == "" {
			return inventory, err
		}
		native, nativeErr := nativezst.ScanNative(ctx, nativeRoot)
		if nativeErr != nil {
			return inventory, nil
		}
		inventory.LogicalSessionBytes += native.Footprint.LogicalBytes
		inventory.TotalPhysicalBytes += native.Footprint.PhysicalBytes
		return inventory, nil
	}
}

type FSNativeZstStatus struct {
	NativeRoot string                 `json:"native_root"`
	Supported  bool                   `json:"supported"`
	Footprint  nativezst.Footprint    `json:"footprint"`
	Manifests  int                    `json:"manifests"`
	Restored   int                    `json:"restored,omitempty"`
	Failures   []string               `json:"failures,omitempty"`
	Sweep      *nativezst.SweepResult `json:"sweep,omitempty"`
}

func newFSNativeZstCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "native-zst",
		Short: "Inspect or restore Codex-compressed rollouts folded into the store",
	}
	command.AddCommand(newFSNativeZstStatusCommand(), newFSNativeZstRestoreCommand())
	return command
}

func resolveNativeZstPaths(codexHome string, storeDir string, nativeRoot string) (string, string, error) {
	home, err := codex.ResolveHome(codexHome)
	if err != nil {
		return "", "", err
	}
	if nativeRoot == "" {
		nativeRoot = filepath.Join(home, "fold-native")
	}
	return resolveFoldStore(home, storeDir), filepath.Clean(nativeRoot), nil
}

func newFSNativeZstStatusCommand() *cobra.Command {
	var codexHome, storeDir, nativeRoot string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report folded and unfolded Codex .zst rollouts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, root, err := resolveNativeZstPaths(codexHome, storeDir, nativeRoot)
			if err != nil {
				return err
			}
			scan, err := nativezst.ScanNative(command.Context(), root)
			if err != nil {
				return err
			}
			entries, _ := os.ReadDir(nativezst.ManifestRoot(store))
			status := FSNativeZstStatus{NativeRoot: root, Supported: codexzstd.Supported(), Footprint: scan.Footprint, Manifests: len(entries)}
			if jsonOutput {
				return writeJSON(command, status)
			}
			footprint := status.Footprint
			_, err = fmt.Fprintf(command.OutOrStdout(), "supported=%t folded=%d (%s encoded, %s decoded) unfolded=%d (%s) manifests=%d native_on_disk=%s\n",
				status.Supported, footprint.FoldedFiles, formatBytes(footprint.FoldedEncodedBytes), formatBytes(footprint.FoldedLogicalBytes),
				footprint.CompressedFiles, formatBytes(footprint.CompressedBytes), status.Manifests, formatBytes(footprint.PhysicalBytes))
			return err
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Native rollout root; defaults to <codex-home>/fold-native")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

func newFSNativeZstRestoreCommand() *cobra.Command {
	var codexHome, storeDir, nativeRoot string
	var all, apply, jsonOutput bool
	command := &cobra.Command{
		Use:   "restore [path...]",
		Short: "Write folded Codex .zst files back as real files; required before downgrading CodexFold",
		Long: "Restore re-encodes each folded .zst from the store, writes the exact original bytes back in place and removes " +
			"its marker. Disable automatic folding first, or the service folds the files again. Dry-run unless --apply.",
		RunE: func(command *cobra.Command, args []string) error {
			if all == (len(args) != 0) {
				return errors.New("name folded files or pass --all")
			}
			store, root, err := resolveNativeZstPaths(codexHome, storeDir, nativeRoot)
			if err != nil {
				return err
			}
			// The service would release the blocks of a file being restored,
			// taking it for a conversion interrupted after its marker.
			if control, err := enroll.LoadControl(enroll.ControlPath(store)); err != nil {
				return err
			} else if control.Enabled && apply {
				return errors.New("automatic folding is enabled; turn it off before restoring folded .zst files")
			}
			paths := args
			if all {
				scan, err := nativezst.ScanNative(command.Context(), root)
				if err != nil {
					return err
				}
				paths = paths[:0]
				for _, file := range scan.Folded {
					paths = append(paths, file.Path)
				}
			}
			status := FSNativeZstStatus{NativeRoot: root, Supported: codexzstd.Supported()}
			if apply {
				source := nativezst.NewSource(store, storePacks{})
				defer source.Close()
				for _, path := range paths {
					if err := nativezst.Restore(command.Context(), source, path); err != nil {
						status.Failures = append(status.Failures, fmt.Sprintf("%s: %v", path, err))
						continue
					}
					status.Restored++
				}
			}
			if jsonOutput {
				if err := writeJSON(command, status); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintf(command.OutOrStdout(), "candidates=%d restored=%d failed=%d dry_run=%t\n", len(paths), status.Restored, len(status.Failures), !apply); err != nil {
				return err
			}
			if len(status.Failures) != 0 {
				return fmt.Errorf("%d folded file(s) could not be restored", len(status.Failures))
			}
			return nil
		},
	}
	command.Flags().StringVar(&codexHome, "codex-home", "", "Codex home directory; defaults to CODEX_HOME or ~/.codex")
	command.Flags().StringVar(&storeDir, "store", "", "Fold store directory; defaults to <codex-home>/fold-store")
	command.Flags().StringVar(&nativeRoot, "native-root", "", "Native rollout root; defaults to <codex-home>/fold-native")
	command.Flags().BoolVar(&all, "all", false, "Restore every folded file under the native root")
	command.Flags().BoolVar(&apply, "apply", false, "Write the files; omitted means dry-run")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return command
}

// storePacks gives the native .zst source the store's published packs.
type storePacks struct{}

func (storePacks) CurrentGeneration(store string) (string, error) {
	return pack.CurrentGeneration(store)
}

func (storePacks) Open(store string) (nativezst.Pack, error) {
	resolver, err := pack.Open(store, pack.OpenOptions{})
	if err != nil {
		return nil, err
	}
	return resolver, nil
}
