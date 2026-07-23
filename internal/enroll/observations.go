package enroll

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type observationFile struct {
	Version      int          `json:"version"`
	Observations Observations `json:"observations"`
}

func LoadObservations(path string) (Observations, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(Observations), nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored observationFile
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode enrollment observations: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode enrollment observations: %w", err)
	}
	if stored.Version != 1 {
		return nil, fmt.Errorf("unsupported enrollment observation version %d", stored.Version)
	}
	if stored.Observations == nil {
		stored.Observations = make(Observations)
	}
	return stored.Observations, nil
}

func SaveObservations(path string, observations Observations) error {
	if !filepath.IsAbs(path) {
		return errors.New("enrollment observation path must be absolute")
	}
	if observations == nil {
		observations = make(Observations)
	}
	data, err := json.MarshalIndent(observationFile{Version: 1, Observations: observations}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".observations-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncObservationDirectory(directory)
}
