package compat

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
)

type ContractOptions struct {
	Platform      string
	ClientKind    string
	ClientVersion string
}

var operationPattern = regexp.MustCompile(`(?i)\b(open|openat|close|release|read|pread|readv|write|pwrite|writev|flush|fsync|fdatasync|stat|stat64|lstat|lstat64|fstat|fstat64|statfs|getattr|readdir|access|chmod|chown|utimens|mmap|truncate|ftruncate|create|mknod|mkdir|rmdir|link|symlink|readlink|rename|renameat|unlink|unlinkat|flock|fcntl|clonefile|getattrlist|setxattr|getxattr|listxattr|removexattr)\b`)
var signaturePattern = regexp.MustCompile(`\([A-Z_]{4,32}\)|<[A-Z0-9_=+-]+>`)

func ParseFSUsage(reader io.Reader, options ContractOptions) (Contract, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	hasher := sha256.New()
	counts := make(map[string]int)
	signatures := make(map[string]map[string]int)
	var names []string
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = hasher.Write(line)
		_, _ = hasher.Write([]byte{'\n'})
		name := fsUsageOperationName(line)
		if name == "" {
			continue
		}
		if counts[name] == 0 {
			names = append(names, name)
		}
		counts[name]++
		if signatures[name] == nil {
			signatures[name] = make(map[string]int)
		}
		for _, signature := range signaturePattern.FindAllString(string(line), -1) {
			signatures[name][signature]++
		}
	}
	if err := scanner.Err(); err != nil {
		return Contract{}, err
	}
	if len(counts) == 0 {
		return Contract{}, errors.New("trace contains no recognized filesystem operations")
	}
	contract := Contract{Version: ContractVersion, Platform: options.Platform, ClientKind: options.ClientKind, ClientVersion: options.ClientVersion, TraceSHA256: hex.EncodeToString(hasher.Sum(nil))}
	for _, name := range names {
		operation := Operation{Name: name, Count: counts[name]}
		values := make([]string, 0, len(signatures[name]))
		for value := range signatures[name] {
			values = append(values, value)
		}
		sort.Strings(values)
		for _, value := range values {
			operation.Signatures = append(operation.Signatures, Signature{Value: value, Count: signatures[name][value]})
		}
		contract.Operations = append(contract.Operations, operation)
	}
	if err := validateContract(contract); err != nil {
		return Contract{}, err
	}
	return contract, nil
}

func fsUsageOperationName(line []byte) string {
	fields := bytes.Fields(line)
	if len(fields) > 1 {
		token := strings.ToLower(string(fields[1]))
		if strings.HasPrefix(token, "operation=") {
			return nativeFSKitOperationName(strings.TrimPrefix(token, "operation="))
		}
		if strings.HasPrefix(token, "io=") {
			return ""
		}
		switch {
		case token == "rddata" || strings.HasPrefix(token, "rddata["):
			return "read"
		case token == "wrdata" || strings.HasPrefix(token, "wrdata["):
			return "write"
		case token == "getdirentries64":
			return "readdir"
		case token == "statfs64" || token == "fstatfs64":
			return "statfs"
		case token == "fstatat64":
			return "fstat"
		case token == "open_dprotected":
			return "open"
		}
	}
	match := operationPattern.FindSubmatch(line)
	if len(match) == 0 {
		return ""
	}
	return strings.ToLower(string(match[1]))
}

func nativeFSKitOperationName(name string) string {
	if name == "sync" {
		return "fsync"
	}
	match := operationPattern.FindStringSubmatch(name)
	if len(match) == 0 || match[0] != name {
		return ""
	}
	return strings.ToLower(match[1])
}
