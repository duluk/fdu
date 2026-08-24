package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Kind is how a mounted filesystem should be treated by the scanner.
type Kind int

const (
	KindLocal     Kind = iota // an ordinary internal disk: descend into it
	KindRemote                // NFS/SMB/sshfs/...: someone else's disk
	KindRemovable             // USB stick, external SSD, optical disc, mounted image
	KindPseudo                // procfs, sysfs, tmpfs, squashfs images: not really disk
	KindCloud                 // cloud storage: the bytes may live online, not here
	KindDuplicate             // the same bytes reachable through another path already counted
	KindUnknown               // could not tell; treated as local
)

func (k Kind) String() string {
	switch k {
	case KindLocal:
		return "local"
	case KindRemote:
		return "remote"
	case KindRemovable:
		return "removable"
	case KindPseudo:
		return "virtual"
	case KindCloud:
		return "cloud"
	case KindDuplicate:
		return "duplicate"
	}
	return "unknown"
}

// Mount is one entry from the system's mount table.
type Mount struct {
	Path   string // mount point, cleaned
	FSType string // "apfs", "ext4", "nfs4", "NTFS", ...
	Source string // device or remote share backing it
	Kind   Kind
}

// Describe renders a mount for a human, e.g. `a remote filesystem (nfs4 from
// fileserver:/vol0)`.
func (m Mount) Describe() string {
	var b strings.Builder
	switch m.Kind {
	case KindRemote:
		b.WriteString("a network filesystem")
	case KindRemovable:
		b.WriteString("an external or removable volume")
	case KindPseudo:
		b.WriteString("a virtual filesystem")
	case KindCloud:
		b.WriteString("cloud storage whose contents may not be on this disk")
	case KindDuplicate:
		b.WriteString("a second view of data counted elsewhere")
	default:
		b.WriteString("a local filesystem")
	}
	if m.FSType != "" {
		b.WriteString(" (" + m.FSType)
		if m.Source != "" {
			b.WriteString(" on " + m.Source)
		}
		b.WriteString(")")
	}
	return b.String()
}

// MountTable is the set of mounts visible when the scan started.
type MountTable struct {
	byPath map[string]Mount
	byLen  []Mount // longest mount point first
}

// NewMountTable indexes mounts for exact and longest-prefix lookups.
func NewMountTable(ms []Mount) *MountTable {
	t := &MountTable{byPath: make(map[string]Mount, len(ms))}
	for _, m := range ms {
		m.Path = normPath(m.Path)
		// Later duplicates of the same mount point shadow earlier ones,
		// which matches what the kernel shows through that path.
		t.byPath[m.Path] = m
	}
	for _, m := range t.byPath {
		t.byLen = append(t.byLen, m)
	}
	sort.Slice(t.byLen, func(i, j int) bool {
		return len(t.byLen[i].Path) > len(t.byLen[j].Path)
	})
	return t
}

// At returns the mount whose mount point is exactly this path.
func (t *MountTable) At(path string) (Mount, bool) {
	if t == nil {
		return Mount{}, false
	}
	m, ok := t.byPath[normPath(path)]
	return m, ok
}

// Containing returns the mount a path lives on (longest matching mount point).
func (t *MountTable) Containing(path string) (Mount, bool) {
	if t == nil {
		return Mount{}, false
	}
	p := normPath(path)
	for _, m := range t.byLen {
		if p == m.Path || strings.HasPrefix(p, withSep(m.Path)) {
			return m, true
		}
	}
	return Mount{}, false
}

// Classify reports what sort of filesystem a path lives on.
func (t *MountTable) Classify(path string) (Kind, string) {
	if k, desc, ok := classifyNative(path); ok {
		return k, desc
	}
	if m, ok := t.Containing(path); ok {
		return m.Kind, m.Describe()
	}
	return KindUnknown, "an unrecognised filesystem"
}

// Mounts returns every known mount, ordered by mount point.
func (t *MountTable) Mounts() []Mount {
	if t == nil {
		return nil
	}
	out := append([]Mount(nil), t.byLen...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func normPath(p string) string {
	if p == "" {
		return p
	}
	p = filepath.Clean(p)
	if isCaseInsensitiveFS {
		p = strings.ToLower(p)
	}
	return p
}

func withSep(p string) string {
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return p
	}
	return p + string(filepath.Separator)
}
