package main

// The compiler-generated equality functions (type:.eq.*) a build has. The
// compiler makes one for a struct or array type that a comparison, a map
// key or an interface's == needs and that memequal cannot compare -- a
// padding hole, a float, a string or an interface among its fields -- and
// lays it out with the code of the package that first needs it. A new one
// among package vm's code moves every function after it up to the next
// pad: three in two days moved hot code by 64 to 128 bytes (strAppend,
// upvalue_string_append 12.6% slower; wtf8.Scan), each found by timing. So
// the lock records each configuration's list (eqRel), and a build with one
// the list has not fails as an anchor off its target does.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	eqRel    = "internal/vm/layouteq.txt"
	eqPrefix = "type:.eq."
)

// eqHeader is the list file's comment, written when the file has none.
const eqHeader = `# The compiler-generated equality functions (type:.eq.*) in a build of
# internal/cmd/v8bench, for each configuration of layoutlock.txt. A new
# one is laid out among the code of the package that first needs it and
# moves every function after it up to the next pad: internal/cmd/layoutpad
# (go generate ./internal/vm) and TestLayoutLock fail on one this file
# does not list, naming the functions that call it. Make the type it
# compares not need one -- no padding hole, or a non-comparable field
# (_ [0]func()) where nothing should compare it -- or, where it is meant,
# record the list with go run ./internal/cmd/layoutpad -eq.
`

// eqLists is the recorded lists, by configuration (config.String()).
type eqLists map[string][]string

func readEq(path string) (eqLists, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return eqLists{}, nil
	}
	if err != nil {
		return nil, err
	}
	l := eqLists{}
	cur := ""
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch f := strings.Fields(line); {
		case len(f) == 3 && f[0] == "config":
			cur = f[1] + " " + f[2]
			if _, ok := l[cur]; !ok {
				l[cur] = []string{}
			}
		case len(f) >= 2 && f[0] == "eq" && cur != "":
			l[cur] = append(l[cur], strings.TrimSpace(strings.TrimPrefix(line, "eq")))
		default:
			return nil, fmt.Errorf("%s:%d: cannot read %q", path, n+1, line)
		}
	}
	return l, nil
}

// writeEq writes the lists in the targets' order of configurations.
func writeEq(path string, t *targets, l eqLists) error {
	var b strings.Builder
	b.WriteString(eqHeader)
	for _, c := range t.configs {
		names, ok := l[c.String()]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\nconfig %s/%s %s\n", c.goos, c.goarch, c.kind())
		for _, n := range names {
			b.WriteString("eq " + n + "\n")
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// eqOf is the equality functions in syms' text, sorted.
func eqOf(syms *symtab) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range syms.list {
		if (s.kind == "T" || s.kind == "t") && strings.HasPrefix(s.name, eqPrefix) && !seen[s.name] {
			seen[s.name] = true
			out = append(out, s.name)
		}
	}
	sort.Strings(out)
	return out
}

// eqDiff is what got has that rec has not, and what rec has that got has
// not.
func eqDiff(rec, got []string) (added, removed []string) {
	in := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range list {
			m[s] = true
		}
		return m
	}
	r, g := in(rec), in(got)
	for _, s := range got {
		if !r[s] {
			added = append(added, s)
		}
	}
	for _, s := range rec {
		if !g[s] {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// eqCheck compares c's build with the recorded list: lines for each new
// equality function, with where it is and what calls it, and a line for
// those no longer built. It reports whether one is new.
func eqCheck(c *config, syms *symtab, l eqLists) ([]string, bool) {
	rec, ok := l[c.String()]
	if !ok {
		return []string{fmt.Sprintf("no equality functions recorded in %s: record them with go run ./internal/cmd/layoutpad -eq", eqRel)}, true
	}
	added, removed := eqDiff(rec, eqOf(syms))
	var out []string
	if len(added) > 0 {
		callers := eqCallers(c, added)
		for _, n := range added {
			s := syms.byName[n]
			line := fmt.Sprintf("new equality function %s at %#x, %d bytes, after %s", n, s.addr, s.size, symBefore(syms, s.addr))
			if cs := callers[n]; len(cs) > 0 {
				line += "; called by " + strings.Join(cs, ", ")
			} else {
				line += "; called by no code: a map's key or an interface's comparison needs it"
			}
			out = append(out, line)
		}
	}
	if len(removed) > 0 {
		out = append(out, fmt.Sprintf("%d recorded equality functions are no longer built (%s): go run ./internal/cmd/layoutpad -eq drops them", len(removed), strings.Join(removed, ", ")))
	}
	return out, len(added) > 0
}

// eqAdvice is what to do about a new equality function.
const eqAdvice = "layoutpad: a new compiler-generated equality function moves the code after it up to the next pad: " +
	"make the type it compares need none (no padding hole, or a non-comparable _ [0]func() field where nothing should compare it), " +
	"or, where it is meant, record the list with go run ./internal/cmd/layoutpad -eq (" + eqRel + ")"

// symBefore names the text symbol before addr that is no equality or hash
// function the compiler made: where the new one is laid out.
func symBefore(syms *symtab, addr uint64) string {
	i := sort.Search(len(syms.list), func(i int) bool { return syms.list[i].addr >= addr })
	for i--; i >= 0; i-- {
		s := syms.list[i]
		if (s.kind == "T" || s.kind == "t") && !strings.HasPrefix(s.name, "type:.") {
			return s.name
		}
	}
	return "the start of the text"
}

// eqCallers builds c again and disassembles it, and names for each of
// names the functions whose code calls it. It runs only when a check has
// found new ones; an error leaves the callers unnamed.
func eqCallers(c *config, names []string) map[string][]string {
	out := map[string][]string{}
	dir, err := os.MkdirTemp("", "layoutpad")
	if err != nil {
		return out
	}
	defer os.RemoveAll(dir)
	exe, err := buildExe(c, dir)
	if err != nil {
		return out
	}
	cmd := exec.Command("go", "tool", "objdump", exe)
	dump, err := cmd.Output()
	if err != nil {
		return out
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	seen := map[string]bool{}
	fn := ""
	sc := bufio.NewScanner(bytes.NewReader(dump))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "TEXT ") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				fn = strings.TrimSuffix(f[1], "(SB)") + " (" + filepath.Base(f[2]) + ")"
			}
			continue
		}
		i := strings.Index(line, eqPrefix)
		if i < 0 {
			continue
		}
		n := line[i:]
		if j := strings.Index(n, "(SB)"); j >= 0 {
			n = n[:j]
		}
		if want[n] && !seen[n+fn] && !strings.HasPrefix(fn, n+" ") {
			seen[n+fn] = true
			out[n] = append(out[n], fn)
		}
	}
	return out
}
