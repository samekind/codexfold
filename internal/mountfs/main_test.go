package mountfs

import (
	"os"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

// See internal/fold/main_test.go: fixtures here write a few hundred bytes, so
// the production free-space reserve would make unrelated tests fail on a host
// that is low on space. Budget behaviour is covered by internal/storage.
func TestMain(m *testing.M) {
	storage.DefaultLimits.FreeSpaceReserveBytes = 1 << 20
	os.Exit(m.Run())
}
