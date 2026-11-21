// Package cli is the vbx command line: argument parsing, the direct
// (scriptable) execution paths, and the inline prompt. cmd/vbx is the thin
// main over it; the legacy cmd/vbox-tui name is a compatibility shim. Keeping
// everything here means both names behave identically and the whole surface is
// testable without touching a process table.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/aditya-shrivastavv/vbox-tui/internal/app"
	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/demo"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
	"github.com/aditya-shrivastavv/vbox-tui/internal/safedisplay"
	appssh "github.com/aditya-shrivastavv/vbox-tui/internal/ssh"
	"github.com/aditya-shrivastavv/vbox-tui/internal/vbox"
	"github.com/aditya-shrivastavv/vbox-tui/internal/version"
)

// AppName is the binary name the messages use. The product directory and
// config file keep the historical vbox-tui name (config compatibility); the
// command is vbx.
const AppName = "vbx"

// exit codes.
const (
	exitOK          = 0
	exitRun         = 1 // an action failed or the user cancelled
	exitUsage       = 2
	exitEnvironment = 3 // VirtualBox missing or unwell, or the settings file unreadable
)

// verbs maps each quick command to the action it performs. The batchable set
// takes one or more VM names; ssh remains one machine per invocation; rename
// is one machine plus (optionally) the new name.
var verbs = map[string]vbox.Action{
	"start":   vbox.ActionStart,
	"pause":   vbox.ActionPause,
	"resume":  vbox.ActionResume,
	"stop":    vbox.ActionStop,
	"save":    vbox.ActionSaveState,
	"restart": vbox.ActionRestart,
	"ssh":     vbox.ActionConnect,
	// App-level journeys: not lifecycle verbs, they open flows.
	"config":    app.ActionSettings,
	"snapshots": app.ActionSnapshots,
	"rename":    app.ActionRename,
}

// options is the parsed command line.
type options struct {
	demo        bool
	vboxmanage  string
	configPath  string
	sshClient   string
	sshTimeout  time.Duration
	showVersion bool
	noColor     bool
	showPath    bool
	autoConfirm bool

	// action, when non-empty, is the focused verb from a subcommand.
	action vbox.Action
	// targets are the VM names or UUIDs given after the subcommand.
	targets []string
}

// Run is the entry point shared by both command names: os.Args tail in,
// exit code out.
func Run(argv []string, stdout, stderr io.Writer) int {
	opts, code, handled := parseArgs(argv, stdout, stderr)
	if handled {
		return code
	}
	if opts.showPath {
		p, err := config.Path()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", AppName, err)
			return exitEnvironment
		}
		fmt.Fprintln(stdout, p)
		return exitOK
	}
	if err := validate(&opts); err != nil {
		fmt.Fprintf(stderr, "%s: %v\nTry %s --help\n", AppName, err, AppName)
		return exitUsage
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, version.String())
		return exitOK
	}
	if opts.noColor {
		// The legacy Windows console renders truecolour as garbage and some
		// over-ssh setups strip support after detection. Forcing the ASCII
		// profile before anything paints keeps the layout identical and drops
		// only colour: every meaning here is carried by text anyway.
		lipgloss.SetColorProfile(termenv.Ascii)
	}

	cfg, cfgErr := loadConfig(opts, stderr)

	if opts.demo {
		return runDemo(cfg, opts, stdout, stderr)
	}
	return runReal(cfg, cfgErr, opts, stdout, stderr)
}

func parseArgs(argv []string, stdout, stderr io.Writer) (options, int, bool) {
	var opts options

	// --help wins over everything, including garbage later on the line, so a
	// confused user can always read the instructions.
	for _, a := range argv {
		if a == "-h" || a == "--help" || a == "-?" {
			fmt.Fprint(stdout, longHelpText())
			return opts, exitOK, true
		}
		if a == "-v" || a == "--version" {
			fmt.Fprintln(stdout, version.String())
			return opts, exitOK, true
		}
	}

	// Peel a subcommand off the front: `vbx start "My VM" "Other"`. The verb
	// is the first non-flag argument; everything after it up to the first
	// flag is a VM target. The config verb takes one optional target plus
	// --path/--yes modifiers.
	rest := argv
	if len(argv) > 0 {
		if action, ok := verbs[argv[0]]; ok {
			opts.action = action
			rest = argv[1:]
			for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
				opts.targets = append(opts.targets, rest[0])
				rest = rest[1:]
			}
		}
	}

	fs := flag.NewFlagSet(AppName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stdout, shortHelp()) }
	fs.BoolVar(&opts.demo, "demo", false, "show fake machines; run nothing real")
	fs.StringVar(&opts.vboxmanage, "vboxmanage", "", "path to VBoxManage")
	fs.StringVar(&opts.configPath, "config", "", "path to the settings file")
	fs.StringVar(&opts.sshClient, "ssh-client", appssh.ClientExe, "ssh program name or path")
	var sshTimeout string
	fs.StringVar(&sshTimeout, "ssh-timeout", "", "SSH readiness timeout, e.g. 45s")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&opts.noColor, "no-color", false, "render without colour")
	fs.BoolVar(&opts.showPath, "path", false, "print the settings file path and exit (with: config)")
	fs.BoolVar(&opts.autoConfirm, "yes", false, "for scripts: no interactive prompts (config: apply directly; verbs: nothing to confirm anyway)")

	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, longHelpText())
			return opts, exitOK, true
		}
		fmt.Fprintf(stderr, "Try %s --help\n", AppName)
		return opts, exitUsage, true
	}
	// Anything positional left here (`vbx start one two more flags then word`,
	// a stray word in a picker run) is a mistake, not a second task: the flag
	// package stopped at it and we refuse rather than guess which argument
	// was meant.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n\n", AppName, fs.Arg(0))
		fmt.Fprint(stderr, shortHelp())
		return opts, exitUsage, true
	}
	if opts.action == app.ActionSettings && opts.autoConfirm && len(opts.targets) == 0 {
		fmt.Fprintf(stderr, "%s: config --yes needs a VM name: %s config \"VM\" --yes\n", AppName, AppName)
		return opts, exitUsage, true
	}
	if sshTimeout != "" {
		d, err := parseDuration(sshTimeout)
		if err != nil {
			fmt.Fprintf(stderr, "%s: --ssh-timeout: %v\nTry %s --help\n", AppName, err, AppName)
			return opts, exitUsage, true
		}
		if d < time.Second {
			fmt.Fprintf(stderr, "%s: --ssh-timeout must be at least 1s\n", AppName)
			return opts, exitUsage, true
		}
		opts.sshTimeout = d
	}
	return opts, 0, false
}

// longHelpText is shown by --help, written for a person at a keyboard. It
// works on a machine with no VirtualBox installed.
func longHelpText() string {
	return version.String() + ` - quick VirtualBox control from your terminal

vbx is a small interactive prompt, not a dashboard: a compact picker appears
under your shell prompt, you choose machines and an action, the work runs, one
result line remains, and you are back at your shell.

It drives VBoxManage for machines you already created. It never creates or
deletes a VM, never changes VM hardware or networking, never forces a machine
off, never stores a password, and never edits your ~/.ssh/config.

Usage:
  vbx                       pick a VM, pick an action, done
  vbx <action> [VM...]      run one action; names are exact names or UUIDs,
                            omitted means pick from the list
                            several names run the action as one batch

Actions:
  start      start a powered-off machine headless
  pause      pause a running machine
  resume     resume a paused machine
  stop       request a graceful ACPI shutdown (the guest decides; nothing is
             ever forced)
  save       save the machine's state (suspend to disk, not a shutdown)
  restart    ask the guest OS to reboot itself — never a hardware reset;
             needs Guest Additions in the guest, or SSH settings plus a
             known guest OS (see: vbx config)
  ssh        connect over SSH, starting the machine first when needed;
             without stored SSH details it asks two or three short questions,
             saves them per VM, and continues
  config     view and edit one VM's saved SSH and restart settings; resets
             are confirmed per VM. --path prints the settings file location
  rename     change one VM's display name (VirtualBox renames only a
             powered-off machine, and vbx never stops one to rename it);
             give the new name to script it, or vbx asks with the current
             name prefilled. Settings follow the machine by UUID, so SSH
             and restart settings survive a rename untouched.
  snapshots  list one VM's snapshot tree (create and restore live inside
             the interactive prompt, where destructive steps get a plan)

In the picker, space marks machines; an action on marked machines runs as a
sequential batch with one summary line (failed or skipped machines exit 1).

Examples:
  vbx
  vbx start "Kali Linux"
  vbx stop "Kali Linux" "ubuntu lab"        # a batch
  vbx restart 2d620512-4b10-45a8-b070-25435e5106a5
  vbx rename "Kali Linux"                   # asks, with the name prefilled
  vbx rename "Kali Linux" "kali blue"       # scripted rename
  vbx config "Kali Linux"
  vbx config --path
  vbx snapshots ubuntu
  vbx ssh                                    # pick a machine, then connect

Flags:
  --demo              realistic fake machines; runs nothing real, writes
                      nothing anywhere — safe anywhere
  --vboxmanage PATH   path to VBoxManage (overrides PATH and the standard
                      install location)
  --config PATH       settings file, default ` + configHint() + `
  --ssh-client NAME   ssh program name or path (default: ssh)
  --ssh-timeout DUR   how long to wait for a guest's SSH banner (default 45s)
  --no-color          render without colour
  -v, --version       print the version and exit
  -h, --help          this help

Names must match exactly; two machines sharing a name must be addressed by
UUID so the wrong VM can never be chosen silently.

The product was named vbox-tui before vbx; the settings file keeps its
original path under ` + config.DirHint() + ` and every older file keeps loading.
`
}

func shortHelp() string {
	return "Usage: vbx [<action> [VM...]] [flags]\n" +
		"       actions: start, pause, resume, stop (graceful shutdown), save, restart,\n" +
		"                rename (\"Old\" [\"New\"]), ssh, config [--path], snapshots\n" +
		"       several VM names = one sequential batch; space marks machines in the picker\n" +
		"       flags:   --demo  --vboxmanage PATH  --config PATH  --ssh-client NAME\n" +
		"                --ssh-timeout DURATION  --no-color  --version  --help\n\n" +
		"vbx is a compact terminal prompt for existing VirtualBox VMs\n" +
		"(product name vbox-tui). An independent project; not an Oracle product.\n"
}

// configHint describes where settings live without creating the directory.
func configHint() string {
	if p, err := config.Path(); err == nil {
		return p
	}
	return filepath.Join(config.DirHint(), config.FileName)
}

// parseDuration accepts "45s" and a bare "45" (seconds).
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("%s is negative", s)
		}
		if d == 0 {
			return 0, errors.New("a zero wait would give the guest no time at all")
		}
		return d, nil
	}
	// strconv, not Sscanf: Sscanf("%g") stops at the first non-numeric
	// character, so "5 minutes" would become 5 seconds — wrong by a factor of
	// sixty, delivered without a complaint.
	var secs float64
	if _, err := fmt.Sscanf(s, "%f", &secs); err == nil && secs > 0 && isPlainNumber(s) {
		return time.Duration(secs * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("%q is not a duration (try 5s, 45s, 3m)", s)
}

// isPlainNumber guards the Sscanf fallback: "%f" would happily accept "5x".
func isPlainNumber(s string) bool {
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return len(s) > 0
}

func validate(opts *options) error {
	if opts.sshTimeout != 0 && (opts.sshTimeout < time.Second || opts.sshTimeout > 15*time.Minute) {
		return fmt.Errorf("--ssh-timeout must be between 1s and 15m (got %s)", opts.sshTimeout)
	}
	if opts.action == app.ActionSnapshots && len(opts.targets) > 1 {
		return fmt.Errorf("snapshots list one machine at a time: %s snapshots \"VM\"", AppName)
	}
	if opts.action == app.ActionRename && len(opts.targets) > 2 {
		return fmt.Errorf("rename is one machine and one new name: %s rename \"Old\" \"New\"", AppName)
	}
	return nil
}

// loadConfig reads settings, reporting a malformed file by path without
// refusing to run.
func loadConfig(opts options, stderr io.Writer) (*config.Config, error) {
	var (
		cfg *config.Config
		err error
	)
	if opts.configPath != "" {
		cfg, err = config.LoadFrom(opts.configPath)
	} else {
		cfg, err = config.Load()
	}
	if cfg == nil {
		cfg = config.Defaults()
	}
	if err != nil {
		var le *config.LoadError
		if errors.As(err, &le) {
			fmt.Fprintf(stderr, "%s: %s\n         falling back to defaults for this run; fix the file to keep your settings\n", AppName, le.Error())
		} else {
			fmt.Fprintf(stderr, "%s: could not read settings: %v\n", AppName, err)
		}
	}
	if opts.vboxmanage != "" {
		cfg.VBoxManagePath = opts.vboxmanage
	}
	if opts.sshTimeout != 0 {
		cfg.SSHReadyTimeoutSeconds = int(opts.sshTimeout / time.Second)
	}
	return cfg, err
}

// Environment seams. Discovery, manager construction, the readiness prober
// and the OpenSSH handoff are the only places the direct path touches the
// world outside this process; tests replace them, production never does.
var (
	vboxDiscover   = vbox.FindVBoxManage
	vboxNewManager = func(path string) vbox.Manager {
		cli := vbox.NewCLI(path)
		cli.SetTimeout(25 * time.Second)
		return cli
	}
	proberForDirect = func() *appssh.Prober { return appssh.NewProber() }
	handoffSSH      = runSSHSpec
	// programOptions lets tests drive serve() without a real console; the
	// production run adds nothing and bubbletea uses the terminal as usual.
	programOptions []tea.ProgramOption
)

// pickerGuard explains why the prompt cannot run without a terminal and
// returns the exit usage code; ok=false means "return this code now". A
// noninteractive run must never block waiting for keystrokes it cannot get.
func pickerGuard(action vbox.Action, stdout, stderr io.Writer) (int, bool) {
	if isTerminal(stdout) {
		return 0, true
	}
	switch {
	case action == "":
		fmt.Fprintf(stderr, "%s: the output is not a terminal, so the picker cannot run.\n", AppName)
		fmt.Fprintf(stderr, "Run a direct command, e.g.  %s start \"exact VM name or UUID\"\n", AppName)
	case action == app.ActionRename:
		fmt.Fprintf(stderr, "%s: renaming without a terminal needs both halves: %s rename \"Old name\" \"New name\".\n", AppName, AppName)
		fmt.Fprintf(stderr, "Without the new name, vbx asks for it where you can type.\n")
	default:
		fmt.Fprintf(stderr, "%s: no VM given and the output is not a terminal, so %s cannot pick one.\n", AppName, action)
		fmt.Fprintf(stderr, "Run: %s %s \"exact VM name or UUID\"\n", AppName, action)
	}
	fmt.Fprintf(stderr, "Try %s --help\n", AppName)
	return exitUsage, false
}

// runReal discovers VirtualBox and serves the prompt or a direct request.
func runReal(cfg *config.Config, _ error, opts options, stdout, stderr io.Writer) int {
	// The settings door every path shares.
	sshDoor := app.ConfigSettings(cfg, true)
	guestDoor := app.ConfigGuestSettings(cfg, true)

	// Config journeys first: they need no VirtualBox at all — they manage
	// this app's own settings file.
	if opts.action == app.ActionSettings {
		return runConfig(cfg, sshDoor, guestDoor, opts, stdout, stderr)
	}

	// A direct command with named VMs works fully without a terminal —
	// that is what makes it scriptable — so the guard fires only when
	// something would have to be picked. Renaming one named machine with
	// the new name on the line is fully scriptable; naming only the
	// machine means the new name has yet to be typed, which means a
	// console.
	needTerminal := len(opts.targets) == 0 ||
		opts.action == "" ||
		opts.action == app.ActionSnapshots ||
		(opts.action == app.ActionRename && len(opts.targets) != 2)
	if needTerminal {
		if code, ok := pickerGuard(opts.action, stdout, stderr); !ok {
			return code
		}
	}

	disc, err := vboxDiscover(cfg.VBoxManagePath)
	if err != nil {
		fmt.Fprint(stderr, vbox.InstallHint(err))
		return exitEnvironment
	}
	backend := vboxNewManager(disc.Path)

	switch {
	case opts.action == app.ActionRename:
		// Two names reach the scripted door; 0 or 1 target is the focused
		// rename flow, where the new name is typed into a prefilled
		// prompt (guarded above to always have a console).
		if len(opts.targets) == 2 {
			return renameDirect(backend, cfg, true, opts.targets[0], opts.targets[1], stdout, stderr)
		}
		var target vbox.Machine
		if len(opts.targets) == 1 {
			vm, code, ok := resolveOne(backend, opts, stderr)
			if !ok {
				return code
			}
			target = vm
		}
		return serve(app.New(app.Options{
			Backend: backend, Info: backendInfo(disc.Path), Config: cfg,
			SSHSettings: sshDoor, GuestSettings: guestDoor,
			SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
			Direct: &app.DirectRun{Action: app.ActionRename, VM: target},
		}), stdout, stderr)
	case opts.action == app.ActionSnapshots:
		// Listing needs a console (the tree is interactive).
		target, code, ok := resolveOne(backend, opts, stderr)
		if !ok {
			return code
		}
		return serve(app.New(app.Options{
			Backend: backend, Info: backendInfo(disc.Path), Config: cfg,
			SSHSettings: sshDoor, GuestSettings: guestDoor,
			SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
			Direct: &app.DirectRun{Action: opts.action, VM: target},
		}), stdout, stderr)
	case len(opts.targets) > 1 && opts.action.Batchable():
		return runBatchDirect(backend, sshDoor, guestDoor, opts, stdout, stderr)
	case len(opts.targets) == 1:
		target, code, ok := resolveOne(backend, opts, stderr)
		if !ok {
			return code
		}
		return executeDirect(backend, cfg, opts, target, stdout, stderr)
	default:
		modelOpts := app.Options{
			Backend: backend, Info: backendInfo(disc.Path), Config: cfg,
			SSHSettings: sshDoor, GuestSettings: guestDoor,
			SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
		}
		if opts.action != "" {
			modelOpts.Direct = &app.DirectRun{Action: opts.action}
		}
		return serve(app.New(modelOpts), stdout, stderr)
	}
}

// runConfig answers the config command. Without a VM name it opens the
// interactive settings flow as before; `--path` printed the file already in
// Run; `--yes` would mean "edit without asking", which for destructive
// settings is exactly what this app refuses to do, so it opens the editor and
// every write there confirms its own scope anyway.
func runConfig(cfg *config.Config, sshDoor app.SSHSettings, guestDoor app.GuestSettings, opts options, stdout, stderr io.Writer) int {
	if len(opts.targets) == 0 {
		// vbx config: pick a machine, manage its settings.
		if code, ok := pickerGuard(opts.action, stdout, stderr); !ok {
			return code
		}
		disc, err := vboxDiscover(cfg.VBoxManagePath)
		if err != nil {
			fmt.Fprint(stderr, vbox.InstallHint(err))
			return exitEnvironment
		}
		backend := vboxNewManager(disc.Path)
		return serve(app.New(app.Options{
			Backend: backend, Info: backendInfo(disc.Path), Config: cfg,
			SSHSettings: sshDoor, GuestSettings: guestDoor,
			SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
			Direct: &app.DirectRun{Action: app.ActionSettings},
		}), stdout, stderr)
	}
	// One named VM: the editor is interactive work; without a terminal the
	// request cannot be honoured.
	if code, ok := pickerGateNamed(stdout, stderr); !ok {
		return code
	}
	disc, err := vboxDiscover(cfg.VBoxManagePath)
	if err != nil {
		fmt.Fprint(stderr, vbox.InstallHint(err))
		return exitEnvironment
	}
	backend := vboxNewManager(disc.Path)
	target, code, ok := resolveOne(backend, opts, stderr)
	if !ok {
		return code
	}
	return serve(app.New(app.Options{
		Backend: backend, Info: backendInfo(disc.Path), Config: cfg,
		SSHSettings: sshDoor, GuestSettings: guestDoor,
		SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
		Direct: &app.DirectRun{Action: app.ActionSettings, VM: target},
	}), stdout, stderr)
}

func pickerGateNamed(stdout, stderr io.Writer) (int, bool) {
	// `vbx config "VM"` still needs a terminal: editing settings is a
	// conversation, and --yes must not turn it into unattended writes.
	if isTerminal(stdout) {
		return 0, true
	}
	fmt.Fprintf(stderr, "%s: editing settings is interactive; run this where you can answer the prompts.\n", AppName)
	fmt.Fprintf(stderr, "To see the current settings without a terminal: %s --path and read that file.\n", AppName)
	return exitUsage, false
}

func backendInfo(path string) app.BackendInfo {
	return app.BackendInfo{Name: "VirtualBox", Path: path}
}

// resolveOne turns a single shell-supplied name or UUID into exactly one
// machine through a read-only list. An unknown name and an ambiguous name are
// both hard usage errors: acting on a fuzzy match could change the wrong VM.
func resolveOne(backend vbox.Manager, opts options, stderr io.Writer) (vbox.Machine, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	inv, err := backend.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: could not list virtual machines: %s\n", AppName, oneLine(err))
		return vbox.Machine{}, exitEnvironment, false
	}
	if len(opts.targets) != 1 {
		fmt.Fprintf(stderr, "%s: this command takes exactly one machine name\n", AppName)
		return vbox.Machine{}, exitUsage, false
	}
	vm, err := inv.Resolve(opts.targets[0])
	if err != nil {
		// Resolve errors quote the caller's own argument; that text passes
		// through the display encoder on its way back out, like every other
		// externally-influenced string.
		fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
		return vbox.Machine{}, exitUsage, false
	}
	return vm, 0, true
}

// resolveAll resolves every named target before anything mutates. The
// returned list keeps the caller's order; duplicates collapse to one
// operation, and an unresolvable name is the whole command's failure — the
// same no-guessing rule a single name follows.
func resolveAll(backend vbox.Manager, targets []string, stderr io.Writer) ([]vbox.Machine, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	inv, err := backend.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: could not list virtual machines: %s\n", AppName, oneLine(err))
		return nil, exitEnvironment, false
	}
	seen := map[string]bool{}
	out := make([]vbox.Machine, 0, len(targets))
	for _, name := range targets {
		vm, err := inv.Resolve(name)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
			return nil, exitUsage, false
		}
		key := config.CanonicalUUID(vm.UUID)
		if seen[key] {
			continue // the same machine named twice means one operation
		}
		seen[key] = true
		out = append(out, vm)
	}
	return out, 0, true
}

// runBatchDirect is the noninteractive multi-name path: resolve everything
// first, then run the plan through the program-free batch engine. The
// command line listing every name is the confirmation a batch needs for a
// script.
func runBatchDirect(backend vbox.Manager, sshDoor app.SSHSettings, guestDoor app.GuestSettings, opts options, stdout, stderr io.Writer) int {
	targets, code, ok := resolveAll(backend, opts.targets, stderr)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), batchTimeout(len(targets)))
	defer cancel()
	routes := app.RestartRoutes{
		SSH:    sshDoor.SSH,
		Guest:  guestDoor.Guest,
		Run:    runner.New(),
		Client: opts.sshClient,
	}
	sum := app.RunBatchDirect(ctx, backend, opts.action, targets, routes, func(line string) {
		fmt.Fprintln(stdout, line)
	})
	code = sum.ExitCode()
	line := sum.SummaryLine()
	// Same honesty rule as the picker: when nothing reached the hypervisor,
	// say so, so a skipped-only batch is never mistaken for requests pending.
	if code != 0 && !sum.AnyAttempt() {
		line += " Nothing was sent to VirtualBox."
	}
	fmt.Fprintln(stdout, render(line, code == 0))
	if code != 0 {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, line)
	}
	return code
}

// batchTimeout gives every machine the action timeout plus re-read slack;
// the engine stops on its own between machines.
func batchTimeout(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return time.Duration(n)*25*time.Second + 10*time.Second
}

// renameDirect is the scripted `vbx rename "Old" "New"` path. It shares the
// app package's single rename door — UUID targeting, fresh state read, the
// powered-off rule, the duplicate-name refusal — so a script and the prompt
// cannot disagree about what a rename means. Exit codes follow the house
// rules: a refusal is a failed run (1); a machine that vanished mid-flight
// or a VirtualBox that stops answering is the environment's failure (3).
// persist=false (demo) keeps every write out of the real settings file.
func renameDirect(backend vbox.Manager, cfg *config.Config, persist bool, oldName, newName string, stdout, stderr io.Writer) int {
	// A new name the command line could never accept is a mistake in the
	// line, not a failed operation: it exits 2 before VirtualBox is asked
	// anything at all.
	if err := vbox.ValidateVMName(newName); err != nil {
		// The refusal text quotes the caller's own argument; encode it like
		// every other externally-influenced string before it reaches the
		// terminal.
		fmt.Fprintf(stderr, "%s: %s\nTry %s --help\n", AppName, oneLine(err), AppName)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The door addresses the target by UUID; the old name is resolved to
	// that UUID here through a fresh read, and an unknown or ambiguous
	// name never becomes an operation at all. Resolve first, then let the
	// door re-read and re-verify at the moment of the request.
	inv, err := backend.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: could not list virtual machines: %s\n", AppName, oneLine(err))
		return exitEnvironment
	}
	vm, err := inv.Resolve(oldName)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
		return exitUsage
	}
	out, err := app.RenameMachine(ctx, backend, vm.UUID, newName)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "%s: VirtualBox did not answer in time; it may or may not have renamed %s.\n", AppName, quote(oldName))
		return exitEnvironment
	default:
		printResult(stdout, stderr, false, fmt.Sprintf("Cannot rename %s: %s. Nothing was changed.",
			quote(vm.Name), oneLine(err)))
		return exitRun
	}
	line := fmt.Sprintf("Renamed %s to %s.", quote(out.Old), quote(out.New))
	if out.Same {
		line = fmt.Sprintf("%s is already named %s: nothing was changed.", quote(out.Old), quote(out.New))
	} else if herr := app.RefreshNameHint(cfg, persist, out.VM.UUID, out.New); herr != nil {
		// VirtualBox renamed the machine; only vbx's cosmetic hint failed.
		// The machine is renamed — that part is said plainly, and the
		// failure is reported with it.
		line += fmt.Sprintf(" vbx renamed the machine, but could not refresh its stored display-name hint: %s", oneLine(herr))
	}
	printResult(stdout, stderr, true, line)
	return exitOK
}

// refusalWords maps a focused action to the verb phrase used in the refusal
// sentence "cannot <phrase> X: reason", so `vbx ssh` reads "cannot reach
// X over ssh" rather than "cannot ssh ssh".
func refusalWords(a vbox.Action) (verb string, subject string) {
	if a == vbox.ActionConnect {
		return "reach", "over ssh"
	}
	return string(a), ""
}

// executeDirect performs exactly one action against a resolved machine,
// sharing the app package's state rules and result wording so the prompt and
// the quick commands can never disagree.
func executeDirect(backend vbox.Manager, cfg *config.Config, opts options, target vbox.Machine, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Re-read state: the shell argument was typed minutes (or a GUI change)
	// ago; the operation must answer to the machine as it is now.
	verb, reason, err := decideVerb(ctx, backend, opts.action, target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
		return exitEnvironment
	}
	if verb == app.VerbRefused {
		verbWord, subject := refusalWords(opts.action)
		if subject != "" {
			fmt.Fprintf(stderr, "%s: cannot %s %s %s: %s. Nothing was changed.\n",
				AppName, verbWord, quote(target.Name), subject, reason)
		} else {
			fmt.Fprintf(stderr, "%s: cannot %s %s: %s. Nothing was changed.\n",
				AppName, verbWord, quote(target.Name), reason)
		}
		return exitRun
	}

	name := quote(target.Name)
	if sshAction(opts.action) {
		if verb != "" {
			if err := app.Perform(ctx, backend, verb, target.UUID); err != nil {
				fmt.Fprintf(stderr, "%s: could not %s %s: %s\n", AppName, opts.action, name, oneLine(err))
				return exitRun
			}
			fmt.Fprintln(stdout, render(app.SuccessLine(verb, name), true))
		}
		return directSSH(ctx, cfg, opts, target, stdout, stderr)
	}
	if opts.action == vbox.ActionRestart {
		// A restart over SSH needs no terminal either (OpenSSH runs
		// non-interactively); the report wording carries the honest class of
		// the answer.
		report := app.PerformRestart(ctx, backend, target, directRoutes(cfg, opts))
		line := report.Summary(name)
		if report.OK() && report.Outcome != app.RestartBlocked {
			printResult(stdout, stderr, true, line)
			return exitOK
		}
		printResult(stdout, stderr, false, line)
		return exitRun
	}
	if verb == "" {
		// The machine was already where the verb wanted it; say so rather than
		// claim a request that was never sent.
		printResult(stdout, stderr, true, alreadyInStateLine(opts.action, name))
		return exitOK
	}
	if err := app.Perform(ctx, backend, verb, target.UUID); err != nil {
		fmt.Fprintf(stderr, "%s: could not %s %s: %s\n", AppName, opts.action, name, oneLine(err))
		return exitRun
	}
	printResult(stdout, stderr, true, app.SuccessLine(verb, name))
	return exitOK
}

func directRoutes(cfg *config.Config, opts options) app.RestartRoutes {
	return app.RestartRoutes{
		SSH:    app.ConfigSettings(cfg, true).SSH,
		Guest:  app.ConfigGuestSettings(cfg, true).Guest,
		Run:    runner.New(),
		Client: opts.sshClient,
	}
}

// decideVerb asks the app package's single verb rule what must actually be
// sent for the machine's current state.
func decideVerb(ctx context.Context, backend vbox.Manager, action vbox.Action, target vbox.Machine) (vbox.Action, string, error) {
	inv, err := backend.List(ctx)
	if err != nil {
		return "", "", err
	}
	vm, ok := inv.Find(target.UUID)
	if !ok {
		return "", "", fmt.Errorf("%s (%s) is no longer registered with VirtualBox", target.Name, target.UUID)
	}
	verb, why := app.DecideVerb(action, vm.State)
	return verb, why, nil
}

func sshAction(a vbox.Action) bool {
	return a == vbox.ActionConnect || a == vbox.ActionStartConn
}

// directSSH waits for the guest's ssh endpoint and hands the terminal over,
// mirroring the interactive journey. On a plain console Ctrl+C ends the whole
// process; the honest caveat stays in the message because a start already
// issued is not undone by dying.
func directSSH(ctx context.Context, cfg *config.Config, opts options, target vbox.Machine, stdout, stderr io.Writer) int {
	sshCfg, ok := app.ConfigSettings(cfg, true).SSH(target.UUID)
	if !ok {
		fmt.Fprintf(stderr, "%s: no SSH settings for %s.\n", AppName, quote(target.Name))
		fmt.Fprintln(stderr, `Set them interactively: `+AppName+` ssh `+quote(target.Name)+` (run it where you can answer the questions)`)
		return exitRun
	}
	sshCfg = sshCfg.Normalise()
	if sshCfg.Mode != config.SSHModeAlias {
		host, port, needsOpenSSH, err := appssh.ProbeTarget(sshCfg, nil)
		if err == nil && !needsOpenSSH {
			addr := appssh.JoinHostPort(host, port)
			timeout := cfg.SSHReadyTimeout()
			fmt.Fprintf(stdout, "Waiting for SSH on %s (up to %s, Ctrl+C cancels)…\n", addr, timeout)
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if _, err := proberForDirect().WaitForSSH(waitCtx, host, port); err != nil {
				if errors.Is(waitCtx.Err(), context.Canceled) {
					fmt.Fprintf(stderr, "%s: cancelled waiting for SSH on %s. The machine keeps running: cancelling the wait does not undo a start.\n", AppName, addr)
				} else {
					fmt.Fprintf(stderr, "%s: SSH did not become ready on %s: %s\n", AppName, addr, oneLine(err))
				}
				return exitRun
			}
		}
	}
	spec, err := appssh.BuildSpecWith(opts.sshClient, sshCfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
		return exitRun
	}
	printResult(stdout, stderr, true, "Connecting to "+quote(target.Name)+" via "+spec.Label)
	return handoffSSH(spec, stderr)
}

// alreadyInStateLine answers the defensive case where the verb rule decided
// no request was needed for a non-SSH action: nothing is invented, because
// nothing was sent.
func alreadyInStateLine(action vbox.Action, name string) string {
	return fmt.Sprintf("%s already allows %s: nothing was sent.", name, action.Label())
}

// quote wraps a VM name so a name with spaces reads as one thing. The name
// is external metadata: it is safe-display encoded first, so imported VM
// metadata carrying escape sequences or BiDi overrides shows as visible
// markers instead of steering the terminal that prints this line. Raw names
// are still what resolution and command arguments use everywhere else.
func quote(name string) string { return `"` + safedisplay.Text(name) + `"` }

// printResult writes one result line to stdout (the scrollback record) and,
// for failures, repeats it on stderr for scripts.
func printResult(stdout, stderr io.Writer, ok bool, line string) {
	fmt.Fprintln(stdout, render(line, ok))
	if !ok {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, line)
	}
}

// runDemo exercises the same prompt against fake machines. It never touches
// VBoxManage, never launches ssh, and never writes the real configuration.
func runDemo(cfg *config.Config, opts options, stdout, stderr io.Writer) int {
	// --demo is the picker with fake machines: it always needs a console, and
	// without one it must explain itself rather than open a renderer nobody
	// can steer. Renaming a named machine with the new name already typed on
	// the line is the exception — it is fully scriptable — but a rename
	// still missing its second half needs the prefilled prompt.
	if len(opts.targets) == 0 || (opts.action == app.ActionRename && len(opts.targets) != 2) {
		if code, ok := pickerGuard(opts.action, stdout, stderr); !ok {
			return code
		}
	}
	backend := demo.New()
	// Demo settings live in memory; nothing here reaches any file.
	if opts.action == app.ActionRename && len(opts.targets) == 2 {
		return renameDirect(backend, cfg, false, opts.targets[0], opts.targets[1], stdout, stderr)
	}
	if len(opts.targets) > 1 && opts.action.Batchable() {
		// The same engine the real CLI uses, against the fake machines.
		return runBatchDirect(backend, demoSettings{backend}, demoGuests{backend}, opts, stdout, stderr)
	}
	var target vbox.Machine
	if len(opts.targets) == 1 {
		vm, code, ok := resolveOne(backend, opts, stderr)
		if !ok {
			return code
		}
		target = vm
	}
	if opts.action == app.ActionSettings {
		if target.UUID == "" {
			if code, ok := pickerGateNamed(stdout, stderr); !ok {
				return code
			}
		}
	}
	modelOpts := app.Options{
		Backend: backend, Info: app.BackendInfo{Name: "demo VirtualBox", Demo: true},
		Config: cfg, Demo: true, SSHSettings: demoSettings{backend}, GuestSettings: demoGuests{backend},
		SSHClient: opts.sshClient, Stderr: stderr, Width: termWidth(),
	}
	if opts.action != "" {
		modelOpts.Direct = &app.DirectRun{Action: opts.action, VM: target}
	}
	return serve(app.New(modelOpts), stdout, stderr)
}

// demoSettings stores demo SSH settings in the demo backend's memory only.
type demoSettings struct{ b *demo.Backend }

func (d demoSettings) SSH(uuid string) (config.SSHConfig, bool) { return d.b.SSH(uuid) }
func (d demoSettings) Store(uuid, name string, sshCfg config.SSHConfig) error {
	d.b.SetSSH(uuid, sshCfg)
	return nil
}
func (d demoSettings) Clear(uuid string) error {
	d.b.ClearSSH(uuid)
	return nil
}

// demoGuests stores demo guest-control settings in memory only.
type demoGuests struct{ b *demo.Backend }

func (d demoGuests) Guest(uuid string) (config.GuestConfig, bool) { return d.b.Guest(uuid) }
func (d demoGuests) StoreGuest(uuid, name string, cfg *config.GuestConfig) error {
	if cfg == nil {
		d.b.ClearGuest(uuid)
		return nil
	}
	d.b.SetGuest(uuid, *cfg)
	return nil
}

// serve runs the inline prompt and acts on its outcome. No alternate screen:
// the prompt draws itself under the shell prompt, keeps existing scrollback,
// and each final line is printed by this function once the renderer is gone,
// so it lands in the scrollback like any other command output.
func serve(m *app.Model, stdout, stderr io.Writer) int {
	p := tea.NewProgram(m, programOptions...) // inline, deliberately without tea.WithAltScreen()
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "%s: interface stopped: %s\n", AppName, oneLine(err))
		// The model may still hold an answer; print it if it does.
		if m.Outcome() != app.OutcomeNone {
			fmt.Fprintln(stdout, render(m.Summary(), m.ExitCode() == 0))
		}
		return exitRun
	}

	switch m.Outcome() {
	case app.OutcomeSSHHandoff:
		fmt.Fprintln(stdout, render(m.Summary(), true))
		return runSSH(m, stderr)
	case app.OutcomeNone:
		// Killed without deciding (or the renderer died with nothing to say).
		return exitRun
	default:
		fmt.Fprintln(stdout, render(m.Summary(), m.ExitCode() == 0))
		code := m.ExitCode()
		if code != 0 {
			// Failures also belong on stderr for scripts; the scrollback line
			// above stays as the human-visible record.
			fmt.Fprintf(stderr, "%s: %s\n", AppName, m.Summary())
		}
		return code
	}
}

// runSSH hands the terminal to OpenSSH after the prompt has released it, then
// exits with ssh's own status. Nothing reopens afterwards — the user asked to
// not return to a dashboard.
func runSSH(m *app.Model, stderr io.Writer) int {
	spec := m.SSHCommand()
	if m.SSHSimulated() {
		fmt.Fprintf(stderr, "%s: demo mode: no real ssh process was started; the command would be %q\n",
			AppName, strings.Join(append([]string{spec.Path}, spec.Args...), " "))
		return exitOK
	}
	return handoffSSH(spec, stderr)
}

// runSSHSpec runs one prepared OpenSSH command with the real terminal.
func runSSHSpec(spec appssh.ExecSpec, stderr io.Writer) int {
	// A fresh context: the prompt is over; ssh owns the terminal and the
	// user's Ctrl+C belongs to ssh from here on.
	cmd, err := appssh.InteractiveCommand(context.Background(), spec.Path, spec.Args, runner.New())
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", AppName, oneLine(err))
		return exitRun
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "%s: ssh could not run: %s\n", AppName, oneLine(err))
		return exitRun
	}
	return exitOK
}

// render prefixes a result with its mark. The ASCII marks are deliberate: the
// legacy Windows console renders them, and every meaning is in the words.
func render(summary string, ok bool) string {
	if ok {
		return "+ " + summary
	}
	return "! " + summary
}

// oneLine flattens an error for a single-line message, safe-display encoded:
// VirtualBox quotes machine-supplied names and host paths into its errors,
// and subprocess stderr is external text — none of it gets to send terminal
// instructions through vbx.
func oneLine(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " ")
	s = safedisplay.Text(s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

// isTerminal reports whether output is attached to a console. vbx is a
// terminal application; with output redirected there is nothing to prompt
// with, and callers must get an explanation instead of a hang.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
}

// termWidth keeps the inline frame inside the window that was current when
// the run began (the prompt can be resized mid-run; Bubble Tea reports the real
// width in that case).
func termWidth() int {
	return 80
}
