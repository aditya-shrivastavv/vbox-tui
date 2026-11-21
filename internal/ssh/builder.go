// Package ssh builds argument lists for the system OpenSSH client and checks
// whether an endpoint is ready to speak SSH. It never implements an SSH client.
package ssh

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
)

// ErrNoEndpoint means the VM has no usable SSH configuration.
var ErrNoEndpoint = errors.New("no SSH endpoint configured for this VM")

// ErrNoOpenSSH means the ssh client could not be found.
var ErrNoOpenSSH = errors.New("the ssh client was not found on PATH")

// OptionsAllowList documents every ssh(1) flag vbox-tui may place on the command
// line. Nothing outside this list is ever constructed, so a stored value cannot
// become an option.
//
//	-tt                 allocate a pseudo-terminal (see fixedFlags)
//	-F <path>           alternate config file, only when the user set one
//	-p <port>           numeric port, from an int we range-checked
//	-o CanonicalizeHostname=no   keeps an alias exactly as written
//	--                  end of options: everything after it is a destination
var OptionsAllowList = []string{"-tt", "-F", "-p", "-o", "--"}

// fixedFlags are the flags always added.
//
// -tt asks for a pseudo-terminal even though stdin is inherited from the TUI.
// Without it a child process attached to a terminal the TUI has just reaped can
// leave the console in a cooked state after ssh exits.
//
// Nothing here weakens security. The only -o option used, CanonicalizeHostname,
// keeps OpenSSH from rewriting the alias the user chose; host-key verification
// and authentication are left exactly as the user configured them.
func fixedFlags(s config.SSHConfig) []string {
	args := []string{"-tt"}
	if s.Mode == config.SSHModeAlias {
		// A config file may enable canonicalisation, which resolves aliases
		// before they reach the config that defines them. Pinning it off means
		// "the alias you typed is the alias OpenSSH uses".
		args = append(args, "-o", "CanonicalizeHostname=no")
	}
	return args
}

// BuildArgs returns the argument vector (without the executable) for an
// interactive SSH session.
//
// User supplied text is only ever used after "--", so ssh(1) treats it as a
// destination and never as an option, or as the operand of a flag it must be a
// value config.ValidateSSH already accepted. The vector is handed to
// exec.CommandContext as an array, never through a shell.
func BuildArgs(s config.SSHConfig) ([]string, error) {
	s = s.Normalise()
	if err := config.ValidateSSH(s); err != nil {
		return nil, err
	}
	switch s.Mode {
	case config.SSHModeAlias:
		args := fixedFlags(s)
		args = append(args, "--", s.Alias)
		return args, nil
	case config.SSHModeEndpoint:
		args := fixedFlags(s)
		if p := s.SSHConfigPath; p != "" {
			args = append(args, "-F", p)
		}
		dest := s.Host
		if s.User != "" {
			dest = s.User + "@" + s.Host
		}
		// The port comes from a validated int, so it cannot carry text.
		args = append(args, "-p", strconv.Itoa(s.Port), "--", dest)
		return args, nil
	default:
		return nil, ErrNoEndpoint
	}
}

// Describe renders the command in a status message so the user can see what
// will run. It is not intended to be pasted into a shell.
func Describe(exe string, s config.SSHConfig) string {
	args, err := BuildArgs(s)
	if err != nil {
		return "ssh: " + err.Error()
	}
	return strings.Join(append([]string{exe}, args...), " ")
}

// ProbeTarget returns the TCP endpoint a readiness check should dial, and
// whether OpenSSH must be used instead.
//
// Endpoint mode dials exactly what ssh(1) would dial, so a pre-banner check is
// meaningful. In alias mode the real route is whatever OpenSSH decides, possibly
// through ProxyJump or ProxyCommand, and a direct TCP connection to the resolved
// address proves nothing about that route. ProbeTarget then reports the resolved
// address for information only and sets needsOpenSSH, so the caller skips TCP
// polling and lets OpenSSH connect and report its own failure.
func ProbeTarget(s config.SSHConfig, resolve Resolver) (host string, port int, needsOpenSSH bool, err error) {
	s = s.Normalise()
	if err := config.ValidateSSH(s); err != nil {
		return "", 0, false, err
	}
	switch s.Mode {
	case config.SSHModeEndpoint:
		return s.Host, s.Port, false, nil
	case config.SSHModeAlias:
		if resolve == nil {
			return "", 0, true, nil
		}
		ep, err := resolve.EffectiveEndpoint(s.Alias)
		if err != nil {
			// ssh -G is a convenience; a failure here must not block the
			// connection, so leave the target empty and defer to OpenSSH.
			return "", 0, true, nil
		}
		return ep.Host, ep.Port, true, nil
	default:
		return "", 0, false, ErrNoEndpoint
	}
}

// JoinHostPort formats a dial address.
func JoinHostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// ParsePort converts a text port. It accepts decimal digits only: strconv.Atoi
// also accepts a leading sign, and a form field that quietly turned "+22" or
// "-22" into something else would leave the user looking at a port they did not
// type.
func ParsePort(s string) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("no port was given")
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a port number", s)
		}
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d is out of range", n)
	}
	return n, nil
}
