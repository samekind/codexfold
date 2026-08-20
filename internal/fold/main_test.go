package fold

import (
	"os"
	"testing"

	"github.com/samekind/codexfold/internal/storage"
)

// Production reserves several gigabytes of free space so CodexFold can never
// fill a disk. A fixture in this package writes a few hundred bytes, so
// inheriting that reserve makes every unrelated test fail on a host that
// happens to be low on space instead of testing anything. Budget behaviour
// itself is covered by internal/storage against explicit limits.
func TestMain(m *testing.M) {
	storage.DefaultLimits.FreeSpaceReserveBytes = 1 << 20
	os.Exit(m.Run())
}
