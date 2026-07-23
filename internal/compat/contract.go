package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const ContractVersion = 1

type Operation struct {
	Name       string      `json:"name"`
	Count      int         `json:"count"`
	Signatures []Signature `json:"signatures,omitempty"`
}

type Signature struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type Contract struct {
	Version       int         `json:"version"`
	Platform      string      `json:"platform"`
	ClientKind    string      `json:"client_kind"`
	ClientVersion string      `json:"client_version"`
	Operations    []Operation `json:"operations"`
	TraceSHA256   string      `json:"trace_sha256"`
}

type ClientVersion struct {
	Platform string `json:"platform,omitempty"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
}

type Evaluation struct {
	Approved   bool            `json:"approved"`
	Quarantine bool            `json:"quarantine"`
	Unknown    []ClientVersion `json:"unknown,omitempty"`
}

func Evaluate(installed []ClientVersion, contracts []Contract) Evaluation {
	known := make(map[string]struct{}, len(contracts))
	for _, contract := range contracts {
		if validateContract(contract) == nil {
			known[contract.Platform+"\x00"+contract.ClientKind+"\x00"+contract.ClientVersion] = struct{}{}
		}
	}
	result := Evaluation{Approved: true}
	for _, client := range installed {
		key := client.Platform + "\x00" + client.Kind + "\x00" + client.Version
		if _, ok := known[key]; !ok || client.Kind == "" || client.Version == "" {
			result.Unknown = append(result.Unknown, client)
		}
	}
	if len(result.Unknown) != 0 {
		result.Approved = false
		result.Quarantine = true
	}
	return result
}

func Save(root string, contract Contract) (string, error) {
	if err := validateContract(contract); err != nil {
		return "", err
	}
	directory := filepath.Join(root, safeName(contract.Platform), safeName(contract.ClientKind))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, safeName(contract.ClientVersion)+".json")
	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".contract-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	return path, nil
}

func Load(path string) (Contract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Contract{}, err
	}
	var contract Contract
	if err := json.Unmarshal(data, &contract); err != nil {
		return Contract{}, err
	}
	if err := validateContract(contract); err != nil {
		return Contract{}, err
	}
	return contract, nil
}

func LoadAll(root string) ([]Contract, error) {
	var contracts []Contract
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		contract, err := Load(path)
		if err != nil {
			return err
		}
		contracts = append(contracts, contract)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].Platform != contracts[j].Platform {
			return contracts[i].Platform < contracts[j].Platform
		}
		if contracts[i].ClientKind != contracts[j].ClientKind {
			return contracts[i].ClientKind < contracts[j].ClientKind
		}
		return contracts[i].ClientVersion < contracts[j].ClientVersion
	})
	return contracts, err
}

func validateContract(contract Contract) error {
	if contract.Version != ContractVersion || contract.Platform == "" || contract.ClientKind == "" || contract.ClientVersion == "" || len(contract.TraceSHA256) != 64 {
		return errors.New("invalid compatibility contract metadata")
	}
	for _, operation := range contract.Operations {
		if operation.Name == "" || operation.Count <= 0 {
			return errors.New("invalid compatibility operation")
		}
		for _, signature := range operation.Signatures {
			if signature.Value == "" || signature.Count <= 0 || strings.ContainsAny(signature.Value, "/\\") {
				return errors.New("invalid compatibility operation signature")
			}
		}
	}
	return nil
}

func safeName(value string) string {
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			return character
		}
		return '_'
	}, value)
	if value == "" || value == "." || value == ".." {
		return fmt.Sprintf("value-%x", []byte(value))
	}
	return value
}
