package ssh

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
	"github.com/aditya-shrivastavv/vbox-tui/internal/runner"
)

// ClientExe is the OpenSSH client program name. LookPath resolves it, so a user
// installation is used rather than anything bundled.
const ClientExe = "ssh"

// ExecSpec is a resolved interactive command: the program and its arguments.
// The UI hands these to Bubble Tea's exec package, which passes the terminal to
// the child and restores the application afterwards.
type ExecSpec struct {
	Path string
	Args []string
	// Label is what the UI shows while the child owns the terminal.
	Label string
}

// BuildSpec resolves the ssh client and its arguments.
func BuildSpec(s config.SSHConfig) (ExecSpec, error) {
	return BuildSpecWith(ClientExe, s)
}

// BuildSpecWith allows tests to substitute the client path.
func BuildSpecWith(exe string, s config.SSHConfig) (ExecSpec, error) {
	if exe == "" {
		exe = ClientExe
	}
	args, err := BuildArgs(s)
	if err != nil {
		return ExecSpec{}, err
	}
	path, lookErr := runner.LookPath(exe)
	if lookErr != nil {
		// The argument list is fine; the client is missing. Say so precisely so
		// the user installs OpenSSH instead of editing their settings.
		return ExecSpec{Args: args}, fmt.Errorf("%w (looked for %q on PATH)", ErrNoOpenSSH, exe)
	}
	return ExecSpec{
		Path:  path,
		Args:  args,
		Label: Describe(exe, s.Normalise()),
	}, nil
}

// Endpoint is a host and port OpenSSH would connect to.
type Endpoint struct {
	Host   string
	Port   int
	User   string
	Proxy  string // ProxyJump value, when the alias uses one
	ProxyC string // ProxyCommand value, when the alias uses one
}

// Target renders the endpoint as a dial address.
func (e Endpoint) Target() string { return JoinHostPort(e.Host, e.Port) }

// Resolver answers "what would OpenSSH actually connect to?" for an alias.
// vbox-tui asks OpenSSH itself with ssh -G rather than parsing ~/.ssh/config,
// which would only ever implement part of the real grammar.
type Resolver interface {
	EffectiveEndpoint(alias string) (Endpoint, error)
}

// OpenSSHResolver implements Resolver with `ssh -G <alias>`.
type OpenSSHResolver struct {
	Exe     string // defaults to ClientExe
	Runner  runner.Runner
	Timeout time.Duration
}

// EffectiveEndpoint asks OpenSSH for its final view of an alias.
func (r OpenSSHResolver) EffectiveEndpoint(alias string) (Endpoint, error) {
	if err := validateAliasForQuery(alias); err != nil {
		return Endpoint{}, err
	}
	exe := r.Exe
	if exe == "" {
		exe = ClientExe
	}
	// Check the program before starting it. LookPath works for a bare name and
	// for a path, so a configured-but-absent client is reported as a missing
	// client rather than as a puzzling "ssh -G failed".
	if _, err := runner.LookPath(exe); err != nil {
		return Endpoint{}, fmt.Errorf("%w (looked for %q)", ErrNoOpenSSH, exe)
	}
	run := r.Runner
	if run == nil {
		run = runner.New()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// -G prints the configuration OpenSSH would use and exits without
	// connecting. CanonicalizeHostname=no matches how the session itself is
	// started, so the answer describes the same destination.
	out, err := runner.Collect(ctx, run, exe, "-G", "-o", "CanonicalizeHostname=no", "--", alias)
	if err != nil {
		return Endpoint{}, fmt.Errorf("ssh -G failed for alias %q: %w", alias, err)
	}
	return parseEffectiveConfig(out)
}

func validateAliasForQuery(alias string) error {
	if strings.TrimSpace(alias) == "" {
		return errors.New("no alias to resolve")
	}
	return config.ValidateSSH(config.SSHConfig{Mode: config.SSHModeAlias, Alias: alias})
}

// parseEffectiveConfig reads the `key value` lines of ssh -G.
func parseEffectiveConfig(out string) (Endpoint, error) {
	ep := Endpoint{Port: 22}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "hostname":
			ep.Host = value
		case "port":
			n, err := strconv.Atoi(value)
			if err == nil && n >= 1 && n <= 65535 {
				ep.Port = n
			}
		case "user":
			ep.User = value
		case "proxyjump":
			ep.Proxy = value
		case "proxycommand":
			ep.ProxyC = value
		}
	}
	if ep.Host == "" {
		return Endpoint{}, fmt.Errorf("ssh -G did not report a hostname")
	}
	return ep, nil
}

// InteractiveCommand builds the command the UI will run with the terminal handed
// over. It deliberately does not suppress window attributes.
func InteractiveCommand(ctx context.Context, exe string, args []string, r runner.Runner) (*exec.Cmd, error) {
	if r == nil {
		r = runner.New()
	}
	return r.Interactive(ctx, exe, args...)
}
