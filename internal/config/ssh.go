package config

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

// MaxFieldLen bounds user supplied text fields.
const MaxFieldLen = 256

// ValidateSSH checks a per-VM SSH configuration. It deliberately rejects values
// that could be mistaken for ssh(1) options, because user input is always
// passed as a positional argument or after a strict "-o" allow-list.
func ValidateSSH(s SSHConfig) error {
	switch s.Mode {
	case SSHModeNone:
		return nil
	case SSHModeAlias:
		alias := strings.TrimSpace(s.Alias)
		if alias == "" {
			return fmt.Errorf("alias mode needs an SSH config alias")
		}
		if err := validateHostToken(alias, "alias"); err != nil {
			return err
		}
		if s.Host != "" || s.Port != 0 {
			return fmt.Errorf("alias mode must not also set host or port")
		}
		return nil
	case SSHModeEndpoint:
		host := strings.TrimSpace(s.Host)
		if host == "" {
			return fmt.Errorf("endpoint mode needs a host or address")
		}
		if err := validateHostToken(host, "host"); err != nil {
			return err
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535, got %d", s.Port)
		}
		if s.User != "" {
			if err := validateHostToken(s.User, "username"); err != nil {
				return err
			}
		}
		if p := strings.TrimSpace(s.SSHConfigPath); p != "" {
			if len(p) > MaxFieldLen {
				return fmt.Errorf("ssh_config_path is too long")
			}
			if strings.ContainsAny(p, "\n\r\x00") {
				return fmt.Errorf("ssh_config_path contains control characters")
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown ssh mode %q", s.Mode)
	}
}

// validateHostToken rejects anything that could be split into extra arguments
// or interpreted as an option: no whitespace, no control characters, and no
// leading dash (which ssh would read as a flag such as -o ProxyCommand=…).
func validateHostToken(value, field string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(v) > MaxFieldLen {
		return fmt.Errorf("%s is longer than %d characters", field, MaxFieldLen)
	}
	if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "~") {
		return fmt.Errorf("%s %q must not start with '-' or '~'", field, v)
	}
	for _, r := range v {
		if r > unicode.MaxASCII {
			// Non-ASCII hostnames are legitimate in IDN form, but OpenSSH on
			// Windows does not resolve them and they are a common place for
			// look-alike characters, so require plain ASCII.
			return fmt.Errorf("%s %q must be plain ASCII", field, v)
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%s %q must not contain spaces or control characters", field, v)
		}
		switch r {
		case '"', '\'', '`', '$', ';', '|', '&', '<', '>', '\\', '\x00':
			return fmt.Errorf("%s %q contains the unsupported character %q", field, v, r)
		}
	}
	return nil
}

// Normalise trims a configuration so stored values are canonical.
func (s SSHConfig) Normalise() SSHConfig {
	out := s
	out.Alias = strings.TrimSpace(out.Alias)
	out.Host = strings.TrimSpace(out.Host)
	out.User = strings.TrimSpace(out.User)
	out.SSHConfigPath = strings.TrimSpace(out.SSHConfigPath)
	if out.Mode == SSHModeNone && (out.Alias != "" || out.Host != "") {
		if out.Alias != "" {
			out.Mode = SSHModeAlias
		} else {
			out.Mode = SSHModeEndpoint
		}
	}
	if out.Mode == SSHModeAlias {
		out.Host, out.Port, out.User = "", 0, ""
	}
	return out
}

// Display is the short one-line summary shown in the details panel.
func (s SSHConfig) Display() string {
	switch s.Mode {
	case SSHModeAlias:
		return "alias " + s.Alias
	case SSHModeEndpoint:
		host := s.Host
		if host == "" {
			host = "?"
		}
		if s.User != "" {
			return fmt.Sprintf("%s@%s:%d", s.User, host, s.Port)
		}
		return fmt.Sprintf("%s:%d", host, s.Port)
	}
	return "not configured"
}

// LocalhostLike reports whether the endpoint names the loopback interface. It is
// used to decide whether to mention VirtualBox's nat-localhostreachable setting,
// which must be enabled for a host to reach its own forwarded port on the loopback
// interface.
func (s SSHConfig) LocalhostLike() bool {
	switch strings.ToLower(strings.TrimSpace(s.Host)) {
	case "localhost", "127.0.0.1", "::1", "ip6-localhost", "ip6-loopback":
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Host)), "127.")
}

// Configured reports whether an SSH connection could be attempted.
func (s SSHConfig) Configured() bool {
	return ValidateSSH(s) == nil && s.Mode != SSHModeNone
}

// UsesProxy reports whether the endpoint is a simple address we can poll.
// Alias mode may hide a ProxyJump or ProxyCommand route, which direct TCP
// polling cannot verify, so readiness checking is limited there.
func (s SSHConfig) UsesProxy() bool { return s.Mode == SSHModeAlias }

// ResolveTarget turns a configuration into a concrete dial target for a
// pre-banner check. It returns ok=false when the endpoint cannot be resolved
// without OpenSSH itself (alias mode without a resolvable -G answer).
func (s SSHConfig) ResolveTarget() (host string, port int, ok bool) {
	if err := ValidateSSH(s); err != nil {
		return "", 0, false
	}
	if s.Mode != SSHModeEndpoint {
		return "", 0, false
	}
	return s.Host, s.Port, true
}

// EndpointTarget is a dial address string, e.g. 127.0.0.1:2222.
func (s SSHConfig) EndpointTarget() string {
	host, port, ok := s.ResolveTarget()
	if !ok {
		return ""
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}
