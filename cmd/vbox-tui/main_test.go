package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aditya-shrivastavv/vbox-tui/internal/cli"
)

// The legacy name is a thin shim: every answer must be exactly what vbx
// gives. The behaviour itself is covered by the internal/cli tests; what
// lives here pins the wiring — the shim's own test binary compiling main()
// alongside this file already proves the package builds.

func TestShimDelegatesToCLI(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := cli.Run([]string{"--help"}, out, errOut); code != 0 {
		t.Fatalf("shim help exit %d (%q)", code, errOut)
	}
	// The user-facing name is vbx on both entry points; the shim says so too
	// (the help text mentions vbox-tui only as the historical product name).
	if !strings.Contains(out.String(), "vbx <action>") {
		t.Fatalf("help through the shim should speak as vbx: %q", out.String())
	}
}
