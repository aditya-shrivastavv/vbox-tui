// Package config stores vbox-tui's settings in the platform user configuration
// directory (under vbox-tui), keyed by VM UUID.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// AppName is the directory name used under the user configuration directory.
const AppName = "vbox-tui"

// FileName is the configuration file name inside that directory.
const FileName = "config.json"

// Defaults.
const (
	DefaultRefreshSeconds    = 5
	MinRefreshSeconds        = 1
	DefaultSSHTimeoutSeconds = 45
	MinSSHTimeoutSeconds     = 5
	MaxSSHTimeoutSeconds     = 900
)

// SSHMode selects how vbox-tui reaches a VM.
type SSHMode string

// Supported SSH modes.
const (
	// SSHModeNone means no SSH endpoint is configured.
	SSHModeNone SSHMode = ""
	// SSHModeAlias hands an existing OpenSSH config alias to ssh(1) and lets
	// OpenSSH resolve host, port, user, jumps and proxies itself.
	SSHModeAlias SSHMode = "alias"
	// SSHModeEndpoint uses an explicit host, port and optional username.
	SSHModeEndpoint SSHMode = "endpoint"
)

// SSHConfig is the per-VM SSH settings. It is stored in the app config file;
// the user's own ~/.ssh/config is never read or written by vbox-tui.
type SSHConfig struct {
	Mode SSHMode `json:"mode,omitempty"`
	// Alias is an argument to ssh(1), resolved through the user's normal OpenSSH
	// configuration. Use this for ProxyJump/ProxyCommand routes.
	Alias string `json:"alias,omitempty"`
	// Host and Port are used in endpoint mode. Host may be an IP or a name;
	// 127.0.0.1 is the usual value for a NAT-forwarded guest.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// User is optional in endpoint mode; when empty, ssh(1) uses its own
	// default (the current login name or an SSH config match).
	User string `json:"user,omitempty"`
	// SSHConfigPath optionally points at an alternate OpenSSH config file.
	// Only used in endpoint mode; it never replaces the user's own file.
	SSHConfigPath string `json:"ssh_config_path,omitempty"`
}

// GuestRestartMode selects how vbox-tui asks a guest operating system to
// reboot itself. Each mode is a commitment, not a guess: a chosen mode that
// cannot work explains its prerequisite rather than silently substituting
// another, and nothing here ever falls back to a hardware reset or a power
// cycle.
type GuestRestartMode string

// Supported guest restart modes.
const (
	// GuestRestartAuto uses the SSH path when it is fully configured
	// (endpoint/alias plus a known guest OS), and Guest Additions otherwise.
	GuestRestartAuto GuestRestartMode = ""
	// GuestRestartGA asks VirtualBox to request a guest reboot
	// (controlvm reboot). Requires Guest Additions with its control service.
	GuestRestartGA GuestRestartMode = "ga"
	// GuestRestartSSH runs one fixed reboot command through the configured
	// SSH settings; OpenSSH owns authentication entirely.
	GuestRestartSSH GuestRestartMode = "ssh"
	// GuestRestartOff refuses graceful restarts for this VM instead of
	// risking anything.
	GuestRestartOff GuestRestartMode = "off"
)

// Valid reports whether the mode is one of the four documented values
// (including the empty auto default).
func (m GuestRestartMode) Valid() bool {
	switch m {
	case GuestRestartAuto, GuestRestartGA, GuestRestartSSH, GuestRestartOff:
		return true
	}
	return false
}

// GuestOS names a guest family so restart commands can be chosen from a
// fixed table instead of guessed.
type GuestOS string

// Supported guest OS families. GuestOSUnknown deliberately has no usable
// reboot command: refusing is safer than guessing which binary reboots.
const (
	GuestOSUnknown GuestOS = ""
	GuestOSLinux   GuestOS = "linux"
	GuestOSWindows GuestOS = "windows"
)

// Valid reports whether the family is recognised.
func (o GuestOS) Valid() bool {
	switch o {
	case GuestOSUnknown, GuestOSLinux, GuestOSWindows:
		return true
	}
	return false
}

// GuestConfig is the per-VM guest-control settings: enough to pick a safe
// restart path, nothing executable of the user's own.
type GuestConfig struct {
	OS      GuestOS          `json:"os,omitempty"`
	Restart GuestRestartMode `json:"restart,omitempty"`
}

// Validate checks the guest settings.
func (g GuestConfig) Validate() error {
	if !g.OS.Valid() {
		return fmt.Errorf("guest os %q is not one of: linux, windows", string(g.OS))
	}
	if !g.Restart.Valid() {
		return fmt.Errorf("restart mode %q is not one of: ga, ssh, off (or unset for auto)", string(g.Restart))
	}
	return nil
}

// VMConfig holds settings for one VM.
type VMConfig struct {
	// Name is remembered only as a hint so the UI can mention a renamed VM.
	Name string    `json:"name,omitempty"`
	SSH  SSHConfig `json:"ssh,omitempty"`
	// Guest is the guest-control section; nil (absent) means all defaults.
	// Pointer so an unconfigured VM writes no empty section and old files
	// keep loading unchanged.
	Guest *GuestConfig `json:"guest,omitempty"`
}

// Config is the whole application configuration.
type Config struct {
	// VBoxManagePath overrides automatic discovery when non-empty.
	VBoxManagePath string `json:"vboxmanage_path,omitempty"`
	// RefreshSeconds is how often the inventory is re-read.
	RefreshSeconds int `json:"refresh_seconds,omitempty"`
	// SSHReadyTimeoutSeconds bounds the readiness wait in start-and-connect.
	SSHReadyTimeoutSeconds int `json:"ssh_ready_timeout_seconds,omitempty"`
	// VMs is keyed by VM UUID.
	VMs map[string]VMConfig `json:"vms,omitempty"`

	// path is where this Config was loaded from, or where it will be written.
	path string
	// loadProblem is non-nil when the file existed but could not be read,
	// parsed, or validated. Settings edits must not paper over it.
	loadProblem error
	// loaded holds the exact bytes the last successful load (or save) put on
	// disk. Save compares against it so a concurrent writer is reported,
	// never silently overwritten.
	loaded []byte
}

// Defaults returns a configuration with sensible values filled in.
func Defaults() *Config {
	return &Config{
		RefreshSeconds:         DefaultRefreshSeconds,
		SSHReadyTimeoutSeconds: DefaultSSHTimeoutSeconds,
		VMs:                    map[string]VMConfig{},
	}
}

// Dir returns the configuration directory (created lazily by Save).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not locate the user configuration directory: %w", err)
	}
	return filepath.Join(base, AppName), nil
}

// DirHint describes where the file lives, for help text and error messages.
func DirHint() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return "~/" + AppName
	}
	return filepath.Join(base, AppName)
}

// Path returns the full configuration file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, FileName), nil
}

// Load reads the configuration. A missing file is not an error: Defaults are
// returned. A malformed file returns a LoadError carrying the path, and the
// returned Config remembers that: saving over a file it could not read or
// understand is refused until the file itself is fixed or moved aside.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		cfg := Defaults()
		cfg.loadProblem = err
		return cfg, err
	}
	return LoadFrom(p)
}

// LoadFrom reads a specific file, which tests use to avoid touching the real
// user configuration.
func LoadFrom(path string) (*Config, error) {
	cfg := Defaults()
	cfg.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		cfg.loadProblem = err
		return cfg, &LoadError{Path: path, Err: err}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		// An existing-but-blank file is understood (it means "defaults"), and
		// the exact bytes are remembered so the conflict check in commit can
		// prove nobody else wrote the file between this read and the first
		// edit.
		cfg.loaded = append([]byte(nil), data...)
		return cfg, nil
	}
	// Reject unknown keys: a typo in a hand-edited file should be told about,
	// not quietly ignored.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	parsed := Config{}
	if err := dec.Decode(&parsed); err != nil {
		cfg.loadProblem = err
		return cfg, &LoadError{Path: path, Err: err}
	}
	// A second JSON document (or any garbage) after the first object means
	// the file is not one configuration; accepting it would mean deciding,
	// silently, which half to honour.
	if dec.More() {
		err := errors.New("unexpected data after the configuration object")
		cfg.loadProblem = err
		return cfg, &LoadError{Path: path, Err: err}
	}
	if parsed.VMs == nil {
		parsed.VMs = map[string]VMConfig{}
	}
	cfg = &parsed
	cfg.path = path
	cfg.loaded = append([]byte(nil), data...)
	// Validate before applying defaults. Otherwise an out-of-range value a person
	// typed by hand would be silently replaced and they would never learn their
	// edit was ignored. The returned Config is still repaired, so the app runs
	// with sane values while the message says what was wrong — and remembers the
	// problem, because repairing in memory is no licence to overwrite the file
	// the user needs to see and fix.
	problems := cfg.Validate()
	cfg.applyDefaults()
	if len(problems) > 0 {
		cfg.loadProblem = errors.Join(problems...)
		return cfg, &LoadError{Path: path, Err: cfg.loadProblem}
	}
	return cfg, nil
}

// LoadError reports a configuration file that could not be read or understood.
type LoadError struct {
	Path string
	Err  error
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("configuration file %s is not usable: %v", e.Path, e.Err)
}

func (e *LoadError) Unwrap() error { return e.Err }

func (c *Config) applyDefaults() {
	if c.RefreshSeconds < MinRefreshSeconds {
		c.RefreshSeconds = DefaultRefreshSeconds
	}
	if c.SSHReadyTimeoutSeconds < MinSSHTimeoutSeconds {
		c.SSHReadyTimeoutSeconds = DefaultSSHTimeoutSeconds
	}
	if c.VMs == nil {
		c.VMs = map[string]VMConfig{}
	}
}

// Validate checks the settings it can check without touching the network.
func (c *Config) Validate() []error {
	var errs []error
	if c.RefreshSeconds != 0 && c.RefreshSeconds < MinRefreshSeconds {
		errs = append(errs, fmt.Errorf("refresh_seconds must be at least %d", MinRefreshSeconds))
	}
	if c.SSHReadyTimeoutSeconds != 0 &&
		(c.SSHReadyTimeoutSeconds < MinSSHTimeoutSeconds || c.SSHReadyTimeoutSeconds > MaxSSHTimeoutSeconds) {
		errs = append(errs, fmt.Errorf("ssh_ready_timeout_seconds must be between %d and %d", MinSSHTimeoutSeconds, MaxSSHTimeoutSeconds))
	}
	for uuid, vm := range c.VMs {
		if !uuidLike(uuid) {
			errs = append(errs, fmt.Errorf("vms key %q is not a VM UUID", uuid))
		}
		if err := ValidateSSH(vm.SSH); err != nil {
			errs = append(errs, fmt.Errorf("vm %s: %w", uuid, err))
		}
		if vm.Guest != nil {
			if err := vm.Guest.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("vm %s: %w", uuid, err))
			}
		}
	}
	return errs
}

// RefreshInterval is the configured inventory refresh interval.
func (c *Config) RefreshInterval() time.Duration {
	n := c.RefreshSeconds
	if n < MinRefreshSeconds {
		n = DefaultRefreshSeconds
	}
	return time.Duration(n) * time.Second
}

// SSHReadyTimeout is the configured readiness timeout.
func (c *Config) SSHReadyTimeout() time.Duration {
	n := c.SSHReadyTimeoutSeconds
	if n < MinSSHTimeoutSeconds {
		n = DefaultSSHTimeoutSeconds
	}
	if n > MaxSSHTimeoutSeconds {
		n = MaxSSHTimeoutSeconds
	}
	return time.Duration(n) * time.Second
}

// VM returns the settings for a UUID, and whether any were stored.
func (c *Config) VM(uuid string) (VMConfig, bool) {
	if c == nil || c.VMs == nil {
		return VMConfig{}, false
	}
	vm, ok := c.VMs[CanonicalUUID(uuid)]
	if !ok {
		// Tolerate differently-cased keys written by hand.
		for k, v := range c.VMs {
			if strings.EqualFold(k, uuid) {
				return v, true
			}
		}
	}
	return vm, ok
}

// VMSSH returns the SSH settings for a UUID.
func (c *Config) VMSSH(uuid string) SSHConfig {
	vm, _ := c.VM(uuid)
	return vm.SSH
}

// SetVMSSH stores SSH settings for one UUID, preserving every other VM's
// settings. name is a display hint only.
func (c *Config) SetVMSSH(uuid, name string, ssh SSHConfig) error {
	if !uuidLike(uuid) {
		return fmt.Errorf("refusing to store settings under %q: not a VM UUID", uuid)
	}
	if err := ValidateSSH(ssh); err != nil {
		return err
	}
	if c.VMs == nil {
		c.VMs = map[string]VMConfig{}
	}
	key := CanonicalUUID(uuid)
	vm := c.VMs[key]
	if name != "" {
		vm.Name = name
	}
	vm.SSH = ssh
	c.VMs[key] = vm
	return nil
}

// RemoveVM drops one VM's settings.
func (c *Config) RemoveVM(uuid string) bool {
	if c.VMs == nil {
		return false
	}
	key := CanonicalUUID(uuid)
	if _, ok := c.VMs[key]; !ok {
		return false
	}
	delete(c.VMs, key)
	return true
}

// VMGuest returns the guest-control settings for a UUID. A nil result means
// nothing is configured and the defaults (auto) apply.
func (c *Config) VMGuest(uuid string) *GuestConfig {
	vm, _ := c.VM(uuid)
	return vm.Guest
}

// SetVMGuest stores the guest-control settings for one UUID, preserving its
// SSH settings and every other VM's settings. A nil cfg means "no preference":
// the block is removed so the entry behaves exactly like an unconfigured VM.
func (c *Config) SetVMGuest(uuid, name string, cfg *GuestConfig) error {
	if !uuidLike(uuid) {
		return fmt.Errorf("refusing to store settings under %q: not a VM UUID", uuid)
	}
	if cfg != nil {
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	if c.VMs == nil {
		c.VMs = map[string]VMConfig{}
	}
	key := CanonicalUUID(uuid)
	vm := c.VMs[key]
	if name != "" {
		vm.Name = name
	}
	if cfg != nil {
		copied := *cfg
		vm.Guest = &copied
	} else {
		vm.Guest = nil
	}
	c.VMs[key] = vm
	return nil
}

// ResetScope selects what ResetVM clears for one VM.
type ResetScope string

const (
	// ResetSSH clears only the saved SSH connection settings.
	ResetSSH ResetScope = "ssh"
	// ResetGuest clears only the saved guest-control settings.
	ResetGuest ResetScope = "guest"
	// ResetEverything removes the whole entry, which is how a VM the user
	// never configured again should read: exactly "no settings saved yet".
	ResetEverything ResetScope = "all"
)

// ResetVM clears saved settings for one VM, addressed by UUID. The scope is
// explicit because "reset the settings for this VM" can mean one section or
// the whole entry, and guessing would be surprising here.
func (c *Config) ResetVM(uuid string, scope ResetScope) bool {
	if c.VMs == nil {
		return false
	}
	key := CanonicalUUID(uuid)
	vm, ok := c.VMs[key]
	if !ok {
		return false
	}
	switch scope {
	case ResetSSH:
		vm.SSH = SSHConfig{}
	case ResetGuest:
		vm.Guest = nil
	case ResetEverything:
		delete(c.VMs, key)
		return true
	default:
		return false
	}
	// An entry with nothing left in it is removed rather than kept as an
	// empty shell, so the file only ever holds real settings.
	if vm.SSH.Mode == SSHModeNone && vm.Guest == nil {
		delete(c.VMs, key)
		return true
	}
	c.VMs[key] = vm
	return true
}

// Path returns the file this configuration came from.
func (c *Config) Path() string { return c.path }

// LoadProblem returns the reason the file was refused for writing, or nil
// when the last load (if any) understood the file cleanly.
func (c *Config) LoadProblem() error { return c.loadProblem }

// SetName updates only the cosmetic display-name hint stored for one UUID
// — the entry created by a settings store carries the name the user last saw.
// Without a stored entry there is nothing to update: the hint lives inside
// real settings, and a rename alone must not fabricate a settings entry.
func (c *Config) SetName(uuid, name string) (bool, error) {
	if !uuidLike(uuid) {
		return false, fmt.Errorf("refusing to store a name under %q: not a VM UUID", uuid)
	}
	key := CanonicalUUID(uuid)
	vm, ok := c.VMs[key]
	if !ok {
		return false, nil
	}
	vm.Name = name
	c.VMs[key] = vm
	return true, nil
}

// CorruptError reports a settings file vbx deliberately refuses to overwrite:
// it could not be read or understood, so ordinary edits must not silently
// replace it, and a copy of whatever is on disk has been preserved untouched.
type CorruptError struct {
	Path string
	// Reason is the underlying load failure.
	Reason error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("vbx will not write %s: %v. The file is kept untouched; fix it, or move it aside and run vbx again to start fresh.",
		e.Path, e.Reason)
}

func (e *CorruptError) Unwrap() error { return e.Reason }

// ConflictError reports that the settings file changed on disk since this
// Config read it. Another process (or a hand edit) wrote it; silently saving
// over that work would lose it, so the write is refused instead.
type ConflictError struct {
	Path string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the settings file %s changed since vbx read it; nothing was written. Re-run vbx to pick up the newer file.", e.Path)
}

// Edit applies one settings change atomically-in-the-large: the change is
// validated, compared against the file on disk, written, and only then kept.
// If anything refuses — a quarantine on an unreadable original, a concurrent
// writer, an invalid result, or a failed write — the in-memory Config is
// restored to exactly the state the disk holds, so a failed save can never
// leave settings that merely look committed.
//
// Read paths (VM, VMSSH, VMGuest, Validate) work regardless: refusing to
// write never means refusing to read.
func (c *Config) Edit(mutate func(*Config) error) error {
	if c.loadProblem != nil {
		return &CorruptError{Path: c.displayPath(), Reason: c.loadProblem}
	}
	before := c.memo()
	if err := mutate(c); err != nil {
		c.restoreMemo(before)
		return err
	}
	if problems := c.Validate(); len(problems) > 0 {
		c.restoreMemo(before)
		return errors.Join(problems...)
	}
	want := c.marshal()
	if err := c.commit(want); err != nil {
		c.restoreMemo(before)
		return err
	}
	return nil
}

// commit writes want after conflict checks, then records it as the state the
// disk holds. On any refusal nothing is written.
func (c *Config) commit(want []byte) error {
	// Conflict check: the file must hold exactly the bytes we last saw.
	// c.loaded == nil means this Config has never faced a file on disk; if
	// one appeared since, another writer wrote it and is honoured, not
	// overwritten.
	now, err := os.ReadFile(c.displayPath())
	switch {
	case c.loaded == nil:
		if err == nil {
			return &ConflictError{Path: c.displayPath()}
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not read %s before writing: %w", c.displayPath(), err)
		}
	default:
		if err != nil {
			return &ConflictError{Path: c.displayPath()}
		}
		if !bytes.Equal(now, c.loaded) {
			return &ConflictError{Path: c.displayPath()}
		}
	}
	if err := writeAtomic(c.displayPath(), want); err != nil {
		return err
	}
	c.loaded = append([]byte(nil), want...)
	return nil
}

// memo serialises the current settings for rollback; it carries exactly the
// persisted fields, which is what a rollback must restore.
func (c *Config) memo() []byte { return c.marshal() }

// restoreMemo rebuilds the persisted fields from a memo snapshot, keeping the
// identity fields (path, quarantine, loaded bytes) untouched.
func (c *Config) restoreMemo(data []byte) {
	cfg := Config{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// The memo comes from marshal(); these types cannot fail to
		// serialise. A failure here means a programming error, and the safe
		// response is to leave the Config alone rather than half-restore it.
		return
	}
	c.VBoxManagePath = cfg.VBoxManagePath
	c.RefreshSeconds = cfg.RefreshSeconds
	c.SSHReadyTimeoutSeconds = cfg.SSHReadyTimeoutSeconds
	if cfg.VMs == nil {
		cfg.VMs = map[string]VMConfig{}
	}
	c.VMs = cfg.VMs
}

func (c *Config) displayPath() string {
	if c.path != "" {
		return c.path
	}
	p, err := Path()
	if err == nil {
		return p
	}
	return "(unset)"
}

// Save writes the file with the configured contents, honouring the same
// quarantine-then-conflict rules as Edit. Prefer Edit; Save exists for
// whole-file operations (SaveTo, first write of a fresh Config).
func (c *Config) Save() error {
	if c.loadProblem != nil {
		return &CorruptError{Path: c.displayPath(), Reason: c.loadProblem}
	}
	if problems := c.Validate(); len(problems) > 0 {
		return errors.Join(problems...)
	}
	return c.commit(c.marshal())
}

// SaveTo writes this configuration to a specific path.
func (c *Config) SaveTo(path string) error {
	c.path = path
	c.loaded = nil // a different file is a file we have not read
	return c.Save()
}

func (c *Config) marshal() []byte {
	type export struct {
		VBoxManagePath         string              `json:"vboxmanage_path,omitempty"`
		RefreshSeconds         int                 `json:"refresh_seconds,omitempty"`
		SSHReadyTimeoutSeconds int                 `json:"ssh_ready_timeout_seconds,omitempty"`
		VMs                    map[string]VMConfig `json:"vms,omitempty"`
	}
	out := export{
		VBoxManagePath:         c.VBoxManagePath,
		RefreshSeconds:         c.RefreshSeconds,
		SSHReadyTimeoutSeconds: c.SSHReadyTimeoutSeconds,
		VMs:                    c.VMs,
	}
	// Sort keys for a stable, diff-friendly file.
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		// The types here cannot fail to marshal; if it ever happens, prefer an
		// explicit failure over silently writing a wrong file.
		return []byte("{}\n")
	}
	return append(sortedKeys(data), '\n')
}

// writeAtomic writes through a temporary file in the destination directory
// and then installs it over the destination. The guarantees are exactly the
// ones a single-rename swap can honour, spelled out because "atomic" promises
// more than the platform gives:
//
//   - Before anything moves, the destination's existing content is captured;
//     a failed install restores it, so the last valid settings file survives
//     a failed write. With MOVEFILE_REPLACE_EXISTING (and os.Rename's POSIX
//     rename), Windows replaces the target in one rename — but no crash-safety
//     beyond that is claimed.
//   - Only files this operation created are ever deleted. The old fixed
//     ".bak" dance is gone: it could collide with a recovery file it did not
//     own, and it deleted a backup the non-Windows path had never made.
//   - If even the restore cannot run, the staged content stays in a
//     <name>.vbx-tui-write-<pid>.new file and the error names it — never a
//     silent loss, and never a file the operation did not create touched.
//   - Permissions: the temp is created 0600 (a private DACL on Windows); on
//     POSIX the destination's mode bits are copied onto it before install,
//     so a rewrite never widens or narrows who can read the file. On Windows
//     the new file inherits the directory's ACL; vbx does not claim to
//     reconstruct an arbitrary custom descriptor.
//   - Concurrent writers are handled above writeAtomic, by the byte-for-byte
//     conflict check in Config.commit — not by hoping a rename serialises
//     two processes, which it does not.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}
	// Capture the previous content and mode for rollback. A read failure
	// that is not "missing" means we cannot prove what was there, and then
	// the only safe action is to leave the file alone entirely.
	prev, readErr := os.ReadFile(path)
	prevExists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("could not read %s before replacing it: %w", path, readErr)
	}
	var prevMode os.FileMode
	if prevExists && runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err == nil {
			prevMode = st.Mode().Perm()
		}
	}

	tmp := path + fmt.Sprintf(".vbx-tui-write-%d.tmp", os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("could not write a temporary file in %s: %w", dir, err)
	}
	tmpName := f.Name()
	if err := func() error {
		defer f.Close() //nolint:errcheck // Close error reported by the explicit Close below when reached
		if _, err := f.Write(data); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return f.Close()
	}(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("could not write %s: %w", tmpName, err)
	}
	if prevExists && runtime.GOOS != "windows" && prevMode != 0 {
		if err := os.Chmod(tmpName, prevMode); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("could not set permissions on %s: %w", tmpName, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		// The destination may or may not still exist depending on where the
		// platform failed; put back exactly the bytes that were captured.
		if prevExists {
			if restoreErr := os.WriteFile(path, prev, 0o600); restoreErr != nil {
				staged := path + fmt.Sprintf(".vbx-tui-write-%d.new", os.Getpid())
				_ = os.Rename(tmpName, staged)
				if prevMode != 0 && runtime.GOOS != "windows" {
					_ = os.Chmod(staged, prevMode)
				}
				return fmt.Errorf("could not install %s: %w; the previous file could not be restored (%v), so the previous content is kept verbatim at %s",
					path, err, restoreErr, staged)
			}
		}
		os.Remove(tmpName)
		return fmt.Errorf("could not install %s: %w", path, err)
	}
	return nil
}

// sortedKeys re-emits a JSON object with its top-level keys in a fixed order,
// and the vms map sorted by UUID, so the file stays readable in a diff.
func sortedKeys(data []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return data
	}
	order := []string{"vboxmanage_path", "refresh_seconds", "ssh_ready_timeout_seconds", "vms"}
	var b strings.Builder
	b.WriteString("{\n")
	first := true
	for _, key := range order {
		raw, ok := top[key]
		if !ok {
			continue
		}
		if key == "vms" {
			raw = sortVMs(raw)
		}
		if !first {
			b.WriteString(",\n")
		}
		first = false
		enc, err := json.Marshal(key)
		if err != nil {
			return data
		}
		b.WriteString("  " + string(enc) + ": ")
		b.WriteString(indentBody(raw, "  "))
	}
	if !first {
		b.WriteString("\n")
	}
	b.WriteString("}")
	return []byte(b.String())
}

func sortVMs(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return raw
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		enc, err := json.Marshal(k)
		if err != nil {
			return raw
		}
		b.WriteString("    " + string(enc) + ": " + indentBody(m[k], "    "))
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  }")
	return []byte(b.String())
}

// indentBody re-marshals a JSON value with two-space indentation and prefixes
// continuation lines with pad.
func indentBody(raw []byte, pad string) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, pad, "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// CanonicalUUID lower-cases and braces-strips a UUID for map keys.
func CanonicalUUID(uuid string) string {
	s := strings.TrimSpace(uuid)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	return strings.ToLower(s)
}

func uuidLike(s string) bool {
	s = CanonicalUUID(s)
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return true
}
