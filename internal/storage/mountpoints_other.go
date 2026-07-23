//go:build !darwin && !linux

package storage

func nestedMountPoints(string) (map[string]struct{}, error) {
	return nil, nil
}
