package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jstar0/codexfold/internal/buildid"
)

type BinaryUpdate struct {
	Target          string `json:"target"`
	Candidate       string `json:"candidate"`
	CurrentSHA256   string `json:"current_sha256"`
	CandidateSHA256 string `json:"candidate_sha256"`
	stagedPath      string
	backupPath      string
}

func StageBinaryUpdate(candidate string, target string) (*BinaryUpdate, error) {
	if !filepath.IsAbs(candidate) || !filepath.IsAbs(target) {
		return nil, errors.New("absolute candidate and target binary paths are required")
	}
	candidate = filepath.Clean(candidate)
	target = filepath.Clean(target)
	if candidate == target {
		return nil, errors.New("candidate binary must be separate from the installed target")
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !targetInfo.Mode().IsRegular() {
		return nil, errors.New("installed service binary is not a regular file")
	}
	candidateInfo, err := os.Stat(candidate)
	if err != nil {
		return nil, err
	}
	if !candidateInfo.Mode().IsRegular() || candidateInfo.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("candidate service binary must be a regular executable file")
	}
	currentSHA256, err := buildid.FileSHA256(target)
	if err != nil {
		return nil, err
	}
	candidateSHA256, err := buildid.FileSHA256(candidate)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(target)
	stagedPath, err := copyBinaryTemporary(candidate, root, ".codexfold-candidate-*", targetInfo.Mode().Perm())
	if err != nil {
		return nil, err
	}
	backupPath, err := copyBinaryTemporary(target, root, ".codexfold-backup-*", targetInfo.Mode().Perm())
	if err != nil {
		_ = os.Remove(stagedPath)
		return nil, err
	}
	if err := syncServiceDirectory(root); err != nil {
		_ = os.Remove(stagedPath)
		_ = os.Remove(backupPath)
		return nil, err
	}
	return &BinaryUpdate{
		Target: target, Candidate: candidate, CurrentSHA256: currentSHA256, CandidateSHA256: candidateSHA256,
		stagedPath: stagedPath, backupPath: backupPath,
	}, nil
}

func (u *BinaryUpdate) Promote() error {
	if u == nil || u.stagedPath == "" || u.Target == "" {
		return errors.New("staged binary update is required")
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
		return fmt.Errorf("promoted binary digest=%s expected=%s", digest, u.CandidateSHA256)
	}
	return nil
}

func (u *BinaryUpdate) Rollback() error {
	if u == nil || u.backupPath == "" || u.Target == "" {
		return errors.New("binary update backup is unavailable")
	}
	if err := replaceServiceBinary(u.backupPath, u.Target); err != nil {
		return err
	}
	u.backupPath = ""
	if err := syncServiceDirectory(filepath.Dir(u.Target)); err != nil {
		return err
	}
	digest, err := buildid.FileSHA256(u.Target)
	if err != nil {
		return err
	}
	if digest != u.CurrentSHA256 {
		return fmt.Errorf("rolled back binary digest=%s expected=%s", digest, u.CurrentSHA256)
	}
	return nil
}

func (u *BinaryUpdate) Commit() error {
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

func copyBinaryTemporary(source string, directory string, pattern string, mode os.FileMode) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, pattern)
	if err != nil {
		_ = input.Close()
		return "", err
	}
	path := temporary.Name()
	cleanup := func(operationErr error) (string, error) {
		_ = input.Close()
		_ = temporary.Close()
		_ = os.Remove(path)
		return "", operationErr
	}
	if err := temporary.Chmod(mode); err != nil {
		return cleanup(err)
	}
	if _, err := io.Copy(temporary, input); err != nil {
		return cleanup(err)
	}
	if err := input.Close(); err != nil {
		return cleanup(err)
	}
	if err := temporary.Sync(); err != nil {
		return cleanup(err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
