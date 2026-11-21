package vbox

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Machine is a registered VirtualBox machine as the app understands it.
type Machine struct {
	Name string
	UUID string
	// State is the normalised state.
	State StateInfo
	// RawState is exactly what VirtualBox reported, e.g. `powered off (since …)`.
	RawState string
	// StateChanged is parsed from VMStateChangeTime or the "since" annotation.
	StateChanged time.Time
	// Notes carries anything unusual discovered while reading the machine, for
	// example an inaccessible settings file or unparsable output.
	Notes []string

	MemoryMB int
	CPUs     int

	GuestOS    string
	ConfigFile string

	NICs     []NIC
	Forwards []Forward

	// HasNATLocalhost / NATLocalhost describe the nat-localhostreachable
	// setting for the machine when VirtualBox reports it.
	HasNATLocalhost bool
	NATLocalhost    bool

	Snapshots int
}

// NIC is one network adapter.
type NIC struct {
	Slot int
	// Type is the attachment type: nat, bridged, hostonly, intnet,
	// natnetwork, generic or none.
	Type string
	// Device is the bridge adapter, host-only interface, internal network or
	// NAT network name for types that have one.
	Device   string
	MAC      string
	Attached bool
}

// String renders the adapter compactly.
func (n NIC) String() string {
	if n.Device == "" {
		return fmt.Sprintf("nic%d: %s", n.Slot, n.Type)
	}
	return fmt.Sprintf("nic%d: %s (%s)", n.Slot, n.Type, n.Device)
}

// Forward is an existing NAT port-forwarding rule. vbox-tui only reads these;
// it never changes VM networking.
type Forward struct {
	// NIC is the adapter the rule belongs to, or 0 when it could not be
	// determined. --machinereadable numbers forwarding rules with one global
	// counter (Forwarding(0), Forwarding(1), …) and does not say which adapter a
	// rule belongs to, so guessing "adapter index+1" would be a fabrication that
	// could point the user at the wrong interface. A machine with a single NAT
	// adapter is the one case where attribution is safe.
	NIC       int
	Name      string
	Proto     string
	HostIP    string
	HostPort  int
	GuestIP   string
	GuestPort int
	Raw       string
}

// String renders the rule. The adapter is only named when it is known.
func (f Forward) String() string {
	prefix := ""
	if f.NIC > 0 {
		prefix = fmt.Sprintf("nic%d ", f.NIC)
	}
	return fmt.Sprintf("%s%s:%s:%d -> %s:%d (%s)", prefix, f.Proto, orAny(f.HostIP), f.HostPort, orAny(f.GuestIP), f.GuestPort, f.Name)
}

func orAny(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// Inventory is the result of reading the registered machine list.
type Inventory struct {
	Machines []Machine
	// Problems records machines that could not be discovered or inspected. An
	// inventory with both machines and problems is a partial failure.
	Problems []Problem
}

// Problem records that one machine could not be read.
type Problem struct {
	UUID string
	Name string
	Err  string
}

// Find returns the machine with the given UUID. Comparison ignores case and
// the braces VirtualBox sometimes prints around UUIDs: the same machine must
// not appear as "not found" because one list wrote {2d62…} and the caller
// quoted 2D62….
func (in Inventory) Find(uuid string) (Machine, bool) {
	want := canonicalUUID(uuid)
	for _, m := range in.Machines {
		if canonicalUUID(m.UUID) == want {
			return m, true
		}
	}
	return Machine{}, false
}

// canonicalUUID lower-cases and strips braces from a UUID for comparison.
func canonicalUUID(uuid string) string {
	s := strings.TrimSpace(uuid)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	return strings.ToLower(s)
}

// ErrAmbiguousName reports a VM name that several registered machines share.
// The caller must disambiguate by UUID: silently choosing one machine could act
// on the wrong VM.
type ErrAmbiguousName struct {
	Name     string
	Machines []Machine
}

func (e *ErrAmbiguousName) Error() string {
	names := make([]string, 0, len(e.Machines))
	for _, m := range e.Machines {
		names = append(names, m.UUID)
	}
	return fmt.Sprintf("%d machines share the name %q (UUIDs: %s); name one by UUID",
		len(e.Machines), e.Name, strings.Join(names, ", "))
}

// Resolve turns a user-supplied VM name or UUID into exactly one machine.
// Matching is strict by design: a UUID matches case- and brace-insensitively,
// a name must match exactly. Nothing is fuzzy-matched, and a name shared by
// several machines is reported as ambiguous rather than resolved to one of
// them.
func (in Inventory) Resolve(nameOrUUID string) (Machine, error) {
	s := strings.TrimSpace(nameOrUUID)
	if s == "" {
		return Machine{}, fmt.Errorf("no VM name or UUID given")
	}
	if uuidLike(s) {
		if m, ok := in.Find(s); ok {
			return m, nil
		}
		return Machine{}, fmt.Errorf("no VM with UUID %s is registered", s)
	}
	var matches []Machine
	for _, m := range in.Machines {
		if m.Name == s {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Machine{}, fmt.Errorf("no VM named %q is registered", s)
	default:
		return Machine{}, &ErrAmbiguousName{Name: s, Machines: matches}
	}
}

// ParseError reports output that could not be understood.
type ParseError struct {
	Context string
	Line    string
	Err     error
}

func (e *ParseError) Error() string {
	if e.Line == "" {
		return fmt.Sprintf("%s: %v", e.Context, e.Err)
	}
	return fmt.Sprintf("%s: %v (line %q)", e.Context, e.Err, e.Line)
}

func (e *ParseError) Unwrap() error { return e.Err }

// unquote decodes one VirtualBox quoted value.
//
// VBoxManage escapes a backslash as \\ and a double quote as \". There is no
// quote-doubling convention, so a doubled quote is two escapes. Any other
// escape sequence is preserved verbatim rather than dropped: silently
// mangling a Windows path would be worse than keeping the backslash.
func unquote(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("expected a quoted value, got %q", s)
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			if c == '"' {
				return "", fmt.Errorf("unescaped quote in %q", s)
			}
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(body) {
			return "", fmt.Errorf("trailing escape in %q", s)
		}
		i++
		switch body[i] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			b.WriteByte('\\')
			b.WriteByte(body[i])
		}
	}
	return b.String(), nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func uuidLike(s string) bool {
	s = strings.TrimSpace(s)
	// `list vms` prints UUIDs wrapped in braces, and that is also how an
	// inaccessible machine shows up in --long output, so accept both forms.
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	return uuidRe.MatchString(s)
}

// ParseListVMS parses the plain output of `VBoxManage list vms` or
// `list runningvms`, whose lines look like:  "my vm" {c907c20b-…}
func ParseListVMS(out string) ([]Machine, error) {
	var machines []Machine
	for _, line := range splitLines(out) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, uuid, err := quotedNameUUID(line)
		if err != nil {
			return machines, &ParseError{Context: "machine list", Line: line, Err: err}
		}
		machines = append(machines, Machine{Name: name, UUID: uuid})
	}
	return machines, nil
}

func quotedNameUUID(line string) (name, uuid string, err error) {
	line = strings.TrimSpace(line)
	open := strings.Index(line, "{")
	end := strings.Index(line, "}")
	if open < 0 || end < open {
		return "", "", fmt.Errorf("malformed machine entry")
	}
	name, err = unquote(strings.TrimSpace(line[:open]))
	if err != nil {
		return "", "", err
	}
	uuid = strings.TrimSpace(line[open+1 : end])
	if !uuidLike(uuid) {
		return "", "", fmt.Errorf("malformed UUID %q", uuid)
	}
	return name, uuid, nil
}

// ParseListVMSLong parses `VBoxManage list vms --long`: a sequence of
// "Key:  value" records separated by blank lines. It yields name, UUID, state
// and guest OS.
func ParseListVMSLong(out string) Inventory {
	var inv Inventory
	for _, block := range splitBlocks(out) {
		props := parseKeyValues(block, ": ")
		name := props["Name"]
		uuid := props["UUID"]
		if name == "" && uuid == "" {
			continue
		}
		if uuid == "" {
			inv.Problems = append(inv.Problems, Problem{Name: name, Err: "machine list did not report a UUID"})
			continue
		}
		if name == "" {
			name = uuid
		}
		raw := props["State"]
		m := Machine{
			Name:       name,
			UUID:       uuid,
			RawState:   raw,
			State:      NormalizeState(raw),
			GuestOS:    props["Guest OS"],
			ConfigFile: props["Config file"],
		}
		if since, ok := strings.CutPrefix(StateAnnotation(raw), "since "); ok {
			if ts, err := parseVBoxTime(since); err == nil {
				m.StateChanged = ts
			}
		}
		if uuidLike(name) {
			// VirtualBox prints the UUID in place of the name when the
			// machine's settings file could not be read.
			m.State = StateInfo{State: StateInaccessible, Kind: KindUnavailable, Raw: "inaccessible"}
			m.Notes = append(m.Notes, "VirtualBox lists this machine by UUID; its settings file could not be read")
		}
		inv.Machines = append(inv.Machines, m)
	}
	sortMachines(inv.Machines)
	return inv
}

func sortMachines(ms []Machine) {
	sort.SliceStable(ms, func(i, j int) bool {
		return strings.ToLower(ms[i].Name) < strings.ToLower(ms[j].Name)
	})
}

// parseKeyValues reads "key<sep>value" lines into a map. Repeated keys keep
// the last value.
//
// Lines indented with whitespace are skipped. `list vms --long` nests detail
// sections inside a machine's record — the snapshot list prints lines like
//
//	[* Snapshots:]
//	   Name: clean_base_fullscr (UUID: 95f99e32-…) *
//
// and treating that indented "Name" as a machine property would rename the
// VM after its snapshot. showvminfo uses the same indentation convention for
// continuation lines, so ignoring them is safe across both outputs.
func parseKeyValues(block, sep string) map[string]string {
	props := map[string]string{}
	for _, line := range splitLines(block) {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		idx := strings.Index(line, sep)
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if key == "" {
			continue
		}
		props[key] = strings.TrimSpace(line[idx+len(sep):])
	}
	return props
}

// splitLines splits on CRLF, LF or CR.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

// splitBlocks splits a document into blank-line separated records.
func splitBlocks(s string) []string {
	var (
		blocks []string
		cur    []string
	)
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, l := range splitLines(s) {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return blocks
}

var assignmentRe = regexp.MustCompile(`^(?P<key>"[^"]*"|[^=]+)=(?P<rest>.*)$`)

// ParseShowVMInfo decodes `VBoxManage showvminfo <uuid> --machinereadable`.
//
// Every line is `key=value`, where the key may itself be quoted
// ("SATA-0-0"="D:\\dir\disk.vdi"). A line indented with whitespace continues
// the previous value and is ignored. Network adapters and port-forwarding
// rules are reassembled per adapter slot instead of guessing fields from an
// arbitrary delimiter.
func ParseShowVMInfo(out string) (Machine, error) {
	raw := map[string]string{}
	unparsed := 0
	snapshots := 0

	for _, line := range splitLines(out) {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		m := assignmentRe.FindStringSubmatch(line)
		if m == nil {
			unparsed++
			continue
		}
		key := strings.TrimSpace(m[1])
		if key == "" {
			unparsed++
			continue
		}
		if key[0] == '"' {
			decoded, err := unquote(key)
			if err != nil {
				// An unrecognisable key is skipped without counting the whole
				// response as damaged. The line is not a machine property, so
				// treating it as one would only inflate the note about unparsable
				// output.
				continue
			}
			key = decoded
		}
		rest := m[2]
		value := rest
		if strings.HasPrefix(rest, `"`) {
			decoded, err := unquote(rest)
			if err != nil {
				// Keep the raw text: an unterminated quote is a reason to warn,
				// not to throw the property away.
				unparsed++
			} else {
				value = decoded
			}
		}
		if key == "SnapshotUUID" {
			snapshots++
		}
		raw[key] = value
	}
	if len(raw) == 0 {
		return Machine{}, &ParseError{Context: "showvminfo", Err: fmt.Errorf("no machine properties found")}
	}

	m := Machine{
		Name:       raw["name"],
		UUID:       raw["UUID"],
		RawState:   raw["VMState"],
		MemoryMB:   atoiDefault(raw["memory"]),
		CPUs:       atoiDefault(raw["cpus"]),
		GuestOS:    raw["ostype"],
		ConfigFile: raw["CfgFile"],
		Snapshots:  snapshots,
	}
	if m.UUID == "" {
		return m, &ParseError{Context: "showvminfo", Err: fmt.Errorf("output did not contain a UUID")}
	}
	if ts, err := parseVBoxTime(raw["VMStateChangeTime"]); err == nil {
		m.StateChanged = ts
	}
	m.State = NormalizeState(m.RawState)

	for slot := 1; slot <= 36; slot++ {
		typ, ok := raw["nic"+strconv.Itoa(slot)]
		if !ok {
			continue
		}
		n := NIC{Type: strings.ToLower(strings.TrimSpace(typ)), Slot: slot}
		if n.Type == "" {
			n.Type = "none"
		}
		for _, devKey := range []string{"bridgeadapter", "hostonlyadapter", "intnet", "nat-network", "generic"} {
			if v := raw[devKey+strconv.Itoa(slot)]; v != "" && !strings.EqualFold(v, "none") {
				n.Device = v
				break
			}
		}
		n.MAC = raw["macaddress"+strconv.Itoa(slot)]
		if v, ok := raw["cableconnected"+strconv.Itoa(slot)]; ok {
			n.Attached = truthy(v)
		}
		if n.Type != "none" {
			m.NICs = append(m.NICs, n)
		}
	}
	m.Forwards = parseForwards(raw, m.NICs)

	if v, ok := raw["localhostReachable"]; ok {
		m.HasNATLocalhost = true
		m.NATLocalhost = truthy(v)
	} else {
		// VirtualBox 7 exposes the same setting per adapter as
		// nat-localhostreachableN.
		for _, n := range m.NICs {
			if n.Type != "nat" {
				continue
			}
			if v, ok := raw["nat-localhostreachable"+strconv.Itoa(n.Slot)]; ok {
				m.HasNATLocalhost = true
				m.NATLocalhost = truthy(v)
				break
			}
		}
	}
	if len(m.NICs) == 0 {
		m.Notes = append(m.Notes, "no network adapters reported")
	}
	if unparsed > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf("%d line(s) of VirtualBox output could not be parsed", unparsed))
	}
	return m, nil
}

// parseForwards reads Forwarding(N)="name,proto,hostIP,hostPort,guestIP,guestPort".
//
// The (N) index is a global counter, not an adapter number. When the machine has
// exactly one NAT adapter the rules must belong to it, so that adapter is named;
// with more than one, NIC is left 0 and Forward.String omits it rather than
// guessing.
func parseForwards(raw map[string]string, nics []NIC) []Forward {
	attributedNIC := 0
	var natSlots []int
	for _, n := range nics {
		if n.Type == "nat" {
			natSlots = append(natSlots, n.Slot)
		}
	}
	if len(natSlots) == 1 {
		attributedNIC = natSlots[0]
	}

	var out []Forward
	for i := 0; i < 64; i++ {
		key := "Forwarding(" + strconv.Itoa(i) + ")"
		value, ok := raw[key]
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		if len(parts) < 6 {
			continue
		}
		f := Forward{
			NIC:       attributedNIC,
			Name:      strings.TrimSpace(parts[0]),
			Proto:     strings.ToLower(strings.TrimSpace(parts[1])),
			HostIP:    strings.TrimSpace(parts[2]),
			HostPort:  atoiDefault(parts[3]),
			GuestIP:   strings.TrimSpace(parts[4]),
			GuestPort: atoiDefault(parts[5]),
			Raw:       value,
		}
		if f.Name == "" {
			f.Name = "rule" + strconv.Itoa(i+1)
		}
		if f.HostPort == 0 {
			continue // not a usable rule
		}
		out = append(out, f)
	}
	// Order by host port: the numbering VirtualBox uses carries no meaning for a
	// reader who is looking for "which local port reaches guest ssh".
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].GuestPort < out[j].GuestPort
	})
	return out
}

func parseVBoxTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

func atoiDefault(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// NATForwardFor returns a forwarding rule that maps a local port to a guest
// SSH port, preferring rules on the given port.
func (m Machine) NATForwardFor(guestPort int) (Forward, bool) {
	var best Forward
	found := false
	for _, f := range m.Forwards {
		if f.GuestPort != guestPort || f.Proto != "tcp" {
			continue
		}
		if !found || f.HostPort < best.HostPort {
			best, found = f, true
		}
	}
	return best, found
}
