//go:build darwin

package main

import (
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
)

// APFS and HFS+ are case-insensitive as shipped, so mount-point comparisons
// have to be too.
const isCaseInsensitiveFS = true

// classifyNative asks the kernel what filesystem a path is actually on.
//
// This is the authoritative answer and does not depend on matching mount-point
// strings, which is important on macOS: getfsstat reports mount points under
// /System/Volumes/Data, while firmlinks mean the same directory is walked as
// /Users/..., /Applications/... and so on. Matching by path silently misses
// those mounts, and a missed NFS mount gets walked instead of skipped.
func classifyNative(path string) (Mount, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Mount{}, false
	}
	m := Mount{
		Path:   cstr(st.Mntonname[:]),
		FSType: cstr(st.Fstypename[:]),
		Source: cstr(st.Mntfromname[:]),
	}
	m.Kind = classifyDarwin(path, m, st.Flags)
	return m, true
}

// From <sys/mount.h>. Declared here rather than taken from the syscall package
// so the build does not depend on which constants a given Go release exported.
const (
	mntRemovable = 0x00000200
	mntLocal     = 0x00001000
	mntNoWait    = 2
)

// SF_DATALESS from <sys/stat.h>. macOS sets it on files whose contents have
// been evicted to iCloud Drive, and on files provided by a File Provider
// extension (the ~/Library/CloudStorage folders used by OneDrive, Dropbox,
// Google Drive and Box). Materialisation is triggered by reading the data;
// lstat is not reading, so the flag can be inspected for free.
const sfDataless = 0x40000000

// isDataless reports whether a file's contents live in the cloud.
func isDataless(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}

// fileSpace returns on-disk usage and whether the file is a cloud placeholder.
// An evicted file reports st_blocks == 0, so the default mode already tells
// the truth about local disk usage without any special casing; the flag is
// what lets the report say how much is parked online.
func fileSpace(path string, fi fs.FileInfo, logical bool) (int64, bool) {
	return diskSize(path, fi, logical), isDataless(fi)
}

var darwinPseudoFS = map[string]bool{
	"devfs": true, "autofs": true, "fdesc": true, "kernfs": true,
	"procfs": true, "volfs": true,
}

// LoadMounts asks the kernel for the mount table directly. MNT_LOCAL is the
// authoritative answer for "is this someone else's disk", covering every
// network filesystem macOS supports without a hardcoded type list.
func LoadMounts() (*MountTable, error) {
	n, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil {
		return NewMountTable(nil), err
	}
	buf := make([]syscall.Statfs_t, n+8)
	n, err = syscall.Getfsstat(buf, mntNoWait)
	if err != nil {
		return NewMountTable(nil), err
	}
	if n > len(buf) {
		n = len(buf)
	}

	var out []Mount
	for _, sf := range buf[:n] {
		m := Mount{
			Path:   cstr(sf.Mntonname[:]),
			FSType: cstr(sf.Fstypename[:]),
			Source: cstr(sf.Mntfromname[:]),
		}
		if m.Path == "" {
			continue
		}
		m.Kind = classifyDarwin(m.Path, m, sf.Flags)
		out = append(out, m)

		// Also index the firmlinked spelling. A mount reported at
		// /System/Volumes/Data/Users/x/Foo is walked as /Users/x/Foo, and
		// having both means the cheap path lookup usually hits and the mount
		// never has to be touched at all.
		if alias, ok := strings.CutPrefix(m.Path, dataVolume); ok && alias != "" {
			a := m
			a.Path = alias
			out = append(out, a)
		}
	}
	return NewMountTable(out), nil
}

const dataVolume = "/System/Volumes/Data"

// classifyDarwin judges a filesystem. walked is the path actually being
// visited, which is not always the mount point: statfs on the firmlinked
// /Users reports the data volume's mount point, so the duplicate rule below
// has to test the walked path or it would skip the entire home directory.
func classifyDarwin(walked string, m Mount, flags uint32) Kind {
	switch {
	case flags&mntLocal == 0:
		return KindRemote
	case darwinPseudoFS[m.FSType]:
		return KindPseudo

	// The data volume holds /Users, /Applications and friends, but firmlinks
	// also expose all of them directly under "/". Walking both paths would
	// count the same bytes twice, so the firmlinked names win and this mount
	// point is passed over.
	case filepath.Clean(walked) == "/System/Volumes/Data":
		return KindDuplicate

	case flags&mntRemovable != 0:
		return KindRemovable

	// Everything a user mounts by hand -- USB disks, external SSDs, disk
	// images, Time Machine local snapshots -- lands under /Volumes. The boot
	// volume appears there only as a symlink, which is never followed.
	case strings.HasPrefix(m.Path, "/Volumes/"):
		return KindRemovable
	}
	return KindLocal
}

// cstr turns a fixed-size NUL-terminated C string into a Go string. The type
// parameter absorbs the fact that these fields are []int8 in some Go releases
// and []byte in others.
func cstr[T ~int8 | ~uint8](b []T) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
