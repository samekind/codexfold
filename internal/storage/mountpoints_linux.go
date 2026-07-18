//go:build linux

package storage

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

func nestedMountPoints(root string) (map[string]struct{}, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	result := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountPoint := filepath.Clean(unescapeMountInfoPath(fields[4]))
		if mountPoint != root && pathWithin(root, mountPoint) {
			result[mountPoint] = struct{}{}
		}
	}
	return result, scanner.Err()
}

func unescapeMountInfoPath(path string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(path)
}
