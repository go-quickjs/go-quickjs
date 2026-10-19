//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// stringGuessSetup's loops, each run once, call an object's method of a
// name String.prototype has too -- charAt, indexOf, slice, trim -- a
// class's, an object literal's and a method shorthand's: its
// read's cache knows nothing, a loop compiled at its back edge as it first
// runs leaving none, so the code takes it for a string's
// (jitFeedback's StringProperty).
const stringGuessSetup = `
	class C { charAt(i) { return i & 1 } indexOf(i) { return i & 3 } slice(i) { return i % 5 } trim(i) { return i % 7 } }
	var lit = { charAt: function (i) { return i & 1 }, indexOf: function (i) { return i & 3 }, slice: function (i) { return i % 5 }, trim: function (i) { return i % 7 } }
	var short = { charAt(i) { return i & 1 }, indexOf(i) { return i & 3 }, slice(i) { return i % 5 }, trim(i) { return i % 7 } }
	function charAts(o, n) { let s = 0; for (let i = 0; i < n; i++) s += o.charAt(i); return s }
	function indexOfs(o, n) { let s = 0; for (let i = 0; i < n; i++) s += o.indexOf(i); return s }
	function slices(o, n) { let s = 0; for (let i = 0; i < n; i++) s += o.slice(i); return s }
	function trims(o, n) { let s = 0; for (let i = 0; i < n; i++) s += o.trim(i); return s }
`

// An object meeting a method read the code took for a string's has the
// read's cache take it, as the interpreter's read would have, and the code
// compiled again for its shape (jitPolySeen): each loop, run once on a
// class's instance, leaves its code a few times, not at every entry --
// where it had, 64 times or more, until it was judged slow and left to the
// existing tiers. Each answer is the interpreter's.
func TestJITObjectMethodNamedLikeAString(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	names := []string{"charAts", "indexOfs", "slices", "trims"}
	rounds := []string{}
	for _, name := range names {
		rounds = append(rounds, name+"(new C(), 4000)")
	}
	// Literals' own methods, of functions run once already: their calls
	// are Go's at first, the read's cache keeping the receiver, not the
	// method, which seeding takes only from a prototype's.
	rounds = append(rounds, "charAts(lit, 4000)", "slices(short, 4000)")
	r := jitAgreesForTest(t, stringGuessSetup, rounds...)
	for _, name := range names {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil {
			t.Errorf("%s has no code", name)
			continue
		}
		if e.ssaStats.hosts > 8 || e.entrySlow {
			t.Errorf("%s left its code %d times, judged slow %v", name, e.ssaStats.hosts, e.entrySlow)
		}
	}
}
