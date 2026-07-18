package mountid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jstar0/codexfold/internal/buildid"
)

const (
	Path     = ".codexfold-health"
	prefixV1 = "codexfold-v1:"
	prefixV2 = "codexfold-v2:"
)

type Identity struct {
	Version     int
	Nonce       string
	BuildSHA256 string
}

func New(buildSHA256 string) (string, error) {
	if !buildid.ValidSHA256(buildSHA256) {
		return "", errors.New("mount identity build SHA-256 is invalid")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefixV2 + hex.EncodeToString(random[:]) + ":" + buildSHA256, nil
}

func Validate(value []byte) error {
	_, err := Parse(value)
	return err
}

func Parse(value []byte) (Identity, error) {
	text := string(value)
	if strings.HasPrefix(text, prefixV1) {
		nonce := strings.TrimPrefix(text, prefixV1)
		if !validNonce(nonce) {
			return Identity{}, errors.New("mount identity payload is invalid")
		}
		return Identity{Version: 1, Nonce: nonce}, nil
	}
	if !strings.HasPrefix(text, prefixV2) {
		return Identity{}, errors.New("mount identity prefix is invalid")
	}
	parts := strings.Split(strings.TrimPrefix(text, prefixV2), ":")
	if len(parts) != 2 || !validNonce(parts[0]) || !buildid.ValidSHA256(parts[1]) {
		return Identity{}, errors.New("mount identity payload is invalid")
	}
	return Identity{Version: 2, Nonce: parts[0], BuildSHA256: parts[1]}, nil
}

func validNonce(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
}
