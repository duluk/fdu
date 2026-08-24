package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ReportOpts controls presentation only.
type ReportOpts struct {
	MinPercent float64
	MaxDepth   int
	ByExt      bool
	SI         bool
}

func writeReport(out io.Writer, res *Result, o ReportOpts) {
	total := res.Root.Total

	fmt.Fprintf(out, "\n%s\n", res.RootPath)
	fmt.Fprintf(out, "%s in %s %s across %s %s   (%s, %s)\n",
		human(total, o.SI),
		comma(res.Files), plural(res.Files, "file", "files"),
		comma(res.Dirs), plural(res.Dirs, "directory", "directories"),
		sizeMode(res.Logical), roundDur(res.Elapsed))
	if res.CloudFiles > 0 {
		fmt.Fprintf(out, "%s cloud %s not downloaded to this disk, counted as zero\n",
			comma(res.CloudFiles), plural(res.CloudFiles, "file is", "files are"))
	}
	if res.Hardlinked > 0 {
		fmt.Fprintf(out, "%s of hard-linked data counted once (use -count-hardlinks to charge every link)\n",
			human(res.Hardlinked, o.SI))
	}

	if total == 0 {
		fmt.Fprintf(out, "\nNothing to report: no files were readable under this path.\n")
		writeProblems(out, res)
		return
	}

	fmt.Fprintf(out, "\n%10s  %6s  %s\n", "SIZE", "SHARE", "DIRECTORY")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	printNode(out, res.Root, 0, total, o, true)

	if len(res.TopFiles) > 0 {
		fmt.Fprintf(out, "\nLargest files\n%s\n", strings.Repeat("-", 60))
		for _, f := range res.TopFiles {
			fmt.Fprintf(out, "%10s  %6s  %s\n",
				human(f.Size, o.SI), pct(f.Size, total), rel(res.RootPath, f.Path))
		}
	}

	if o.ByExt && len(res.Exts) > 0 {
		fmt.Fprintf(out, "\nBy file type\n%s\n", strings.Repeat("-", 60))
		n := 0
		for _, e := range res.Exts {
			if n >= 15 || float64(e.Size)*100/float64(total) < o.MinPercent {
				break
			}
			fmt.Fprintf(out, "%10s  %6s  %-12s %s files\n",
				human(e.Size, o.SI), pct(e.Size, total), e.Ext, comma(e.Count))
			n++
		}
	}

	writeProblems(out, res)
}

func writeProblems(out io.Writer, res *Result) {
	if len(res.Skipped) > 0 {
		fmt.Fprintf(out, "\nSkipped (%d)\n%s\n", len(res.Skipped), strings.Repeat("-", 60))
		shown := res.Skipped
		if len(shown) > 20 {
			shown = shown[:20]
		}
		for _, s := range shown {
			fmt.Fprintf(out, "  %s\n      %s\n", s.Path, s.Reason)
		}
		if len(res.Skipped) > len(shown) {
			fmt.Fprintf(out, "  ... and %d more\n", len(res.Skipped)-len(shown))
		}
	}

	if res.ErrorCount > 0 {
		fmt.Fprintf(out, "\n%s unreadable path(s) -- their contents are missing from the totals\n%s\n",
			comma(res.ErrorCount), strings.Repeat("-", 60))
		for _, e := range res.Errors {
			fmt.Fprintf(out, "  %s\n", e)
		}
		if res.ErrorCount > int64(len(res.Errors)) {
			fmt.Fprintf(out, "  ... and %s more (run with elevated privileges to see inside)\n",
				comma(res.ErrorCount-int64(len(res.Errors))))
		}
	}
	fmt.Fprintln(out)
}

// printNode prints one directory and recurses into the children that clear the
// threshold. Everything below the threshold is rolled into a single summary
// line so the total still visibly adds up.
func printNode(out io.Writer, d *Dir, depth int, total int64, o ReportOpts, isRoot bool) {
	name := d.Name
	if isRoot {
		name = filepath.Clean(d.Name)
	} else {
		name += string(filepath.Separator)
	}
	fmt.Fprintf(out, "%10s  %6s  %s%s\n", human(d.Total, o.SI), pct(d.Total, total), indent(depth), name)

	if o.MaxDepth > 0 && depth+1 > o.MaxDepth {
		return
	}

	threshold := int64(float64(total) * o.MinPercent / 100)

	var hiddenSize, hiddenCount int64
	for _, c := range d.Children {
		if c.Total < threshold {
			hiddenSize += c.Total
			hiddenCount++
			continue
		}
		printNode(out, c, depth+1, total, o, false)
	}

	// Files sitting directly in this directory matter as much as a subdir.
	if d.SelfSize >= threshold && len(d.Children) > 0 {
		fmt.Fprintf(out, "%10s  %6s  %s%s %s here\n",
			human(d.SelfSize, o.SI), pct(d.SelfSize, total), indent(depth+1), comma(d.SelfFiles), plural(d.SelfFiles, "file", "files"))
	}
	// Only worth a line if the leftovers are themselves significant; otherwise
	// every branch ends in a trail of rounding dust.
	if hiddenCount > 0 && hiddenSize >= threshold {
		fmt.Fprintf(out, "%10s  %6s  %s(%s smaller %s)\n",
			human(hiddenSize, o.SI), pct(hiddenSize, total), indent(depth+1),
			comma(hiddenCount), plural(hiddenCount, "directory", "directories"))
	}
}

func indent(depth int) string { return strings.Repeat("  ", depth) }

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

func pct(n, total int64) string {
	if total <= 0 {
		return "   ---"
	}
	return fmt.Sprintf("%5.1f%%", float64(n)*100/float64(total))
}

func sizeMode(logical bool) string {
	if logical {
		return "file lengths"
	}
	return "space allocated on disk"
}

func human(n int64, si bool) string {
	neg := n < 0
	if neg {
		n = -n
	}
	unit, units := int64(1024), []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	if si {
		unit, units = 1000, []string{"B", "kB", "MB", "GB", "TB", "PB"}
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	val, i := float64(n), 0
	for val >= float64(unit) && i < len(units)-1 {
		val /= float64(unit)
		i++
	}
	s := fmt.Sprintf("%.1f %s", val, units[i])
	if val >= 100 {
		s = fmt.Sprintf("%.0f %s", val, units[i])
	}
	if neg {
		s = "-" + s
	}
	return s
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// roundDur keeps timings readable: nobody needs nanoseconds on a disk scan.
func roundDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// ---- JSON ----

type jsonDir struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	SelfSize   int64     `json:"self_size"`
	Files      int64     `json:"files"`
	Dirs       int64     `json:"dirs"`
	CloudBytes int64     `json:"cloud_bytes_online,omitempty"`
	CloudFiles int64     `json:"cloud_files,omitempty"`
	Percent    float64   `json:"percent"`
	Truncated  bool      `json:"truncated,omitempty"`
	Children   []jsonDir `json:"children,omitempty"`
}

type jsonReport struct {
	Root       string    `json:"root"`
	Total      int64     `json:"total_bytes"`
	Files      int64     `json:"files"`
	Dirs       int64     `json:"dirs"`
	SizeMode   string    `json:"size_mode"`
	ElapsedMS  int64     `json:"elapsed_ms"`
	Hardlinked int64     `json:"hardlinked_bytes_saved"`
	CloudFiles int64     `json:"cloud_files_not_on_disk"`
	Tree       jsonDir   `json:"tree"`
	TopFiles   []FileEnt `json:"top_files"`
	ByExt      []ExtStat `json:"by_extension,omitempty"`
	Skipped    []Skip    `json:"skipped"`
	Errors     []string  `json:"errors"`
	ErrorCount int64     `json:"error_count"`
}

func writeJSON(out io.Writer, res *Result, o ReportOpts) error {
	total := res.Root.Total
	rep := jsonReport{
		Root:       res.RootPath,
		Total:      total,
		Files:      res.Files,
		Dirs:       res.Dirs,
		SizeMode:   sizeMode(res.Logical),
		ElapsedMS:  res.Elapsed.Milliseconds(),
		Hardlinked: res.Hardlinked,
		CloudFiles: res.CloudFiles,
		Tree:       toJSON(res.Root, res.Root.Name, 0, total, o),
		TopFiles:   res.TopFiles,
		ByExt:      res.Exts,
		Skipped:    res.Skipped,
		Errors:     res.Errors,
		ErrorCount: res.ErrorCount,
	}
	if rep.TopFiles == nil {
		rep.TopFiles = []FileEnt{}
	}
	if rep.Skipped == nil {
		rep.Skipped = []Skip{}
	}
	if rep.Errors == nil {
		rep.Errors = []string{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func toJSON(d *Dir, path string, depth int, total int64, o ReportOpts) jsonDir {
	j := jsonDir{
		Path:     path,
		Name:     d.Name,
		Size:     d.Total,
		SelfSize: d.SelfSize,
		Files:    d.TotalFiles,
		Dirs:     d.TotalDirs,
	}
	if total > 0 {
		j.Percent = float64(d.Total) * 100 / float64(total)
	}
	if o.MaxDepth > 0 && depth+1 > o.MaxDepth {
		j.Truncated = len(d.Children) > 0
		return j
	}
	threshold := int64(float64(total) * o.MinPercent / 100)
	for _, c := range d.Children {
		if c.Total < threshold {
			j.Truncated = true
			continue
		}
		j.Children = append(j.Children, toJSON(c, filepath.Join(path, c.Name), depth+1, total, o))
	}
	return j
}
