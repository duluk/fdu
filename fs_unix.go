//go:build darwin || linux

package main

import (
	"io/fs"
	"syscall"
)

// inodeKey identifies a specific file on a specific filesystem, which is how
// hard links and repeated mounts of the same tree get spotted.
type inodeKey struct{ Dev, Ino uint64 }

func statOf(fi fs.FileInfo) (*syscall.Stat_t, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return st, ok
}

func deviceOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := statOf(fi)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

func inodeOf(fi fs.FileInfo) (inodeKey, uint64, bool) {
	st, ok := statOf(fi)
	if !ok {
		return inodeKey{}, 0, false
	}
	return inodeKey{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, uint64(st.Nlink), true
}

// diskSize reports the space a file really occupies. st_blocks is always in
// 512-byte units regardless of the filesystem's block size, and it is what
// makes sparse files, compressed files and APFS clones come out right.
func diskSize(_ string, fi fs.FileInfo, logical bool) int64 {
	if logical {
		return fi.Size()
	}
	if st, ok := statOf(fi); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}

// skipDirNative is a Windows concept (reparse points); on Unix, mount
// boundaries and unfollowed symlinks already cover the same ground.
func skipDirNative(string, fs.FileInfo) (string, bool) { return "", false }
