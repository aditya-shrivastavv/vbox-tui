package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the settings-file integrity rules: a file vbx cannot
// read or understand is never overwritten by an ordinary edit; a concurrent
// writer is reported instead of silently replaced; a failed write cannot
// leave in-memory settings pretending they were committed; and a replacement
// only ever touches files this operation created.

func TestQuarantineRefusesEditsAndKeepsOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{ "vboxmanage_path": broken json,,, `)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("a malformed file must report a load error")
	}
	if cfg == nil {
		t.Fatal("a repaired-in-memory Config must still be returned")
	}
	if cfg.LoadProblem() == nil {
		t.Fatal("the Config must remember why its file was refused")
	}
	// Reads continue: the repaired values are usable this run.
	if cfg.RefreshSeconds < MinRefreshSeconds {
		t.Errorf("defaults were not applied to the repaired config: %+v", cfg)
	}
	// Ordinary edits refuse, naming the path and the recovery.
	err = cfg.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	})
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Edit must refuse with CorruptError, got %v", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "fix it") {
		t.Errorf("the refusal must name the path and the recovery: %v", err)
	}
	if err := cfg.Save(); !errors.As(err, &corrupt) {
		t.Fatalf("Save must refuse too, got %v", err)
	}
	// The file is byte-for-byte what it was.
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(original) {
		t.Errorf("the malformed original must survive untouched:\n got %q\nwant %q", now, original)
	}
}

func TestTrailingGarbageAfterObjectIsRefused(t *testing.T) {
	for _, body := range []string{
		`{"refresh_seconds": 7} {"refresh_seconds": 9}`, // a second document
		"{}\nrandom bytes\n",                            // trailing noise
		`{"refresh_seconds": 7}trailing`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadFrom(path)
		if err == nil {
			t.Errorf("trailing data must be refused: %q", body)
			continue
		}
		if cfg.LoadProblem() == nil {
			t.Errorf("trailing data must quarantine: %q", body)
		}
	}
}

func TestExistingButBlankFileLoadsCleanlyAndFirstEditWorks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a blank file means defaults, not corruption: %v", err)
	}
	// The bytes were seen (loaded recorded), so the first ordinary edit must
	// not mistake its own file for a concurrent writer.
	if err := cfg.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	}); err != nil {
		t.Fatalf("first edit after a blank file: %v", err)
	}
}

func TestUnreadableFileQuarantinesAndConfirmsReadsContinue(t *testing.T) {
	// A directory in the config file's place fails every platform's open or
	// read with a non-ErrNotExist error — the portable way to stand in for
	// a permissions problem.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("an unreadable file must report a load error")
	}
	if cfg.LoadProblem() == nil {
		t.Fatal("an unreadable file must quarantine writes")
	}
	// Read-only operation: the repaired configuration still serves.
	if ssh := cfg.VMSSH(uuidA); ssh.Mode != SSHModeNone {
		t.Errorf("reads on a repaired config must simply work: %+v", ssh)
	}
	err = cfg.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "1.2.3.4", Port: 2})
	})
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Edit over an unreadable file must refuse, got %v", err)
	}
	// The stand-in directory is untouched.
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("the unreadable original must not be replaced")
	}
}

func TestConcurrentWriterIsDetectedNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg1, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	// First writer puts the file down.
	if err := cfg1.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "first", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	}); err != nil {
		t.Fatal(err)
	}
	// Second writer's view of the file is stale bytes (there were none):
	// its save must be refused, not allowed to clobber the first result.
	err = cfg2.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidB, "second", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.2", Port: 22})
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("a stale writer must get ConflictError, got %v", err)
	}
	if !strings.Contains(err.Error(), "changed since") {
		t.Errorf("conflict must explain: %v", err)
	}
	// The file holds exactly the first writer's content.
	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the written file must load: %v", err)
	}
	if vm, ok := loaded.VM(uuidA); !ok || vm.Name != "first" {
		t.Fatalf("first writer's entry lost: %+v", loaded.VMs)
	}
	if _, ok := loaded.VM(uuidB); ok {
		t.Fatal("the refused second writer left nothing behind")
	}
	// And cfg2's in-memory mutation rolled back: the failed commit must not
	// leave settings that merely look committed.
	if _, ok := cfg2.VM(uuidB); ok {
		t.Fatal("a failed save must roll back the in-memory change")
	}
}

func TestFailedWriteRollsBackInMemoryCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "kept", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	}); err != nil {
		t.Fatal(err)
	}
	// Force the write to fail: the deterministic temp name this process
	// would use is pre-created, so O_EXCL refuses it.
	block := path + ".vbx-tui-write-" + itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(block, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := cfg.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidB, "ghost", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.2", Port: 22})
	})
	if wantErr == nil {
		t.Fatal("a forced write failure must surface as an error")
	}
	// Memory must match the disk: the failed edit rolled back, and the
	// earlier committed entry survives.
	if _, ok := cfg.VM(uuidB); ok {
		t.Fatal("a failed save must not leave the change in memory")
	}
	if vm, ok := cfg.VM(uuidA); !ok || vm.Name != "kept" {
		t.Fatalf("the previous content must remain: %+v", cfg.VMs)
	}
	disk, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.VM(uuidB); ok {
		t.Fatal("nothing new may reach disk on a failed write")
	}
}

func TestMutateErrorInsideEditRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMSSH(uuidA, "kept", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("nope")
	err = cfg.Edit(func(c *Config) error {
		c.VMs[CanonicalUUID(uuidB)] = VMConfig{Name: "half-written"}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("the mutate error must surface: %v", err)
	}
	if _, ok := cfg.VM(uuidB); ok {
		t.Fatal("a failed mutate must roll back its own partial change")
	}
	if _, ok := cfg.VM(uuidA); !ok {
		t.Fatal("the rollback must restore what was there before")
	}
}

func TestNoForeignBakIsTouchedAndNoneIsCreatedByReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	bak := path + ".bak"
	// A foreign .bak that predates any vbx write must survive every save
	// untouched — the old fixed-name backup scheme is gone.
	foreign := []byte("someone else's recovery file")
	if err := os.WriteFile(bak, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := cfg.Edit(func(c *Config) error {
			return c.SetVMSSH(uuidA, "ed", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("the foreign .bak vanished: %v", err)
	}
	if string(got) != string(foreign) {
		t.Errorf("the foreign .bak was modified: %q", got)
	}
	// And it did not get created by the operation when it did not exist.
	dir2 := t.TempDir()
	path2 := filepath.Join(dir2, "config.json")
	cfg2, err := LoadFrom(path2)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg2.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir2)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != FileName {
			t.Errorf("save left extra files behind: %q", e.Name())
		}
	}
}

func TestSetNameOnlyUpdatesExistingEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	// No stored entry: nothing is updated and no settings entry is invented.
	updated, err := cfg.SetName(uuidA, "ghost")
	if err != nil || updated {
		t.Fatalf("SetName over no entry: updated=%v err=%v", updated, err)
	}
	if _, ok := cfg.VM(uuidA); ok {
		t.Fatal("a name hint must not fabricate a settings entry")
	}
	// An invalid UUID is refused, not stored.
	if _, err := cfg.SetName("not-a-uuid", "x"); err == nil {
		t.Fatal("SetName must refuse a non-UUID key")
	}
	if err := cfg.SetVMSSH(uuidA, "before", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if updated, err := cfg.SetName(uuidA, "after"); err != nil || !updated {
		t.Fatalf("SetName on a real entry: updated=%v err=%v", updated, err)
	}
	if vm, _ := cfg.VM(uuidA); vm.Name != "after" {
		t.Errorf("hint not updated: %+v", vm)
	}
}

func TestLoadProblemClearedNeverImplicitly(t *testing.T) {
	// Quarantine is permanent until the program restarts against a fixed
	// file: neither a read nor a refused write may clear it.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("want load error")
	}
	_ = cfg.VMSSH(uuidA)
	_ = cfg.Edit(func(*Config) error { return nil })
	if cfg.LoadProblem() == nil {
		t.Fatal("the quarantine must persist for this in-memory Config")
	}
	// Once the file is fixed, a FRESH load is the explicit recovery path.
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a fixed file must load: %v", err)
	}
	if err := fresh.Edit(func(c *Config) error {
		return c.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22})
	}); err != nil {
		t.Fatalf("after explicit recovery, edits work again: %v", err)
	}
}

func TestOtherVMEntriesSurviveEveryEditPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetVMSSH(uuidA, "a", SSHConfig{Mode: SSHModeEndpoint, Host: "10.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	// A committed B, then a failed edit for C must leave A and B as they were.
	if err := cfg.Edit(func(c *Config) error {
		return c.SetVMGuest(uuidB, "b", &GuestConfig{OS: GuestOSLinux, Restart: GuestRestartGA})
	}); err != nil {
		t.Fatal(err)
	}
	before := cfg.marshal()
	// Force failure via the blocked deterministic temp name.
	block := path + ".vbx-tui-write-" + itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(block, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Edit(func(c *Config) error {
		return c.SetVMSSH("33333333-4444-5555-6666-777777777777", "c",
			SSHConfig{Mode: SSHModeAlias, Alias: "ghost"})
	}); err == nil {
		t.Fatal("expected the blocked write to fail")
	}
	if string(cfg.marshal()) != string(before) {
		t.Errorf("memory drifted from disk state after a failed save:\n%s\nvs\n%s", cfg.marshal(), before)
	}
}

// itoa is a one-liner to avoid pulling strconv into a focused test file that
// otherwise has no number formatting.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
