package buildid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func CurrentSHA256() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return FileSHA256(executable)
}

func FileSHA256(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("absolute executable path is required")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func ValidSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
