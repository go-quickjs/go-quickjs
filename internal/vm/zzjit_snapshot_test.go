//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitSnapOracle is the snapshot's oracle (QJS_JIT_SNAPCHECK): before each
// build on the runtime's goroutine, fb's snapshot is asked, as a compile in
// the background asks it, and the build goes on from fb; once it is made
// (jitSnapBuilt), p is built from the snapshot, on a copy of fb, as a
// worker builds it, and the two must be the same, but for the addresses
// they hold -- a string's cell is made anew at each question -- the
// snapshot's build asking nothing it was not asked ahead.
func jitSnapOracle(fb *jitFeedback, p *ir.Program) ssa.Feedback {
	// Asked of a copy of the feedback as it is before the build, whose
	// caches its questions fill -- a compile in the background has no
	// build from fb -- so that the build goes on from fb untouched.
	asked := *fb
	before := jitSnapClone(&asked)
	snap := jitSnapshotOf(&asked, p)
	jitSnapChecks.mu.Lock()
	jitSnapChecks.pending[fb] = jitSnapPending{snap, &asked, before}
	jitSnapChecks.mu.Unlock()
	return fb
}

// jitSnapPending is a build the oracle is to compare: its snapshot, and
// its feedback's lists before it.
type jitSnapPending struct {
	snap          *jitSnapshot
	asked, before *jitFeedback
}

// jitSnapClone is a feedback with f's lists, copied.
func jitSnapClone(f *jitFeedback) *jitFeedback {
	return &jitFeedback{shapes: slices.Clone(f.shapes), holders: slices.Clone(f.holders), cells: slices.Clone(f.cells),
		links: slices.Clone(f.links), inlined: slices.Clone(f.inlined), made: slices.Clone(f.made),
		wanted: slices.Clone(f.wanted), linked: slices.Clone(f.linked), pools: slices.Clone(f.pools)}
}

// jitSnapListsDiffer says how the lists of a, the build's, and b, the
// replay's, differ, or "": by identity, but cells, which the feedback makes
// anew at each question, by their values.
func jitSnapListsDiffer(a, b *jitFeedback) string {
	same := func(name string, x, y int, eq func(i int) bool) string {
		if x != y {
			return fmt.Sprintf("%s: %d against %d", name, x, y)
		}
		for i := range x {
			if !eq(i) {
				return fmt.Sprintf("%s[%d] differs", name, i)
			}
		}
		return ""
	}
	for _, d := range []string{
		same("shapes", len(a.shapes), len(b.shapes), func(i int) bool { return a.shapes[i] == b.shapes[i] }),
		same("holders", len(a.holders), len(b.holders), func(i int) bool { return a.holders[i] == b.holders[i] }),
		same("cells", len(a.cells), len(b.cells), func(i int) bool { return a.cells[i].SameValue(*b.cells[i]) }),
		same("links", len(a.links), len(b.links), func(i int) bool { return a.links[i] == b.links[i] }),
		same("inlined", len(a.inlined), len(b.inlined), func(i int) bool { return a.inlined[i] == b.inlined[i] }),
		same("made", len(a.made), len(b.made), func(i int) bool { return a.made[i] == b.made[i] }),
		same("wanted", len(a.wanted), len(b.wanted), func(i int) bool { return a.wanted[i] == b.wanted[i] }),
		same("linked", len(a.linked), len(b.linked), func(i int) bool { return a.linked[i] == b.linked[i] }),
		same("pools", len(a.pools), len(b.pools), func(i int) bool { return a.pools[i] == b.pools[i] }),
	} {
		if d != "" {
			return d
		}
	}
	return ""
}

// jitSnapBuilt is the oracle's comparison of a build with its snapshot's.
func jitSnapBuilt(fb *jitFeedback, p *ir.Program, want *ssa.Func, wantErr error) {
	jitSnapChecks.mu.Lock()
	pending, ok := jitSnapChecks.pending[fb]
	delete(jitSnapChecks.pending, fb)
	jitSnapChecks.mu.Unlock()
	if !ok {
		return
	}
	snap := pending.snap
	w := jit.GetWorkspace()
	defer jit.PutWorkspace(w)
	copied := *pending.asked
	got, gotErr := jitSnapBuild(w, fb, &copied, p, snap)
	name := fb.fn.Name
	// Runtimes on goroutines of their own share what the oracle found.
	jitSnapChecks.mu.Lock()
	defer jitSnapChecks.mu.Unlock()
	jitSnapChecks.builds++
	var miss jitSnapMiss
	switch {
	case errors.As(gotErr, &miss):
		jitSnapChecks.misses[jitSnapNames[miss.method]]++
		jitSnapChecks.note(fmt.Sprintf("%s: %v", name, miss))
	case (wantErr == nil) != (gotErr == nil):
		jitSnapChecks.note(fmt.Sprintf("%s: built %v from the feedback, %v from the snapshot", name, wantErr, gotErr))
	case wantErr == nil:
		a, b := jitSnapAddresses.ReplaceAllString(want.String(), "0x?"), jitSnapAddresses.ReplaceAllString(got.String(), "0x?")
		if a != b {
			al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
			i := 0
			for i < len(al) && i < len(bl) && al[i] == bl[i] {
				i++
			}
			line := func(l []string) string {
				if i < len(l) {
					return l[i]
				}
				return "(end)"
			}
			jitSnapChecks.note(fmt.Sprintf("%s: line %d: feedback %q, snapshot %q", name, i, line(al), line(bl)))
			return
		}
		// The lists the replay of the build's questions leaves, on the
		// lists the feedback had before it, against the build's own.
		replayed := map[*jitFeedback]*jitFeedback{pending.asked: pending.before}
		for _, u := range snap.uses {
			e := u.s.effects[u.key]
			own := replayed[u.s.fb]
			if own == nil {
				own = jitSnapClone(u.s.fb)
				replayed[u.s.fb] = own
			}
			e.own.apply(own)
			if k := u.s.fb.keep(); k != u.s.fb {
				kept := replayed[k]
				if kept == nil {
					kept = jitSnapClone(k)
					replayed[k] = kept
				}
				e.kept.apply(kept)
			}
		}
		if d := jitSnapListsDiffer(fb, pending.before); d != "" {
			jitSnapChecks.note(fmt.Sprintf("%s: the lists the replay leaves: %s", name, d))
		}
	}
}

// jitSnapBuild builds p from snap, as a worker does, on fb, a copy of the
// compile's feedback, a miss its error.
func jitSnapBuild(w *jit.Workspace, real, fb *jitFeedback, p *ir.Program, snap *jitSnapshot) (f *ssa.Func, err error) {
	defer func() {
		if v := recover(); v != nil {
			m, ok := v.(jitSnapMiss)
			if !ok {
				panic(v)
			}
			f, err = nil, m
		}
	}()
	return jitBuildFrom(w, real.fn, p, fb, snap, nil)
}

var jitSnapAddresses = regexp.MustCompile(`0x[0-9a-f]+`)

// jitSnapChecks is what the oracle found.
var jitSnapChecks = struct {
	mu      sync.Mutex
	builds  int
	misses  map[string]int
	found   []string
	note    func(string)
	pending map[*jitFeedback]jitSnapPending
}{misses: map[string]int{}, pending: map[*jitFeedback]jitSnapPending{}}

func init() {
	jitSnapChecks.note = func(s string) { jitSnapChecks.found = append(jitSnapChecks.found, s) }
}

// jitSnapReport writes what the oracle found and reports whether it found
// anything.
func jitSnapReport(w io.Writer) bool {
	fmt.Fprintf(w, "snapshot oracle: %d builds, %d differing or missing\n", jitSnapChecks.builds, len(jitSnapChecks.found))
	var ms []string
	for m, n := range jitSnapChecks.misses {
		ms = append(ms, fmt.Sprintf("%s:%d", m, n))
	}
	sort.Strings(ms)
	if len(ms) != 0 {
		fmt.Fprintf(w, "  misses: %s\n", strings.Join(ms, " "))
	}
	for i, s := range jitSnapChecks.found {
		if i == 40 {
			fmt.Fprintf(w, "  ...\n")
			break
		}
		fmt.Fprintf(w, "  %s\n", s)
	}
	return len(jitSnapChecks.found) != 0
}
