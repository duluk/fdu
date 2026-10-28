package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Docker keeps images, containers and volumes inside its own storage: a
// virtual disk image on macOS and Windows (Docker.raw, OrbStack's data.img),
// /var/lib/docker on Linux. A directory walk sees that as one opaque lump, so
// -suggest asks Docker itself what could be pruned.

// dockerTimeout bounds the wait for the Docker daemon. The query runs while
// the scan does, so on all but the smallest scans it costs nothing.
const dockerTimeout = 20 * time.Second

type dockerUsage struct {
	Type        string
	Total       int
	Active      int
	Size        int64
	Reclaimable int64
}

type dockerResult struct {
	Usage []dockerUsage
	Err   string // Docker is installed but could not be asked
}

// queryDocker runs `docker system df`. If there is no docker command at all,
// it returns nothing and the report says nothing.
func queryDocker() dockerResult {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return dockerResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "system", "df", "--format", "{{json .}}")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return dockerResult{Err: fmt.Sprintf("Docker did not answer within %s", dockerTimeout)}
	}
	if err != nil {
		msg := firstLine(stderr.String())
		var ee *exec.ExitError
		if msg == "" || !errors.As(err, &ee) {
			msg = err.Error()
		}
		return dockerResult{Err: msg}
	}
	usage, err := parseDockerDF(out)
	if err != nil {
		return dockerResult{Err: "could not read `docker system df` output: " + err.Error()}
	}
	return dockerResult{Usage: usage}
}

// parseDockerDF reads one JSON object per line, e.g.
// {"Active":"9","Reclaimable":"9.631GB (48%)","Size":"19.71GB","TotalCount":"25","Type":"Images"}
func parseDockerDF(out []byte) ([]dockerUsage, error) {
	var usage []dockerUsage
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct{ Type, TotalCount, Active, Size, Reclaimable string }
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		u := dockerUsage{Type: row.Type}
		u.Total, _ = strconv.Atoi(row.TotalCount)
		u.Active, _ = strconv.Atoi(row.Active)
		u.Size, _ = parseDockerSize(row.Size)
		// "9.631GB (48%)": the percentage is not wanted.
		reclaim, _, _ := strings.Cut(row.Reclaimable, " ")
		u.Reclaimable, _ = parseDockerSize(reclaim)
		usage = append(usage, u)
	}
	return usage, sc.Err()
}

// parseDockerSize reads Docker's human-readable sizes, which are decimal
// ("1.5GB" is 1.5e9 bytes) and rounded, so the result is approximate.
func parseDockerSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{
		"": 1, "B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
	}[strings.TrimSpace(s[i:])]
	if mult == 0 {
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return int64(n * mult), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// dockerSuggestions turns Docker's report into suggestions.
func dockerSuggestions(d dockerResult) ([]Suggestion, string) {
	var out []Suggestion
	for _, u := range d.Usage {
		if u.Reclaimable < minSuggest {
			continue
		}
		it := Suggestion{Size: u.Reclaimable}
		unused := u.Total - u.Active
		switch u.Type {
		case "Images":
			it.Title = fmt.Sprintf("Unused images (%d of %d)", unused, u.Total)
			it.Safety, it.Note, it.Command = Safe, "re-pulled or rebuilt when next needed", "docker image prune -a"
		case "Build Cache":
			it.Title = "Build cache"
			it.Safety, it.Note, it.Command = Safe, "only makes rebuilds faster; refilled by the next build", "docker builder prune -a"
		case "Containers":
			it.Title = fmt.Sprintf("Stopped containers (%d of %d)", unused, u.Total)
			it.Safety, it.Note, it.Command = Review, "anything written inside them is lost", "docker container prune"
		case "Local Volumes":
			it.Title = fmt.Sprintf("Unused volumes (%d of %d)", unused, u.Total)
			it.Safety, it.Note, it.Command = Review, "volumes often hold databases; check what is in them first", "docker volume prune -a"
		default:
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, d.Err
}
