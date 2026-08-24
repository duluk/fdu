// Command fdu answers one question: where did the disk space go?
//
// It walks a directory tree (every subdirectory, all the way down) and reports
// the directories and files that actually account for the space, instead of
// dumping a line per directory the way du does. Network shares and
// external/removable volumes are skipped by default; flags turn them back on.
//
// Runs on macOS, Linux and Windows.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const version = "1.0.0"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func usage() {
	fmt.Fprintf(os.Stderr, `fdu %s -- find out where the disk space went

usage: fdu [options] [directory]

Scans DIRECTORY (default: the current directory) and every subdirectory
beneath it, then prints the branches of the tree that actually hold the
space, the largest individual files, and anything it had to skip.

  fdu /                 whole boot volume
  fdu /home             everything under /home
  fdu C:\Users          a Windows profile tree
  fdu -remote /mnt/nas  include a network share that would be skipped

By default it ignores network filesystems, external/removable volumes and
virtual filesystems (procfs, tmpfs, squashfs images, ...). It stays inside
ordinary local disks, crossing local mount points as it goes.

options:
`, version)
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr)
}

func main() {
	var (
		cfg      Config
		ropt     ReportOpts
		excludes multiFlag
		jsonOut  bool
		showVer  bool
	)

	flag.IntVar(&cfg.TopN, "top", 20, "list this many of the largest individual files (0 disables)")
	flag.Float64Var(&ropt.MinPercent, "min-percent", 1.0, "hide directories holding less than this percent of the total")
	flag.IntVar(&ropt.MaxDepth, "depth", 0, "maximum directory depth to print (0 = no limit)")
	flag.BoolVar(&ropt.ByExt, "by-ext", false, "also summarise usage by file extension")

	flag.BoolVar(&cfg.IncludeRemote, "remote", false, "include network/remote filesystems (NFS, SMB, sshfs, ...)")
	flag.BoolVar(&cfg.IncludeExternal, "external", false, "include external/removable volumes (USB disks, DVDs, mounted images)")
	flag.BoolVar(&cfg.IncludeCloud, "cloud", false, "walk cloud-storage FUSE mounts, whose contents do not live on this disk")
	flag.BoolVar(&cfg.IncludePseudo, "pseudo", false, "include virtual filesystems (procfs, sysfs, tmpfs, squashfs, ...)")
	flag.BoolVar(&cfg.OneFileSystem, "one-file-system", false, "never cross a mount point at all, not even a local one")

	flag.BoolVar(&cfg.Logical, "logical", false, "report file length instead of space allocated on disk")
	flag.BoolVar(&cfg.CountHardlinks, "count-hardlinks", false, "count hard-linked data once per link (default: charge it to the first link seen)")
	flag.Var(&excludes, "exclude", "skip paths matching this glob; repeatable (e.g. -exclude '*/node_modules')")

	flag.IntVar(&cfg.Jobs, "jobs", runtime.NumCPU()*2, "concurrent directory readers")
	flag.BoolVar(&jsonOut, "json", false, "emit JSON instead of the text report")
	flag.BoolVar(&ropt.SI, "si", false, "use powers of 1000 (kB, MB, GB) rather than 1024 (KiB, MiB, GiB)")
	flag.BoolVar(&cfg.Quiet, "quiet", false, "suppress the progress line")
	flag.BoolVar(&showVer, "version", false, "print version and exit")

	flag.Usage = usage
	flag.Parse()

	if showVer {
		fmt.Printf("fdu %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	start := "."
	if flag.NArg() > 0 {
		start = flag.Arg(0)
	}
	if flag.NArg() > 1 {
		fatal("only one directory can be scanned at a time")
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		fatal("%v", err)
	}
	abs = filepath.Clean(abs)

	fi, err := os.Lstat(abs)
	if err != nil {
		fatal("%v", err)
	}
	if !fi.IsDir() {
		fatal("%s is not a directory", abs)
	}

	cfg.Excludes = excludes
	cfg.ByExt = ropt.ByExt
	if cfg.Jobs < 1 {
		cfg.Jobs = 1
	}
	if cfg.TopN < 0 {
		cfg.TopN = 0
	}
	if jsonOut {
		cfg.Quiet = true
	}

	// The mount table drives every skip decision, so warn loudly if we could
	// not read it rather than silently pretending everything is local.
	mounts, mErr := LoadMounts()
	if mErr != nil && !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "warning: could not read the mount table (%v); treating every filesystem as local\n", mErr)
	}

	// Refuse to quietly scan a share/removable disk the user pointed us at
	// directly -- that is almost always a mistake, but it is theirs to make.
	if k, desc := mounts.Classify(abs); k != KindLocal && k != KindUnknown {
		allowed := (k == KindRemote && cfg.IncludeRemote) ||
			(k == KindRemovable && cfg.IncludeExternal) ||
			(k == KindPseudo && cfg.IncludePseudo) ||
			(k == KindCloud && cfg.IncludeCloud) ||
			k == KindDuplicate
		if !allowed {
			fatal("%s sits on %s, which is skipped by default.\n       Re-run with %s to scan it anyway.", abs, desc, flagFor(k))
		}
	}

	res := Scan(abs, fi, cfg, mounts)

	if jsonOut {
		if err := writeJSON(os.Stdout, res, ropt); err != nil {
			fatal("%v", err)
		}
		return
	}
	writeReport(os.Stdout, res, ropt)
}

func flagFor(k Kind) string {
	switch k {
	case KindRemote:
		return "-remote"
	case KindRemovable:
		return "-external"
	case KindPseudo:
		return "-pseudo"
	case KindCloud:
		return "-cloud"
	}
	return "-remote/-external/-pseudo/-cloud"
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "fdu: "+format+"\n", a...)
	os.Exit(1)
}
