//go:build darwin

package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (f *Filesystem) WatchNativeNamespace(ctx context.Context) error {
	f.mu.RLock()
	root := f.nativeRoot
	f.mu.RUnlock()
	if root == "" {
		return nil
	}
	queue, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("create native namespace kqueue: %w", err)
	}
	defer unix.Close(queue)
	watchers := make(map[int]string)
	closeWatchers := func() {
		for descriptor := range watchers {
			_ = unix.Close(descriptor)
		}
		clear(watchers)
	}
	defer closeWatchers()
	rescan := func() error {
		closeWatchers()
		for _, namespace := range []string{"sessions", "archived_sessions"} {
			base := filepath.Join(root, namespace)
			err := filepath.WalkDir(base, func(name string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					if errors.Is(walkErr, os.ErrNotExist) {
						return nil
					}
					return walkErr
				}
				if !entry.IsDir() {
					return nil
				}
				descriptor, err := unix.Open(name, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
				if err != nil {
					if errors.Is(err, unix.ENOENT) {
						return nil
					}
					return err
				}
				event := unix.Kevent_t{
					Ident: uint64(descriptor), Filter: unix.EVFILT_VNODE,
					Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
					Fflags: unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME |
						unix.NOTE_ATTRIB | unix.NOTE_LINK | unix.NOTE_REVOKE,
				}
				if _, err := unix.Kevent(queue, []unix.Kevent_t{event}, nil, nil); err != nil {
					_ = unix.Close(descriptor)
					return err
				}
				watchers[descriptor] = name
				return nil
			})
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("watch native namespace %s: %w", namespace, err)
			}
		}
		return nil
	}
	if err := rescan(); err != nil {
		return err
	}
	f.bumpNamespaceVersion()
	events := make([]unix.Kevent_t, 64)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := unix.NsecToTimespec((500 * time.Millisecond).Nanoseconds())
		count, err := unix.Kevent(queue, nil, events, &timeout)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("wait for native namespace event: %w", err)
		}
		if count == 0 {
			continue
		}
		f.bumpNamespaceVersion()
		if err := rescan(); err != nil {
			return err
		}
	}
}
