package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTree makes a scanned tree from relative directory paths and the bytes
// sitting directly in each.
func buildTree(root string, self map[string]int64) *Dir {
	top := &Dir{Name: root}
	for rel, size := range self {
		d := top
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			var next *Dir
			for _, c := range d.Children {
				if c.Name == part {
					next = c
				}
			}
			if next == nil {
				next = &Dir{Name: part, Parent: d}
				d.Children = append(d.Children, next)
			}
			d = next
		}
		d.SelfSize += size
	}
	rollup(top)
	return top
}

func findItem(sg *Suggestions, title string) *Suggestion {
	for i := range sg.Items {
		if sg.Items[i].Title == title {
			return &sg.Items[i]
		}
	}
	return nil
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cache inside a cache is listed on its own with its own command, and its
// bytes are not counted a second time in the outer one.
func TestSuggestNestedLocationsAreNotDoubleCounted(t *testing.T) {
	home := filepath.FromSlash("/h/jane")
	locs := []locRule{
		{path: "~/.cache", title: "outer", safety: Safe, how: rmContents},
		{path: "~/.cache/pip", title: "pip", safety: Safe, how: action{cmd: "pip cache purge"}},
	}
	root := buildTree(home, map[string]int64{
		".cache/misc":       40 << 20,
		".cache/pip/http":   300 << 20,
		"Documents/notes":   900 << 20,
		".cache/small/tiny": 1 << 20,
	})
	sg := newSuggester(home, []string{home}, locs, nil, nil).run(&Result{Root: root})

	outer, pip := findItem(sg, "outer"), findItem(sg, "pip")
	if outer == nil || pip == nil {
		t.Fatalf("want both caches suggested, got %+v", sg.Items)
	}
	if outer.Size != 41<<20 {
		t.Errorf("outer cache = %d, want %d (without pip's bytes)", outer.Size, 41<<20)
	}
	if pip.Size != 300<<20 || pip.Command != "pip cache purge" {
		t.Errorf("pip = %d %q", pip.Size, pip.Command)
	}
	if filepath.Separator == '/' && outer.Command != "rm -rf ~/.cache/*" {
		t.Errorf("outer command = %q", outer.Command)
	}
	if sg.Items[0].Title != "pip" {
		t.Errorf("suggestions not ranked by size: %q first", sg.Items[0].Title)
	}
}

// Build folders are only suggested when the project file that proves what
// they are sits beside them, and never inside a tool's own dot-folder.
func TestSuggestNameRulesNeedMarkers(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	touch(t, filepath.Join(home, "app", "package.json"))
	touch(t, filepath.Join(home, "rs", "Cargo.toml"))
	touch(t, filepath.Join(home, "py", ".venv", "pyvenv.cfg"))
	touch(t, filepath.Join(home, ".vscode", "ext", "package.json"))

	root := buildTree(home, map[string]int64{
		"app/node_modules/react":            20 << 20,
		"app/node_modules/x/node_modules/y": 5 << 20,  // nested: part of the outer one
		"loose/node_modules/z":              30 << 20, // no package.json
		"rs/target/debug":                   50 << 20,
		"notrust/target/debug":              50 << 20, // no Cargo.toml
		"py/.venv/lib":                      15 << 20,
		".vscode/ext/node_modules":          60 << 20, // an editor's extension
	})
	sg := newSuggester(home, []string{home}, nil, commonNameRules, nil).run(&Result{Root: root})

	nm := findItem(sg, "node_modules folders")
	if nm == nil || len(nm.Paths) != 1 || nm.Size != 25<<20 {
		t.Fatalf("node_modules = %+v, want only app/node_modules at 25 MiB", nm)
	}
	if want := filepath.Join(home, "app", "node_modules"); nm.Paths[0].Path != want {
		t.Errorf("node_modules path = %q, want %q", nm.Paths[0].Path, want)
	}
	rs := findItem(sg, "Rust build output (target)")
	if rs == nil || len(rs.Paths) != 1 || !strings.Contains(rs.Paths[0].Path, filepath.Join("rs", "target")) {
		t.Errorf("target = %+v, want only rs/target", rs)
	}
	if v := findItem(sg, "Python virtual environments"); v == nil || v.Safety != Review {
		t.Errorf("venv = %+v, want a review suggestion", v)
	}
}

func TestSuggestFilesSkipsClaimedZonedAndRecent(t *testing.T) {
	home := filepath.FromSlash("/h/jane")
	locs := []locRule{{path: "~/.cache", title: "cache", safety: Safe, how: rmContents}}
	root := buildTree(home, map[string]int64{".cache/a": 20 << 20, "Downloads": 1 << 30})

	now := time.Now()
	old, recent := now.Add(-400*24*time.Hour), now.Add(-24*time.Hour)
	p := func(rel string) string { return filepath.Join(home, filepath.FromSlash(rel)) }
	res := &Result{Root: root, Candidates: []FileEnt{
		{Path: p("Downloads/installer.dmg"), Size: 600 << 20, Modified: recent},
		{Path: p("Downloads/talk.mp4"), Size: 400 << 20, Modified: old},
		{Path: p("Downloads/new.mp4"), Size: 300 << 20, Modified: recent},
		{Path: p(".cache/a/blob.zip"), Size: 200 << 20, Modified: old},                 // already in a cache
		{Path: p("src/repo/.git/objects/pack/p.pack"), Size: 200 << 20, Modified: old}, // history
		{Path: p("Library/Mail/big.mbox"), Size: 200 << 20, Modified: old},             // app data
	}}
	zones := []string{"~/Library"}
	sg := newSuggester(home, []string{home}, locs, nil, zones).run(res)

	inst, stale := findItem(sg, titleInstallers), findItem(sg, titleStale)
	if inst == nil || len(inst.Paths) != 1 || !strings.HasSuffix(inst.Paths[0].Path, "installer.dmg") {
		t.Errorf("installers = %+v", inst)
	}
	if stale == nil || len(stale.Paths) != 1 || !strings.HasSuffix(stale.Paths[0].Path, "talk.mp4") {
		t.Errorf("stale = %+v", stale)
	}
	if inst != nil && inst.Command != "" {
		t.Errorf("individual files should not get a blanket command, got %q", inst.Command)
	}
}

func TestParseDockerDF(t *testing.T) {
	out := []byte(`{"Active":"9","Reclaimable":"9.631GB (48%)","Size":"19.71GB","TotalCount":"25","Type":"Images"}
{"Active":"9","Reclaimable":"10.56kB (0%)","Size":"165MB","TotalCount":"10","Type":"Containers"}
{"Active":"7","Reclaimable":"517.4MB (13%)","Size":"3.906GB","TotalCount":"43","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"756.2MB","Size":"958.9MB","TotalCount":"120","Type":"Build Cache"}
`)
	usage, err := parseDockerDF(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 4 {
		t.Fatalf("got %d rows", len(usage))
	}
	if u := usage[0]; u.Reclaimable != 9631000000 || u.Total != 25 || u.Active != 9 {
		t.Errorf("images = %+v", u)
	}

	items, note := dockerSuggestions(dockerResult{Usage: usage})
	if note != "" {
		t.Errorf("unexpected note %q", note)
	}
	// Containers are below the threshold and left out; the rest biggest first.
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	want := "Unused images (16 of 25)|Build cache|Unused volumes (36 of 43)"
	if got := strings.Join(titles, "|"); got != want {
		t.Errorf("docker suggestions = %s, want %s", got, want)
	}

	for in, want := range map[string]int64{"0B": 0, "12kB": 12000, "1.5GB": 1500000000, "2TB": 2e12} {
		if got, err := parseDockerSize(in); err != nil || got != want {
			t.Errorf("parseDockerSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseDockerSize("3 parsecs"); err == nil {
		t.Error("expected an error for an unknown unit")
	}
}

func TestShellArg(t *testing.T) {
	cases := map[string]string{
		"/Users/jane/Library/Caches":      "~/Library/Caches",
		"/Users/jane/Library/iOS Support": "'/Users/jane/Library/iOS Support'",
		"/opt/data/build":                 "/opt/data/build",
		"/tmp/it's":                       `'/tmp/it'\''s'`,
	}
	for in, want := range cases {
		if got := shellArg(in, "/Users/jane"); got != want {
			t.Errorf("shellArg(%q) = %s, want %s", in, got, want)
		}
	}
}

// Without -suggest, neither report changes at all.
func TestReportsUnchangedWithoutSuggest(t *testing.T) {
	root := cloudTree()
	res := &Result{RootPath: root.Name, Root: root, Files: 39, Dirs: 3}

	var text, js bytes.Buffer
	writeReport(&text, res, ReportOpts{MinPercent: 1})
	if err := writeJSON(&js, res, ReportOpts{MinPercent: 1}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "Could be cleared") {
		t.Errorf("text report has a suggestions section without -suggest\n%s", text.String())
	}
	if strings.Contains(js.String(), "suggestions") {
		t.Errorf("JSON report has suggestions without -suggest\n%s", js.String())
	}

	res.Suggestions = &Suggestions{Items: []Suggestion{
		{Title: "npm download cache", Safety: Safe, Size: 3 << 30, Command: "npm cache clean --force",
			Paths: []SuggestPath{{Path: "/h/.npm/_cacache", Size: 3 << 30}}},
	}}
	text.Reset()
	writeReport(&text, res, ReportOpts{MinPercent: 1})
	for _, want := range []string{"Could be cleared", "Safe to clear", "npm cache clean --force", "3.0 GiB  safe to clear"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report missing %q\n%s", want, text.String())
		}
	}
}
