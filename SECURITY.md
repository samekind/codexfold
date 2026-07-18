# Security Policy

CodexFold processes local conversation rollouts that may contain secrets or private data. The project does not transmit scan inputs or report field contents.

## Supported Code

Security fixes target the latest release and the current `main` branch. Preview filesystem branches may change quickly and must not be treated as production-safe unless the repository explicitly publishes a platform readiness claim.

## Reporting

Please report vulnerabilities privately through GitHub Security Advisories. Do not include real session files, credentials, private prompts, local filesystem paths, service tokens, or unredacted logs in public issues.

Include the affected version or commit, operating system, impact, and a minimal synthetic reproducer when possible. Maintainers will acknowledge the report, assess whether private coordination is required, and publish remediation details after a safe fix is available.

## Data Handling

Tests and bug reports must use generated or redacted fixtures. A contributor must never commit a real Codex rollout, Codex state database, credential, signing identity export, provisioning profile, or production service definition.
