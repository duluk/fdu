//go:build windows

package main

import (
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const isCaseInsensitiveFS = true

// Windows has no stable device/inode pair available without opening every
// file, so the mount-boundary and hard-link logic is driven by reparse points
// and drive letters instead.
type inodeKey struct{ Dev, Ino uint64 }

func deviceOf(fs.FileInfo) (uint64, bool)          { return 0, false }
func inodeOf(fs.FileInfo) (inodeKey, uint64, bool) { return inodeKey{}, 0, false }

// Cloud-placeholder attributes. OneDrive Files On-Demand, Google Drive for
// desktop, Dropbox Smart Sync and every other Cloud Files API provider mark
// dehydrated files with these, and the sync engine hydrates on data access.
const (
	fileAttributeOffline            = 0x00001000
	fileAttributeRecallOnOpen       = 0x00040000
	fileAttributeRecallOnDataAccess = 0x00400000

	cloudAttributes = fileAttributeOffline | fileAttributeRecallOnOpen | fileAttributeRecallOnDataAccess
)

func attrsOf(fi fs.FileInfo) (uint32, bool) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, false
	}
	return d.FileAttributes, true
}

// isPlaceholder reports whether the file's data lives in the cloud rather than
// on this disk. Nothing here opens the file: the attributes come from the
// directory enumeration that already happened.
func isPlaceholder(fi fs.FileInfo) bool {
	a, ok := attrsOf(fi)
	return ok && a&cloudAttributes != 0
}

// diskSize measures a directory entry. Directories have no meaningful size on
// NTFS, so this stays cheap.
func diskSize(path string, fi fs.FileInfo, logical bool) int64 {
	n, _ := fileSpace(path, fi, logical)
	return n
}

// fileSpace returns the space a file occupies on this volume, and whether it
// is a cloud placeholder.
//
// A dehydrated file occupies (near enough) nothing locally, so it contributes
// zero -- which is the honest answer to "what is filling my disk". Critically,
// GetCompressedFileSize is *not* called on placeholders: it opens a handle,
// and for a file marked RECALL_ON_OPEN that alone would make the sync engine
// download it. Skipping the call is what keeps a scan from pulling a terabyte
// of OneDrive onto a laptop.
func fileSpace(path string, fi fs.FileInfo, logical bool) (int64, bool) {
	if isPlaceholder(fi) {
		if logical {
			return fi.Size(), true
		}
		return 0, true
	}
	if logical {
		return fi.Size(), false
	}
	if n, ok := compressedSize(path); ok {
		return n, false
	}
	return fi.Size(), false
}

// Reparse tags. IsReparseTagNameSurrogate is the documented test for "this
// entry stands in for something somewhere else": symlinks and junctions have
// it set, while cloud placeholders, deduplication stubs and WIM-backed files
// do not, because those really are the file or directory in question.
const (
	tagSymlink    = 0xA000000C
	tagMountPoint = 0xA0000003
	tagCloudBase  = 0x9000001A // IO_REPARSE_TAG_CLOUD, plus _1.._9 variants
	tagCloudMask  = 0xFFFF0FFF
)

func isNameSurrogate(tag uint32) bool { return tag&0x20000000 != 0 }
func isCloudTag(tag uint32) bool      { return tag&tagCloudMask == tagCloudBase&tagCloudMask }

// reparseTag reads an entry's reparse tag with FindFirstFile, which returns it
// in dwReserved0 without opening the file. Only called for the rare entry that
// actually has the reparse attribute set.
func reparseTag(path string) (uint32, bool) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return 0, false
	}
	var fd syscall.Win32finddata
	h, err := syscall.FindFirstFile(p, &fd)
	if err != nil {
		return 0, false
	}
	syscall.FindClose(h)
	if fd.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, false
	}
	return fd.Reserved0, true
}

// skipDirNative decides whether a subdirectory should be walked.
func skipDirNative(path string, fi fs.FileInfo) (string, bool) {
	a, ok := attrsOf(fi)
	if !ok || a&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return "", false
	}
	tag, ok := reparseTag(path)
	if !ok {
		return "", false
	}
	switch {
	case tag == tagSymlink:
		return "directory symlink", true
	case tag == tagMountPoint:
		return "junction or volume mount point", true
	case isNameSurrogate(tag):
		return fmt.Sprintf("reparse point redirecting elsewhere (tag 0x%08X)", tag), true
	}
	// Cloud placeholder, dedup stub, WIM-backed: a real directory. Walk it.
	// Its file contents are measured from enumeration data alone, so nothing
	// gets downloaded.
	return "", false
}

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGetCompressedFileSizeW  = kernel32.NewProc("GetCompressedFileSizeW")
	procGetDriveTypeW           = kernel32.NewProc("GetDriveTypeW")
	procGetLogicalDriveStringsW = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetVolumePathNameW      = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeInformationW   = kernel32.NewProc("GetVolumeInformationW")
)

const (
	driveUnknown   = 0
	driveNoRootDir = 1
	driveRemovable = 2
	driveFixed     = 3
	driveRemote    = 4
	driveCDROM     = 5
	driveRAMDisk   = 6

	invalidFileSize = 0xFFFFFFFF
)

// compressedSize must never be called on a cloud placeholder; see fileSpace.
func compressedSize(path string) (int64, bool) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return 0, false
	}
	var high uint32
	r, _, errno := procGetCompressedFileSizeW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&high)),
	)
	runtime.KeepAlive(p)
	low := uint32(r)
	if low == invalidFileSize && errno != syscall.Errno(0) {
		return 0, false
	}
	return int64(high)<<32 | int64(low), true
}

// longPath adds the \\?\ prefix so paths beyond MAX_PATH still work when we
// call the Win32 API directly (the os package already does this internally).
func longPath(p string) string {
	if len(p) < 240 || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

func driveType(root string) uint32 {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return driveUnknown
	}
	r, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	return uint32(r)
}

func kindOfDrive(t uint32) Kind {
	switch t {
	case driveFixed:
		return KindLocal
	case driveRemote:
		return KindRemote
	case driveRemovable, driveCDROM:
		return KindRemovable
	case driveRAMDisk:
		return KindPseudo
	}
	return KindUnknown
}

func driveDesc(t uint32) string {
	switch t {
	case driveFixed:
		return "a fixed local disk"
	case driveRemote:
		return "a network drive"
	case driveRemovable:
		return "a removable drive"
	case driveCDROM:
		return "an optical drive"
	case driveRAMDisk:
		return "a RAM disk"
	case driveNoRootDir:
		return "an unmounted path"
	}
	return "a drive of unknown type"
}

// fsName returns the filesystem name ("NTFS", "exFAT", ...) for a volume root.
func fsName(root string) string {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 64)
	r, _, _ := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(p)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	runtime.KeepAlive(p)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// volumeRoot maps any path to the volume it lives on, e.g. C:\Users\x -> C:\
// and \\server\share\dir -> \\server\share\.
func volumeRoot(path string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, syscall.MAX_PATH+1)
	r, _, _ := procGetVolumePathNameW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	runtime.KeepAlive(p)
	if r == 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

// classifyNative answers directly from the drive letter, which also covers UNC
// paths that never appear in the logical-drive list.
func classifyNative(path string) (Mount, bool) {
	if strings.HasPrefix(path, `\\`) && !strings.HasPrefix(path, `\\?\`) {
		return Mount{Path: path, FSType: "UNC", Kind: KindRemote}, true
	}
	root, ok := volumeRoot(path)
	if !ok {
		return Mount{}, false
	}
	t := driveType(root)
	if t == driveUnknown {
		return Mount{}, false
	}
	return Mount{
		Path:   root,
		FSType: fsName(root),
		Source: strings.TrimSuffix(root, `\`),
		Kind:   kindOfDrive(t),
	}, true
}

// LoadMounts enumerates the drive letters. Volumes mounted into a folder are
// not listed here, but they are reparse points and get skipped on sight.
func LoadMounts() (*MountTable, error) {
	buf := make([]uint16, 512)
	n, _, errno := procGetLogicalDriveStringsW.Call(
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if n == 0 {
		return NewMountTable(nil), fmt.Errorf("GetLogicalDriveStrings: %v", errno)
	}
	if int(n) > len(buf) {
		n = uintptr(len(buf))
	}

	var out []Mount
	for _, root := range splitNulUTF16(buf[:n]) {
		if root == "" {
			continue
		}
		t := driveType(root)
		out = append(out, Mount{
			Path:   root,
			FSType: fsName(root),
			Source: strings.TrimSuffix(root, `\`),
			Kind:   kindOfDrive(t),
		})
	}
	return NewMountTable(out), nil
}

func splitNulUTF16(b []uint16) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, syscall.UTF16ToString(b[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
