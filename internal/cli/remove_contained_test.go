package cli

import (
	"io"
	"strings"
	"testing"
)

func TestRemoveContainedRecoverRequiresExplicitApply(t *testing.T) {
	command := newRemoveContainedCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"recover", "contained"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --apply") {
		t.Fatalf("recover without apply error = %v", err)
	}
}
