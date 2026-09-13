package cli

import (
	"os"
	"testing"
)

func TestInterruptedMigrationSkipsNativeContentAfterWrites(t *testing.T) {
	for _, mode := range []string{"backing", "delta", "untouched"} {
		t.Run(mode, func(t *testing.T) {
			fixture := interruptedCanonicalMigrationFixture(t)
			defer fixture.resolver.Close()
			state := fixture.managed.State()
			if err := os.Remove(fixture.nativePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(fixture.nativePath, 0700); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "backing":
				state.BackingPath = "/already-materialized"
			case "delta":
				if err := os.WriteFile(state.DeltaPath, []byte("appended\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := recoverInterruptedCanonicalMigration(fixture.home, fixture.store, fixture.nativeRoot, state)
			if recovered {
				t.Fatal("unexpected recovery of non-regular native source")
			}
			if mode == "untouched" {
				if err == nil {
					t.Fatal("untouched migration must still validate native content")
				}
			} else if err != nil {
				t.Fatalf("unnecessary native content access after writes: %v", err)
			}
			if _, pending, err := readRetirementRequest(fixture.store, state.SessionID); err != nil || pending {
				t.Fatalf("unexpected retirement request: pending=%t err=%v", pending, err)
			}
		})
	}
}
