# Security

## Reporting

Use [GitHub private vulnerability reporting](https://github.com/aditya-shrivastavv/vbox-tui/security/advisories/new) if enabled for this repository. If unavailable, open an issue requesting a private channel **without** exploit details or sensitive data. Do not include credentials, private endpoints or real machine inventories. No response-time commitment is currently offered.

Include the affected commit/version, platform, prerequisites, synthetic reproduction and realistic impact. There is no published long-term support matrix yet.

## Scope and boundaries

`vbx` is a local CLI using the invoking user's VirtualBox/OpenSSH authority. It is not a sandbox, credential vault, hypervisor or remote service. The OS account, intentionally selected executables and configured SSH files are trusted. VM/snapshot metadata, peer data and captured output are untrusted display text.

Assets include VM state, accurate target/action confirmations, saved settings and terminal integrity. The following are required properties, not proof that every path is defect-free:

- Validated VM/snapshot identity and unambiguous target resolution.
- Argument-array local processes and normal SSH host-key verification.
- No stored passwords or silent forced shutdown/reset fallback.
- Escaping external text before application-owned rendering.
- Revalidation before destructive operations, preservation on failed config writes, and aborting restore when requested protection fails.
- Demo/tests isolated from real machines and settings.

Reports should identify the actual boundary crossed and additional impact. A same-user selection of an executable alone does not establish escalation; that assumption must not dismiss imported-metadata, dependency, persistence or misleading-confirmation vulnerabilities. No blanket finding-class exclusions are declared.

## Limitations

VirtualBox, OpenSSH, terminal and OS security must be considered separately. A banner is not authentication; a snapshot is not a backup. Tests/static review do not establish absence of vulnerabilities. See the [release checklist](docs/RELEASING.md) for dependency and privacy checks.
