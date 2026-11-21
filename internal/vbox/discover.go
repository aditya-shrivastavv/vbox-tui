package vbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNotFound means no VBoxManage executable could be located.
var ErrNotFound = errors.New("VBoxManage not found")

// wellKnownPaths returns the platform's usual install locations, most specific
// first. Only existing files are returned.
func wellKnownPaths(env map[string]string) []string {
	if env == nil {
		env = osEnviron()
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		progFiles := env["PROGRAMFILES"]
		if progFiles == "" {
			progFiles = `C:\Program Files`
		}
		progFilesX86 := env["PROGRAMFILES(X86)"]
		if progFilesX86 == "" {
			progFilesX86 = `C:\Program Files (x86)`
		}
		candidates = append(candidates,
			filepath.Join(progFiles, "Oracle", "VirtualBox", "VBoxManage.exe"),
			filepath.Join(progFilesX86, "Oracle", "VirtualBox", "VBoxManage.exe"),
			// Some admins install directly into Program Files.
			filepath.Join(progFiles, "Oracle", "VirtualBox", "VBoxManage"),
		)
	default:
		candidates = append(candidates,
			"/usr/bin/VBoxManage",
			"/usr/local/bin/VBoxManage",
			"/opt/VirtualBox/VBoxManage",
			"/Applications/VirtualBox.app/Contents/MacOS/VBoxManage",
		)
	}
	var found []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			found = append(found, c)
			seen[c] = true
		}
	}
	return found
}

func osEnviron() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[strings.ToUpper(k)] = v
	}
	return env
}

// Discover locates VBoxManage.
//
// Order: explicit override, PATH, standard install location. overrideAllowed
// must be false when the override came from an unvalidated source; it exists
// so callers can refuse an override that does not look like a real binary.
//
// The returned Sources records where each candidate was found, which the UI
// shows so users can tell an override from an automatic discovery.
type Discovery struct {
	Path   string
	Source string // "override", "PATH" or "standard location"
	// Candidates lists everything considered, for error messages.
	Candidates []string
}

// FindVBoxManage resolves the VBoxManage executable in the documented order:
// explicit override, PATH, then the standard install location.
//
// The Source field records where the binary came from, which the UI shows so a
// user can tell an override from automatic discovery. Candidates lists what was
// considered, for error messages that say where we looked.
func FindVBoxManage(explicit string) (Discovery, error) {
	d := Discovery{}
	if explicit != "" {
		if p, err := resolveExplicit(explicit); err == nil {
			d.Path, d.Source = p, "override"
			d.Candidates = []string{p}
			return d, nil
		}
		d.Candidates = append(d.Candidates, explicit+" (override: not an executable file)")
	}
	if p, err := exec.LookPath(exeName("VBoxManage")); err == nil {
		if abs, err2 := filepath.Abs(p); err2 == nil {
			p = abs
		}
		d.Path, d.Source = p, "PATH"
		d.Candidates = append(d.Candidates, p+" (PATH)")
		return d, nil
	}
	d.Candidates = append(d.Candidates, exeName("VBoxManage")+" (PATH: not found)")
	for _, p := range wellKnownPaths(nil) {
		d.Path, d.Source = p, "standard location"
		d.Candidates = append(d.Candidates, p+" (standard location)")
		return d, nil
	}
	for _, p := range wellKnownPathsHint() {
		d.Candidates = append(d.Candidates, p+" (not installed)")
	}
	return d, fmt.Errorf("%w on %s", ErrNotFound, runtime.GOOS)
}

// wellKnownPathsHint lists candidate paths without touching the filesystem so
// error messages can tell the user where we looked.
func wellKnownPathsHint() []string {
	if runtime.GOOS == "windows" {
		return []string{
			`C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`,
			`C:\Program Files (x86)\Oracle\VirtualBox\VBoxManage.exe`,
		}
	}
	return []string{"/usr/bin/VBoxManage", "/usr/local/bin/VBoxManage", "/opt/VirtualBox/VBoxManage"}
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// resolveExplicit validates a user supplied path. It must be an existing
// regular file (a bare directory name is a common mistake), and on Windows an
// .exe is appended when the suffix was omitted.
func resolveExplicit(explicit string) (string, error) {
	expanded := os.ExpandEnv(strings.TrimSpace(explicit))
	if expanded == "" {
		return "", errors.New("empty path")
	}
	expanded = strings.Trim(expanded, `"'`)
	candidates := []string{expanded}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(expanded), ".exe") {
		candidates = append(candidates, expanded+".exe")
	}
	for _, c := range candidates {
		st, err := os.Stat(c)
		if err != nil || st.IsDir() {
			continue
		}
		if abs, err := filepath.Abs(c); err == nil {
			return abs, nil
		}
		return c, nil
	}
	// A PATH lookup is tried last so "VBoxManage" works as an override too.
	if strings.ContainsAny(expanded, `/\`) {
		if p, err := exec.LookPath(expanded); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("not an executable file")
}

// InstallHint explains how to fix a missing VirtualBox install.
func InstallHint(err error) string {
	var sb strings.Builder
	sb.WriteString("VirtualBox was not found.\n\n")
	sb.WriteString("vbox-tui needs the VirtualBox command line tool:\n")
	for _, p := range wellKnownPathsHint() {
		sb.WriteString("  " + p + "\n")
	}
	sb.WriteString("\nOptions:\n")
	sb.WriteString("  1. Install VirtualBox from https://www.virtualbox.org/wiki/Downloads\n")
	sb.WriteString("     (a normal install is enough; vbox-tui does not need administrator rights)\n")
	sb.WriteString("  2. Point vbox-tui at an existing copy:\n")
	switch runtime.GOOS {
	case "windows":
		sb.WriteString("       vbox-tui --vboxmanage \"C:\\Program Files\\Oracle\\VirtualBox\\VBoxManage.exe\"\n")
	default:
		sb.WriteString("       vbox-tui --vboxmanage /usr/bin/VBoxManage\n")
	}
	sb.WriteString("  3. Or add the VirtualBox directory to PATH.\n")
	if err != nil {
		sb.WriteString("\nUnderlying error: " + err.Error() + "\n")
	}
	return sb.String()
}
