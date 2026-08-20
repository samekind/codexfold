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

type NativeSnapshotRetention string

const (
	NativeSnapshotRetentionExactProof NativeSnapshotRetention = "until-exact-recovery-proof"
	NativeSnapshotRetentionManual     NativeSnapshotRetention = "until-explicit-retirement"
)

var DefaultLimits = Limits{
	MaxPhysicalBytes:      512 << 30,
	MaxTemporaryBytes:     16 << 30,
	FreeSpaceReserveBytes: 5 << 30,
}

var DefaultRetention = RetentionPolicy{
	NativeSnapshots: NativeSnapshotRetentionExactProof,
}

type RetentionPolicy struct {
	NativeSnapshots NativeSnapshotRetention `json:"native_snapshots"`
}

type policyFile struct {
	Version   int             `json:"version"`
	Limits    Limits          `json:"limits"`
	Retention RetentionPolicy `json:"retention,omitempty"`
}

type Checker interface {
	Check(context.Context, Projection) (Assessment, error)
}

func LoadLimits(storeDir string) (Limits, error) {
	policy, err := loadPolicy(storeDir)
	if err != nil {
		return Limits{}, err
	}
	return policy.Limits, nil
}

func LoadRetentionPolicy(storeDir string) (RetentionPolicy, error) {
	policy, err := loadPolicy(storeDir)
	if err != nil {
		return RetentionPolicy{}, err
	}
	return policy.Retention, nil
}

func loadPolicy(storeDir string) (policyFile, error) {
	if storeDir == "" {
		return policyFile{}, errors.New("storage policy store directory is required")
	}
	path := filepath.Join(filepath.Clean(storeDir), PolicyFilename)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return policyFile{Version: 1, Limits: DefaultLimits, Retention: DefaultRetention}, nil
	}
	if err != nil {
		return policyFile{}, fmt.Errorf("read storage policy: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy policyFile
	if err := decoder.Decode(&policy); err != nil {
		return policyFile{}, fmt.Errorf("decode storage policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return policyFile{}, fmt.Errorf("decode storage policy: %w", err)
	}
	if policy.Version != 1 {
		return policyFile{}, fmt.Errorf("unsupported storage policy version %d", policy.Version)
	}
	if policy.Limits.MaxPhysicalBytes <= 0 || policy.Limits.MaxTemporaryBytes <= 0 || policy.Limits.FreeSpaceReserveBytes <= 0 {
		return policyFile{}, errors.New("storage policy limits must all be positive")
	}
	if policy.Retention.NativeSnapshots == "" {
		policy.Retention = DefaultRetention
	}
	switch policy.Retention.NativeSnapshots {
	case NativeSnapshotRetentionExactProof, NativeSnapshotRetentionManual:
	default:
		return policyFile{}, fmt.Errorf("unsupported native snapshot retention %q", policy.Retention.NativeSnapshots)
	}
	return policy, nil
}

func DefaultGuard(storeDir string) (Guard, error) {
	limits, err := LoadLimits(storeDir)
	if err != nil {
		return Guard{}, err
	}
	return Guard{StoreDir: filepath.Clean(storeDir), Limits: limits}, nil
}
