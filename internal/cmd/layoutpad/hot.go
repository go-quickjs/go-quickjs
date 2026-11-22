package main

// The lock's rule for adding a position: no function with hotShare of a
// configuration's CPU samples over the micro-benchmarks lies more than
// hotDistance bytes past the anchor of the range it is in. Code between a
// pad's anchor and the next pad keeps its own layout and moves with what
// changes before it in the range, so a hot function far into a range is
// moved by every change there (docs/optimizing.md, "The layout lock").
// -hot builds a configuration as the targets are measured, runs its
// micro-benchmarks under the CPU profiler, and lists the functions that
// break the rule, with the range they are in: each wants a position, a
// pad file sorted right before its file, and an anchor on it.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A hotFunc is a function of the profile and where it lies.
type hotFunc struct {
	name    string
	seconds float64
	share   float64 // of the profile's samples
	lever   string  // the range's lever
	past    int64   // bytes past the range's anchor; 0 before it
	file    string  // its source file, where go tool objdump says
}

// hotRun profiles c's micro-benchmarks, runs times each, and lists the
// functions with at least share of the samples that lie more than dist
// bytes past their range's anchor, hottest first.
func hotRun(c *config, runs int, share float64, dist int64) ([]hotFunc, error) {
	dir, err := os.MkdirTemp("", "layoutpad")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	exe, err := buildExe(c, dir)
	if err != nil {
		return nil, err
	}
	prof := filepath.Join(dir, "micro.cpu")
	args := []string{"-mode", "micro", "-n", strconv.Itoa(runs), "-cpuprofile", prof}
	if c.jit {
		args = append([]string{"-jit"}, args...)
	}
	if out, err := exec.Command(exe, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: v8bench %s: %v\n%s", c, strings.Join(args, " "), err, out)
	}
	top, err := exec.Command("go", "tool", "pprof", "-top", "-nodefraction=0", "-nodecount=1000000", exe, prof).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool pprof: %w", err)
	}
	nm, err := exec.Command("go", "tool", "nm", "-size", "-sort", "address", exe).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool nm: %w", err)
	}
	list := hotList(c, parseNM(nm), parseTop(top), share, dist)
	// A function in its anchor's own file is held as well as a position
	// can hold it: a position is a file.
	files := map[string]string{}
	fileOf := func(name string) string {
		if f, ok := files[name]; ok {
			return f
		}
		f := ""
		out, err := exec.Command("go", "tool", "objdump", "-s", "^"+regexp.QuoteMeta(name)+"$", exe).Output()
		if err == nil {
			if line, _, _ := strings.Cut(string(out), "\n"); strings.HasPrefix(line, "TEXT ") {
				if g := strings.Fields(line); len(g) >= 3 {
					f = g[len(g)-1]
				}
			}
		}
		files[name] = f
		return f
	}
	anchorOf := map[string]string{}
	for _, a := range c.anchors {
		anchorOf[a.lever] = a.symbol
	}
	var kept []hotFunc
	for _, h := range list {
		if f := fileOf(h.name); f != "" && f == fileOf(anchorOf[h.lever]) {
			continue
		}
		h.file = filepath.Base(fileOf(h.name))
		kept = append(kept, h)
	}
	return kept, nil
}

// topLine is a line of go tool pprof -top: flat, flat%, sum%, cum, cum%,
// the function.
var topLine = regexp.MustCompile(`^\s*([\d.]+)(m?s)\s+[\d.]+%\s+[\d.]+%\s+[\d.]+m?s\s+[\d.]+%\s+(.+)$`)

// parseTop reads each function's flat seconds from go tool pprof -top's
// output. A function inlined into another is left out: its code is its
// caller's.
func parseTop(out []byte) map[string]float64 {
	m := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		g := topLine.FindStringSubmatch(sc.Text())
		if g == nil || strings.HasSuffix(g[3], " (inline)") {
			continue
		}
		v, err := strconv.ParseFloat(g[1], 64)
		if err != nil {
			continue
		}
		if g[2] == "ms" {
			v /= 1000
		}
		m[strings.TrimSpace(g[3])] += v
	}
	return m
}

// hotList is hotRun's list of a build's symbols and a profile's flat
// seconds.
func hotList(c *config, syms *symtab, flat map[string]float64, share float64, dist int64) []hotFunc {
	type rng struct {
		pad, anchor uint64
		lever       string
	}
	var rs []rng
	for _, a := range c.anchors {
		l, _ := leverByName(a.lever)
		if l.kind == 'd' {
			continue
		}
		p, ok1 := syms.byName[l.pad]
		s, ok2 := syms.byName[a.symbol]
		if ok1 && ok2 {
			rs = append(rs, rng{p.addr, s.addr, a.lever})
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].pad < rs[j].pad })
	total := 0.0
	for _, v := range flat {
		total += v
	}
	var out []hotFunc
	for name, v := range flat {
		if total == 0 || v/total < share {
			continue
		}
		s, ok := syms.byName[name]
		if !ok {
			// An assembly function's symbol is its ABI0 one.
			if s, ok = syms.byName[name+".abi0"]; !ok {
				continue
			}
		}
		i := sort.Search(len(rs), func(i int) bool { return rs[i].pad > s.addr }) - 1
		if i < 0 {
			continue // before every pad: the runtime and what is linked first
		}
		past := int64(s.addr) - int64(rs[i].anchor)
		if past <= dist {
			continue
		}
		out = append(out, hotFunc{name: name, seconds: v, share: v / total, lever: rs[i].lever, past: past})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seconds > out[j].seconds })
	return out
}
