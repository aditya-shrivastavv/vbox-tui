# Terminal workflow decisions

## Inline task tools

Do not request alternate-screen mode or fit output to terminal height. Render only the question, a small row window, validation/status, and relevant hints. Height comes from content, bounded by available rows. Handle missing width before the first resize message without creating a giant blank frame.

When an action finishes, end the renderer and print one durable result. Avoid printing directly from a command while the renderer owns stdout. Preserve prior scrollback and cursor visibility. Windows PTYs, legacy consoles and redirected stdio differ: test the supported host, not only a Unix pipe.

Type-to-filter and letter shortcuts compete. Decide which keys insert text in each mode; do not let quit shortcuts eat ordinary form input. Keep marks by stable identity while filtering. Cancellation of a wait must not claim cancellation of a mutation already issued.

## Async commands

Capture target ID, generation, parameters and backend references before constructing a command. A result should carry enough context for `Update` to reject it if the user has navigated elsewhere. Return data or errors; never write model fields from an async closure. Start a spinner/timer only for active work and retire its generation when leaving the mode.

For dependent effects, let completion trigger the next effect. Concurrent batching is appropriate only for independent work. If persistence fails, preserve the prior committed model state; a visually successful edit must not outlive a failed disk write.

## Interactive child processes

Choose one lifecycle:

1. Persistent app: supported Bubble Tea execution mechanism suspends rendering, gives the child stdin/stdout/stderr, then delivers completion and restores the interface.
2. One-shot tool: finish the inline picker, restore terminal state, run the child with inherited terminal streams, then propagate its exit status to the shell.

Do not apply hidden-window/detached process flags to a child that needs keyboard interaction. Keep captured bounded runners for background queries. Verify Ctrl+C behavior, host-key/password prompts, nonzero exits, process-start errors and cleanup. Avoid a one-minute command context around an arbitrarily long user session unless that limit is deliberate.

## Safe text and cell layout

Control text from filenames, server messages, process errors and resource names can manipulate terminal state. Encode dangerous controls visibly at the external-text boundary, including CR/backspace/ESC/OSC/C1; consider directional formatting for identifiers. Apply trusted styles afterward. Keep the interactive child stream separate: it legitimately uses terminal control sequences.

Use maintained ANSI-aware width and grapheme tools rather than rune counts or manual wide-character tables. Include padding/borders in the width budget. Prefer name-first rows; remove optional badges before truncating identity. Do not use a visible short name as an operation key.

## Version compatibility

Inspect `go.mod`, then local dependency source/docs. Check the exact `Model`, key messages, `View`, execution, styles, cursor and program-option APIs for that version. Do not paste a v1 model into a v2 project or vice versa. Upgrade a major version only as a deliberate change with companion dependency and regression review.
