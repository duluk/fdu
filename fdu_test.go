package main

import (
	"bytes"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// A directory tree standing in for a Windows profile with OneDrive Files
// On-Demand: some files hydrated onto the disk, most of them still online.
// Only the hydrated ones are local storage, so only they should be counted.
func cloudTree() *Dir {
	root := &Dir{Name: "/home/jane", SelfSize: 1 << 20, SelfFiles: 4}

	od := &Dir{Name: "OneDrive", Parent: root, SelfSize: 2 << 20, SelfFiles: 3}
	photos := &Dir{Name: "Photos", Parent: od, SelfSize: 8 << 20, SelfFiles: 20}
	od.Children = []*Dir{photos}

	local := &Dir{Name: "Videos", Parent: root, SelfSize: 6 << 30, SelfFiles: 12}
	root.Children = []*Dir{od, local}

	rollup(root)
	return root
}

func TestRollupCountsOnlyLocalBytes(t *testing.T) {
	root := cloudTree()

	wantDisk := int64(1<<20) + int64(2<<20) + int64(8<<20) + int64(6<<30)
	if root.Total != wantDisk {
		t.Errorf("on-disk total = %d, want %d", root.Total, wantDisk)
	}
	// Hydrated OneDrive files are real local storage and must be attributed
	// to the folder holding them.
	if root.Children[1].Total != 10<<20 {
		t.Errorf("OneDrive local total = %d, want %d", root.Children[1].Total, 10<<20)
	}
	if root.Children[0].Name != "Videos" {
		t.Errorf("children not ranked by local size: got %q first", root.Children[0].Name)
	}
}

func TestReportNotesUndownloadedFilesWithoutSizing(t *testing.T) {
	root := cloudTree()
	res := &Result{RootPath: root.Name, Root: root, Files: 39, Dirs: 3, CloudFiles: 17000}

	var buf bytes.Buffer
	writeReport(&buf, res, ReportOpts{MinPercent: 1})
	out := buf.String()

	if !strings.Contains(out, "17,000 cloud files are not downloaded") {
		t.Errorf("report should note undownloaded files\n%s", out)
	}
	if !strings.Contains(out, "6.0 GiB  100.0%") {
		t.Errorf("expected local total of 6.0 GiB\n%s", out)
	}
	// The report is about local disk; online totals have no business here.
	for _, unwanted := range []string{"online", "GiB online", "placeholders]"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("report should not mention %q\n%s", unwanted, out)
		}
	}
}

func TestExcludeMatchesNameFullPathAndSuffix(t *testing.T) {
	sep := "/"
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"node_modules", "/srv/app/node_modules", true},
		{"*/node_modules", "/srv/app/node_modules", true},
		{"app/node_modules", "/srv/app/node_modules", true},
		{"/srv/app/node_modules", "/srv/app/node_modules", true},
		{"*.iso", "/data/ubuntu.iso", true},
		{"node_modules", "/srv/app/node_modules_old", false},
		{"*/node_modules", "/srv/node_modules/inner", false},
		{"cache", "/srv/app/node_modules", false},
	}
	for _, c := range cases {
		w := &walker{cfg: Config{Excludes: []string{c.pattern}}}
		name := c.path[strings.LastIndex(c.path, sep)+1:]
		if got := w.excluded(c.path, name); got != c.want {
			t.Errorf("exclude %q against %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestHumanAndComma(t *testing.T) {
	cases := []struct {
		n    int64
		si   bool
		want string
	}{
		{0, false, "0 B"},
		{999, false, "999 B"},
		{1024, false, "1.0 KiB"},
		{1536, false, "1.5 KiB"},
		{1 << 30, false, "1.0 GiB"},
		{1000000, true, "1.0 MB"},
		{107374182400, false, "100 GiB"},
	}
	for _, c := range cases {
		if got := human(c.n, c.si); got != c.want {
			t.Errorf("human(%d, si=%v) = %q, want %q", c.n, c.si, got, c.want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4321: "-4,321"} {
		if got := comma(n); got != want {
			t.Errorf("comma(%d) = %q, want %q", n, got, want)
		}
	}
}

// The OrbStack case: a real mount that the mount table failed to name,
// because its mount point does not spell the way the directory is walked.
// Before the statfs fallback this fell through to "assume local" and the
// scanner walked straight into it. /proc stands in for the NFS export.
func TestMountMissingFromTableIsStillClassified(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("no procfs on this platform")
	}
	rootInfo, err := os.Lstat("/")
	if err != nil {
		t.Fatal(err)
	}
	rootDev, ok := deviceOf(rootInfo)
	if !ok {
		t.Skip("no device numbers on this platform")
	}

	entries, err := os.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var proc fs.DirEntry
	for _, e := range entries {
		if e.Name() == "proc" {
			proc = e
		}
	}
	if proc == nil {
		t.Skip("/proc not present")
	}

	// A deliberately empty mount table: every path lookup misses.
	w := &walker{cfg: Config{}, mounts: NewMountTable(nil)}
	for i := range w.seen {
		w.seen[i].m = make(map[inodeKey]struct{})
	}

	if _, _, walk := w.considerDir("/proc", proc, rootDev); walk {
		t.Fatal("descended into /proc with an empty mount table; " +
			"an unnamed mount is being treated as local disk")
	}
	if len(w.skip) != 1 || !strings.Contains(w.skip[0].Reason, "virtual") {
		t.Fatalf("expected a virtual-filesystem skip, got %+v", w.skip)
	}
}
