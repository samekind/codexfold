package cli

import "testing"

func TestFSServiceInstallEnablesBoundedAutomaticEnrollmentByDefault(t *testing.T) {
	interval, err := resolveServiceEnrollmentInterval(true, false, 0)
	if err != nil || interval != defaultServiceEnrollmentInterval {
		t.Fatalf("canonical enrollment interval = %s, err=%v, want %s", interval, err, defaultServiceEnrollmentInterval)
	}
	disabled, err := resolveServiceEnrollmentInterval(true, true, 0)
	if err != nil || disabled != 0 {
		t.Fatalf("explicitly disabled enrollment interval = %s, err=%v", disabled, err)
	}
	noncanonical, err := resolveServiceEnrollmentInterval(false, false, 0)
	if err != nil || noncanonical != 0 {
		t.Fatalf("noncanonical enrollment interval = %s, err=%v", noncanonical, err)
	}
	if _, err := resolveServiceEnrollmentInterval(false, true, defaultServiceEnrollmentInterval); err == nil {
		t.Fatal("noncanonical service accepted explicit periodic enrollment")
	}

	command := newFSServiceInstallCommand()
	stable := command.Flags().Lookup("enrollment-stable-for")
	if stable == nil || stable.DefValue != "1h0m0s" {
		t.Fatalf("stable window default = %#v, want 1h0m0s", stable)
	}
	batch := command.Flags().Lookup("enrollment-batch-size")
	if batch == nil || batch.DefValue != "1" {
		t.Fatalf("batch size default = %#v, want 1", batch)
	}
}
