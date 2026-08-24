//go:build linux

package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const isCaseInsensitiveFS = false

// Filesystem magic numbers from <linux/magic.h>. statfs(2) reports these for
// whatever filesystem a path is really on, which is how a mount gets caught
// even when its mount point never matched by name.
var remoteMagic = map[uint32]string{
	0x6969: "nfs", 0xFF534D42: "cifs", 0xFE534D42: "smb2", 0x517B: "smbfs",
	0x01021997: "9p", 0x00C36400: "ceph", 0x5346414F: "afs", 0x6B414653: "afs",
	0x73757245: "coda", 0x564C: "ncpfs", 0x0BD00BD0: "lustre",
	0x01161970: "gfs2", 0x7461636F: "ocfs2",
}

var pseudoMagic = map[uint32]string{
	0x9FA0: "proc", 0x62656572: "sysfs", 0x01021994: "tmpfs", 0x858458F6: "ramfs",
	0x1CD1: "devpts", 0x27E0EB: "cgroup", 0x63677270: "cgroup2",
	0x73717368: "squashfs", 0xE0F5E1E2: "erofs", 0x64626720: "debugfs",
	0x74726163: "tracefs", 0x73636673: "securityfs", 0xCAFE4A11: "bpf",
	0x19800202: "mqueue", 0x0187: "autofs", 0xDE5E81E4: "efivarfs",
	0x6E736673: "nsfs", 0x958458F6: "hugetlbfs", 0x62656570: "configfs",
	0x6165676C: "pstore", 0x42494E4D: "binfmt_misc", 0xF97CFF8C: "selinuxfs",
	0x67596969: "rpc_pipefs", 0x65735543: "fusectl",
}

// classifyNative asks the kernel what a path is really mounted on. Only remote
// and virtual filesystems are answered here: anything else falls through to
// the mount table, which is the only place that knows whether a local disk is
// removable (statfs cannot tell an internal SSD from a USB stick).
func classifyNative(path string) (Mount, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Mount{}, false
	}
	magic := uint32(st.Type)
	if name, ok := remoteMagic[magic]; ok {
		return Mount{Path: path, FSType: name, Kind: KindRemote}, true
	}
	if name, ok := pseudoMagic[magic]; ok {
		return Mount{Path: path, FSType: name, Kind: KindPseudo}, true
	}
	return Mount{}, false
}

// Filesystems whose contents live on another machine.
var remoteFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb3": true,
	"afs": true, "ncpfs": true, "coda": true, "9p": true, "ceph": true,
	"glusterfs": true, "lustre": true, "beegfs": true, "gpfs": true,
	"davfs": true, "ftpfs": true, "curlftpfs": true, "afpfs": true,
	"fuse.sshfs": true, "fuse.glusterfs": true, "fuse.davfs": true,
	"fuse.rozofs": true, "fuse.cephfs": true, "fuse.curlftpfs": true,
}

// fileSpace on Linux has no placeholder concept to worry about: cloud storage
// arrives as a FUSE mount, which is classified and skipped at the mount level
// rather than file by file.
func fileSpace(path string, fi fs.FileInfo, logical bool) (int64, bool) {
	return diskSize(path, fi, logical), false
}

// Cloud storage exposed through FUSE. Walking these means a metadata round
// trip per directory and, depending on the driver's caching, possibly pulling
// file contents down, so they stay out of the scan unless asked for.
var cloudFSTypes = map[string]bool{
	"fuse.rclone": true, "fuse.gcsfuse": true, "fuse.s3fs": true,
	"fuse.blobfuse": true, "fuse.blobfuse2": true, "fuse.onedriver": true,
	"fuse.drivefs": true, "fuse.google-drive-ocamlfuse": true,
	"fuse.mega": true, "fuse.megasync": true, "fuse.dropbox": true,
	"fuse.insync": true, "fuse.pcloudfs": true, "fuse.storj": true,
	"fuse.cryptomator": true, "fuse.expandrive": true, "fuse.odrive": true,
	"fuse.gvfsd-fuse": true, "fuse.nextcloud": true, "fuse.owncloud": true,
	"fuse.seafile": true, "fuse.box": true, "fuse.jottad": true,
	"fuse.s3ql": true, "fuse.yandex-disk": true,
}

// FUSE drivers that really are a local disk or a local file. An unrecognised
// fuse type is walked rather than skipped: missing genuinely local storage
// (a mergerfs pool, an encrypted volume) is the worse mistake here, and
// walking it costs metadata lookups, never a download, because file contents
// are never read.
var localFUSETypes = map[string]bool{
	"fuseblk": true, "fuse.ntfs-3g": true, "fuse.ntfs": true, "fuse.lowntfs-3g": true,
	"fuse.exfat": true, "fuse.encfs": true, "fuse.gocryptfs": true, "fuse.cryfs": true,
	"fuse.bindfs": true, "fuse.mergerfs": true, "fuse.archivemount": true,
	"fuse.squashfuse": true, "fuse.fuseiso": true, "fuse.apfs-fuse": true,
	"fuse.hfsfuse": true, "fuse.lklfuse": true, "fuse.veracrypt": true,
	"fuse.jmtpfs": true, "fuse.mtpfs": true, "fuse.ifuse": true, "fuse.vmware-vmblock": true,
}

// Filesystems that do not consume disk space where they are mounted. squashfs
// and erofs are read-only images (snap packages, for instance) whose real cost
// is the image file elsewhere on disk, so counting their contents would double
// up. overlay is deliberately absent: inside a container it *is* the disk.
var pseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "ramfs": true, "cgroup": true, "cgroup2": true,
	"securityfs": true, "debugfs": true, "tracefs": true, "pstore": true,
	"bpf": true, "configfs": true, "fusectl": true, "hugetlbfs": true,
	"mqueue": true, "autofs": true, "binfmt_misc": true, "efivarfs": true,
	"nsfs": true, "selinuxfs": true, "rpc_pipefs": true, "squashfs": true,
	"erofs": true, "fuse.portal": true, "fuse.snapfuse": true,
}

// LoadMounts reads /proc/self/mountinfo, which (unlike /proc/mounts) reports
// the device numbers and lets us tell bind mounts apart.
func LoadMounts() (*MountTable, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return NewMountTable(nil), err
	}
	defer f.Close()

	var out []Mount
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		m, ok := parseMountinfoLine(sc.Text())
		if ok {
			out = append(out, m)
		}
	}
	return NewMountTable(out), sc.Err()
}

// mountinfo lines look like:
//
//	36 35 98:0 /root /mnt rw,noatime shared:1 - ext4 /dev/sda1 rw
//
// with a variable number of optional fields before the "-" separator.
func parseMountinfoLine(line string) (Mount, bool) {
	dash := strings.Index(line, " - ")
	if dash < 0 {
		return Mount{}, false
	}
	left := strings.Fields(line[:dash])
	right := strings.Fields(line[dash+3:])
	if len(left) < 5 || len(right) < 2 {
		return Mount{}, false
	}

	m := Mount{
		Path:   unescapeOctal(left[4]),
		FSType: right[0],
		Source: unescapeOctal(right[1]),
	}

	switch {
	case remoteFSTypes[m.FSType] || strings.HasPrefix(m.Source, "//"):
		m.Kind = KindRemote
	case cloudFSTypes[m.FSType]:
		m.Kind = KindCloud
	case pseudoFSTypes[m.FSType]:
		m.Kind = KindPseudo
	case isRemovableDev(left[2]):
		m.Kind = KindRemovable
	default:
		m.Kind = KindLocal
	}
	return m, true
}

// isRemovableDev asks sysfs whether a "major:minor" belongs to a hot-pluggable
// device. USB and MMC transports count as removable even when the kernel's own
// removable flag is 0, which is what external SSDs typically report.
func isRemovableDev(majmin string) bool {
	maj, _, ok := strings.Cut(majmin, ":")
	if !ok {
		return false
	}
	if n, err := strconv.Atoi(maj); err == nil && n == 0 {
		return false // virtual device, no backing block device
	}

	base := filepath.Join("/sys/dev/block", majmin)
	target, err := os.Readlink(base)
	if err != nil {
		return false
	}
	if strings.Contains(target, "/usb") || strings.Contains(target, "/mmc") ||
		strings.Contains(target, "/firewire") || strings.Contains(target, "/ieee1394") {
		return true
	}
	// Whole disks expose "removable"; partitions inherit it from the parent.
	for _, p := range []string{base + "/removable", base + "/../removable"} {
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == "1" {
			return true
		}
	}
	return false
}

// unescapeOctal reverses the \040-style escaping the kernel applies to mount
// paths containing spaces, tabs, newlines or backslashes.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
