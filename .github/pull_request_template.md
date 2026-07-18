## Outcome

Describe the user-visible or maintainer-visible result.

## Safety Impact

Describe effects on session bytes, routing, storage, filesystem behavior, service lifecycle, compatibility, and rollback. Write `None` only when none apply.

## Verification

List the exact commands and real environments used. Distinguish unit, synthetic, mounted-adapter, real-client, restart, and production evidence.

## Checklist

- [ ] The change matches the product contract and does not weaken a release gate.
- [ ] Tests cover the behavior or regression.
- [ ] `go test ./...`, race tests, vet, formatting, and required cross-builds pass.
- [ ] Platform-specific validation was run when platform code changed.
- [ ] Documentation and readiness language match the available evidence.
- [ ] No real rollout, credential, private prompt, local path, database, log, build artifact, or Xcode user state is included.
- [ ] Production Codex data and production service definitions were not used for development validation.
