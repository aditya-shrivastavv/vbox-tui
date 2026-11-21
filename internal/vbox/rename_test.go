package vbox

import (
	"context"
	"strings"
	"testing"
)

// Renaming: the name rules enforced before anything leaves, and the exact
// argument vector VbxManage receives. Every case here runs against a fake
// recorder — no VirtualBox is installed or needed.

func TestValidateVMNameAcceptsRealNames(t *testing.T) {
	for _, name := range []string{
		"kali",
		"Kali Linux",
		"开发机 🐧 dev-box",
		"Workbench — Win11",
		`Ünïcødé 日本語`,
		"café",   // combining marks
		"vm (2)", // ordinary punctuation
		"dot.in.the.middle",
		"tab-free name",
	} {
		if err := ValidateVMName(name); err != nil {
			t.Errorf("ValidateVMName(%q) = %v, want acceptance", name, err)
		}
	}
}

func TestValidateVMNameRefusesUnusableNames(t *testing.T) {
	cases := []struct{ name, want string }{
		{"", "needs a name"},
		{"   ", "needs a name"},
		{"-rf", "read as an option"},
		{"trail.", "end in"},
		{"trail ", "end in"},
		{"esc\x1b[2J", "control characters"},
		{"bell\x07", "control characters"},
		{"nul\x00", "control characters"},
		{"c1\u0085next", "control characters"},
		{"del\x7f", "control characters"},
		{`has:colon`, "reserved"},
		{"has/slash", "reserved"},
		{"has\\slash", "reserved"},
		{"has<angle>", "reserved"},
		{"has\"quote", "reserved"},
		{"has|pipe", "reserved"},
		{"has?quest", "reserved"},
		{"has*star", "reserved"},
		{strings.Repeat("x", 201), "too long"},
	}
	for _, c := range cases {
		err := ValidateVMName(c.name)
		if err == nil {
			t.Errorf("ValidateVMName(%q) accepted, want refusal (%s)", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateVMName(%q) = %q, want it to mention %q", c.name, err.Error(), c.want)
		}
	}
	// The 200-rune boundary is inclusive.
	if err := ValidateVMName(strings.Repeat("日", 200)); err != nil {
		t.Errorf("200 runes must be legal: %v", err)
	}
}

func TestRenameVMArgVectorIsModifyvmByNameWithUUID(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	const name = "kali blue"
	if err := cli.RenameVM(context.Background(), uuid, name); err != nil {
		t.Fatalf("RenameVM: %v", err)
	}
	calls := r.recorded()
	if len(calls) != 1 {
		t.Fatalf("one call expected: %v", calls)
	}
	// The name travels as ONE element of the array. A shell is nowhere in
	// this path; spaces and every legal character survive as data.
	want := []string{cli.Path(), "--nologo", "modifyvm", uuid, "--name=" + name}
	if strings.Join(calls[0], "|") != strings.Join(want, "|") {
		t.Errorf("args = %v\nwant %v", calls[0], want)
	}
}

func TestRenameVMRefusesBeforeRunning(t *testing.T) {
	cli, r := newCLIWithFixtures(t, fixtures{})
	const uuid = "2d620512-4b10-45a8-b070-25435e5106a5"
	for _, bad := range []struct{ u, n string }{
		{"", "fine"},           // no machine to address
		{"   ", "fine"},        //   …still none
		{uuid, ""},             // nothing to name it
		{uuid, "-rf"},          // option injection attempt
		{uuid, "has:colon"},    // would break the file rename VirtualBox does
		{uuid, "esc\x1b[31mx"}, // control characters never ship
	} {
		if err := cli.RenameVM(context.Background(), bad.u, bad.n); err == nil {
			t.Errorf("RenameVM(%q, %q) accepted", bad.u, bad.n)
		}
	}
	if len(r.recorded()) != 0 {
		t.Fatalf("a refused rename must run nothing: %v", r.recorded())
	}
}

// RenameVM is not in LifecycleActions: renaming must never appear in a
// batchable set, and AllowedActions only offers it while powered off.
func TestRenameIsSingleMachineAndPoweredOffOnly(t *testing.T) {
	if ActionRename.Batchable() {
		t.Fatal("rename must never be batchable")
	}
	for _, kind := range []Kind{KindRunning, KindPaused, KindSaved, KindTransitional, KindUnavailable, KindUnknown} {
		info := StateInfo{Kind: kind}
		if AllowedActions(info)[ActionRename] {
			t.Errorf("state %v must not offer a rename", kind)
		}
	}
	for _, kind := range []Kind{KindOff, KindAborted} {
		info := StateInfo{Kind: kind}
		if !AllowedActions(info)[ActionRename] {
			t.Errorf("state %v must offer a rename", kind)
		}
	}
	if ActionRename.Label() != "rename" {
		t.Errorf("label = %q", ActionRename.Label())
	}
}
