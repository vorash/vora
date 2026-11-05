//go:build linux

package sandboxreset

import (
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	ID       int    `json:"id"`
	ParentID int    `json:"parent_id"`
	Path     string `json:"path"`
	FSType   string `json:"fstype"`
	Writable bool   `json:"writable"`
}

// parseMountinfo returns the table sorted by mount ID. Any line it cannot read
// fails the whole table: a skipped line could be a writable mount.
func parseMountinfo(r io.Reader) ([]Mount, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	// A line cut short can still parse, with the wrong options.
	text, complete := strings.CutSuffix(string(data), "\n")
	if !complete {
		return nil, errors.New("mountinfo does not end in a newline")
	}
	var table []Mount
	for line := range strings.SplitSeq(text, "\n") {
		m, err := parseMountLine(line)
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %q: %w", line, err)
		}
		table = append(table, m)
	}
	sort.Slice(table, func(i, j int) bool { return table[i].ID < table[j].ID })
	return table, nil
}

// parseMountLine reads
//
//	id parent major:minor root mountpoint options [optional...] - fstype source superoptions
func parseMountLine(line string) (Mount, error) {
	fields := strings.Split(line, " ")
	sep := slices.Index(fields, "-")
	if sep < 6 || len(fields) != sep+4 {
		return Mount{}, errors.New("unexpected field layout")
	}
	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return Mount{}, fmt.Errorf("mount id: %w", err)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return Mount{}, fmt.Errorf("parent id: %w", err)
	}
	mountpoint, err := unescapeOctal(fields[4])
	if err != nil {
		return Mount{}, fmt.Errorf("mount point: %w", err)
	}
	mountRW, err := readWriteFlag(fields[5])
	if err != nil {
		return Mount{}, fmt.Errorf("mount options: %w", err)
	}
	superRW, err := readWriteFlag(fields[sep+3])
	if err != nil {
		return Mount{}, fmt.Errorf("superblock options: %w", err)
	}
	return Mount{
		ID:       id,
		ParentID: parent,
		Path:     mountpoint,
		FSType:   fields[sep+1],
		Writable: mountRW && superRW,
	}, nil
}

func readWriteFlag(options string) (bool, error) {
	opts := strings.Split(options, ",")
	rw, ro := slices.Contains(opts, "rw"), slices.Contains(opts, "ro")
	if rw == ro {
		return false, fmt.Errorf("want exactly one of rw and ro in %q", options)
	}
	return rw, nil
}

// unescapeOctal decodes the \ooo escapes the kernel writes for space, tab,
// newline and backslash in a mount path.
func unescapeOctal(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", fmt.Errorf("truncated escape in %q", s)
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("bad escape in %q: %w", s, err)
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}

// plannedMount is a writable mount to list and the mounts directly inside it,
// by path relative to it.
type plannedMount struct {
	Path   string
	Nested map[string]bool
}

// selectMounts decides what a table means for a reset: which mounts get a
// listing and which names inside them are other mounts. It is the only place
// that decides this, for Capture and for checking a manifest later.
func selectMounts(table []Mount) ([]plannedMount, error) {
	visible, err := visibleMounts(table)
	if err != nil {
		return nil, err
	}
	root, ok := visible["/"]
	if !ok {
		return nil, errors.New("mount table has no root mount")
	}
	if root.Writable {
		return nil, ErrRootWritable
	}

	planned := map[string]*plannedMount{}
	for p, m := range visible {
		if m.Writable && p != "/" && !isExcludedMount(p) {
			planned[p] = &plannedMount{Path: p, Nested: map[string]bool{}}
		}
	}
	if len(planned) == 0 {
		return nil, errors.New("mount table has no writable mount")
	}
	for p := range visible {
		if p == "/" || isExcludedMount(p) {
			continue
		}
		container := nearestMount(visible, p)
		if parent, ok := planned[container]; ok {
			parent.Nested[strings.TrimPrefix(p, container+"/")] = true
		}
	}

	out := make([]plannedMount, 0, len(planned))
	for _, p := range planned {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// visibleMounts maps each path to the mount a lookup reaches. Mounts can be
// stacked at one path, where only the top one counts: the mount that no other
// mount at that path has as its parent.
func visibleMounts(table []Mount) (map[string]Mount, error) {
	stacks := map[string][]Mount{}
	for _, m := range table {
		if !path.IsAbs(m.Path) || path.Clean(m.Path) != m.Path {
			return nil, fmt.Errorf("mount path %q is not absolute and clean", m.Path)
		}
		stacks[m.Path] = append(stacks[m.Path], m)
	}
	visible := make(map[string]Mount, len(stacks))
	for p, stack := range stacks {
		var tops []Mount
		for _, m := range stack {
			// gVisor's generator gives a mount with no parent its own ID as parent.
			covered := slices.ContainsFunc(stack, func(o Mount) bool {
				return o.ID != m.ID && o.ParentID == m.ID
			})
			if !covered {
				tops = append(tops, m)
			}
		}
		if len(tops) != 1 {
			return nil, fmt.Errorf("mounts stacked at %q have no single top", p)
		}
		visible[p] = tops[0]
	}
	return visible, nil
}

func isExcludedMount(p string) bool {
	for _, excluded := range []string{"/proc", "/sys"} {
		if p == excluded || strings.HasPrefix(p, excluded+"/") {
			return true
		}
	}
	return false
}

// nearestMount returns the closest visible mount strictly above p. The walk
// ends because visible holds "/".
func nearestMount(visible map[string]Mount, p string) string {
	for {
		p = path.Dir(p)
		if _, ok := visible[p]; ok {
			return p
		}
	}
}
