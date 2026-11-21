package vbox

import (
	"fmt"
	"sort"
	"strings"
)

// Snapshot is one node of a machine's snapshot tree, as reported by
// `VBoxManage snapshot <uuid> list --machinereadable`. Identity is the
// snapshot UUID: names may repeat at different points of the tree, so every
// destructive operation addresses snapshots by UUID.
type Snapshot struct {
	Name        string
	UUID        string
	Description string
	// Node is VirtualBox's hierarchical key suffix for this snapshot
	// ("", "-1", "-1-2", …), the order it printed the tree in.
	Node string
	// Depth is the nesting level: 0 for the root, +1 per parent.
	Depth int
	// Current marks the snapshot the machine's current state derives from.
	// The machine's working state — anything done since — is not part of any
	// snapshot, so "current" names a point in the tree, not the live disk.
	Current bool
}

// ShortID is the leading segment of the UUID, enough to read a row when a
// description crowds the line. It is display advice only: every operation
// still uses the full UUID.
func (s Snapshot) ShortID() string {
	if len(s.UUID) >= 8 {
		return s.UUID[:8]
	}
	return s.UUID
}

// SnapshotTree is a machine's snapshots in tree order (depth-first, the
// order VirtualBox prints them).
type SnapshotTree struct {
	Snapshots   []Snapshot
	CurrentUUID string
}

// Empty reports whether the machine has no snapshots at all.
func (t SnapshotTree) Empty() bool { return len(t.Snapshots) == 0 }

// Find returns the snapshot with a UUID, case- and brace-insensitively.
func (t SnapshotTree) Find(uuid string) (Snapshot, bool) {
	want := canonicalUUID(uuid)
	for _, s := range t.Snapshots {
		if canonicalUUID(s.UUID) == want {
			return s, true
		}
	}
	return Snapshot{}, false
}

// ErrAmbiguousSnapshotName reports a snapshot name shared by several nodes
// of the tree (VirtualBox allows duplicates). The caller must disambiguate
// by snapshot UUID.
type ErrAmbiguousSnapshotName struct {
	Name  string
	Nodes []Snapshot
}

func (e *ErrAmbiguousSnapshotName) Error() string {
	ids := make([]string, 0, len(e.Nodes))
	for _, s := range e.Nodes {
		ids = append(ids, s.UUID)
	}
	return fmt.Sprintf("%d snapshots share the name %q (UUIDs: %s); name one by snapshot UUID",
		len(e.Nodes), e.Name, strings.Join(ids, ", "))
}

// Resolve turns a snapshot name or UUID into exactly one node. A UUID must
// exist; a name must be unique in the tree. Nothing is fuzzy-matched: the
// wrong target here would restore the wrong point in time.
func (t SnapshotTree) Resolve(nameOrUUID string) (Snapshot, error) {
	s := strings.TrimSpace(nameOrUUID)
	if s == "" {
		return Snapshot{}, fmt.Errorf("no snapshot name or UUID given")
	}
	if uuidLike(s) {
		if snap, ok := t.Find(s); ok {
			return snap, nil
		}
		return Snapshot{}, fmt.Errorf("no snapshot with UUID %s on this machine", s)
	}
	var matches []Snapshot
	for _, snap := range t.Snapshots {
		if snap.Name == s {
			matches = append(matches, snap)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Snapshot{}, fmt.Errorf("no snapshot named %q on this machine", s)
	default:
		return Snapshot{}, &ErrAmbiguousSnapshotName{Name: s, Nodes: matches}
	}
}

// noSnapshotsPhrase is the sentence VirtualBox prints (and exits non-zero
// with) when a machine has never been snapshotted. It is an empty result,
// not a failure worth alarming the user about.
const noSnapshotsPhrase = "does not have any snapshots"

// ErrNoSnapshots reports that message so callers can treat it as the empty
// tree it describes.
type ErrNoSnapshots struct{ Msg string }

func (e *ErrNoSnapshots) Error() string { return e.Msg }

// ParseSnapshotList reads the --machinereadable output of
// `VBoxManage snapshot <uuid> list`:
//
//	SnapshotName="base"
//	SnapshotUUID="…"
//	SnapshotDescription="…"        (optional)
//	CurrentSnapshotName="…"        (present when this node is current)
//	CurrentSnapshotUUID="…"
//	CurrentSnapshotNode="SnapshotName-1-2"
//	SnapshotName-1="child"         (branch-suffixed keys for children)
//
// Keys carry the node path as a -N suffix; the current node is taken from
// CurrentSnapshotNode, falling back to matching CurrentSnapshotUUID.
func ParseSnapshotList(out string) (SnapshotTree, error) {
	if strings.Contains(strings.ToLower(out), noSnapshotsPhrase) {
		return SnapshotTree{}, nil
	}
	// Reuse the generic key=value reader; only the Snapshot*/Current* keys
	// concern us, and quoted values use the same escaping convention.
	raw := map[string]string{}
	for _, line := range splitLines(out) {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		m := assignmentRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := strings.TrimSpace(m[1])
		value := m[2]
		if strings.HasPrefix(value, `"`) {
			if decoded, err := unquote(value); err == nil {
				value = decoded
			}
		}
		raw[key] = value
	}

	type node struct {
		name, uuid, desc string
		depth            int
	}
	nodes := map[string]*node{}
	order := []string{}
	for key := range raw {
		suffix, ok := strings.CutPrefix(key, "SnapshotName")
		if !ok {
			continue
		}
		if suffix != "" && !snapshotNodeSuffix(suffix) {
			continue
		}
		nodes[suffix] = &node{name: raw[key], depth: strings.Count(suffix, "-")}
		order = append(order, suffix)
	}
	if len(nodes) == 0 {
		if len(raw) == 0 {
			return SnapshotTree{}, &ParseError{Context: "snapshot list",
				Err: fmt.Errorf("no snapshot entries found")}
		}
		return SnapshotTree{}, nil
	}
	for suffix, n := range nodes {
		n.uuid = raw["SnapshotUUID"+suffix]
		n.desc = raw["SnapshotDescription"+suffix]
	}
	// Depth-first order by hierarchical key ("", "-1", "-1-1", "-2", …) so
	// the tree reads top-down regardless of source ordering.
	sort.SliceStable(order, func(i, j int) bool { return nodeLess(order[i], order[j]) })

	currentUUID := canonicalUUID(raw["CurrentSnapshotUUID"])
	currentSuffix := ""
	if nodeKey, ok := snapshotNodeFromKeyName(raw["CurrentSnapshotNode"]); ok {
		if _, exists := nodes[nodeKey]; exists {
			currentSuffix = nodeKey
		}
	}

	var tree SnapshotTree
	for _, suffix := range order {
		n := nodes[suffix]
		if n.name == "" || n.uuid == "" {
			return SnapshotTree{}, &ParseError{Context: "snapshot list",
				Err: fmt.Errorf("snapshot node %q lacks a name or UUID", suffix)}
		}
		isCurrent := currentSuffix == suffix || (currentSuffix == "" && canonicalUUID(n.uuid) == currentUUID)
		if isCurrent {
			tree.CurrentUUID = canonicalUUID(n.uuid)
		}
		tree.Snapshots = append(tree.Snapshots, Snapshot{
			Name: n.name, UUID: n.uuid, Description: n.desc,
			Node: suffix, Depth: n.depth, Current: isCurrent,
		})
	}
	return tree, nil
}

// snapshotNodeSuffix accepts "" plus one or more numeric branches ("-1", "-1-2").
func snapshotNodeSuffix(s string) bool {
	for _, part := range strings.Split(strings.TrimPrefix(s, "-"), "-") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return strings.HasPrefix(s, "-")
}

// snapshotNodeFromKeyName maps "SnapshotName-1-2" to its node suffix "-1-2";
// the root's key "SnapshotName" maps to "".
func snapshotNodeFromKeyName(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "SnapshotName" {
		return "", true
	}
	if base, suffix, found := strings.Cut(key, "SnapshotName"); found && base == "" && snapshotNodeSuffix(suffix) {
		return suffix, true
	}
	return "", false
}

// nodeLess orders node keys breadth-consistent with VirtualBox's printing:
// shorter paths first, same-depth siblings by numeric segment.
func nodeLess(a, b string) bool {
	as, bs := strings.TrimPrefix(a, "-"), strings.TrimPrefix(b, "-")
	if as == "" {
		return bs != ""
	}
	if bs == "" {
		return false
	}
	ap, bp := strings.Split(as, "-"), strings.Split(bs, "-")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		an, bn := atoiDefault(ap[i]), atoiDefault(bp[i])
		if an != bn {
			return an < bn
		}
	}
	return len(ap) < len(bp)
}
