package main

import (
	"container/heap"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is everything the scanner needs to know before it starts.
type Config struct {
	Jobs            int
	TopN            int
	Excludes        []string
	IncludeRemote   bool
	IncludeExternal bool
	IncludePseudo   bool
	IncludeCloud    bool
	OneFileSystem   bool
	Logical         bool
	CountHardlinks  bool
	ByExt           bool
	Quiet           bool
}

// Dir is one node of the directory tree. Files are deliberately *not* nodes:
// on a full-disk scan there can be tens of millions of them, and keeping only
// directories is the difference between a few hundred MB of RAM and a few.
// A parent pointer plus the base name reconstructs the path on demand.
type Dir struct {
	Name   string
	Parent *Dir

	SelfSize  int64 // bytes of files sitting directly in this directory
	SelfFiles int64

	Total      int64 // this directory and everything under it
	TotalFiles int64
	TotalDirs  int64

	Children []*Dir
}

// Path rebuilds the absolute path of this directory.
func (d *Dir) Path() string {
	if d.Parent == nil {
		return d.Name
	}
	parts := []string{d.Name}
	for p := d.Parent; p != nil; p = p.Parent {
		parts = append(parts, p.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return filepath.Join(parts...)
}

// FileEnt is one of the largest files found.
type FileEnt struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Skip records a subtree that was deliberately not descended into.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ExtStat is per-extension usage.
type ExtStat struct {
	Ext   string `json:"ext"`
	Size  int64  `json:"size"`
	Count int64  `json:"count"`
}

// Result is everything a scan produced.
type Result struct {
	RootPath   string
	Root       *Dir
	TopFiles   []FileEnt
	Skipped    []Skip
	Exts       []ExtStat
	Errors     []string
	ErrorCount int64
	Files      int64
	Dirs       int64
	Elapsed    time.Duration
	Logical    bool
	Hardlinked int64 // bytes not charged twice because of hard links

	// Placeholders: files listed in the tree whose data is not on this disk.
	// Counted only so the report can explain why a cloud folder looks small.
	CloudFiles int64
}

const maxStoredErrors = 25

type walker struct {
	cfg    Config
	mounts *MountTable

	sem chan struct{}
	wg  sync.WaitGroup

	files      atomic.Int64
	dirs       atomic.Int64
	bytes      atomic.Int64
	errCount   atomic.Int64
	hardSave   atomic.Int64
	cloudFiles atomic.Int64
	topMin     atomic.Int64

	mu   sync.Mutex
	top  fileHeap
	errs []string
	skip []Skip

	seen  [16]inodeSet
	exts  [32]extShard
	start time.Time
}

type inodeSet struct {
	mu sync.Mutex
	m  map[inodeKey]struct{}
}

type extShard struct {
	mu sync.Mutex
	m  map[string]*ExtStat
}

// Scan walks root and returns where the space went.
func Scan(root string, fi fs.FileInfo, cfg Config, mounts *MountTable) *Result {
	w := &walker{
		cfg:    cfg,
		mounts: mounts,
		sem:    make(chan struct{}, cfg.Jobs),
		start:  time.Now(),
	}
	w.topMin.Store(1) // never bother tracking empty files
	for i := range w.seen {
		w.seen[i].m = make(map[inodeKey]struct{})
	}
	for i := range w.exts {
		w.exts[i].m = make(map[string]*ExtStat)
	}

	rootDev, _ := deviceOf(fi)
	if k, _, ok := inodeOf(fi); ok {
		w.markVisited(k)
	}

	node := &Dir{Name: root}
	done := make(chan struct{})
	if !cfg.Quiet {
		go w.progress(done)
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.walk(node, root, rootDev, dirOwnSize(root, fi, cfg.Logical))
	}()
	w.wg.Wait()
	close(done)
	if !cfg.Quiet {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}

	rollup(node)

	res := &Result{
		RootPath:   root,
		Root:       node,
		Skipped:    w.skip,
		Errors:     w.errs,
		ErrorCount: w.errCount.Load(),
		Files:      w.files.Load(),
		Dirs:       w.dirs.Load(),
		Elapsed:    time.Since(w.start),
		Logical:    cfg.Logical,
		Hardlinked: w.hardSave.Load(),
		CloudFiles: w.cloudFiles.Load(),
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Path < res.Skipped[j].Path })

	for w.top.Len() > 0 {
		res.TopFiles = append(res.TopFiles, heap.Pop(&w.top).(FileEnt))
	}
	// heap pops smallest-first; the report wants biggest-first.
	for i, j := 0, len(res.TopFiles)-1; i < j; i, j = i+1, j-1 {
		res.TopFiles[i], res.TopFiles[j] = res.TopFiles[j], res.TopFiles[i]
	}

	if cfg.ByExt {
		for i := range w.exts {
			for _, e := range w.exts[i].m {
				res.Exts = append(res.Exts, *e)
			}
		}
		sort.Slice(res.Exts, func(i, j int) bool { return res.Exts[i].Size > res.Exts[j].Size })
	}
	return res
}

// walk reads one directory. Child directories are handed to spawn, which
// either starts a goroutine (if a worker slot is free) or recurses inline, so
// concurrency stays bounded without the queue ever being able to deadlock.
func (w *walker) walk(d *Dir, path string, dev uint64, own int64) {
	w.dirs.Add(1)

	entries, err := os.ReadDir(path)
	if err != nil {
		w.addErr(err)
		if entries == nil {
			return
		}
	}

	// A directory entry is itself a file on disk (typically 4 KiB), and with
	// enough of them that is real space. du counts it, so we do too.
	selfSize, selfFiles := own, int64(0)
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(path, name)

		if w.excluded(full, name) {
			continue
		}

		if e.IsDir() {
			childDev, childOwn, ok := w.considerDir(full, e, dev)
			if !ok {
				continue
			}
			node := &Dir{Name: name, Parent: d}
			d.Children = append(d.Children, node)
			w.spawn(node, full, childDev, childOwn)
			continue
		}

		// Symlinks are counted at their own (tiny) size and never followed:
		// following them double-counts and invites loops.
		info, err := e.Info()
		if err != nil {
			w.addErr(err)
			continue
		}

		size, cloud := fileSpace(full, info, w.cfg.Logical)
		if cloud {
			w.cloudFiles.Add(1)
		}

		if !w.cfg.CountHardlinks {
			if key, nlink, ok := inodeOf(info); ok && nlink > 1 {
				if w.markVisited(key) {
					// Already charged to the first path we saw.
					w.hardSave.Add(size)
					continue
				}
			}
		}

		selfSize += size
		selfFiles++
		w.offerTop(full, size)
		if w.cfg.ByExt {
			w.addExt(name, size)
		}
	}

	d.SelfSize = selfSize
	d.SelfFiles = selfFiles
	w.files.Add(selfFiles)
	w.bytes.Add(selfSize)
}

// considerDir decides whether to descend into a subdirectory, and returns the
// device the subdirectory lives on.
func (w *walker) considerDir(full string, e fs.DirEntry, parentDev uint64) (dev uint64, own int64, ok bool) {
	info, err := e.Info()
	if err != nil {
		w.addErr(err)
		return 0, 0, false
	}
	own = dirOwnSize(full, info, w.cfg.Logical)

	// Windows: junctions, symlinks and volume mount points redirect elsewhere
	// and must not be walked. Cloud placeholder directories are also reparse
	// points but are genuinely this directory, so they have to be told apart
	// by reparse tag rather than by the attribute bit alone.
	if reason, skip := skipDirNative(full, info); skip {
		w.addSkip(full, reason)
		return 0, 0, false
	}

	dev, ok = deviceOf(info)
	if !ok || dev == parentDev {
		return parentDev, own, true
	}

	// We just crossed a mount point.
	if w.cfg.OneFileSystem {
		w.addSkip(full, "different filesystem (-one-file-system)")
		return 0, 0, false
	}

	m, known := w.mounts.At(full)
	if !known {
		// A boundary the mount table did not name: bind mounts, btrfs
		// subvolumes, snapshots. Treat as local, but the inode check below
		// still protects against counting the same tree twice.
		m = Mount{Path: full, Kind: KindLocal}
	}

	switch m.Kind {
	case KindRemote:
		if !w.cfg.IncludeRemote {
			w.addSkip(full, "network filesystem"+fsNote(m)+" (-remote to include)")
			return 0, 0, false
		}
	case KindRemovable:
		if !w.cfg.IncludeExternal {
			w.addSkip(full, "external/removable volume"+fsNote(m)+" (-external to include)")
			return 0, 0, false
		}
	case KindPseudo:
		if !w.cfg.IncludePseudo {
			w.addSkip(full, "virtual filesystem"+fsNote(m)+" (-pseudo to include)")
			return 0, 0, false
		}
	case KindCloud:
		if !w.cfg.IncludeCloud {
			w.addSkip(full, "cloud storage"+fsNote(m)+"; contents may not be on this disk (-cloud to include)")
			return 0, 0, false
		}
	case KindDuplicate:
		w.addSkip(full, "same data is reachable elsewhere in this scan, counted there")
		return 0, 0, false
	}

	// Same filesystem reached by two paths (bind mounts, repeated mounts):
	// count it once.
	if key, _, ok := inodeOf(info); ok {
		if w.markVisited(key) {
			w.addSkip(full, "already counted under another mount point")
			return 0, 0, false
		}
	}

	return dev, own, true
}

// dirOwnSize is the space the directory entry itself occupies. In logical
// mode it is zero: a directory's st_size is a filesystem bookkeeping detail,
// not content, and this is what du --apparent-size reports too.
func dirOwnSize(path string, info fs.FileInfo, logical bool) int64 {
	if logical {
		return 0
	}
	return diskSize(path, info, false)
}

func fsNote(m Mount) string {
	if m.FSType == "" {
		return ""
	}
	return " [" + m.FSType + "]"
}

func (w *walker) spawn(d *Dir, path string, dev uint64, own int64) {
	select {
	case w.sem <- struct{}{}:
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			w.walk(d, path, dev, own)
		}()
	default:
		w.walk(d, path, dev, own) // all workers busy: just do it here
	}
}

// excluded matches a pattern against the entry's own name, its full path, and
// the trailing path components. filepath.Match never lets '*' cross a
// separator, so without the last rule an intuitive pattern like
// "*/node_modules" would silently match nothing.
func (w *walker) excluded(full, name string) bool {
	if len(w.cfg.Excludes) == 0 {
		return false
	}
	if isCaseInsensitiveFS {
		full, name = strings.ToLower(full), strings.ToLower(name)
	}
	sep := string(filepath.Separator)
	for _, pat := range w.cfg.Excludes {
		if isCaseInsensitiveFS {
			pat = strings.ToLower(pat)
		}
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, full); ok {
			return true
		}
		// Compare only as many trailing components as the pattern has.
		if n := strings.Count(pat, sep) + 1; n > 1 {
			parts := strings.Split(full, sep)
			if len(parts) >= n {
				tail := strings.Join(parts[len(parts)-n:], sep)
				if ok, _ := filepath.Match(pat, tail); ok {
					return true
				}
			}
		}
	}
	return false
}

// markVisited returns true if the key had already been seen.
func (w *walker) markVisited(k inodeKey) bool {
	s := &w.seen[k.Ino&15]
	s.mu.Lock()
	_, dup := s.m[k]
	if !dup {
		s.m[k] = struct{}{}
	}
	s.mu.Unlock()
	return dup
}

func (w *walker) offerTop(path string, size int64) {
	if w.cfg.TopN == 0 || size < w.topMin.Load() {
		return
	}
	w.mu.Lock()
	heap.Push(&w.top, FileEnt{Path: path, Size: size})
	if w.top.Len() > w.cfg.TopN {
		heap.Pop(&w.top)
	}
	if w.top.Len() == w.cfg.TopN {
		w.topMin.Store(w.top[0].Size)
	}
	w.mu.Unlock()
}

func (w *walker) addExt(name string, size int64) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		ext = "(none)"
	} else if len(ext) > 12 {
		ext = "(none)" // not really an extension
	}
	h := fnv.New32a()
	h.Write([]byte(ext))
	s := &w.exts[h.Sum32()&31]
	s.mu.Lock()
	e := s.m[ext]
	if e == nil {
		e = &ExtStat{Ext: ext}
		s.m[ext] = e
	}
	e.Size += size
	e.Count++
	s.mu.Unlock()
}

func (w *walker) addErr(err error) {
	w.errCount.Add(1)
	w.mu.Lock()
	if len(w.errs) < maxStoredErrors {
		w.errs = append(w.errs, cleanErr(err))
	}
	w.mu.Unlock()
}

func (w *walker) addSkip(path, reason string) {
	w.mu.Lock()
	w.skip = append(w.skip, Skip{Path: path, Reason: reason})
	w.mu.Unlock()
}

func (w *walker) progress(done <-chan struct{}) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			fmt.Fprintf(os.Stderr, "\r\033[Kscanning... %s dirs, %s files, %s so far",
				comma(w.dirs.Load()), comma(w.files.Load()), human(w.bytes.Load(), false))
		}
	}
}

func cleanErr(err error) string {
	var pe *fs.PathError
	if e, ok := err.(*fs.PathError); ok {
		pe = e
		return fmt.Sprintf("%s: %v", pe.Path, pe.Err)
	}
	return err.Error()
}

// rollup totals each subtree and orders children biggest-first.
func rollup(d *Dir) {
	d.Total = d.SelfSize
	d.TotalFiles = d.SelfFiles
	d.TotalDirs = int64(len(d.Children))
	for _, c := range d.Children {
		rollup(c)
		d.Total += c.Total
		d.TotalFiles += c.TotalFiles
		d.TotalDirs += c.TotalDirs
	}
	sort.Slice(d.Children, func(i, j int) bool {
		if d.Children[i].Total != d.Children[j].Total {
			return d.Children[i].Total > d.Children[j].Total
		}
		return d.Children[i].Name < d.Children[j].Name
	})
}

type fileHeap []FileEnt

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return h[i].Size < h[j].Size }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x any)        { *h = append(*h, x.(FileEnt)) }
func (h *fileHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
