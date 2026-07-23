package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samekind/codexfold/internal/buildid"
)

type DefinitionUpdate struct {
	Target          string `json:"target"`
	CurrentSHA256   string `json:"current_sha256,omitempty"`
	CandidateSHA256 string `json:"candidate_sha256"`
	HadTarget       bool   `json:"had_target"`
	stagedPath      string
	backupPath      string
}

func StageDefinitionUpdate(target string, definition []byte) (*DefinitionUpdate, error) {
	if !filepath.IsAbs(target) || len(definition) == 0 {
		return nil, errors.New("absolute definition target and non-empty content are required")
	}
	target = filepath.Clean(target)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".codexfold-definition-candidate-*")
	if err != nil {
		return nil, err
	}
	stagedPath := temporary.Name()
	cleanup := func(operationErr error) (*DefinitionUpdate, error) {
		_ = temporary.Close()
		_ = os.Remove(stagedPath)
		return nil, operationErr
	}
	if err := temporary.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err := temporary.Write(definition); err != nil {
		return cleanup(err)
	}
	if err := temporary.Sync(); err != nil {
		return cleanup(err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(stagedPath)
		return nil, err
	}
	digest := sha256.Sum256(definition)
	update := &DefinitionUpdate{Target: target, CandidateSHA256: hex.EncodeToString(digest[:]), stagedPath: stagedPath}
	if info, err := os.Stat(target); err == nil {
		if !info.Mode().IsRegular() {
			_ = update.Commit()
			return nil, errors.New("installed service definition is not a regular file")
		}
		update.HadTarget = true
		update.CurrentSHA256, err = buildid.FileSHA256(target)
		if err != nil {
			_ = update.Commit()
			return nil, err
		}
		update.backupPath, err = copyBinaryTemporary(target, filepath.Dir(target), ".codexfold-definition-backup-*", info.Mode().Perm())
		if err != nil {
			_ = update.Commit()
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = update.Commit()
		return nil, err
	}
	if err := syncServiceDirectory(filepath.Dir(target)); err != nil {
		_ = update.Commit()
		return nil, err
	}
	return update, nil
}

func (u *DefinitionUpdate) Promote() error {
	if u == nil || u.Target == "" || u.stagedPath == "" {
		return errors.New("staged definition update is required")
	}
	if err := replaceServiceBinary(u.stagedPath, u.Target); err != nil {
		return err
	}
	u.stagedPath = ""
	if err := syncServiceDirectory(filepath.Dir(u.Target)); err != nil {
		return err
	}
	digest, err := buildid.FileSHA256(u.Target)
	if err != nil {
		return err
	}
	if digest != u.CandidateSHA256 {
		return fmt.Errorf("promoted definition digest=%s expected=%s", digest, u.CandidateSHA256)
	}
	return nil
}

func (u *DefinitionUpdate) Rollback() error {
	if u == nil || u.Target == "" {
		return nil
	}
	if u.HadTarget {
		if u.backupPath == "" {
			return errors.New("definition rollback backup is unavailable")
		}
		if err := replaceServiceBinary(u.backupPath, u.Target); err != nil {
			return err
		}
		u.backupPath = ""
	} else if err := os.Remove(u.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncServiceDirectory(filepath.Dir(u.Target)); err != nil {
		return err
	}
	if u.HadTarget {
		digest, err := buildid.FileSHA256(u.Target)
		if err != nil {
			return err
		}
		if digest != u.CurrentSHA256 {
			return fmt.Errorf("rolled back definition digest=%s expected=%s", digest, u.CurrentSHA256)
		}
	}
	return nil
}

func (u *DefinitionUpdate) Commit() error {
	if u == nil {
		return nil
	}
	var result error
	for _, path := range []string{u.stagedPath, u.backupPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	u.stagedPath = ""
	u.backupPath = ""
	return errors.Join(result, syncServiceDirectory(filepath.Dir(u.Target)))
}
