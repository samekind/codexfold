package mountid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	Path   = ".codexfold-health"
	prefix = "codexfold-v1:"
)

func New() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func Validate(value []byte) error {
	text := string(value)
	if !strings.HasPrefix(text, prefix) {
		return errors.New("mount identity prefix is invalid")
	}
	digest := strings.TrimPrefix(text, prefix)
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 16 {
		return errors.New("mount identity payload is invalid")
	}
	return nil
}
