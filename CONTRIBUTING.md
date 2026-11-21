# Contributing

Small changes that make everyday VM actions faster and clearer are welcome. Discuss larger workflows in an issue before implementation; preserve the inline prompt experience.

## Setup and validation

Use Go 1.25 or newer:

```text
go vet ./...
go test ./...
go build -trimpath -o dist/vbx.exe ./cmd/vbx
```

VirtualBox is unnecessary for fake-backed tests and `--demo`. OpenSSH is required only for real SSH sessions. See [architecture](docs/ARCHITECTURE.md) and [agent instructions](AGENTS.md).

- Keep CLI parsing, prompt transitions, backend operations, and persistence separate.
- Add regression tests for changed behavior, including cancellation and failures.
- Use fake managers and temporary config directories; never automate real VM mutations without authorization.
- For interface changes, inspect narrow widths, long Unicode names, Escape, Ctrl+C, and final shell output.
- Format changed Go files. Report unavailable checks explicitly.
- Update help and examples when behavior changes. Avoid speculative support or release claims.

## Pull requests and reports

Explain the user-visible problem, resulting behavior, and validation. Include a synthetic demo recording/transcript when useful. Keep unrelated cleanup out of the change.

Never attach real SSH settings, credentials, production inventories or personal filesystem paths. Review Git author name/email if you prefer public-handle attribution; commit metadata is separate from file content.

For bugs, include OS, terminal, build/VirtualBox versions and a sanitized reproduction. Report vulnerabilities through [SECURITY.md](SECURITY.md).

Be respectful and focus feedback on the work. Contributions are under the repository's [MIT license](LICENSE).
