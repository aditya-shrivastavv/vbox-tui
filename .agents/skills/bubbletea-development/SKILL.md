---
name: bubbletea-development
description: Build, refine, and debug Go terminal applications using Bubble Tea, Bubbles, and Lip Gloss. Use for inline CLI prompts or persistent terminal interfaces, including keyboard flows, async operations, resizing, and subprocess handoff.
---

# Bubble Tea development

## Establish the interaction contract

Identify whether this tool is a short inline journey or a persistent full-screen application. Preserve an established product choice. For a new short task, prefer a compact select → action → result → shell flow unless the user wants a dashboard. Do not silently turn a picker into a terminal-sized GUI.

Read the module's actual dependency versions and nearby code before choosing APIs. Bubble Tea major versions and companion libraries differ; match the project's imports. Verify unfamiliar APIs in the installed module or official documentation rather than mixing tutorials from different versions.

Official sources: [Bubble Tea](https://github.com/charmbracelet/bubbletea), [versioned Go documentation](https://pkg.go.dev/github.com/charmbracelet/bubbletea), [Bubbles](https://github.com/charmbracelet/bubbles), [Lip Gloss](https://github.com/charmbracelet/lipgloss). For new projects verify current module paths; these links do not pin a version or authorize upgrades.

## Model transitions explicitly

Keep `View` pure. Only `Update` applies state changes; commands perform effects and return typed messages. Capture immutable inputs in commands instead of mutating model fields from goroutines. Keep identity separate from cursor position and display labels.

Each multi-step flow needs an explicit entry/reset, back destination, cancellation behavior, validation, and commit boundary. Set chooser identity on entry. Audit Escape and Ctrl+C in every mode, including nested choices and busy states; a footer promising cancellation is not a handler.

Use contexts/timeouts for external work. Tag async results with a generation and relevant target so late replies cannot overwrite a newer interaction. Avoid duplicate tick chains. `tea.Batch` does not order commands: dependent work must be sequenced explicitly with the version-supported API or messages.

## Rendering and terminal ownership

Read [references/terminal-workflows.md](references/terminal-workflows.md) when implementing inline prompts, child processes, or rendering problems.

- Inline: no alternate screen, full-height padding or screen clearing. Render content-sized rows, a bounded list viewport and one final summary after the renderer exits.
- Full-screen: allocate layout from available cells and preserve focus/selection across resize; use it only when the interaction warrants it.
- Sanitize external text before adding trusted application styles. ANSI-aware truncation preserves escape sequences; it is not sanitization. Keep raw identifiers for commands/resolution. Use maintained cell/grapheme-aware utilities for layout.
- Interactive children must own the terminal, via the framework's process API or by exiting the prompt before handoff. Do not pipe a child that needs password/key prompts through a captured-output runner.

## Verification and maintainability

Keep CLI wiring thin, effects in focused backend packages, and prompt transitions testable through fake effects. Do not enforce an elaborate folder tree for a small application.

Test meaningful journeys: success, refusal, validation, back/cancel, state changes, stale results and child failure. Add rendering cases for narrow widths, Unicode, long values and hostile control characters. Exercise actual terminal behavior when tooling permits; do not claim strings/snapshots alone prove scrollback preservation or terminal cleanup.

Run formatting, vet, tests and a target-platform build appropriate to the change. Report unavailable runtime checks. Demo mode should use isolated fake persistence/effects, not production configuration. Product-specific permissions and invariants come from the user's task and repository instructions; this skill grants no external side effects.
