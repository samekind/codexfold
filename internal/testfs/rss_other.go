//go:build !darwin && !linux

package testfs

func maxRSSBytes() uint64 { return 0 }
