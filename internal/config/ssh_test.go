package config

import (
	"strings"
	"testing"
)

// TestValidateSSHRejectsOptionInjection is the security-relevant test. Values are
// always passed to ssh as a positional destination, but a value that begins with
// '-' would be parsed as an option, which is how "-oProxyCommand=..." style
// injection works. Rejecting it is defence in depth, not the only defence.
func TestValidateSSHRejectsOptionInjection(t *testing.T) {
	bad := []string{
		"-oProxyCommand=calc.exe",
		"-o ProxyCommand=calc",
		"-J attacker.example",
		"-l root",
		"-p2222",
		"--",
		"---",
	}
	for _, v := range bad {
		t.Run("host="+v, func(t *testing.T) {
			cfg := SSHConfig{Mode: SSHModeEndpoint, Host: v, Port: 2222}
			if err := ValidateSSH(cfg); err == nil {
				t.Errorf("host %q was accepted: it would reach ssh as an option", v)
			}
		})
		t.Run("alias="+v, func(t *testing.T) {
			cfg := SSHConfig{Mode: SSHModeAlias, Alias: v}
			if err := ValidateSSH(cfg); err == nil {
				t.Errorf("alias %q was accepted", v)
			}
		})
		t.Run("user="+v, func(t *testing.T) {
			cfg := SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: v}
			if err := ValidateSSH(cfg); err == nil {
				t.Errorf("user %q was accepted", v)
			}
		})
	}
}

// TestValidateSSHRejectsArgumentSplitting covers values that a shell or a
// command-line parser could turn into extra arguments.
func TestValidateSSHRejectsArgumentSplitting(t *testing.T) {
	bad := []string{
		"host extra",                       // space
		"host\textra",                      // tab
		"host\nrm -rf x",                   // newline
		"host;calc",                        // command separator
		"host|calc",                        // pipe
		"host&&calc",                       // and
		"host>out.txt",                     // redirect
		`host\extra`,                       // backslash
		"host\"quoted\"",                   // quote
		"host$(cmd)",                       // substitution
		"host`cmd`",                        // backtick
		"~ Expansion",                      // tilde forms a path in some shells
		strings.Repeat("h", MaxFieldLen+1), // over-long
		"ホスト",                              // non-ASCII, which OpenSSH for Windows cannot resolve
	}
	for _, v := range bad {
		cfg := SSHConfig{Mode: SSHModeEndpoint, Host: v, Port: 2222}
		if err := ValidateSSH(cfg); err == nil {
			t.Errorf("host %q should be refused", v)
		}
	}
}

// TestValidateSSHAcceptsRealValues keeps validation from being so strict that
// ordinary configurations stop working.
func TestValidateSSHAcceptsRealValues(t *testing.T) {
	good := []SSHConfig{
		{Mode: SSHModeAlias, Alias: "lab-ubuntu"},
		{Mode: SSHModeAlias, Alias: "kali.jump-host_1"},
		{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222},
		{Mode: SSHModeEndpoint, Host: "localhost", Port: 22},
		{Mode: SSHModeEndpoint, Host: "10.0.2.15", Port: 22, User: "kali"},
		{Mode: SSHModeEndpoint, Host: "vm-01.lab.example.com", Port: 2200},
		{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "a.b-c_1"},
		{Mode: SSHModeEndpoint, Host: "[::1]", Port: 22},
		{Mode: SSHModeNone},
	}
	for _, cfg := range good {
		if err := ValidateSSH(cfg); err != nil {
			t.Errorf("valid configuration %+v was rejected: %v", cfg, err)
		}
		if cfg.Mode != SSHModeNone && !cfg.Configured() {
			t.Errorf("%+v should report itself configured", cfg)
		}
	}
	// A composite literal at the start of a statement needs parentheses in Go.
	if (SSHConfig{}).Configured() {
		t.Error("an empty configuration must not look configured")
	}
}

func TestValidateSSHRejectsOutOfRangePorts(t *testing.T) {
	for _, p := range []int{0, -1, 65536, 1 << 20} {
		cfg := SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: p}
		if err := ValidateSSH(cfg); err == nil {
			t.Errorf("port %d accepted", p)
		}
	}
	for _, p := range []int{1, 22, 2222, 65535} {
		cfg := SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: p}
		if err := ValidateSSH(cfg); err != nil {
			t.Errorf("port %d rejected: %v", p, err)
		}
	}
}

// TestValidateSSHRejectsContradictoryModes stops a stored file from being
// ambiguous about where the connection actually goes.
func TestValidateSSHRejectsContradictoryModes(t *testing.T) {
	if err := ValidateSSH(SSHConfig{Mode: SSHModeAlias, Alias: "lab", Host: "1.2.3.4", Port: 22}); err == nil {
		t.Error("an alias plus an endpoint is ambiguous and should be refused")
	}
	if err := ValidateSSH(SSHConfig{Mode: SSHModeAlias}); err == nil {
		t.Error("alias mode with no alias should be refused")
	}
	if err := ValidateSSH(SSHConfig{Mode: SSHModeEndpoint}); err == nil {
		t.Error("endpoint mode with no host should be refused")
	}
	if err := ValidateSSH(SSHConfig{Mode: "tunnel"}); err == nil {
		t.Error("an unknown mode should be refused")
	}
}

func TestNormaliseFillsTheModeFromTheFields(t *testing.T) {
	got := SSHConfig{Alias: "  lab  "}.Normalise()
	if got.Mode != SSHModeAlias || got.Alias != "lab" {
		t.Errorf("Normalise = %+v, want an alias mode with a trimmed alias", got)
	}

	got = SSHConfig{Host: " 127.0.0.1 ", Port: 2222, User: " op "}.Normalise()
	if got.Mode != SSHModeEndpoint || got.Host != "127.0.0.1" || got.User != "op" {
		t.Errorf("Normalise = %+v", got)
	}

	// Alias mode must not keep a stale host/port that Validate would reject.
	got = SSHConfig{Mode: SSHModeAlias, Alias: "lab", Host: "1.2.3.4", Port: 22, User: "x"}.Normalise()
	if got.Host != "" || got.Port != 0 || got.User != "" {
		t.Errorf("alias mode kept endpoint fields: %+v", got)
	}
	if err := ValidateSSH(got); err != nil {
		t.Errorf("a normalised alias config should validate: %v", err)
	}

	// An empty config stays empty.
	if err := ValidateSSH(SSHConfig{}.Normalise()); err != nil {
		t.Errorf("an empty config should validate: %v", err)
	}
}

func TestDisplayIsReadableAndNeverLeaksMoreThanTheEndpoint(t *testing.T) {
	cases := []struct {
		cfg  SSHConfig
		want string
	}{
		{SSHConfig{}, "not configured"},
		{SSHConfig{Mode: SSHModeAlias, Alias: "lab"}, "alias lab"},
		{SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222}, "127.0.0.1:2222"},
		{SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "op"}, "op@127.0.0.1:2222"},
	}
	for _, tc := range cases {
		if got := tc.cfg.Display(); got != tc.want {
			t.Errorf("Display(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestLocalhostLike(t *testing.T) {
	yes := []string{"127.0.0.1", "localhost", "LOCALHOST", "127.0.0.53", "::1", " ip6-localhost "}
	for _, h := range yes {
		if !(SSHConfig{Host: h}).LocalhostLike() {
			t.Errorf("%q should count as loopback: the nat-localhostreachable hint depends on this", h)
		}
	}
	no := []string{"10.0.2.15", "vm.lab.example.com", "192.168.1.5", ""}
	for _, h := range no {
		if (SSHConfig{Host: h}).LocalhostLike() {
			t.Errorf("%q is not loopback", h)
		}
	}
}

// TestResolveTargetOnlyForEndpoints is the readiness-check boundary: an alias can
// hide a ProxyJump, so a direct TCP probe of an alias would prove nothing.
func TestResolveTargetOnlyForEndpoints(t *testing.T) {
	if host, port, ok := (SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222}).ResolveTarget(); !ok || host != "127.0.0.1" || port != 2222 {
		t.Errorf("ResolveTarget = %q/%d/%v", host, port, ok)
	}
	if _, _, ok := (SSHConfig{Mode: SSHModeAlias, Alias: "lab"}).ResolveTarget(); ok {
		t.Error("an alias must not resolve to a dial target: OpenSSH may route it through a jump host")
	}
	if _, _, ok := (SSHConfig{}).ResolveTarget(); ok {
		t.Error("an unconfigured endpoint must not resolve")
	}
	if got := (SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222}).EndpointTarget(); got != "127.0.0.1:2222" {
		t.Errorf("EndpointTarget = %q, want 127.0.0.1:2222", got)
	}
}

// TestUsesProxyMatchesResolveTarget keeps the two statements about aliases in
// agreement, so the UI's wording cannot contradict the code path.
func TestUsesProxyMatchesResolveTarget(t *testing.T) {
	alias := SSHConfig{Mode: SSHModeAlias, Alias: "lab"}
	_, _, ok := alias.ResolveTarget()
	if alias.UsesProxy() == ok {
		t.Errorf("UsesProxy=%v but resolvable=%v; these must be opposites for an alias", alias.UsesProxy(), ok)
	}
}

// TestControlCharsInSSHConfigPath guards a config file path that would otherwise
// smuggle a second argument through a newline.
func TestControlCharsInSSHConfigPath(t *testing.T) {
	cfg := SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 22, SSHConfigPath: "C:\\x\r\n-bad"}
	if err := ValidateSSH(cfg); err == nil {
		t.Error("a config path containing newlines must be refused")
	}
	long := SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 22, SSHConfigPath: strings.Repeat("C:\\x\\", 200)}
	if err := ValidateSSH(long); err == nil {
		t.Error("an absurdly long config path should be refused")
	}
}
