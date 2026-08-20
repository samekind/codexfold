//go:build linux

package vfs

import (
	"errors"
	"math"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestSessionDeletionObjectFromFileLinuxRequiresExactIdentity(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	base := sessionDeletionStatxForInfoLinux(t, info)
	base.Btime = unix.StatxTimestamp{Sec: 5, Nsec: 7}
	base.Mnt_id = 0xfedcba98

	oldStatx := sessionDeletionStatxLinux
	oldGeneration := sessionDeletionGenerationLinux
	t.Cleanup(func() {
		sessionDeletionStatxLinux = oldStatx
		sessionDeletionGenerationLinux = oldGeneration
	})

	t.Run("complete proof", func(t *testing.T) {
		sessionDeletionStatxLinux = func(dirfd int, path string, flags int, mask int, statx *unix.Statx_t) error {
			if dirfd != int(file.Fd()) || path != "" || flags != unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT || mask != sessionDeletionLinuxStatxMask {
				t.Fatalf("unexpected statx request: fd=%d path=%q flags=%#x mask=%#x", dirfd, path, flags, mask)
			}
			*statx = base
			return nil
		}
		sessionDeletionGenerationLinux = func(*os.File) (uint64, error) { return 17, nil }

		object, err := sessionDeletionObjectFromFile(file, info)
		if err != nil {
			t.Fatal(err)
		}
		if object.IdentityProof != sessionDeletionObjectProofLinux ||
			object.GenerationSource != sessionDeletionGenerationSourceLinux ||
			object.BirthtimeSource != sessionDeletionBirthtimeSourceLinux ||
			object.Generation != 17 || object.Generation == base.Mnt_id ||
			object.BirthtimeUnixNano != 5_000_000_007 {
			t.Fatalf("unexpected Linux deletion identity: %#v", object)
		}
	})

	tests := []struct {
		name       string
		mutate     func(*unix.Statx_t)
		statxErr   error
		generation uint64
		genErr     error
		want       string
	}{
		{name: "statx unavailable", statxErr: unix.ENOSYS, generation: 17, want: "statx"},
		{name: "missing birthtime mask", mutate: func(statx *unix.Statx_t) { statx.Mask &^= unix.STATX_BTIME }, generation: 17, want: "required mask"},
		{name: "zero birthtime", mutate: func(statx *unix.Statx_t) { statx.Btime = unix.StatxTimestamp{} }, generation: 17, want: "no birth time"},
		{name: "invalid birthtime nanoseconds", mutate: func(statx *unix.Statx_t) { statx.Btime.Nsec = 1e9 }, generation: 17, want: "nanosecond"},
		{name: "birthtime overflow", mutate: func(statx *unix.Statx_t) {
			statx.Btime.Sec = math.MaxInt64/sessionDeletionNanosecondsPerSecondLinux + 1
		}, generation: 17, want: "overflows"},
		{name: "zero inode", mutate: func(statx *unix.Statx_t) { statx.Ino = 0 }, generation: 17, want: "zero inode"},
		{name: "zero device", mutate: func(statx *unix.Statx_t) { statx.Dev_major, statx.Dev_minor = 0, 0 }, generation: 17, want: "zero device"},
		{name: "inode mismatch", mutate: func(statx *unix.Statx_t) { statx.Ino++ }, generation: 17, want: "differs"},
		{name: "device mismatch", mutate: func(statx *unix.Statx_t) { statx.Dev_minor++ }, generation: 17, want: "differs"},
		{name: "uid mismatch", mutate: func(statx *unix.Statx_t) { statx.Uid++ }, generation: 17, want: "differs"},
		{name: "gid mismatch", mutate: func(statx *unix.Statx_t) { statx.Gid++ }, generation: 17, want: "differs"},
		{name: "generation unsupported", generation: 17, genErr: unix.EOPNOTSUPP, want: "operation not supported"},
		{name: "zero generation", generation: 0, want: "generation is zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			sessionDeletionStatxLinux = func(_ int, _ string, _ int, _ int, statx *unix.Statx_t) error {
				if test.statxErr != nil {
					return test.statxErr
				}
				*statx = candidate
				return nil
			}
			sessionDeletionGenerationLinux = func(*os.File) (uint64, error) {
				return test.generation, test.genErr
			}
			if _, err := sessionDeletionObjectFromFile(file, info); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("sessionDeletionObjectFromFile() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSessionDeletionBirthtimeUnixNanoLinux(t *testing.T) {
	tests := []struct {
		name      string
		timestamp unix.StatxTimestamp
		want      int64
		wantErr   bool
	}{
		{name: "positive", timestamp: unix.StatxTimestamp{Sec: 1, Nsec: 2}, want: 1_000_000_002},
		{name: "zero", timestamp: unix.StatxTimestamp{}, wantErr: true},
		{name: "negative", timestamp: unix.StatxTimestamp{Sec: -1}, wantErr: true},
		{name: "upper overflow", timestamp: unix.StatxTimestamp{Sec: math.MaxInt64/sessionDeletionNanosecondsPerSecondLinux + 1}, wantErr: true},
		{name: "lower overflow", timestamp: unix.StatxTimestamp{Sec: math.MinInt64/sessionDeletionNanosecondsPerSecondLinux - 1}, wantErr: true},
		{name: "addition overflow", timestamp: unix.StatxTimestamp{Sec: math.MaxInt64 / sessionDeletionNanosecondsPerSecondLinux, Nsec: 999_999_999}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sessionDeletionBirthtimeUnixNanoLinux(test.timestamp)
			if test.wantErr {
				if err == nil {
					t.Fatalf("sessionDeletionBirthtimeUnixNanoLinux()=%d, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("sessionDeletionBirthtimeUnixNanoLinux()=%d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestReadSessionDeletionGenerationLinux(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "generation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	oldIoctl := sessionDeletionIoctlLinux
	t.Cleanup(func() { sessionDeletionIoctlLinux = oldIoctl })

	t.Run("unsigned generation", func(t *testing.T) {
		sessionDeletionIoctlLinux = func(fd uintptr, request uintptr, argument uintptr) unix.Errno {
			if fd != file.Fd() || request != sessionDeletionFSIOCGetVersionRequestLinux() {
				t.Fatalf("unexpected ioctl request: fd=%d request=%#x", fd, request)
			}
			*(*int32)(unsafe.Pointer(argument)) = -1
			return 0
		}
		generation, err := readSessionDeletionGenerationLinux(file)
		if err != nil || generation != math.MaxUint32 {
			t.Fatalf("readSessionDeletionGenerationLinux()=%d, %v", generation, err)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		sessionDeletionIoctlLinux = func(uintptr, uintptr, uintptr) unix.Errno { return unix.ENOTTY }
		if _, err := readSessionDeletionGenerationLinux(file); err == nil || !errors.Is(err, unix.ENOTTY) {
			t.Fatalf("readSessionDeletionGenerationLinux() error=%v, want ENOTTY", err)
		}
	})

	t.Run("zero", func(t *testing.T) {
		sessionDeletionIoctlLinux = func(_ uintptr, _ uintptr, argument uintptr) unix.Errno {
			*(*int32)(unsafe.Pointer(argument)) = 0
			return 0
		}
		if _, err := readSessionDeletionGenerationLinux(file); err == nil {
			t.Fatal("zero FS_IOC_GETVERSION generation was accepted")
		}
	})
}

func TestSessionDeletionFSIOCGetVersionRequestLinux(t *testing.T) {
	const ioctlTypeMask = uintptr(0xff) << 8
	request := sessionDeletionFSIOCGetVersionRequestLinux()
	getFlags := uintptr(unix.FS_IOC_GETFLAGS)
	if request&^ioctlTypeMask != getFlags&^ioctlTypeMask {
		t.Fatalf("FS_IOC_GETVERSION request changed fields other than the ioctl type: got=%#x base=%#x", request, getFlags)
	}
	if request&ioctlTypeMask != uintptr('v')<<8 {
		t.Fatalf("FS_IOC_GETVERSION request has the wrong ioctl type: %#x", request)
	}
}

func sessionDeletionStatxForInfoLinux(t *testing.T, info os.FileInfo) unix.Statx_t {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		t.Fatal("temporary file has no Linux stat identity")
	}
	device := uint64(stat.Dev)
	if device == 0 {
		t.Skip("temporary filesystem reports a zero device identity")
	}
	return unix.Statx_t{
		Mask:      uint32(sessionDeletionLinuxStatxMask),
		Ino:       uint64(stat.Ino),
		Uid:       stat.Uid,
		Gid:       stat.Gid,
		Dev_major: unix.Major(device),
		Dev_minor: unix.Minor(device),
	}
}
