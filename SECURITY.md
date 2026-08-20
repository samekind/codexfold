# Security Policy

CodexFold processes local conversation rollouts that may contain secrets or private data. The project does not transmit scan inputs or report field contents.

## Supported Code

Security fixes target the latest release and the current `main` branch. Preview filesystem branches may change quickly and must not be treated as production-safe unless the repository explicitly publishes a platform readiness claim.

## Reporting

Please report vulnerabilities privately through GitHub Security Advisories. Do not include real session files, credentials, private prompts, local filesystem paths, service tokens, or unredacted logs in public issues.

Include the affected version or commit, operating system, impact, and a minimal synthetic reproducer when possible. Maintainers will acknowledge the report, assess whether private coordination is required, and publish remediation details after a safe fix is available.

## Data Handling

Tests and bug reports must use generated or redacted fixtures. A contributor must never commit a real Codex rollout, Codex state database, credential, signing identity export, provisioning profile, or production service definition.

## Local Trust Boundary

Managed-deletion receipts defend against crashes, stale or replaced paths, interrupted replay, and cooperating concurrent CodexFold processes. Ordinary processes running as a different unprivileged account cannot modify a correctly permissioned store. Receipt hashes are integrity bindings, however, not keyed authentication against the account that owns the store.

CodexFold therefore does not claim that purge receipt v3 is a security boundary against `root` or a malicious same-UID process that can coherently rewrite store paths, metadata, tombstones, receipts, and checkpoints. Such an actor is inside the current local trust boundary. Reports and readiness claims must state this limitation rather than presenting crash-safe deletion as protection from a hostile store owner.

Physical managed deletion requires an explicit platform identity proof for the quarantine root and every recorded entry. Receipts that predate the proof/source fields, omit them, or mix fields from different platform proof profiles do not carry deletion authority even if their version is still `3` and their remaining hashes are structurally valid.

On Linux, device/inode/UID/GID, file length and content hashes, and `statx.mnt_id` do not substitute for a durable inode generation. CodexFold requires both `statx` birth time from the same open descriptor and a nonzero `FS_IOC_GETVERSION` value. If either capability is absent, unsupported, zero, inconsistent, or changes during capture, physical purge fails closed and preserves the tombstone and quarantine.
