# Working on vbox-tui

## Product contract

The project is **vbox-tui**; the command is **vbx**. It is a compact inline action picker, not a fullscreen VirtualBox GUI replacement. Preserve shell scrollback and the select → action → result → exit flow. Keep VM names readable. Do not add alternate-screen mode, details panels, or persistent dashboards without an explicit product decision.

## Start here

Read `README.md`, `docs/ARCHITECTURE.md`, and relevant source. Check `git status` and preserve existing work. Versions in `go.mod` are authoritative; do not mix Bubble Tea major-version APIs or migrate frameworks incidentally.

For framework work, read `.agents/skills/bubbletea-development/SKILL.md`. That skill is reusable; project-specific behavior is defined here.

## Code map

- `cmd/vbx`: primary executable; `cmd/vbox-tui`: compatibility entry point.
- `internal/cli`: parsing, direct commands, wiring and terminal handoff.
- `internal/app`: prompt state, actions, batch/settings/rename/snapshot journeys.
- `internal/vbox`: parsing, UUID resolution and bounded VBoxManage operations.
- `internal/config`: validated, backwards-compatible persistence.
- `internal/ssh`: OpenSSH arguments, readiness and fixed guest reboot commands.
- `internal/safedisplay`: external-text escaping before trusted terminal styles.
- `internal/demo`: fake backend/settings; no real processes or production config.

## Invariants

- Address mutations and snapshot restore by validated UUIDs. Never silently fuzzy-match targets.
- Use subprocess argument arrays. Do not introduce shell interpolation, credential capture, disabled host-key checking, or forced shutdown/reset fallbacks.
- Keep raw metadata for matching and encode it for application-owned output. Do not filter the interactive SSH stream.
- Recheck state at mutation boundaries; confirmation-time state may be stale. Abort restore if protective snapshot creation/verification fails.
- Keep settings keyed by UUID after rename. Preserve malformed original config and detect conflicting writes. Update memory only after persistence succeeds.
- Commands return messages; `Update` owns state; `View` is pure. Bound/cancel waits and ignore stale completions after navigation.
- Every prompt defines Escape and Ctrl+C behavior. Set chooser identity explicitly on entry.

## Verify

```text
gofmt -w <changed Go files>
go vet ./...
go test ./...
go build -trimpath -o dist/vbx.exe ./cmd/vbx
```

Smoke-test help/version and demo journeys. Use fakes, temporary configuration, and adversarial display fixtures. Read-only discovery is acceptable; real start/stop/pause/restart/rename/snapshot/restore needs explicit user authorization.

Explain what changed, why, checks actually run, and limitations. Update help/docs when commands change. Do not claim unrun checks passed. Do not publish or push unless requested.

## Public repository hygiene

Use synthetic names, UUIDs, loopback addresses and generic paths. Never commit production config, private inventories, personal contacts, real-session recordings, or credentials. The repository owner handle is public attribution. Review Git author metadata separately from files; do not rewrite published history without an explicit plan and authorization.
