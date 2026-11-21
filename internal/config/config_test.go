package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	uuidA = "c907c20b-5a41-4dc2-99c5-0e9bc7773c38"
	uuidB = "2d620512-4b10-45a8-b070-25435e5106a5"
)

// TestMissingFileIsDefaults pins the friendly first-run path: no file means
// defaults, and no error, so a first launch does not look broken.
func TestMissingFileIsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if cfg.RefreshInterval() != DefaultRefreshSeconds*time.Second {
		t.Errorf("refresh = %s, want the default", cfg.RefreshInterval())
	}
	if cfg.SSHReadyTimeout() != DefaultSSHTimeoutSeconds*time.Second {
		t.Errorf("ssh timeout = %s, want the default", cfg.SSHReadyTimeout())
	}
	if cfg.Path() != path {
		t.Errorf("path = %q, want %q: the message about a bad file must name it", cfg.Path(), path)
	}
}

func TestEmptyFileIsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("an empty file should be treated as unconfigured, not corrupt: %v", err)
	}
	if cfg.RefreshInterval() <= 0 {
		t.Error("defaults should still be applied")
	}
}

// TestMalformedFileReportsPath covers the requirement that a broken file is
// reported with its location rather than producing a mystery failure.
func TestMalformedFileReportsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"refresh_seconds": "ten"`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("a malformed file must be reported")
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("want *LoadError, got %T", err)
	}
	if le.Path != path {
		t.Errorf("the error must carry the path, got %q", le.Path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the message should name the file so the user can fix it:\n%s", err.Error())
	}
	// The app keeps running with usable values rather than returning nil.
	if cfg == nil || cfg.RefreshInterval() <= 0 {
		t.Error("usable defaults must still be returned so the app can start")
	}
}

// TestUnknownKeysAreReported turns a silent typo into a message. A user who
// writes "refresh_seccond" would otherwise wonder why it is ignored forever.
func TestUnknownKeysAreReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"refresh_seccond": 9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Error("a misspelled key should be reported rather than quietly ignored")
	}
}

// TestInvalidValuesAreReportedWithTheirReason checks that validation names the
// setting, so the message is actionable.
func TestInvalidValuesAreReportedWithTheirReason(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		wants []string
	}{
		{"timeout too small", `{"ssh_ready_timeout_seconds": 1}`, []string{"ssh_ready_timeout_seconds"}},
		{"timeout too large", `{"ssh_ready_timeout_seconds": 99999}`, []string{"ssh_ready_timeout_seconds"}},
		{"non-uuid key", `{"vms": {"not-a-uuid": {"name": "x"}}}`, []string{"not-a-uuid", "UUID"}},
		{"port out of range", `{"vms": {"` + uuidA + `": {"ssh": {"mode": "endpoint", "host": "127.0.0.1", "port": 70000}}}}`, []string{uuidA}},
		{"alias with no value", `{"vms": {"` + uuidA + `": {"ssh": {"mode": "alias"}}}}`, []string{"alias"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFrom(path)
			if err == nil {
				t.Fatalf("expected a validation problem for %s", tc.json)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the message should mention %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

// ---------- guest settings ----------

func TestGuestBlockRoundTripsAlongsideSSH(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "kali", SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "kali"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMGuest(uuidA, "kali", &GuestConfig{OS: GuestOSLinux, Restart: GuestRestartGA}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	g := loaded.VMGuest(uuidA)
	if g == nil || *g != (GuestConfig{OS: GuestOSLinux, Restart: GuestRestartGA}) {
		t.Fatalf("guest block lost: %+v", g)
	}
	// Both blocks coexist in one entry: editing one never evicts the other.
	if ssh := loaded.VMSSH(uuidA); ssh.Mode != SSHModeEndpoint || ssh.Port != 2222 {
		t.Fatalf("ssh lost during guest store: %+v", ssh)
	}
}

func TestGuestBlockIsOmittedWhenUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "kali", SSHConfig{Mode: SSHModeAlias, Alias: "kali"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Older binaries reject unknown keys: an unset guest block must not be
	// written at all.
	if strings.Contains(string(data), "guest") {
		t.Fatalf("an unconfigured guest block must not appear in the file:\n%s", data)
	}
}

func TestGuestValidationRejectsUnknownValues(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMGuest(uuidA, "x", &GuestConfig{OS: "plan9"}); err == nil {
		t.Fatal("an unknown guest OS must be refused")
	}
	if err := cfg.SetVMGuest(uuidA, "x", &GuestConfig{Restart: "magic"}); err == nil {
		t.Fatal("an unknown restart mode must be refused")
	}
	if err := cfg.SetVMGuest("not-a-uuid", "x", &GuestConfig{OS: GuestOSLinux}); err == nil {
		t.Fatal("guest settings need a UUID key too")
	}
}

func TestGuestFileFromTheFutureIsReportedByPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"vms": {"` + uuidA + `": {"guest": {"os": "linux", "restart": "teleport"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("a bad restart mode in the file must report as a load error: %v", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("the error must name the file and the bad value: %v", err)
	}
}

func TestSetVMGuestNilClearsOnlyTheGuestBlock(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "kali", SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMGuest(uuidA, "kali", &GuestConfig{OS: GuestOSWindows, Restart: GuestRestartSSH}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMGuest(uuidA, "kali", nil); err != nil {
		t.Fatal(err)
	}
	if g := cfg.VMGuest(uuidA); g != nil {
		t.Fatalf("nil store must clear the block: %+v", g)
	}
	if ssh := cfg.VMSSH(uuidA); ssh.Mode != SSHModeEndpoint {
		t.Fatalf("SSH must survive a guest clear: %+v", ssh)
	}
}

// ---------- scoped resets ----------

func TestResetScopesAreSurgical(t *testing.T) {
	newCfg := func(t *testing.T) *Config {
		t.Helper()
		cfg := Defaults()
		if err := cfg.SetVMSSH(uuidA, "kali", SSHConfig{Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "k"}); err != nil {
			t.Fatal(err)
		}
		if err := cfg.SetVMGuest(uuidA, "kali", &GuestConfig{OS: GuestOSLinux, Restart: GuestRestartGA}); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("ssh scope keeps the guest block", func(t *testing.T) {
		cfg := newCfg(t)
		if !cfg.ResetVM(uuidA, ResetSSH) {
			t.Fatal("reset reported nothing to clear")
		}
		if ssh := cfg.VMSSH(uuidA); ssh.Configured() {
			t.Fatalf("ssh survived its scope: %+v", ssh)
		}
		if g := cfg.VMGuest(uuidA); g == nil || g.Restart != GuestRestartGA {
			t.Fatalf("the guest block must survive an SSH reset: %+v", g)
		}
	})

	t.Run("guest scope keeps ssh", func(t *testing.T) {
		cfg := newCfg(t)
		if !cfg.ResetVM(uuidA, ResetGuest) {
			t.Fatal("reset reported nothing to clear")
		}
		if g := cfg.VMGuest(uuidA); g != nil {
			t.Fatalf("guest block survived: %+v", g)
		}
		if ssh := cfg.VMSSH(uuidA); ssh.Mode != SSHModeEndpoint {
			t.Fatalf("ssh must survive a guest reset: %+v", ssh)
		}
	})

	t.Run("everything removes the entry", func(t *testing.T) {
		cfg := newCfg(t)
		if !cfg.ResetVM(uuidA, ResetEverything) {
			t.Fatal("reset reported nothing to clear")
		}
		if _, ok := cfg.VM(uuidA); ok {
			t.Fatal("entry should be gone")
		}
		// Resetting an unknown VM is a no-op, not a failure.
		if cfg.ResetVM(uuidA, ResetEverything) {
			t.Fatal("resetting twice should report nothing was cleared")
		}
	})

	t.Run("empty shells are not kept", func(t *testing.T) {
		cfg := Defaults()
		if err := cfg.SetVMSSH(uuidA, "kali", SSHConfig{Mode: SSHModeAlias, Alias: "kali"}); err != nil {
			t.Fatal(err)
		}
		// Only SSH stored: resetting it must delete the entry outright.
		if !cfg.ResetVM(uuidA, ResetSSH) {
			t.Fatal("nothing cleared")
		}
		if _, ok := cfg.VM(uuidA); ok {
			t.Fatal("an entry with nothing left in it must be removed, not stored empty")
		}
	})
}

func TestResetAndGuestKeysAreCanonicalUUIDs(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMGuest(strings.ToUpper("{"+uuidA+"}"), "kali", &GuestConfig{OS: GuestOSLinux}); err != nil {
		t.Fatal(err)
	}
	if g := cfg.VMGuest(uuidA); g == nil {
		t.Fatal("braced and uppercase UUIDs must address the same entry")
	}
	if !cfg.ResetVM("{"+strings.ToUpper(uuidA)+"}", ResetGuest) {
		t.Fatal("reset must find the entry through the same canonical key")
	}
}

// ---------- backward compatibility ----------

// TestOldFilesKeepLoading is the compatibility promise of the rename: the
// settings file written before the guest block existed must load unchanged.
func TestOldFilesKeepLoading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{
  "vboxmanage_path": "C:\\Custom\\VBoxManage.exe",
  "ssh_ready_timeout_seconds": 30,
  "vms": {
    "` + uuidA + `": {
      "name": "kali",
      "ssh": { "mode": "endpoint", "host": "127.0.0.1", "port": 2222, "user": "kali" }
    }
  }
}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("an older settings file must keep loading: %v", err)
	}
	if got := loaded.VMSSH(uuidA); got.Mode != SSHModeEndpoint || got.Port != 2222 || got.User != "kali" {
		t.Fatalf("ssh settings lost: %+v", got)
	}
	if g := loaded.VMGuest(uuidA); g != nil {
		t.Fatalf("a file without guest settings must read as unconfigured, got %+v", g)
	}
	// And it round-trips: saving after load keeps a guest-less entry
	// byte-compatible (no guest key appears).
	out := filepath.Join(t.TempDir(), "again.json")
	if err := loaded.SaveTo(out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "guest") {
		t.Fatalf("saving an old file must not invent guest blocks:\n%s", data)
	}
	if _, err := LoadFrom(out); err != nil {
		t.Fatalf("the rewritten file must load too: %v", err)
	}
}

// TestRoundTripPreservesEverythingPerVM is the core persistence guarantee: what
// the user stored comes back unchanged, including Unicode and awkward hosts.
func TestRoundTripPreservesEverythingPerVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := Defaults()
	cfg.path = path
	cfg.VBoxManagePath = `C:\Custom Location\VBoxManage.exe`
	cfg.RefreshSeconds = 11
	cfg.SSHReadyTimeoutSeconds = 90

	// Hosts, ports and users are ASCII (see validateHostToken); the VM *name* is
	// free text, which is where VirtualBox's Unicode actually shows up.
	if err := cfg.SetVMSSH(uuidA, "ubuntu — lab ñame \"quoted\"", SSHConfig{
		Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: `guest`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMSSH(uuidB, "kali", SSHConfig{Mode: SSHModeAlias, Alias: "kali.jump-host_1"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom after Save: %v", err)
	}
	if loaded.VBoxManagePath != cfg.VBoxManagePath {
		t.Errorf("override path lost: %q", loaded.VBoxManagePath)
	}
	if loaded.RefreshSeconds != 11 {
		t.Errorf("refresh = %d, want 11", loaded.RefreshSeconds)
	}
	if loaded.SSHReadyTimeout() != 90*time.Second {
		t.Errorf("ssh timeout = %s", loaded.SSHReadyTimeout())
	}

	a := loaded.VMSSH(uuidA)
	if a.Mode != SSHModeEndpoint || a.Host != "127.0.0.1" || a.Port != 2222 || a.User != "guest" {
		t.Errorf("endpoint settings lost: %+v", a)
	}
	b := loaded.VMSSH(uuidB)
	if b.Mode != SSHModeAlias || b.Alias != "kali.jump-host_1" {
		t.Errorf("alias settings lost: %+v", b)
	}
	if vm, ok := loaded.VM(uuidA); !ok || vm.Name != `ubuntu — lab ñame "quoted"` {
		t.Errorf("name hint lost or mangled: %q ok=%v", vm.Name, ok)
	}
	// A UUID never stored must not appear configured.
	if _, ok := loaded.VM("11111111-2222-3333-4444-555555555555"); ok {
		t.Error("VM() invented an entry")
	}
}

// TestSaveCreatesMissingDirectories lets the first save work without a setup step.
func TestSaveCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "config.json")
	cfg := Defaults()
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was not written: %v", err)
	}
}

// TestSaveIsAtomicByRename asserts the write goes through a temporary file, so a
// crash or a full disk cannot leave a half-written config that would then be
// rejected on the next start.
func TestSaveIsAtomicByRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Seed a valid file, then make the second save fail after the temp file has
	// been created by pointing the config at a path whose parent is a file. That
	// is the same class of failure a rename would hit.
	cfg := Defaults()
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A second save must succeed and leave no stray temporary files behind.
	cfg.RefreshSeconds = 7
	if err := cfg.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("leftover file after save: %q (a crash would leave this behind)", e.Name())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Error("the second save did not change the file")
	}
	// No temp files remain, and the file still parses.
	if _, err := LoadFrom(path); err != nil {
		t.Errorf("file unreadable after rewrite: %v", err)
	}
}

// TestSaveRejectsInvalidSettings stops a bad in-memory state from being written
// and then failing to load.
func TestSaveRejectsInvalidSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Defaults()
	cfg.SSHReadyTimeoutSeconds = 1 // below the minimum
	if err := cfg.SaveTo(path); err == nil {
		t.Error("Save should refuse to persist settings it would reject on load")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("nothing should have been written")
	}
}

// TestSaveKeepsOtherVMs is why settings are stored per UUID and updated in place.
func TestSaveKeepsOtherVMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	// A separate load, as a new process would do, then one edit.
	again, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.SetVMSSH(uuidB, "b", SSHConfig{Mode: SSHModeAlias, Alias: "lab"}); err != nil {
		t.Fatal(err)
	}
	if err := again.Save(); err != nil {
		t.Fatal(err)
	}
	final, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.VMSSH(uuidA).Port != 22 {
		t.Errorf("the first VM's settings were lost: %+v", final.VMSSH(uuidA))
	}
	if final.VMSSH(uuidB).Alias != "lab" {
		t.Errorf("the second VM's settings were lost: %+v", final.VMSSH(uuidB))
	}
}

// TestSetVMSSHRefusesNonUUID stops settings being stored under a key that could
// never be looked up again, e.g. a display name.
func TestSetVMSSHRefusesNonUUID(t *testing.T) {
	cfg := Defaults()
	for _, bad := range []string{"", "ubuntu", "{not-a-uuid}", "c907c20b5a414dc299c50e9bc7773c3"} {
		if err := cfg.SetVMSSH(bad, "x", SSHConfig{Mode: SSHModeAlias, Alias: "l"}); err == nil {
			t.Errorf("SetVMSSH accepted the key %q", bad)
		}
	}
	if len(cfg.VMs) != 0 {
		t.Errorf("nothing should have been stored: %+v", cfg.VMs)
	}
}

// TestLookupsIgnoreCaseAndBraces make hand-edited keys work, and match the way
// VirtualBox sometimes prints UUIDs.
func TestLookupsIgnoreCaseAndBraces(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeAlias, Alias: "lab"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		strings.ToUpper(uuidA),
		"{" + uuidA + "}",
		"{" + strings.ToUpper(uuidA) + "}",
		"  " + uuidA + "  ",
	} {
		if got := cfg.VMSSH(key); got.Alias != "lab" {
			t.Errorf("VMSSH(%q) = %+v, want the stored alias", key, got)
		}
	}
	if CanonicalUUID("{AABBCCDD-1122-3344-5566-778899AABBCC}") != "aabbccdd-1122-3344-5566-778899aabbcc" {
		t.Error("CanonicalUUID should lower-case and strip braces")
	}
}

// TestStoredFileHasNoCredentialFields asserts, against the struct tags rather
// than a hand-written list, that nothing password-shaped can ever be serialised.
func TestStoredFileHasNoCredentialFields(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{
		Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, User: "op", SSHConfigPath: `C:\users\me\.ssh\config`,
	}); err != nil {
		t.Fatal(err)
	}
	blob := string(cfg.marshal())
	for _, forbidden := range []string{"password", "passwd", "secret", "token", "key_data", "identity"} {
		if strings.Contains(strings.ToLower(blob), forbidden) {
			t.Errorf("the settings file must not carry %q, got:\n%s", forbidden, blob)
		}
	}
	// It must, however, carry what is needed to reconnect.
	for _, want := range []string{"127.0.0.1", "2222", "op"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the file should record %q:\n%s", want, blob)
		}
	}
}

// TestStoredFileIsSorted guards the readable-diff property: keys are stable, so
// a settings change shows as a small diff instead of a reshuffle.
func TestStoredFileIsSorted(t *testing.T) {
	cfg := Defaults()
	for _, u := range []string{uuidA, uuidB} { // inserted with the larger key first
		if err := cfg.SetVMSSH(u, "n", SSHConfig{Mode: SSHModeAlias, Alias: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	blob := string(cfg.marshal())
	// uuidB begins with "2" and uuidA with "c", so the sorted file names B first
	// even though A was stored first.
	if strings.Index(blob, uuidB) > strings.Index(blob, uuidA) {
		t.Errorf("vms should be sorted by UUID:\n%s", blob)
	}
	// It must be valid JSON.
	var check map[string]any
	if err := json.Unmarshal(cfg.marshal(), &check); err != nil {
		t.Errorf("marshal produced invalid JSON: %v\n%s", err, blob)
	}
}

// TestClearingAnEndpointRemovesIt: the form's clear action stores a zero config,
// and that must read back as "not configured".
func TestClearingAnEndpointRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "h", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.SetVMSSH(uuidA, "a", SSHConfig{}); err != nil {
		t.Fatal(err)
	}
	if again.VMSSH(uuidA).Configured() {
		t.Error("the endpoint should read as cleared")
	}
	if again.VMSSH(uuidA).Mode != SSHModeNone {
		t.Errorf("the mode should be cleared too, got %q", again.VMSSH(uuidA).Mode)
	}
	if err := again.Save(); err != nil {
		t.Fatal(err)
	}
	final, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.VMSSH(uuidA).Configured() {
		t.Errorf("the cleared endpoint came back: %+v", final.VMSSH(uuidA))
	}
	if got := final.VMSSH(uuidA); got.Mode != SSHModeNone || got.Host != "" || got.Port != 0 {
		t.Errorf("expected an empty configuration, got %+v", got)
	}
	// The VM's name hint survives: clearing an endpoint is not unregistering a VM.
	if vm, ok := final.VM(uuidA); !ok || vm.Name != "a" {
		t.Errorf("the name hint should survive clearing the endpoint: %+v ok=%v", vm, ok)
	}
}

func TestRemoveVM(t *testing.T) {
	cfg := Defaults()
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeAlias, Alias: "x"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoveVM(strings.ToUpper(uuidA)) {
		t.Error("RemoveVM should find a differently-cased key")
	}
	if cfg.RemoveVM(uuidA) {
		t.Error("removing twice should report nothing to remove")
	}
	if _, ok := cfg.VM(uuidA); ok {
		t.Error("the entry is still there")
	}
}

func TestRefreshAndTimeoutAreBounded(t *testing.T) {
	cfg := Defaults()
	cfg.RefreshSeconds = 0
	if got := cfg.RefreshInterval(); got != DefaultRefreshSeconds*time.Second {
		t.Errorf("zero refresh = %s, want the default rather than a busy loop", got)
	}
	cfg.RefreshSeconds = -5
	if got := cfg.RefreshInterval(); got <= 0 {
		t.Errorf("negative refresh = %s", got)
	}
	cfg.SSHReadyTimeoutSeconds = 100000
	if got := cfg.SSHReadyTimeout(); got > MaxSSHTimeoutSeconds*time.Second {
		t.Errorf("ssh timeout = %s, want it capped at %ds", got, MaxSSHTimeoutSeconds)
	}
}

// TestNilConfigIsSafe matters because the model may be built before settings are
// loaded, and a nil receiver must not crash the UI.
func TestNilConfigIsSafe(t *testing.T) {
	var cfg *Config
	if _, ok := cfg.VM(uuidA); ok {
		t.Error("nil config should have no VMs")
	}
	if cfg.VMSSH(uuidA).Configured() {
		t.Error("nil config should report nothing configured")
	}
}

func TestSSHConfigPathIsHonouredButNeverRewritten(t *testing.T) {
	cfg := Defaults()
	want := `C:\Users\me\.ssh\lab.config`
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{
		Mode: SSHModeEndpoint, Host: "127.0.0.1", Port: 2222, SSHConfigPath: want,
	}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.VMSSH(uuidA).SSHConfigPath; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	// Writing settings must not touch that file.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err == nil {
		t.Error("vbox-tui must never create or modify an ssh_config file")
	}
}
