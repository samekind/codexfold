//go:build !darwin && !linux

package nativezst

func setTestXattr(string, string, []byte) error { return ErrUnsupported }

func getTestXattr(string, string, []byte) (int, error) { return 0, ErrUnsupported }

func removeTestXattr(string, string) error { return ErrUnsupported }
