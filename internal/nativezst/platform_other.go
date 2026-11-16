//go:build !darwin && !linux

package nativezst

import (
	"os"
	"time"
)

func ReadMarker(string) (Marker, bool, error) { return Marker{}, false, nil }

func writeMarker(*os.File, Marker) error { return ErrUnsupported }

func removeMarker(*os.File) error { return ErrUnsupported }

func releaseBlocks(*os.File, int64) error { return ErrUnsupported }

func durableSync(file *os.File) error { return file.Sync() }

func allocatedBytes(info os.FileInfo) int64 { return info.Size() }

func metadataAllocation(os.FileInfo) int64 { return 0 }

func inode(os.FileInfo) uint64 { return 0 }

func fileTimes(info os.FileInfo) (time.Time, time.Time) { return info.ModTime(), info.ModTime() }

func changeTime(os.FileInfo) time.Time { return time.Time{} }

func supported() bool { return false }
