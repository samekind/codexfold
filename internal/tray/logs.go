package tray

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samekind/codexfold/internal/service"
)

func logDirectories(store string) map[string]string {
	locations := map[string]string{"filesystem": filepath.Join(store, "fs"), "enrollment": filepath.Join(store, "enrollment")}
	root := os.Getenv("ProgramData")
	if root == "" {
		return locations
	}
	root = filepath.Join(root, "CodexFold")
	read := func(path string) []byte {
		file, err := openSharedFile(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 65537))
		if err != nil || len(data) > 65536 {
			return nil
		}
		return data
	}
	if config, err := service.ParseWindowsConfig(read(filepath.Join(root, "service.json"))); err == nil && config.ServiceName == "com.codexfold.fs" {
		for index, arg := range config.Arguments {
			if arg == "--store" && index+1 < len(config.Arguments) && strings.EqualFold(filepath.Clean(config.Arguments[index+1]), filepath.Clean(store)) {
				locations["filesystem"] = filepath.Dir(config.StderrPath)
			}
		}
	}
	if config, err := service.ParseWindowsEnrollmentConfig(read(filepath.Join(root, "Enrollment", "service.json"))); err == nil && config.ServiceName == "com.codexfold.enroll" && strings.EqualFold(filepath.Clean(config.Store), filepath.Clean(store)) {
		locations["enrollment-service"] = filepath.Dir(config.StderrPath)
	}
	return locations
}
