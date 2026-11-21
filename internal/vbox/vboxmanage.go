package vbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
)

// DefaultTimeout bounds a single VBoxManage invocation. VBoxManage answers
// queries immediately and lifecycle calls return as soon as the request has been
// handed to the VM process, so this only guards against a wedged machine.
const DefaultTimeout = 20 * time.Second

// Manager is everything the UI needs from a VirtualBox backend. The demo mode
// implements the same interface, so the whole application can be exercised
// without touching a real hypervisor.
type Manager interface {
	// List returns the registered machines and their states. Problems inside
	// the returned Inventory describe machines that could not be read.
	List(ctx context.Context) (Inventory, error)
	// Detail returns configuration for one machine, addressed by UUID.
	Detail(ctx context.Context, uuid string) (Machine, error)
	// PowerOn starts a powered-off machine headless, or restores a machine
	// whose state was saved. VirtualBox uses the same request for both.
	PowerOn(ctx context.Context, uuid string) error
	// Resume un-pauses a paused machine.
	Resume(ctx context.Context, uuid string) error
	// Pause freezes a running machine: its processes stop executing until it
	// is resumed, and nothing is written to disk.
	Pause(ctx context.Context, uuid string) error
	// ShutdownGracefully sends the ACPI power button. Success means the request
	// was delivered, not that the guest has stopped.
	ShutdownGracefully(ctx context.Context, uuid string) error
	// SaveState suspends a running machine to disk. This is not a shutdown.
	SaveState(ctx context.Context, uuid string) error
	// GuestReboot asks the guest operating system to reboot itself. It is
	// never a hardware reset and never a power cycle; the backend must
	// report honestly when no guest-control channel can deliver it.
	GuestReboot(ctx context.Context, uuid string) error
	// Version identifies the backend for the header, e.g. "VirtualBox 7.2.20".
	Version(ctx context.Context) (string, error)
	// ListSnapshots returns the machine's snapshot tree. No snapshots is
	// the empty tree, not an error.
	ListSnapshots(ctx context.Context, uuid string) (SnapshotTree, error)
	// TakeSnapshot records the machine's current state under a name. A
	// running machine's snapshot captures its memory state as well; the
	// operation is non-destructive to the machine.
	TakeSnapshot(ctx context.Context, uuid, name, description string) error
	// RestoreSnapshot returns the machine to a snapshot, discarding the
	// state created since. The caller must guarantee the machine is
	// powered off; VirtualBox rejects live restores with a saved state.
	RestoreSnapshot(ctx context.Context, uuid, snapshotUUID string) error
	// RenameVM changes a machine's display name. The machine is addressed
	// by UUID, the new name travels as one argument array element, and the
	// operation never stops, restarts, unregisters, clones, or moves the
	// machine: VirtualBox only honours a rename while the machine is
	// powered off, and a refusal is returned verbatim.
	RenameVM(ctx context.Context, uuid, newName string) error
}

// CLI talks to a real VBoxManage binary.
type CLI struct {
	path    string
	timeout time.Duration
	runner  runner.Runner
}

// NewCLI wraps an already-discovered VBoxManage path.
func NewCLI(path string) *CLI {
	return &CLI{path: path, timeout: DefaultTimeout, runner: runner.New()}
}

// SetTimeout overrides the per-command timeout.
func (c *CLI) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout = d
	}
}

// SetRunner replaces the command runner.
func (c *CLI) SetRunner(r runner.Runner) {
	if r != nil {
		c.runner = r
	}
}

// Path is the executable this CLI invokes.
func (c *CLI) Path() string { return c.path }

// Run invokes VBoxManage. --nologo suppresses the banner so output stays
// machine readable. Arguments reach exec as an array, never through a shell, so
// names with spaces, punctuation or Unicode need no quoting.
func (c *CLI) Run(ctx context.Context, args ...string) (string, error) {
	if c.path == "" {
		return "", fmt.Errorf("%w: no VBoxManage path configured", ErrNotFound)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	r := c.runner
	if r == nil {
		r = runner.New()
	}
	out, err := runner.Collect(ctx, r, c.path, append([]string{"--nologo"}, args...)...)
	if err != nil {
		return out, &CommandError{
			Args:     args,
			Stdout:   out,
			Stderr:   stderrOf(err),
			ExitCode: exitCodeOf(err),
			Err:      err,
			TimedOut: isTimeout(err, ctx),
		}
	}
	return out, nil
}

func stderrOf(err error) string {
	var re *runner.Error
	if errors.As(err, &re) {
		return re.Stderr
	}
	return ""
}

func exitCodeOf(err error) int {
	var re *runner.Error
	if errors.As(err, &re) {
		return re.ExitCode
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func isTimeout(err error, ctx context.Context) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var re *runner.Error
	if errors.As(err, &re) && re.TimedOut {
		return true
	}
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// CommandError describes a VBoxManage invocation that did not succeed, with the
// relevant stderr attached so the UI can show a real reason.
type CommandError struct {
	// Args are the arguments after the executable, so a path containing spaces
	// never makes the message ambiguous.
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
	Err      error
	TimedOut bool
}

func (e *CommandError) Error() string {
	var b strings.Builder
	b.WriteString("VBoxManage")
	if len(e.Args) > 0 {
		b.WriteString(" " + strings.Join(e.Args, " "))
	}
	switch {
	case e.TimedOut:
		b.WriteString(": timed out")
	case e.ExitCode == -1:
		b.WriteString(": could not be started")
	default:
		fmt.Fprintf(&b, ": failed with exit code %d", e.ExitCode)
	}
	if msg := vboxErrorMessage(e.Stderr); msg != "" {
		b.WriteString("\nVBoxManage says: " + msg)
	}
	return b.String()
}

// Unwrap exposes the underlying exec or context error.
func (e *CommandError) Unwrap() error { return e.Err }

// vboxErrorMessage pulls the informative lines out of VBoxManage's chatter:
// its own "error:" line and the indented continuation that explains it.
func vboxErrorMessage(stderr string) string {
	var keep []string
	for _, line := range splitLines(stderr) {
		l := strings.TrimRight(line, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		switch {
		case strings.Contains(strings.ToLower(l), "error:"),
			strings.HasPrefix(l, "Failed to "),
			strings.HasPrefix(l, "\t"),
			strings.HasPrefix(l, "  "):
			keep = append(keep, strings.TrimSpace(l))
		}
	}
	if len(keep) == 0 {
		// Fall back to the last non-empty line of whatever VirtualBox printed.
		// VBoxManage normally uses "error:", but a different build, a locale or
		// a wrapper script may word it differently; swallowing the only text we
		// were given would leave the user with "exited with code 1" and nothing
		// to act on.
		lines := splitLines(stderr)
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return runner.Trim(l)
			}
		}
		return ""
	}
	return runner.Trim(strings.Join(keep, " | "))
}

// Version returns the VirtualBox version string, e.g. 7.2.20r175154.
func (c *CLI) Version(ctx context.Context) (string, error) {
	out, err := c.Run(ctx, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// BackendName identifies the backend for the header bar.
func (c *CLI) BackendName() string { return "VirtualBox" }

// List reads the registered machines with their states.
//
// `list vms --long` carries the state, and `list runningvms` is a second, cheap
// view of the truth. The two are reconciled so a machine that is mid-transition
// is shown as transitional rather than as a state the user could act on.
func (c *CLI) List(ctx context.Context) (Inventory, error) {
	out, err := c.Run(ctx, "list", "vms", "--long")
	if err != nil {
		return Inventory{}, err
	}
	inv := ParseListVMSLong(out)

	running := map[string]bool{}
	rout, rerr := c.Run(ctx, "list", "runningvms")
	switch {
	case rerr == nil:
		if machines, perr := ParseListVMS(rout); perr == nil {
			for _, m := range machines {
				running[strings.ToLower(m.UUID)] = true
			}
		} else {
			inv.Problems = append(inv.Problems, Problem{Err: "could not read the running machine list: " + perr.Error()})
		}
	case len(inv.Machines) == 0:
		// Nothing at all could be read, so surface the failure.
		return inv, rerr
	default:
		// A partial failure: keep the machines we have and report the gap.
		inv.Problems = append(inv.Problems, Problem{Err: "running machine list unavailable: " + briefErr(rerr)})
	}

	for i := range inv.Machines {
		m := &inv.Machines[i]
		if m.State.Kind == KindTransitional || m.State.Kind == KindUnavailable {
			continue // nothing to reconcile for a machine we already distrust
		}
		isRunning := running[strings.ToLower(m.UUID)]
		if isRunning && m.State.Kind != KindRunning && m.State.Kind != KindPaused {
			m.State = StateInfo{State: StateTransitioning, Kind: KindTransitional, Raw: "starting"}
			m.Notes = append(m.Notes, "the machine list and the running machine list disagreed")
		}
		if !isRunning && m.State.Kind == KindRunning {
			m.State = StateInfo{State: StateTransitioning, Kind: KindTransitional, Raw: "stopping"}
			m.Notes = append(m.Notes, "the machine is no longer in the running machine list")
		}
	}
	return inv, nil
}

// RunningUUIDs returns the machines VirtualBox currently reports as running.
func (c *CLI) RunningUUIDs(ctx context.Context) (map[string]bool, error) {
	out, err := c.Run(ctx, "list", "runningvms")
	if err != nil {
		return nil, err
	}
	machines, err := ParseListVMS(out)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(machines))
	for _, m := range machines {
		set[strings.ToLower(m.UUID)] = true
	}
	return set, nil
}

// Detail reads one machine's configuration by UUID. Display names are never used
// here: a name may contain spaces or be ambiguous after a rename.
func (c *CLI) Detail(ctx context.Context, uuid string) (Machine, error) {
	if strings.TrimSpace(uuid) == "" {
		return Machine{}, errors.New("cannot inspect a VM without a UUID")
	}
	out, err := c.Run(ctx, "showvminfo", uuid, "--machinereadable")
	if err != nil {
		return Machine{}, err
	}
	m, perr := ParseShowVMInfo(out)
	if perr != nil {
		return m, fmt.Errorf("could not understand VirtualBox output for %s: %w", uuid, perr)
	}
	return m, nil
}

// PowerOn starts a powered-off machine headless. A saved machine is restored by
// the same request: VirtualBox resumes from the saved state when one exists, so
// there is no separate "restore" verb to get wrong.
func (c *CLI) PowerOn(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "startvm", uuid, "--type", "headless")
	return err
}

// Resume un-pauses a machine.
func (c *CLI) Resume(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "controlvm", uuid, "resume")
	return err
}

// Pause freezes a running machine.
func (c *CLI) Pause(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "controlvm", uuid, "pause")
	return err
}

// ShutdownGracefully sends the ACPI power button.
//
// VirtualBox returns as soon as the guest received the request. The guest then
// decides what to do, so callers must keep polling the state and only report
// "powered off" when VirtualBox says so. vbox-tui never escalates to poweroff.
func (c *CLI) ShutdownGracefully(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "controlvm", uuid, "acpipowerbutton")
	return err
}

// SaveState suspends a running machine to disk. The guest's memory is kept and
// restored later; this is deliberately not a shutdown.
func (c *CLI) SaveState(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "controlvm", uuid, "savestate")
	return err
}

func requireUUID(uuid string) error {
	if strings.TrimSpace(uuid) == "" {
		return errors.New("refusing to run a VM operation without a UUID")
	}
	return nil
}

// GuestReboot asks the guest operating system to reboot itself
// (`controlvm reboot`). VirtualBox documents this as a guest OS request that
// requires Guest Additions; it is explicitly NOT `controlvm reset` (a cold
// hardware reset that loses unsaved state) and never powers the machine off.
// When the guest control channel is unavailable, VBoxManage says so and the
// error is surfaced verbatim rather than papered over with a reset.
func (c *CLI) GuestReboot(ctx context.Context, uuid string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	_, err := c.Run(ctx, "controlvm", uuid, "reboot")
	return err
}

// ListSnapshots reads the machine's snapshot tree in machine-readable form.
// "This machine does not have any snapshots" is VirtualBox's ordinary answer
// for an un-snaphotshot machine (printed with a failure status), so it maps
// to the empty tree rather than a scary error.
func (c *CLI) ListSnapshots(ctx context.Context, uuid string) (SnapshotTree, error) {
	if err := requireUUID(uuid); err != nil {
		return SnapshotTree{}, err
	}
	out, err := c.Run(ctx, "snapshot", uuid, "list", "--machinereadable")
	if err != nil {
		var ce *CommandError
		if errors.As(err, &ce) && mentionsNoSnapshots(ce.Stdout+"\n"+ce.Stderr) {
			return SnapshotTree{}, nil
		}
		return SnapshotTree{}, err
	}
	return ParseSnapshotList(out)
}

func mentionsNoSnapshots(blob string) bool {
	return strings.Contains(strings.ToLower(blob), noSnapshotsPhrase)
}

// TakeSnapshot records the machine's current state. A running machine's
// snapshot captures memory as well and is taken with --live so the guest is
// not paused to do it. Snapshot creation is non-destructive; it is not a
// backup — the images stay in the machine's folder.
func (c *CLI) TakeSnapshot(ctx context.Context, uuid, name, description string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}
	args := []string{"snapshot", uuid, "take", name}
	if d := strings.TrimSpace(description); d != "" {
		args = append(args, "--description="+d)
	}
	if running, rerr := c.RunningUUIDs(ctx); rerr == nil && running[canonicalUUID(uuid)] {
		// Live snapshotting only when VirtualBox itself says the machine runs;
		// a stopped machine must not be handed a --live request it would refuse.
		args = append(args, "--live")
	}
	_, err := c.Run(ctx, args...)
	return err
}

// RestoreSnapshot reverts the machine to the identified snapshot by UUID,
// discarding everything that happened since. The caller must have verified
// the machine is powered off: restoring while running takes a saved state of
// the live session, which silently throws that session away.
func (c *CLI) RestoreSnapshot(ctx context.Context, uuid, snapshotUUID string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	if !uuidLike(snapshotUUID) {
		return errors.New("refusing to restore a snapshot that is not identified by UUID")
	}
	_, err := c.Run(ctx, "snapshot", uuid, "restore", snapshotUUID)
	return err
}

// RenameVM changes a registered machine's name with
// `VBoxManage modifyvm <uuid> --name=<new>`. The VirtualBox manual is
// explicit on both halves of the contract: modifyvm operates only on a
// powered-off machine (a running or saved-state machine is refused by
// VirtualBox itself, and that refusal is returned verbatim), and the rename
// covers the machine's related internal files as well. vbx never stops,
// restarts, unregisters, clones, or moves anything to make the rename
// "work": the machine stays exactly where it is, and it is addressed by
// UUID so a duplicate display name can never aim the rename at the wrong
// VM. The new name travels as one element of an argument array — never
// through a shell.
func (c *CLI) RenameVM(ctx context.Context, uuid, newName string) error {
	if err := requireUUID(uuid); err != nil {
		return err
	}
	if err := ValidateVMName(newName); err != nil {
		return err
	}
	_, err := c.Run(ctx, "modifyvm", uuid, "--name="+newName)
	return err
}

// ValidateSnapshotName rejects names VBoxManage could misread as options or
// never find again. Everything else — spaces, Unicode, quotes — is legal,
// because arguments travel to exec as an array, never through a shell.
func ValidateSnapshotName(name string) error {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return errors.New("a snapshot needs a name")
	case strings.HasPrefix(n, "-"):
		return fmt.Errorf("snapshot name %q starts with '-' and would be read as an option", n)
	case len([]rune(n)) > 200:
		return errors.New("snapshot name is too long (max 200 characters)")
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return errors.New("snapshot name contains control characters")
		}
	}
	return nil
}

// capture records how a command was built, for tests.
type capture struct {
	name string
	args []string
}

func briefErr(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	if len([]rune(msg)) > 200 {
		r := []rune(msg)
		msg = string(r[:200])
	}
	return msg
}
