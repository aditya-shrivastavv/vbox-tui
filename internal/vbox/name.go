package vbox

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateVMName checks a proposed display name for `modifyvm --name`.
//
// The rules mirror what reality enforces, so the refusal says the real
// reason instead of a later cryptic hypervisor error:
//
//   - empty (or only spaces) names are nothing to set;
//   - a leading '-' would read as an option to VBoxManage;
//   - control characters (C0, DEL, C1) are refused outright: every consumer
//     of the name is a terminal, and a name that carries escape or carriage
//     semantics is an invitation to display spoofing;
//   - characters Windows reserves for paths ( < > : " / \ | ? * ) are
//     refused because renaming a VM renames its files and folder too
//     (VirtualBox 7+), so a name legal on Linux still breaks there;
//   - a name can never end in a dot or space, same file-system reason;
//   - a length cap keeps the name inside every line the app draws it on.
//
// Everything else passes — Unicode of any script, spaces inside the name,
// emoji, and ordinary punctuation all make legitimate VM names. The name is
// checked exactly as typed here; the caller trims for the comparison, while
// arguments travel to exec as an array, never through a shell.
func ValidateVMName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a VM needs a name")
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("name %q starts with '-' and would be read as an option", name)
	}
	if strings.HasSuffix(name, ".") {
		return errors.New("a VM name cannot end in '.' (its files would be unreadable on some systems)")
	}
	if name != strings.TrimRight(name, " \t") {
		return errors.New("a VM name cannot end in a space")
	}
	if r := []rune(name); len(r) > 200 {
		return errors.New("VM name is too long (max 200 characters)")
	}
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return errors.New("VM name contains control characters")
		case r >= 0x80 && r <= 0x9f:
			return errors.New("VM name contains control characters")
		case strings.ContainsRune(`<>:"/\|?*`, r):
			return fmt.Errorf("VM name cannot contain %q: VirtualBox renames the machine's files too, and that character is reserved for paths", string(r))
		}
	}
	return nil
}
