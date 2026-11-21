package vbox

import (
	"strings"
	"testing"
	"time"
)

// listVMSLongActual is captured from a real VirtualBox 7.2 `list vms --long`,
// trimmed between sections but keeping nested detail lines verbatim. The real
// output contains no blank lines inside a machine's record; blank lines only
// separate machines. Nested lines are indented (snapshot names, audio/video
// detail, …) and must never be mistaken for machine properties: an earlier
// version of the parser read "   Name: <snapshot>" as the machine's own name.
const listVMSLongActual = `Name:            ubuntu
Groups:          /
OSType:          Ubuntu (64-bit)
Guest OS:        Ubuntu (64-bit)
UUID:            c907c20b-5a41-4dc2-99c5-0e9bc7773c38
Config file:     D:\VirtualBox VMs\ubuntu\ubuntu.vbox
Snapshot folder: D:\VirtualBox VMs\ubuntu\Snapshots
State:           powered off (since 2026-10-16T16:40:30.000000000)
    Record audio:            no
    Destination:             File
    Video FPS:               25fps
[* Snapshots:]
   Name: clean_base_fullscr (UUID: ea6fcffc-5076-48f8-be60-a46544d1af80) *
[* Guest:]
Configured memory balloon:   0MB

Name:            kali
Groups:          /
Guest OS:        Debian (64-bit)
UUID:            2d620512-4b10-45a8-b070-25435e5106a5
Config file:     D:\VirtualBox VMs\kali\kali.vbox
State:           running
[* Snapshots:]
   Name: clean-kali (UUID: 95f99e32-9e1e-466a-8296-c1500d642b02) *
   Named: hard disk states:
      UUID (BC): 1,

Name:            omarchy
Groups:          /
Guest OS:        Arch Linux (64-bit)
UUID:            f421275d-fe89-4b28-b68b-7663ee521a5f
State:           saved
[* Snapshots:]
   Name: Snapshot 1 (UUID: cf08e0cc-2280-4e81-868d-df03c1b5ecc9) *
`

func TestParseListVMSLong(t *testing.T) {
	inv := ParseListVMSLong(listVMSLongActual)
	if len(inv.Machines) != 3 {
		t.Fatalf("got %d machines, want 3 (%+v)", len(inv.Machines), inv.Machines)
	}
	if len(inv.Problems) != 0 {
		t.Errorf("unexpected problems: %+v", inv.Problems)
	}

	// Sorted case-insensitively by name.
	want := []struct {
		name  string
		uuid  string
		state State
	}{
		{"kali", "2d620512-4b10-45a8-b070-25435e5106a5", StateRunning},
		{"omarchy", "f421275d-fe89-4b28-b68b-7663ee521a5f", StateSaved},
		{"ubuntu", "c907c20b-5a41-4dc2-99c5-0e9bc7773c38", StatePoweredOff},
	}
	for i, w := range want {
		got := inv.Machines[i]
		if got.Name != w.name || got.UUID != w.uuid || got.State.State != w.state {
			t.Errorf("machine %d = %+v, want %+v", i, got, w)
		}
	}

	ubuntu := inv.Machines[2]
	if ubuntu.StateChanged.Year() != 2026 {
		t.Errorf("state change time not parsed: %v", ubuntu.StateChanged)
	}
	if ubuntu.GuestOS != "Ubuntu (64-bit)" {
		t.Errorf("guest OS = %q", ubuntu.GuestOS)
	}
	if ubuntu.ConfigFile == "" {
		t.Error("config file not captured")
	}
}

// TestParseListVMSLongNestedSections pins the bug that made real VMs
// undiscoverable by name: `list vms --long` indents nested detail lines, and a
// snapshot's "Name:" line must not rename the machine that contains it.
func TestParseListVMSLongNestedSections(t *testing.T) {
	inv := ParseListVMSLong(listVMSLongActual)
	for _, m := range inv.Machines {
		if strings.Contains(m.Name, "(") || strings.Contains(m.Name, "UUID") || strings.Contains(m.Name, "*") {
			t.Errorf("machine name looks like a snapshot entry: %q", m.Name)
		}
	}
	// The indented continuation under kali's snapshot list ("Named: hard
	// disk states:") must not become a machine either.
	if len(inv.Machines) != 3 || len(inv.Problems) != 0 {
		t.Fatalf("machines=%+v problems=%+v; nested output invented or dropped entries", inv.Machines, inv.Problems)
	}
	if _, err := inv.Resolve("ubuntu"); err != nil {
		t.Errorf("Resolve(ubuntu) failed despite the name being listed: %v", err)
	}
	if _, err := inv.Resolve("clean_base_fullscr"); err == nil {
		t.Error("a snapshot name must not resolve as a VM")
	}
}

func TestParseListVMSLongEmpty(t *testing.T) {
	for _, in := range []string{"", "\n\n", "   \n"} {
		inv := ParseListVMSLong(in)
		if len(inv.Machines) != 0 || len(inv.Problems) != 0 {
			t.Errorf("empty input %q produced %+v", in, inv)
		}
	}
}

// TestParseListVMSLongInaccessible pins the handling of a machine whose settings
// file could not be read. VirtualBox prints the UUID in place of the name, so the
// machine must be reported as unavailable rather than as startable.
func TestParseListVMSLongInaccessible(t *testing.T) {
	fixture := `Name:            {12345678-1234-1234-1234-123456789abc}
Groups:          /
UUID:            {12345678-1234-1234-1234-123456789abc}
State:           inaccessible
`
	inv := ParseListVMSLong(fixture)
	if len(inv.Machines) != 1 {
		t.Fatalf("want one machine, got %+v", inv.Machines)
	}
	m := inv.Machines[0]
	if m.State.Kind != KindUnavailable {
		t.Errorf("kind = %d, want unavailable; an inaccessible VM must not look startable", m.State.Kind)
	}
	if len(m.Notes) == 0 {
		t.Error("an explanation should accompany an inaccessible machine")
	}
	if AllowedActions(m.State)[ActionStart] {
		t.Error("starting an inaccessible machine must not be offered")
	}
}

// TestParseListVMSLongMissingUUID records the problem instead of dropping a
// machine silently.
func TestParseListVMSLongMissingUUID(t *testing.T) {
	inv := ParseListVMSLong("Name:            odd-entry\nState:           running\n")
	if len(inv.Machines) != 0 {
		t.Errorf("a machine without a UUID cannot be addressed; got %+v", inv.Machines)
	}
	if len(inv.Problems) != 1 {
		t.Fatalf("want a recorded problem, got %+v", inv.Problems)
	}
}

func TestParseListVMS(t *testing.T) {
	out := `"ubuntu" {c907c20b-5a41-4dc2-99c5-0e9bc7773c38}
"kali" {2d620512-4b10-45a8-b070-25435e5106a5}
"Workbench — Win11" {7c3f9a11-0d45-4b3c-8f58-1a2b3c4d5e6f}
`
	machines, err := ParseListVMS(out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(machines) != 3 {
		t.Fatalf("got %d machines", len(machines))
	}
	if machines[2].Name != "Workbench — Win11" {
		t.Errorf("unicode name mangled: %q", machines[2].Name)
	}
}

func TestParseListVMSRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"not-a-machine-entry",
		`"name" {not-a-uuid}`,
		`"unterminated {`,
	} {
		if _, err := ParseListVMS(bad); err == nil {
			t.Errorf("ParseListVMS(%q) accepted malformed output", bad)
		}
	}
}

// showVminfoActual mirrors `showvminfo <uuid> --machinereadable` from a real
// VirtualBox 7.2 Kali machine, including the multi-line recording block and a
// quoted storage key whose value contains an escaped Windows path.
const showVminfoActual = `name="kali"
encryption="disabled"
groups="/"
ostype="Debian (64-bit)"
UUID="2d620512-4b10-45a8-b070-25435e5106a5"
CfgFile="D:\\VirtualBox VMs\\kali\\kali.vbox"
memory=8192
vram=128
cpus=4
chipset="piix3"
firmware="BIOS"
VMState="poweroff"
VMStateChangeTime="2026-11-19T13:58:14.000000000"
storagecontrollername0="SATA"
"SATA-0-0"="D:\\VirtualBox VMs\\kali\\Snapshots/{314c95a5-81e5-4033-8a63-b5e79c02a1b2}.vdi"
"SATA-1-0"="none"
natnet1="nat"
macaddress1="0800272CFDDD"
cableconnected1="on"
nic1="nat"
nictype1="82540EM"
localhostReachable="1"
Forwarding(0)="ssh,tcp,127.0.0.1,2222,,22"
hostonlyadapter2="VirtualBox Host-Only Ethernet Adapter"
macaddress2="08002707C886"
cableconnected2="on"
nic2="hostonly"
nictype2="82540EM"
nic3="none"
hidpointing="ps2mouse"
uart1="off"
audio="default"
vrde="off"
recording_started="off"
recording_screens=1
 rec_screen0
 rec_screen_enabled="on"
SnapshotName="clean-kali"
SnapshotUUID="95f99e32-9e1e-466a-8296-c1500d642b02"
SnapshotDescription="Stock Kali 2026.2 VBox image, first boot verified (XFCE)"
CurrentSnapshotName="clean-kali"
CurrentSnapshotUUID="95f99e32-9e1e-466a-8296-c1500d642b02"
GuestMemoryBalloon=0
`

func TestParseShowVMInfo(t *testing.T) {
	m, err := ParseShowVMInfo(showVminfoActual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "kali" {
		t.Errorf("name = %q", m.Name)
	}
	if m.UUID != "2d620512-4b10-45a8-b070-25435e5106a5" {
		t.Errorf("uuid = %q", m.UUID)
	}
	if m.State.State != StatePoweredOff {
		t.Errorf("state = %q, want powered off", m.State.State)
	}
	if m.MemoryMB != 8192 {
		t.Errorf("memory = %d, want 8192", m.MemoryMB)
	}
	if m.CPUs != 4 {
		t.Errorf("cpus = %d, want 4", m.CPUs)
	}
	if m.GuestOS != "Debian (64-bit)" {
		t.Errorf("guest os = %q", m.GuestOS)
	}

	// The escaped Windows path must survive exactly one level of unescaping.
	wantCfg := `D:\VirtualBox VMs\kali\kali.vbox`
	if m.ConfigFile != wantCfg {
		t.Errorf("config file = %q, want %q", m.ConfigFile, wantCfg)
	}

	if m.StateChanged.IsZero() {
		t.Error("state change time should parse")
	}
	if m.StateChanged.Year() != 2026 || m.StateChanged.Month() != time.November {
		t.Errorf("state change time = %v", m.StateChanged)
	}
}

func TestParseShowVMInfoNetworkAndForwards(t *testing.T) {
	m, err := ParseShowVMInfo(showVminfoActual)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.NICs) != 2 {
		t.Fatalf("got %d adapters, want 2 (nic3=none must not count): %+v", len(m.NICs), m.NICs)
	}
	if m.NICs[0].Type != "nat" || m.NICs[0].Slot != 1 {
		t.Errorf("nic1 = %+v", m.NICs[0])
	}
	if m.NICs[1].Type != "hostonly" || m.NICs[1].Device != "VirtualBox Host-Only Ethernet Adapter" {
		t.Errorf("nic2 = %+v, want hostonly with its interface name", m.NICs[1])
	}
	if !m.NICs[0].Attached {
		t.Error("cableconnected1=on should mean attached")
	}

	if len(m.Forwards) != 1 {
		t.Fatalf("got %d forwards: %+v", len(m.Forwards), m.Forwards)
	}
	f := m.Forwards[0]
	if f.Name != "ssh" || f.Proto != "tcp" || f.HostIP != "127.0.0.1" || f.HostPort != 2222 || f.GuestPort != 22 || f.NIC != 1 {
		t.Errorf("forward parsed wrongly: %+v", f)
	}
	if !strings.Contains(f.String(), "2222") {
		t.Errorf("forward rendering: %q", f.String())
	}

	// The NAT helper must be able to point the user at the SSH rule.
	got, ok := m.NATForwardFor(22)
	if !ok || got.HostPort != 2222 {
		t.Errorf("NATForwardFor(22) = %+v/%v", got, ok)
	}
	if _, ok := m.NATForwardFor(3389); ok {
		t.Error("no RDP rule exists, so none should be found")
	}

	if !m.HasNATLocalhost || !m.NATLocalhost {
		t.Errorf("localhostReachable=1 should be reported: %+v/%+v", m.HasNATLocalhost, m.NATLocalhost)
	}
}

// TestParseShowVMInfoQuotedAndEscapedValue covers the "do not assume every line
// can be split on a delimiter" requirement: names with embedded quotes, backslashes
// and non-ASCII text have to survive intact.
func TestParseShowVMInfoQuotedAndEscapedValue(t *testing.T) {
	src := "name=\"my \\\"quoted\\\" vm — Ω\"\n" +
		"UUID=\"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\"\n" +
		"VMState=\"poweroff\"\n" +
		`CfgFile="C:\\Users\\Ü ser\\vm with space\\vm.vbox"` + "\n" +
		"SnapshotDescription=\"a = b, c=d\"\n"
	m, err := ParseShowVMInfo(src)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if m.Name != `my "quoted" vm — Ω` {
		t.Errorf("name round trip failed: %q", m.Name)
	}
	if m.ConfigFile != `C:\Users\Ü ser\vm with space\vm.vbox` {
		t.Errorf("path round trip failed: %q", m.ConfigFile)
	}
	// A value containing = and , must stay whole: it is stored under its own key,
	// not split.
	if len(m.Forwards) != 0 {
		t.Errorf("a description must never be parsed as a forwarding rule: %+v", m.Forwards)
	}
}

func TestParseShowVMInfoMissingUUID(t *testing.T) {
	if _, err := ParseShowVMInfo("name=\"x\"\nVMState=\"poweroff\"\n"); err == nil {
		t.Error("output without a UUID should be rejected: it cannot be safely addressed")
	}
}

func TestParseShowVMInfoEmptyIsError(t *testing.T) {
	if _, err := ParseShowVMInfo(""); err == nil {
		t.Error("empty output should be an error, not an empty machine")
	}
	if _, err := ParseShowVMInfo("this is not machine readable at all"); err == nil {
		t.Error("unrecognisable output should be an error")
	}
}

func TestParseShowVMInfoNotesUnparsableLines(t *testing.T) {
	src := showVminfoActual + "this line has no equals sign\n"
	m, err := ParseShowVMInfo(src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range m.Notes {
		if strings.Contains(n, "could not be parsed") {
			found = true
		}
	}
	if !found {
		t.Errorf("a partially unparsable response should be reported in notes: %+v", m.Notes)
	}
}

// TestParseShowVMInfoBridgeAndDetachedAdapter checks the network rendering inputs,
// including an adapter with no cable.
func TestParseShowVMInfoBridgeAndDetachedAdapter(t *testing.T) {
	src := `name="bridge"
UUID="11111111-2222-3333-4444-555555555555"
VMState="poweroff"
nic1="bridged"
bridgeadapter1="Intel(R) Wi-Fi 6E AX211"
cableconnected1="off"
macaddress1="080027000001"
nic2="intnet"
intnet2="lab"
`
	m, err := ParseShowVMInfo(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.NICs) != 2 {
		t.Fatalf("want 2 adapters: %+v", m.NICs)
	}
	if m.NICs[0].Type != "bridged" || m.NICs[0].Device != "Intel(R) Wi-Fi 6E AX211" {
		t.Errorf("bridged adapter = %+v", m.NICs[0])
	}
	if m.NICs[0].Attached {
		t.Error("cableconnected=off must be reported as detached")
	}
	if m.NICs[1].Type != "intnet" || m.NICs[1].Device != "lab" {
		t.Errorf("internal network adapter = %+v", m.NICs[1])
	}
	if got := m.NICs[0].String(); !strings.Contains(got, "bridged") {
		t.Errorf("adapter rendering = %q", got)
	}
}

func TestParseForwardsSortedAndSane(t *testing.T) {
	src := `name="multi"
UUID="22222222-2222-3333-4444-555555555555"
VMState="poweroff"
nic1="nat"
Forwarding(0)="high,tcp,,9999,,9999"
Forwarding(1)="low,tcp,,1222,,22"
Forwarding(2)="broken,tcp,only-three"
Forwarding(3)=",tcp,,8080,,80"
`
	m, err := ParseShowVMInfo(src)
	if err != nil {
		t.Fatal(err)
	}
	var ports []int
	for _, f := range m.Forwards {
		ports = append(ports, f.HostPort)
	}
	if len(ports) != 3 {
		t.Fatalf("got %d rules (%v); malformed rules are skipped", len(ports), ports)
	}
	if ports[0] != 1222 || ports[1] != 8080 || ports[2] != 9999 {
		t.Errorf("rules not sorted by host port: %v", ports)
	}
	// An unnamed rule still needs a label so the UI has something to show.
	if m.Forwards[1].Name == "" {
		t.Error("an unnamed rule should get a placeholder name")
	}
}

// TestParseForwardsAdapterAttribution pins the honesty rule: --machinereadable
// numbers rules with one global counter, so the adapter may only be named when
// there is exactly one NAT adapter to attribute them to.
func TestParseForwardsAdapterAttribution(t *testing.T) {
	oneNAT := `name="one"
UUID="33333333-3333-4444-5555-666666666666"
VMState="poweroff"
nic1="hostonly"
hostonlyadapter1="vboxnet0"
nic2="nat"
Forwarding(0)="ssh,tcp,,2222,,22"
`
	// The machine's only NAT adapter is slot 2, so the rule must be attributed
	// to nic2 rather than to nic1 (which a naive "index+1" would produce for
	// Forwarding(0)).
	m, err := ParseShowVMInfo(oneNAT)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Forwards) != 1 {
		t.Fatalf("want one rule: %+v", m.Forwards)
	}
	if m.Forwards[0].NIC != 2 {
		t.Errorf("NIC = %d, want 2: the only NAT adapter is slot 2", m.Forwards[0].NIC)
	}

	twoNAT := `name="two"
UUID="44444444-3333-4444-5555-666666666666"
VMState="poweroff"
nic1="nat"
nic2="nat"
Forwarding(0)="a,tcp,,2222,,22"
Forwarding(1)="b,tcp,,3333,,33"
`
	m2, err := ParseShowVMInfo(twoNAT)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Forwards) != 2 {
		t.Fatalf("want two rules: %+v", m2.Forwards)
	}
	for _, f := range m2.Forwards {
		if f.NIC != 0 {
			t.Errorf("%s: NIC = %d; with two NAT adapters the adapter is unknown and must not be guessed", f.Name, f.NIC)
		}
		if strings.Contains(f.String(), "nic") {
			t.Errorf("%s: rendering names an adapter that is not known: %q", f.Name, f.String())
		}
	}
	// Sorted by host port, which is what a reader scanning for a port wants.
	if m2.Forwards[0].HostPort != 2222 || m2.Forwards[1].HostPort != 3333 {
		t.Errorf("not sorted by host port: %+v", m2.Forwards)
	}
}

func TestInventoryFindIsCaseInsensitive(t *testing.T) {
	inv := ParseListVMSLong(listVMSLongActual)
	if _, ok := inv.Find("2D620512-4B10-45A8-B070-25435E5106A5"); !ok {
		t.Error("Find should match a different-cased UUID")
	}
	if _, ok := inv.Find("nope"); ok {
		t.Error("Find should not invent a machine")
	}
}

func TestUnquoteRejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{`unterminated"`, `nope`, `"unescaped " quote"`} {
		if _, err := unquote(bad); err == nil {
			t.Errorf("unquote(%q) should fail", bad)
		}
	}
}
