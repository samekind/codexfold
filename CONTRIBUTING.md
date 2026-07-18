# Contributing to CodexFold

CodexFold handles private local session data and implements storage and filesystem behavior where a silent mismatch is unacceptable. Contributions should be small enough to review, explicit about safety boundaries, and backed by evidence that matches the claim.

## Before You Start

- Read the [product contract](docs/superpowers/specs/2026-07-11-transparent-session-filesystem-design.md), the [implementation alignment](docs/superpowers/specs/2026-07-14-transparent-session-filesystem-implementation-alignment.md), and the [maintainer guide](docs/maintainer-guide.md).
- Open an issue for product behavior, format, compatibility, or architecture changes before implementing a large change.
- Never use a real rollout, state database, credential, signing export, or private prompt as a fixture.
- Do not weaken a release gate to make an implementation pass.

## Development Setup

Requirements:

- Go version declared in `go.mod`.
- Git.
- Xcode 27 and XcodeGen for native macOS FSKit work.
- Platform prerequisites only when testing the corresponding preview adapter.

Run the common quality gate:

```bash
./scripts/test-cross-platform.sh
git diff --check
```

For a focused change, run the smallest relevant package tests first, then the common gate before requesting review.

Windows currently has compile and cross-build gates only. Do not describe those checks as real WinFsp or Windows Service runtime validation.

## Branches and Commits

- Create a branch from current `main`; use a descriptive prefix such as `feat/`, `fix/`, `docs/`, or `test/`.
- Use conventional, imperative commit subjects such as `fix: reject stale native writer state`.
- Keep generated files, binaries, user-specific Xcode state, local databases, and test artifacts out of commits.
- Update `platform/darwin/fskit/project.yml` first and regenerate the Xcode project; do not hand-maintain personal Xcode state.

## Pull Requests

- Explain the user-visible outcome, safety impact, and exact evidence.
- Distinguish unit, synthetic, mounted-adapter, real-client, host-restart, and production evidence. One category never substitutes for another.
- Add or update tests before changing a status or readiness claim.
- Keep production Codex homes and production service definitions untouched during development validation.
- Resolve review conversations and keep the branch current with `main` before merge.

## Filesystem Changes

Native filesystem work must remain isolated until every relevant gate passes. Use disposable homes, stores, native roots, mount points, and service labels. A test may not route a production SQLite record or remove a production JSONL source.

The macOS terminal architecture is:

```text
Codex CLI / Desktop
        -> Apple-native Swift FSKit extension
        -> versioned binary UDS IPC
        -> Go CodexFold daemon
        -> packfile + index + manifest + append delta + COW backing
```

NFS and FUSE-T are development fallback or historical evidence only. They are not acceptable replacements for native FSKit release gates.

## Security

Report vulnerabilities through GitHub Security Advisories. Public issues and pull requests must contain only synthetic or fully redacted data; see [SECURITY.md](SECURITY.md).
