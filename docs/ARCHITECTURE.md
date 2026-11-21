# Architecture and extension guide

## One action, then exit

`cmd/vbx` calls `internal/cli`. The CLI resolves explicit targets or starts an inline Bubble Tea model. Prompt state gathers choices, issues bounded backend commands, and ends with a summary or OpenSSH handoff. The compatibility executable shares the CLI implementation.

Current source is authoritative, not a historical prompt or roadmap.

| Package | Responsibility |
| --- | --- |
| `internal/cli` | Flags, direct commands, dependency wiring, output and handoff |
| `internal/app` | Prompt model, transitions, views and feature journeys |
| `internal/vbox` | Inventory parsing, UUID resolution, lifecycle and snapshots |
| `internal/config` | UUID-bound settings, validation and safe persistence |
| `internal/ssh` | OpenSSH arguments, readiness and fixed restart commands |
| `internal/safedisplay` | Render untrusted metadata as visible text |
| `internal/runner` | Captured bounded commands versus interactive children |
| `internal/demo` | Fake operations and in-memory settings |

## State and identity

UUIDs bind selection, settings and operations across name changes. Snapshot UUIDs bind restore even when names repeat. Never use a truncated or sanitized display value to construct a command or recover identity.

Prompt modes include selection, settings, generic choices, text input, snapshots, confirmation, progress and completion. Feature entry functions initialize their routing state. Async messages carry generation/target context so obsolete results cannot mutate a newer interaction.

## Extending an action

1. Define state rules and CLI syntax.
2. Add the backend operation and fake, with safe argv/UUID tests.
3. Add direct and inline journeys using shared execution rules.
4. Specify back/cancel/save behavior; separate unsaved edits from stored data.
5. Test refusal, cancellation, state changes and partial failures.
6. Update help, README and `docs/USAGE.md`.

Do not dispatch generic arbitrary command strings to add capabilities. Keep irreversible operations explicit and bounded.

## Review-sensitive boundaries

- External metadata/errors cross a terminal boundary: encode before trusted styling.
- A delivered ACPI request does not prove shutdown. A dropped SSH connection does not prove reboot.
- Confirmation can remain open while VM state changes. Revalidate at execution and propagate locking failures.
- Preserve valid data on failed writes; refuse incidental overwrite of invalid originals.
- Interactive SSH owns the terminal. Application logging/rendering must not interfere.

See [SECURITY.md](../SECURITY.md) and the [reusable Bubble Tea skill](../.agents/skills/bubbletea-development/SKILL.md).
