# CodexFold V0.1 Implementation Plan

## Goal

Publish a standalone, read-only Codex session deduplication evaluator with no private control-plane dependency or private data.

## Tasks

1. Scaffold the Go module, public documentation, Apache-2.0 license, security policy, and CI.
2. Add tests for the disk-backed dedup index, exact raw JSON token distinction, arbitrary-position field reuse, and CDC realignment after insertion.
3. Implement the dedup index and scanners with bounded memory and independent layer accounting.
4. Add read-only Codex SQLite discovery with configurable `--codex-home`.
5. Implement the `scan` CLI, explicit selection gate, JSON/text output, active-file change reporting, and disposable index behavior.
6. Run unit tests, build the standalone binary, and run a bounded real-session read-only smoke test.
7. Scan repository content and Git history for private paths, domains, credentials, real session data, and private control-plane names.
8. Create and push the public `jstar0/codexfold` repository only after the audit passes.
9. Add a private downstream wrapper that installs and invokes a pinned CodexFold release without requiring CodexFold to know about that consumer.
