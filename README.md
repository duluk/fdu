# fdu (Fuller Disk Usage)

Find out where the disk space went, on macOS, Linux and Windows.

`du` tells you about every directory. `fdu` tells you about the handful that
matter: it walks the whole tree below wherever you start it, then prints only
the branches that actually hold the space, the largest individual files, and
anything it deliberately skipped.

Network shares and external/removable volumes are ignored by default, so
pointing it at `/` on a work laptop measures the laptop, not the NAS.

## Build

Pure standard library, no dependencies. Go 1.21+.

```
go build -o fdu .
```

Cross-compiling from any one machine:

```
GOOS=darwin  GOARCH=arm64 go build -o fdu-macos-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o fdu-macos-intel .
GOOS=linux   GOARCH=amd64 go build -o fdu-linux .
GOOS=windows GOARCH=amd64 go build -o fdu.exe .
```

## Use

```
fdu /                     # the whole boot volume
fdu /home                 # everything under /home
fdu ~/Library             # a macOS home directory
fdu C:\Users              # a Windows profile tree
fdu -remote /mnt/nas      # a network share, which is skipped by default
fdu -json / > usage.json  # machine-readable
```

Sample:

```
/usr
5.2 GB in 115,073 files across 12,892 directories   (space allocated on disk, 519ms)

      SIZE   SHARE  DIRECTORY
------------------------------------------------------------
    5.2 GB  100.0%  /usr
    1.7 GB   32.4%    lib/
    886 MB   17.0%      x86_64-linux-gnu/
    328 MB    6.3%      libreoffice/
    272 MB    5.2%      (45 smaller directories)
    1.6 GB   31.5%    local/
    459 MB    8.8%    bin/

Largest files
------------------------------------------------------------
    199 MB    3.8%  bin/pandoc
    144 MB    2.8%  lib/x86_64-linux-gnu/libLLVM.so.20.1
```

Every number is a share of the grand total, not of the parent, so a line four
levels deep can be read on its own.

Anything smaller than `-min-percent` (1% by default) is collapsed into a
`(N smaller directories)` line rather than dropped, so the column still adds
up. Lower it to `0.1` to go deeper, raise it to `5` for a one-screen summary.

## Options

| Flag | Meaning |
|---|---|
| `-min-percent F` | Hide directories below this share of the total. Default 1. |
| `-depth N` | Stop printing below this depth. Default 0 (no limit). |
| `-top N` | How many of the largest individual files to list. Default 20. |
| `-by-ext` | Also break usage down by file extension. |
| `-remote` | Include network filesystems. |
| `-external` | Include external/removable volumes. |
| `-pseudo` | Include virtual filesystems (procfs, tmpfs, squashfs, ...). |
| `-cloud` | Walk cloud-storage FUSE mounts (rclone, gcsfuse, ...), whose contents are not local. |
| `-one-file-system` | Never cross a mount point at all, even a local one. |
| `-logical` | Report file lengths instead of space allocated on disk. |
| `-count-hardlinks` | Charge hard-linked data to every link, not just the first. |
| `-exclude GLOB` | Skip matching paths. Repeatable. |
| `-jobs N` | Concurrent directory readers. Default 2x CPU count. |
| `-json` | JSON instead of the text report. |
| `-si` | Powers of 1000 (kB, MB, GB) instead of 1024 (KiB, MiB, GiB). |
| `-quiet` | No progress line. |

`-exclude` matches against the entry's own name, its full path, or the trailing
path components, so all of these work:

```
-exclude node_modules            # any directory of that name, anywhere
-exclude '*/node_modules'        # same thing, written the other way
-exclude '/home/*/Downloads'     # a specific shape of path
-exclude '*.iso'                 # by extension
```

## What gets skipped, and how it decides

Skipped subtrees are always listed at the end of the report with a reason, so
nothing disappears silently.

Classification asks the kernel, via `statfs(2)`, what a directory is really
sitting on. It does **not** work by matching mount-point strings, because mount
points do not always spell the way they are walked: on macOS `getfsstat`
reports a mount under `/System/Volumes/Data/Users/you/Thing` while firmlinks
mean the same directory is reached as `/Users/you/Thing`. A name-matching
scanner misses that mount entirely and walks into it. If the filesystem turns
out to be one that has to be skipped, that costs a single `statfs` call and
nothing underneath is ever touched.

The mount table is still read at startup and consulted *first*, because a hit
there is free and means a slow or wedged network mount is never touched at all.
It is also the only source that knows whether a local disk is removable, which
`statfs` cannot tell.

**Linux** reads `/proc/self/mountinfo`. Filesystem type decides remote (`nfs`,
`cifs`, `sshfs`, `ceph`, ...) and virtual (`proc`, `sysfs`, `tmpfs`, `cgroup`,
...). Removable is resolved through sysfs: the kernel's `removable` flag plus
USB/MMC/FireWire transport, which is what external SSDs actually report.

`squashfs` and `erofs` count as virtual on purpose. A snap package mounted at
`/snap/foo` is a read-only image whose real cost is the `.snap` file under
`/var/lib/snapd`; counting both would double it. `overlay` is deliberately
*not* virtual, because inside a container it is the disk.

**macOS** calls `getfsstat`. `MNT_LOCAL` is the authoritative answer for
"is this someone else's disk", so every network filesystem is covered without a
hardcoded list. Removable comes from `MNT_REMOVABLE` plus anything under
`/Volumes` — USB disks, external SSDs, mounted `.dmg` files and Time Machine
local snapshots all live there.

`/System/Volumes/Data` is skipped as a duplicate. Firmlinks already expose its
contents as `/Users`, `/Applications` and friends, so walking both paths would
count the same bytes twice. Scanning `/` still covers everything on the data
volume. (You can still point `fdu` at `/System/Volumes/Data` directly.)

**Windows** uses `GetDriveType` on the volume, so remote (`4`), removable
(`2`), optical (`5`) and RAM disks (`6`) are recognised, and UNC paths are
treated as remote. Directory reparse points — junctions, directory symlinks and
volumes mounted into a folder instead of onto a drive letter — are never walked,
since they either leave the volume or lead back into it.

Everywhere: symlinks are counted at their own size and never followed, so
symlink loops cannot hang the scan and linked data is not double-counted.
Filesystems reached twice through different mount points are counted once.

## Cloud storage: OneDrive, iCloud Drive, Google Drive, Dropbox

These folders are measured for **what they occupy on the local disk**, and
nothing is ever downloaded to find out.

- A file that has been downloaded (hydrated, or "always keep on this device")
  is real local storage and is counted normally, in the folder holding it.
- A file that is still only in the cloud contributes zero, because it is not
  taking up your disk.
- `fdu` never opens or reads a file. Every byte comes from directory
  enumeration and `lstat`, neither of which hydrates a placeholder.

So a OneDrive folder with 400 GB online and 3 GB downloaded reports 3 GB, which
is the number that matters when the disk is full. A footnote gives the count of
files skipped as not-downloaded, so a small figure is never mysterious:

```
/Users/jane
48.2 GiB in 412,004 files across 21,880 directories   (space allocated on disk, 6s)
17,000 cloud files are not downloaded to this disk, counted as zero
```

**Windows.** Dehydrated files carry `FILE_ATTRIBUTE_OFFLINE`,
`RECALL_ON_OPEN` or `RECALL_ON_DATA_ACCESS`, which arrive free with the
directory listing. Those files count as zero, and `GetCompressedFileSize` is
deliberately **not** called on them: it opens a handle, and on a file marked
`RECALL_ON_OPEN` that alone is enough to start a download. Hydrated files have
none of those attributes and are measured normally.

Placeholder directories are also reparse points, so junctions and symlinks are
told apart from cloud folders by reparse *tag*, using the documented
`IsReparseTagNameSurrogate` test, rather than by the attribute bit. Tags that
redirect elsewhere (`IO_REPARSE_TAG_SYMLINK`, `IO_REPARSE_TAG_MOUNT_POINT`) are
skipped; tags that are genuinely this directory (`IO_REPARSE_TAG_CLOUD` and its
nine variants, `_ONEDRIVE`, `_PROJFS`, `_DEDUP`, `_WOF`, `_HSM`) are walked.
Checking only the attribute bit, as a naive implementation does, silently
reports the entire OneDrive tree as 0 bytes.

**macOS.** iCloud Drive evictions and the File Provider extensions behind
`~/Library/CloudStorage/` (OneDrive, Dropbox, Google Drive, Box) mark
not-downloaded files with `SF_DATALESS` in `st_flags`. Materialisation is
triggered by reading data, and `lstat` is not reading, so the flag is free to
inspect. Such files also report `st_blocks == 0`, so the local total is right
either way.

**Linux.** Cloud storage arrives as a FUSE mount (`rclone`, `gcsfuse`, `s3fs`,
`onedriver`, `gvfs`, `blobfuse`, ...). Nothing under those mount points lives on
the local disk, so they are skipped by default and listed in the report;
`-cloud` walks them anyway. An *unrecognised* `fuse.*` type is walked rather
than skipped, since missing a genuinely local mount (a mergerfs pool, an
encrypted volume) is the worse mistake.

**Where the local cost actually shows up.** Sync clients keep their caches
outside the synced folder: `~/.cache/rclone`, `%LOCALAPPDATA%\Google\DriveFS`,
`~/Library/Caches`. Those are ordinary local directories, counted where they
sit, so scanning a home directory or `C:\` finds them on its own. Google Drive
for desktop on Windows also presents a virtual drive letter (often `G:`) whose
contents are not local; scan `C:\` rather than `G:` and the cache is still
accounted for.

If you would rather not touch a cloud folder at all:

```
fdu -exclude OneDrive -exclude 'Mobile Documents' ~
```

Two honest caveats. Listing a cloud folder that has never been enumerated can
prompt the sync client to fetch **metadata** for its children (kilobytes, not
your files; Explorer and Finder do the same). And a partially hydrated file
counts as zero rather than the fraction actually on disk, which can slightly
undercount a folder mid-download.

## Accuracy

`fdu` reports **space allocated on disk** by default, not file length. That
is `st_blocks * 512` on Unix and `GetCompressedFileSize` on Windows, so sparse
files, compressed files, APFS clones and NTFS compression come out right, and
a million 100-byte files correctly show up as several gigabytes. Directory
entries themselves are counted too, which is real space once you have hundreds
of thousands of them. `-logical` switches to plain file lengths.

Verified byte-for-byte against GNU `du` in both modes:

| Tree | `fdu` | `du -sk` | `fdu -logical` | `du -sb` |
|---|---|---|---|---|
| `/usr/share` | 1218514944 | 1218514944 | 1095145863 | 1095145863 |
| `/usr/lib` | 1688162304 | 1688162304 | 1660019835 | 1660019835 |
| `/usr/local` | 1638567936 | 1638567936 | 1532219560 | 1532219560 |

Hard links are counted once, matching `du`; `-count-hardlinks` matches `du -l`.

Unreadable directories are counted and listed rather than crashing the scan,
with a note that their contents are missing from the totals. Scanning system
directories generally wants `sudo` on Unix or an elevated prompt on Windows;
without it the numbers are a lower bound.

## Performance and memory

Directories are read concurrently, bounded by `-jobs`. On a test tree of
190,000 files the full scan takes well under a second.

Only directories become nodes in memory; files are folded into their parent's
total as they are seen, with a fixed-size heap holding the current top N. A
full-disk scan of millions of files stays in the tens of megabytes rather than
running the machine out of RAM.

## Layout

```
main.go        flags, start-path validation
walk.go        concurrent walker, tree, hardlink and mount dedup
report.go      text and JSON output
mounts.go      cross-platform mount table
fs_unix.go     shared Linux/macOS stat helpers
fs_linux.go    /proc/self/mountinfo, sysfs removable detection
fs_darwin.go   getfsstat, MNT_LOCAL/MNT_REMOVABLE, firmlinks
fs_windows.go  GetDriveType, reparse tags, cloud placeholders
fdu_test.go    tests for classification, cloud accounting, excludes
```

## Tests

```
go test ./...
```
