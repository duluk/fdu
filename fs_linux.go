//go:build linux

package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const isCaseInsensitiveFS = false

// classifyNative is only used on Windows, where drive letters carry the
// answer directly.
func classifyNative(string) (Kind, string, bool) { return KindUnknown, "", false }

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
