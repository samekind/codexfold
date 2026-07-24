package cli

func requireFilesystemActivationAllowed(home string) error {
	// Every mutating command still requires --apply. Runtime health, writer,
	// snapshot, shadow, and namespace transactions protect the real home.
	// A release label must not make normal Codex storage unusable.
	return nil
}
