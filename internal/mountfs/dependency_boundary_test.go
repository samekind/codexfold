package mountfs

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAdapterCoreDoesNotDependOnCodexDatabase(t *testing.T) {
	repoRoot := dependencyRepoRoot(t)

	assertPackageExcludesDependencies(t, repoRoot, "./internal/fold", []string{
		"github.com/samekind/codexfold/internal/codex",
		"modernc.org",
	})
	assertPackageExcludesDependencies(t, repoRoot, "./internal/mountfs", []string{
		"github.com/samekind/codexfold/internal/codex",
		"github.com/samekind/codexfold/internal/service",
		"modernc.org",
	})
}

func dependencyRepoRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve dependency boundary test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}

func assertPackageExcludesDependencies(t *testing.T, repoRoot, packagePath string, forbidden []string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", packagePath)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list dependencies for %s: %v\n%s", packagePath, err, output)
	}

	dependencies := make(map[string]struct{})
	for _, dependency := range strings.Fields(string(output)) {
		dependencies[dependency] = struct{}{}
	}
	for dependency := range dependencies {
		for _, forbiddenPrefix := range forbidden {
			if dependency == forbiddenPrefix || strings.HasPrefix(dependency, forbiddenPrefix+"/") {
				t.Errorf("%s must not depend on %s", packagePath, dependency)
			}
		}
	}
}
