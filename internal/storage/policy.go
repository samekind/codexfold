package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const PolicyFilename = "storage-policy.json"

var DefaultLimits = Limits{
	MaxPhysicalBytes:      512 << 30,
	MaxTemporaryBytes:     16 << 30,
	FreeSpaceReserveBytes: 5 << 30,
}

type policyFile struct {
	Version int    `json:"version"`
	Limits  Limits `json:"limits"`
}

type Checker interface {
	Check(context.Context, Projection) (Assessment, error)
}

func LoadLimits(storeDir string) (Limits, error) {
	if storeDir == "" {
		return Limits{}, errors.New("storage policy store directory is required")
	}
	path := filepath.Join(filepath.Clean(storeDir), PolicyFilename)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultLimits, nil
	}
	if err != nil {
		return Limits{}, fmt.Errorf("read storage policy: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy policyFile
	if err := decoder.Decode(&policy); err != nil {
		return Limits{}, fmt.Errorf("decode storage policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Limits{}, fmt.Errorf("decode storage policy: %w", err)
	}
	if policy.Version != 1 {
		return Limits{}, fmt.Errorf("unsupported storage policy version %d", policy.Version)
	}
	if policy.Limits.MaxPhysicalBytes <= 0 || policy.Limits.MaxTemporaryBytes <= 0 || policy.Limits.FreeSpaceReserveBytes <= 0 {
		return Limits{}, errors.New("storage policy limits must all be positive")
	}
	return policy.Limits, nil
}

func DefaultGuard(storeDir string) (Guard, error) {
	limits, err := LoadLimits(storeDir)
	if err != nil {
		return Guard{}, err
	}
	return Guard{StoreDir: filepath.Clean(storeDir), Limits: limits}, nil
}
