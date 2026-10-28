package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Suggestions are what -suggest thinks could be deleted to free space.
// Nothing is ever deleted: each comes with the command that would do it and a
// Safety saying how much thought that deserves first.
type Suggestions struct {
	Items      []Suggestion // found in the scanned tree
	Docker     []Suggestion // reported by Docker itself, from its own disk
	DockerNote string       // why Docker could not be asked, if it could not
}

// Safety is how much thought a suggestion needs before acting on it.
type Safety int

const (
	Safe   Safety = iota // rebuilt or re-downloaded automatically when needed
	Review               // might hold something wanted: look before deleting
)

func (s Safety) MarshalText() ([]byte, error) {
	if s == Safe {
		return []byte("safe"), nil
	}
	return []byte("review"), nil
}

// Suggestion is one kind of thing that could go, gathered from every place it
// was found: all the node_modules folders are one suggestion, not twenty.
type Suggestion struct {
	Title   string        `json:"title"`
	Safety  Safety        `json:"safety"`
	Size    int64         `json:"size"`
	Note    string        `json:"note,omitempty"`
	Command string        `json:"command,omitempty"`
	Paths   []SuggestPath `json:"paths,omitempty"`

	how action
}

type SuggestPath struct {
	Path     string     `json:"path"`
	Size     int64      `json:"size"`
	Modified *time.Time `json:"modified,omitempty"` // files only
}

const (
	minSuggest     = 10 << 20 // anything smaller is not worth the reader's time
	suggestFiles   = 500      // large files kept as candidates while walking
	minFileSuggest = 50 << 20 // smallest single file worth suggesting
	staleAge       = 182 * 24 * time.Hour
)

// Files that have usually done their job once installed or unpacked.
var installerExts = map[string]bool{
	".dmg": true, ".iso": true, ".pkg": true, ".mpkg": true, ".msi": true, ".xip": true,
	".ipsw": true, ".img": true, ".zip": true, ".tar": true, ".tgz": true, ".gz": true,
	".bz2": true, ".xz": true, ".zst": true, ".7z": true, ".rar": true,
}

var vcsDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, ".jj": true}

// Folders that are really one document or program to the user: an app, or a
// Photos or Music library, whose insides are not theirs to prune by hand.
var bundleExts = map[string]bool{
	".app": true, ".photoslibrary": true, ".musiclibrary": true, ".tvlibrary": true,
	".imovielibrary": true, ".fcpbundle": true, ".logicx": true, ".band": true,
	".aplibrary": true, ".photolibrary": true, ".vmwarevm": true, ".pvm": true, ".utm": true,
}

const (
	titleInstallers = "Installers, disk images and archives"
	titleStale      = "Large files not modified in over 6 months"
)

// totals splits the reclaimable space by kind.
func (sg *Suggestions) totals() (safe, review, docker int64) {
	for _, it := range sg.Items {
		if it.Safety == Safe {
			safe += it.Size
		} else {
			review += it.Size
		}
	}
	for _, it := range sg.Docker {
		docker += it.Size
	}
	return safe, review, docker
}

// Suggest looks over a finished scan, and what Docker reported, for space
// that could be reclaimed.
func Suggest(res *Result, docker dockerResult) *Suggestions {
	own, homes := homeDirs()
	s := newSuggester(own, homes, platformLocRules(), commonNameRules, platformZones())
	sg := s.run(res)
	sg.Docker, sg.DockerNote = dockerSuggestions(docker)
	return sg
}

type suggester struct {
	home     string // the invoking user's, for ~ in commands
	homeList []string
	homes    map[string]bool    // pathKey of every home directory
	locs     map[string]locRule // pathKey of each location, with ~ expanded
	names    []nameRule
	zones    map[string]bool
	now      time.Time

	items   map[string]*Suggestion
	claimed []string // pathKey of every folder already suggested
}

func newSuggester(own string, homes []string, locs []locRule, names []nameRule, zones []string) *suggester {
	s := &suggester{
		home:  own,
		homes: map[string]bool{},
		locs:  map[string]locRule{},
		names: names,
		zones: map[string]bool{},
		now:   time.Now(),
		items: map[string]*Suggestion{},
	}
	for _, h := range homes {
		h = filepath.Clean(h)
		s.homeList = append(s.homeList, h)
		s.homes[pathKey(h)] = true
	}
	for _, r := range locs {
		for _, p := range s.expand(r.path) {
			s.locs[pathKey(p)] = r
		}
	}
	for _, z := range zones {
		for _, p := range s.expand(z) {
			s.zones[pathKey(p)] = true
		}
	}
	return s
}

// expand turns a rule's path into the absolute paths it stands for: one per
// home directory for "~/" paths, and the symlink-resolved spelling as well,
// since that is what a scan walks (macOS's /tmp is really /private/tmp).
func (s *suggester) expand(p string) []string {
	var raw []string
	if rel, ok := strings.CutPrefix(p, "~/"); ok {
		for _, h := range s.homeList {
			raw = append(raw, filepath.Join(h, filepath.FromSlash(rel)))
		}
	} else {
		raw = []string{filepath.Clean(p)}
	}
	out := raw
	for _, r := range raw {
		if real, err := filepath.EvalSymlinks(r); err == nil && real != r {
			out = append(out, real)
		}
	}
	return out
}

// pathKey is how paths are compared: case-folded where the filesystem is.
func pathKey(p string) string {
	if isCaseInsensitiveFS {
		return strings.ToLower(p)
	}
	return p
}

func (s *suggester) run(res *Result) *Suggestions {
	root := res.Root.Name

	// The scan may have started inside a location or a zone; what holds for
	// the folders above it holds for it too. The nearest location wins.
	names, inherited := true, (*locRule)(nil)
	for p := filepath.Dir(root); ; p = filepath.Dir(p) {
		if s.isZone(p) {
			names = false
		}
		if r, ok := s.locs[pathKey(p)]; ok && inherited == nil {
			inherited = &r
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if inherited != nil {
		s.matchLoc(res.Root, root, *inherited)
	} else {
		s.visit(res.Root, root, names)
	}

	s.files(res.Candidates)

	sg := &Suggestions{}
	for _, it := range s.items {
		if it.Size < minSuggest {
			continue
		}
		sort.Slice(it.Paths, func(i, j int) bool { return it.Paths[i].Size > it.Paths[j].Size })
		it.Command = s.command(it.how, it.Paths)
		sg.Items = append(sg.Items, *it)
	}
	sort.Slice(sg.Items, func(i, j int) bool {
		if sg.Items[i].Size != sg.Items[j].Size {
			return sg.Items[i].Size > sg.Items[j].Size
		}
		return sg.Items[i].Title < sg.Items[j].Title
	})
	return sg
}

// visit looks for suggestions at and below d, and returns how many of its
// bytes were claimed by them. names is false once inside a zone, or inside a
// folder already suggested, where name rules would only find its innards.
func (s *suggester) visit(d *Dir, path string, names bool) int64 {
	if r, ok := s.locs[pathKey(path)]; ok {
		return s.matchLoc(d, path, r)
	}
	if names && s.isZone(path) {
		names = false
	}
	if names {
		if r := s.matchName(path); r != nil {
			s.claim(path)
			s.add(r.title, r.safety, r.note, rmDir, SuggestPath{Path: path, Size: d.Total})
			return d.Total
		}
	}
	var claimed int64
	for _, c := range d.Children {
		claimed += s.visit(c, filepath.Join(path, c.Name), names)
	}
	return claimed
}

// matchLoc suggests a known location, less anything inside it that has a
// suggestion of its own: the Homebrew cache is listed separately from the
// rest of ~/Library/Caches, with its own command, rather than twice.
func (s *suggester) matchLoc(d *Dir, path string, r locRule) int64 {
	s.claim(path)
	var inner int64
	for _, c := range d.Children {
		inner += s.visit(c, filepath.Join(path, c.Name), false)
	}
	s.add(r.title, r.safety, r.note, r.how, SuggestPath{Path: path, Size: d.Total - inner})
	return d.Total
}

func (s *suggester) matchName(path string) *nameRule {
	base := filepath.Base(path)
	for i := range s.names {
		r := &s.names[i]
		if !nameIn(base, r.names) {
			continue
		}
		if len(r.markers) == 0 {
			return r
		}
		dir := filepath.Dir(path)
		if r.inside {
			dir = path
		}
		for _, m := range r.markers {
			if fileExists(filepath.Join(dir, m)) {
				return r
			}
		}
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func nameIn(name string, names []string) bool {
	for _, n := range names {
		if n == name || (isCaseInsensitiveFS && strings.EqualFold(n, name)) {
			return true
		}
	}
	return false
}

// isZone reports whether p is the top of a tree that name rules and file
// suggestions keep out of: a listed zone, an app or library bundle, a
// version-control store (old pack files there are history, not clutter), or
// a dot-folder
// directly in a home directory (~/.vscode, ~/.nvm, ...), which belong to the
// tools that made them.
func (s *suggester) isZone(p string) bool {
	k := pathKey(p)
	if s.zones[k] {
		return true
	}
	base := filepath.Base(p)
	if bundleExts[strings.ToLower(filepath.Ext(base))] || vcsDirs[base] {
		return true
	}
	return strings.HasPrefix(base, ".") && s.homes[pathKey(filepath.Dir(p))]
}

func (s *suggester) inZone(p string) bool {
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		if s.isZone(d) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

func (s *suggester) claim(path string) {
	s.claimed = append(s.claimed, pathKey(path))
}

func (s *suggester) isClaimed(path string) bool {
	k := pathKey(path)
	sep := string(filepath.Separator)
	for _, c := range s.claimed {
		if strings.HasPrefix(k, c+sep) {
			return true
		}
	}
	return false
}

// files sorts the largest files into installers and archives (any age) and
// files nobody has touched in months.
func (s *suggester) files(cands []FileEnt) {
	for _, f := range cands {
		if s.isClaimed(f.Path) || s.inZone(f.Path) {
			continue
		}
		mod := f.Modified
		p := SuggestPath{Path: f.Path, Size: f.Size, Modified: &mod}
		switch {
		case installerExts[strings.ToLower(filepath.Ext(f.Path))]:
			s.add(titleInstallers, Review, "usually safe to delete once installed or unpacked", action{}, p)
		case !mod.IsZero() && s.now.Sub(mod) >= staleAge:
			s.add(titleStale, Review, "", action{}, p)
		}
	}
}

func (s *suggester) add(title string, safety Safety, note string, how action, p SuggestPath) {
	if p.Size <= 0 {
		return
	}
	it := s.items[title]
	if it == nil {
		it = &Suggestion{Title: title, Safety: safety, Note: note, how: how}
		s.items[title] = it
	}
	it.Size += p.Size
	it.Paths = append(it.Paths, p)
}

// command renders the action as something to paste into a shell. With more
// than one path, the folder is left as a placeholder.
func (s *suggester) command(a action, paths []SuggestPath) string {
	if a.cmd != "" || a.clear == clearNone {
		return a.cmd
	}
	if runtime.GOOS == "windows" {
		arg := "<folder>"
		if len(paths) == 1 {
			arg = paths[0].Path
		}
		if a.clear == clearContents {
			arg = filepath.Join(arg, "*")
		}
		if len(paths) == 1 {
			arg = "'" + strings.ReplaceAll(arg, "'", "''") + "'"
		}
		return "Remove-Item -Recurse -Force " + arg
	}
	arg := "<folder>"
	if len(paths) == 1 {
		arg = shellArg(paths[0].Path, s.home)
	}
	if a.clear == clearContents {
		arg += "/*"
	}
	cmd := "rm -rf " + arg
	if a.sudo {
		cmd = "sudo " + cmd
	}
	return cmd
}

// shellArg quotes a path for a POSIX shell, writing it as ~/... where that
// needs no quoting.
func shellArg(p, home string) string {
	if home != "" {
		if rest, ok := strings.CutPrefix(p, home+"/"); ok && shellSafe(rest) {
			return "~/" + rest
		}
	}
	if shellSafe(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

func shellSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("/._-+,:@%=", c):
		default:
			return false
		}
	}
	return true
}
