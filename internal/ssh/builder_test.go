package ssh

import (
	"errors"
	"strings"
	"testing"

	"github.com/aditya-shrivastavv/vbox-tui/internal/config"
)

// TestBuildArgsPutUserDataAfterTheDoubleDash is the core safety property: text the
// user typed is placed only after "--", where OpenSSH is defined to stop parsing
// options. A "--" that appears later than a user value would mean the value could
// be read as a flag.
func TestBuildArgsPutUserDataAfterTheDoubleDash(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.SSHConfig
		want []string
	}{
		{
			name: "alias",
			cfg:  config.SSHConfig{Mode: config.SSHModeAlias, Alias: "lab-ubuntu"},
			want: []string{"-tt", "-o", "CanonicalizeHostname=no", "--", "lab-ubuntu"},
		},
		{
			name: "endpoint without a user",
			cfg:  config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222},
			want: []string{"-tt", "-p", "2222", "--", "127.0.0.1"},
		},
		{
			name: "endpoint with a user",
			cfg:  config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "kali"},
			want: []string{"-tt", "-p", "2222", "--", "kali@127.0.0.1"},
		},
		{
			name: "explicit ssh_config",
			cfg: config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "10.0.2.15", Port: 22,
				SSHConfigPath: `C:\Users\me\.ssh\lab.config`},
			want: []string{"-tt", "-F", `C:\Users\me\.ssh\lab.config`, "-p", "22", "--", "10.0.2.15"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildArgs(tc.cfg)
			if err != nil {
				t.Fatalf("BuildArgs: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("args = %v\nwant %v", got, tc.want)
			}
			assertOnlyAllowedOptions(t, got)
			assertUserDataTrailing(t, got)
		})
	}
}

// assertOnlyAllowedOptions proves nothing outside the documented allow-list is
// ever constructed, so a stored value cannot turn into an option.
func assertOnlyAllowedOptions(t *testing.T, args []string) {
	t.Helper()
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return // everything after this is a destination
		}
		if !strings.HasPrefix(a, "-") {
			t.Errorf("a bare argument %q appeared before the end-of-options marker", a)
			continue
		}
		allowed := false
		for _, opt := range OptionsAllowList {
			if a == opt {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Errorf("option %q is not in the allow-list %v", a, OptionsAllowList)
		}
		// -p and -F take a value we construct or validate; step over it.
		if a == "-p" || a == "-F" || a == "-o" {
			i++
		}
	}
}

// assertUserDataTrailing checks that user-supplied text is the final element and
// comes after "--", which is what stops it being parsed as an option.
func assertUserDataTrailing(t *testing.T, args []string) {
	t.Helper()
	dash := -1
	for i, a := range args {
		if a == "--" {
			dash = i
			break
		}
	}
	if dash < 0 {
		t.Fatal("no end-of-options marker was produced")
	}
	if dash != len(args)-2 {
		t.Errorf("\"--\" should be immediately before the destination: %v", args)
	}
}

// TestBuildArgsRefusesInjectionAttempts asserts values that config validation
// permits but that still look like options never make it through the builder.
func TestBuildArgsRefusesInjectionAttempts(t *testing.T) {
	attempts := []string{
		"-oProxyCommand=calc",
		"-J evil.example",
		"evil.example -oProxyCommand=calc",
		"-lroot",
	}
	for _, a := range attempts {
		cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: a, Port: 22}
		if got, err := BuildArgs(cfg); err == nil {
			t.Errorf("BuildArgs accepted host %q and produced %v", a, got)
		}
		cfgAlias := config.SSHConfig{Mode: config.SSHModeAlias, Alias: a}
		if got, err := BuildArgs(cfgAlias); err == nil {
			t.Errorf("BuildArgs accepted alias %q and produced %v", a, got)
		}
	}
}

// TestBuildArgsNeverAddsOptionsThatWeakenSecurity spells out the promise in the
// form of a test, so a future edit that adds StrictHostKeyChecking=no or a
// password flag fails here rather than in someone's terminal.
func TestBuildArgsNeverAddsOptionsThatWeakenSecurity(t *testing.T) {
	cfgs := []config.SSHConfig{
		{Mode: config.SSHModeAlias, Alias: "lab"},
		{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "op"},
		{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, SSHConfigPath: "C:\\x\\config"},
	}
	forbidding := []string{
		"StrictHostKeyChecking=no", "StrictHostKeyChecking=off",
		"UserKnownHostsFile=/dev/null", "UserKnownHostsFile=NUL",
		"NumberOfPasswordPrompts", "PreferredAuthentications",
		"ProxyCommand", "ProxyJump", "IdentityFile", "BatchMode",
	}
	for _, cfg := range cfgs {
		args, err := BuildArgs(cfg)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		for _, f := range forbidding {
			if strings.Contains(joined, f) {
				t.Errorf("%q was passed to ssh; host-key checking and authentication must be left as the user configured them\n%q", f, joined)
			}
		}
		// Passwords are never passed on the command line, where they would appear
		// in the process list.
		for _, a := range args {
			if strings.HasPrefix(a, "-y") || strings.HasPrefix(a, "-w") {
				t.Errorf("unexpected option %q", a)
			}
		}
		// A pseudo-terminal is requested so the console is left sane afterwards.
		if !strings.Contains(joined, "-tt") {
			t.Errorf("-tt is missing from %q; without it the console can be left cooked", joined)
		}
	}
}

func TestBuildArgsRejectsUnconfiguredAndBroken(t *testing.T) {
	if _, err := BuildArgs(config.SSHConfig{}); !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("empty config: got %v, want ErrNoEndpoint", err)
	}
	if _, err := BuildArgs(config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 0}); err == nil {
		t.Error("a zero port should be refused rather than producing a broken command")
	}
	if _, err := BuildArgs(config.SSHConfig{Mode: config.SSHModeEndpoint}); err == nil {
		t.Error("endpoint mode with no host should be refused")
	}
	// Unknown modes must not silently produce an empty command line.
	if _, err := BuildArgs(config.SSHConfig{Mode: "socks"}); err == nil {
		t.Error("an unknown mode should error")
	}
}

func TestBuildArgsTrimsStoredWhitespace(t *testing.T) {
	// A value edited by hand can carry padding; it must not be passed through.
	got, err := BuildArgs(config.SSHConfig{Mode: config.SSHModeAlias, Alias: "  lab  "})
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1] != "lab" {
		t.Errorf("alias was not trimmed: %v", got)
	}
}

func TestDescribeMatchesBuildArgs(t *testing.T) {
	cfg := config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "op"}
	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "ssh " + strings.Join(args, " ")
	if got := Describe("ssh", cfg); got != want {
		t.Errorf("Describe = %q, want %q: the preview and the real command must agree", got, want)
	}
	if !strings.Contains(Describe("ssh", config.SSHConfig{}), "no SSH endpoint") {
		t.Error("Describe should explain why there is nothing to run")
	}
}

func TestParsePort(t *testing.T) {
	for _, ok := range []string{"22", " 2222 ", "1", "65535"} {
		if _, err := ParsePort(ok); err != nil {
			t.Errorf("ParsePort(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "abc", "0", "-1", "65536", "22 22", "1e3", "+22"} {
		if n, err := ParsePort(bad); err == nil {
			t.Errorf("ParsePort(%q) = %d, want an error", bad, n)
		}
	}
}

// TestProbeTargetIsDialableOnlyForEndpoints is the boundary that keeps the
// readiness check honest. An alias may route through ProxyJump, and a successful
// TCP connection to the resolved address would say nothing about that route.
func TestProbeTargetIsDialableOnlyForEndpoints(t *testing.T) {
	host, port, needsOpenSSH, err := ProbeTarget(config.SSHConfig{
		Mode: config.SSHModeEndpoint, Host: "127.0.0.1", Port: 2222,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port != 2222 || needsOpenSSH {
		t.Errorf("endpoint probe target = %q:%d openSSH=%v", host, port, needsOpenSSH)
	}

	// Alias with no resolver: hand it to OpenSSH, and do not claim a target.
	host, port, needsOpenSSH, err = ProbeTarget(config.SSHConfig{
		Mode: config.SSHModeAlias, Alias: "lab",
	}, nil)
	if err != nil {
		t.Fatalf("an unresolvable alias must not be an error: %v", err)
	}
	if !needsOpenSSH {
		t.Error("an alias must be routed to OpenSSH rather than TCP-polled")
	}
	if host != "" || port != 0 {
		t.Errorf("no target should be claimed, got %q:%d", host, port)
	}

	// Alias with a working resolver: the address is reported for information but
	// needsOpenSSH stays true, so the caller cannot mistake it for a verified route.
	host, port, needsOpenSSH, err = ProbeTarget(config.SSHConfig{
		Mode: config.SSHModeAlias, Alias: "lab",
	}, staticResolver{ep: Endpoint{Host: "10.0.0.9", Port: 2202}})
	if err != nil {
		t.Fatal(err)
	}
	if !needsOpenSSH {
		t.Error("a resolved alias still cannot be verified by a direct dial")
	}
	if host != "10.0.0.9" || port != 2202 {
		t.Errorf("the resolved address should still be shown: %q:%d", host, port)
	}

	// A failing resolver must not block connecting.
	if _, _, needsOpenSSH, err := ProbeTarget(config.SSHConfig{
		Mode: config.SSHModeAlias, Alias: "lab",
	}, failingResolver{}); err != nil || !needsOpenSSH {
		t.Errorf("a resolver failure must fall through to OpenSSH: %v %v", needsOpenSSH, err)
	}
}

func TestProbeTargetRejectsBadConfig(t *testing.T) {
	if _, _, _, err := ProbeTarget(config.SSHConfig{}, nil); err == nil {
		t.Error("an unconfigured endpoint should error rather than dial nothing")
	}
	if _, _, _, err := ProbeTarget(config.SSHConfig{Mode: config.SSHModeEndpoint, Host: "-ofoo", Port: 22}, nil); err == nil {
		t.Error("an injection-looking host should be refused here as well as in BuildArgs")
	}
}

func TestJoinHostPortHandlesIPv6(t *testing.T) {
	if got := JoinHostPort("::1", 22); got != "[::1]:22" {
		t.Errorf("JoinHostPort(::1) = %q, want [::1]:22", got)
	}
	if got := JoinHostPort("127.0.0.1", 2222); got != "127.0.0.1:2222" {
		t.Errorf("JoinHostPort = %q", got)
	}
}

func TestParseEffectiveConfig(t *testing.T) {
	out := strings.Join([]string{
		"canonicalize false",
		"hostname vm-01.lab.example.com",
		"port 2202",
		"user kali",
		"proxyjump bastion.example.com",
		"addressfamily any",
	}, "\n")
	ep, err := parseEffectiveConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "vm-01.lab.example.com" || ep.Port != 2202 || ep.User != "kali" {
		t.Errorf("parsed %+v", ep)
	}
	if ep.Proxy != "bastion.example.com" {
		t.Errorf("ProxyJump not captured: %+v", ep)
	}
	if ep.Target() != "vm-01.lab.example.com:2202" {
		t.Errorf("Target = %q", ep.Target())
	}

	// A missing port line means OpenSSH's default, not zero.
	ep, err = parseEffectiveConfig("hostname host\nuser u\n")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Port != 22 {
		t.Errorf("port = %d, want the 22 default", ep.Port)
	}

	// No hostname is not usable.
	if _, err := parseEffectiveConfig("port 22\n"); err == nil {
		t.Error("output without a hostname should be an error")
	}
	// A junk port must not become port 0.
	ep, _ = parseEffectiveConfig("hostname h\nport abc\n")
	if ep.Port != 22 {
		t.Errorf("junk port gave %d", ep.Port)
	}
	// ProxyCommand is reported too, since it also defeats a direct TCP check.
	ep, _ = parseEffectiveConfig("hostname h\nport 22\nproxycommand ssh -W %h:%p bastion\n")
	if ep.ProxyC == "" {
		t.Error("ProxyCommand not captured")
	}
}

// TestEffectiveEndpointRefusesHostileAliases keeps ssh -G from being handed a
// value it would read as an option, which the same allow-list logic covers.
func TestEffectiveEndpointRefusesHostileAliases(t *testing.T) {
	r := OpenSSHResolver{}
	for _, alias := range []string{"", "   ", "-oProxyCommand=calc", "two words"} {
		if _, err := r.EffectiveEndpoint(alias); err == nil {
			t.Errorf("EffectiveEndpoint(%q) should refuse the input outright", alias)
		}
	}
}

type staticResolver struct{ ep Endpoint }

func (s staticResolver) EffectiveEndpoint(string) (Endpoint, error) { return s.ep, nil }

type failingResolver struct{}

func (failingResolver) EffectiveEndpoint(string) (Endpoint, error) {
	return Endpoint{}, errors.New("ssh -G is not available")
}
