package cli

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContentChangingReconcilePackageHasOneExplicitCLIBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
	internalRoot := filepath.Join(repoRoot, "internal")
	fset := token.NewFileSet()
	err := filepath.Walk(internalRoot, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			if imported.Path.Value != `"github.com/samekind/codexfold/internal/reconcile"` {
				continue
			}
			if filepath.Clean(path) != filepath.Join(repoRoot, "internal", "cli", "fs_reconcile.go") {
				t.Errorf("content-changing reconcile package imported by %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
