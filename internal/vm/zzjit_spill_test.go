//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// jitLoopSpillsForTest lists the values of a compile's loops that its
// allocation put in spill slots, from its SSA listing and its locations
// (mir.Code's Listing and Locations): those defined in a block on a cycle
// through a loop's header, but a phi that is its own argument, a value the
// loop does not change, which the loop's code neither reads nor writes.
func jitLoopSpillsForTest(listing, locs string) []string {
	succs := map[string][]string{}
	defs := map[string][]string{}
	var headers []string
	cur := ""
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) > 1 && strings.HasPrefix(f[0], "b") && f[1] == "(pc":
			cur = f[0]
			if strings.Contains(line, " loop") {
				headers = append(headers, cur)
			}
		case cur == "" || len(f) == 0:
		case f[0] == "goto":
			succs[cur] = append(succs[cur], f[1])
		case f[0] == "if" && len(f) == 4:
			succs[cur] = append(succs[cur], f[2], f[3])
		case len(f) > 3 && f[2] == "phi" && slices.Contains(f[4:], f[0]):
		case len(f) > 2 && f[1] == "=":
			defs[cur] = append(defs[cur], f[0])
		}
	}
	reach := func(from string) map[string]bool {
		seen := map[string]bool{from: true}
		stack := []string{from}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, s := range succs[b] {
				if !seen[s] {
					seen[s] = true
					stack = append(stack, s)
				}
			}
		}
		return seen
	}
	var out []string
	locs = " " + locs
	for _, h := range headers {
		for b := range reach(h) {
			if !reach(b)[h] {
				continue
			}
			for _, v := range defs[b] {
				if strings.Contains(locs, " "+v+":spill") {
					out = append(out, b+":"+v)
				}
			}
		}
	}
	return out
}

// awfyListSource is AWFY List, its methods as the suite has them.
const awfyListSource = `
	class Element {
		constructor(v) { this.val = v; this.next = null; }
		length() { if (this.next === null) return 1; return 1 + this.next.length(); }
	}
	class List {
		benchmark() {
			const result = this.tail(this.makeList(15), this.makeList(10), this.makeList(6));
			return result.length();
		}
		makeList(length) {
			if (length === 0) return null;
			const e = new Element(length);
			e.next = this.makeList(length - 1);
			return e;
		}
		isShorterThan(x, y) {
			let xTail = x;
			let yTail = y;
			while (yTail !== null) {
				if (xTail === null) { return true; }
				xTail = xTail.next;
				yTail = yTail.next;
			}
			return false;
		}
		tail(x, y, z) {
			if (this.isShorterThan(y, x)) {
				return this.tail(this.tail(x.next, y, z), this.tail(y.next, z, x), this.tail(z.next, x, y));
			}
			return z;
		}
	}
	const list = new List();
	function sums(a, b, c, d, n) {
		let s0 = 0, s1 = 0, s2 = 0, s3 = 0;
		for (let i = 0; i < n; i++) { s0 = (s0 + i) | 0; s1 = (s1 + s0) | 0; s2 = (s2 ^ s1) | 0; s3 = (s3 + s2) | 0; }
		return (a + b + c + d + s0 + s1 + s2 + s3) | 0;
	}`

// The allocation spills the interval whose slot costs least for what it
// gives up (mir's allocate), not whichever ends last. AWFY List's tail,
// with isShorterThan's loop inlined, had the second property read's cell
// address in a spill slot, stored and loaded back every turn, beside the
// caller's values that only the loop's exits read, which ended at the same
// back edge; sums had two of its sums in slots, as they live to the
// return, past the parameters it reads once there. No value of either's
// loop is in a slot now, and each answer is the interpreter's, with the
// collector idle and marking.
func TestJITSpillsWhatTheLoopDoesNotRead(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	inlined := jitInlinedForTest(t)
	listings, locations := map[string]string{}, map[string]string{}
	mir.Annotate = true
	JITCodeHook = func(f JITFunction, _ []byte, c *mir.Code) {
		listings[f.Name], locations[f.Name] = c.Listing, c.Locations()
	}
	t.Cleanup(func() { JITCodeHook, mir.Annotate = nil, false })
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking %v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			clear(listings)
			both := jitBothForTest(t, awfyListSource)
			for range 30 {
				both.agree(t, `list.benchmark()`, `sums(1, 2, 3, 4, 1000)`, `sums(0x7fffffff, 1, -1, 2, 70000)`)
			}
			for _, src := range []string{`list.benchmark()`, `sums(1, 2, 3, 4, 1000)`, `sums(0x7fffffff, 1, -1, 2, 70000)`} {
				v, err := both.jit.Run(compileForTest(t, src))
				want := map[string]float64{`list.benchmark()`: 10, `sums(1, 2, 3, 4, 1000)`: jitSumsWant(1+2+3+4, 1000),
					`sums(0x7fffffff, 1, -1, 2, 70000)`: jitSumsWant(0x7fffffff+1-1+2, 70000)}[src]
				if err != nil || !v.IsNumber() || v.Number() != want {
					t.Fatalf("%s = %v, %v; want %v", src, v, err, want)
				}
			}
			if !jitInlinedFnForTest(t, both.jit, inlined, "tail", "list.isShorterThan") {
				t.Fatalf("tail's code does not inline isShorterThan: %x", inlined["tail"])
			}
			if runtime.GOARCH != "amd64" {
				// arm64's nineteen registers hold both loops' values.
				return
			}
			for _, name := range []string{"tail", "sums"} {
				if listings[name] == "" {
					t.Fatalf("%s not compiled", name)
				}
				if s := jitLoopSpillsForTest(listings[name], locations[name]); len(s) != 0 {
					t.Errorf("%s's loop's values spilled: %v\n%s\n%s", name, s, listings[name], locations[name])
				}
			}
			if !strings.Contains(locations["sums"], "spill") {
				t.Errorf("sums spilled nothing, the test asks for too few registers: %s", locations["sums"])
			}
		})
	}
}

// jitSumsWant is sums's answer for parameters adding up to p, as the
// interpreter makes it: int32 arithmetic, wrapping.
func jitSumsWant(p, n int) float64 {
	var s0, s1, s2, s3 int32
	for i := range n {
		s0 += int32(i)
		s1 += s0
		s2 ^= s1
		s3 += s2
	}
	return float64(int32(int64(p) + int64(s0) + int64(s1) + int64(s2) + int64(s3)))
}
