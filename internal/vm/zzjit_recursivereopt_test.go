//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

// recursiveSetup's walker is made as Typescript's AstWalker is: walk calls
// its visitor, this.pre, then has a function of the node's kind walk the
// node's children, each by a call of walk -- runs of walk's code inside
// runs of it, as deep as the tree. kids picks how each kind's function is
// called: natively, through Reflect.apply, which Go makes, or by a function
// that leaves native code at each call.
const recursiveSetup = `
	var sum = 0
	function Walker(pre, kids) { this.pre = pre; this.kids = kids; this.n = 0 }
	Walker.prototype.walk = function (node) {
		var r = this.pre(node, this)
		if (r === undefined) r = node
		this.n++
		if (node.kids !== undefined) this.kids[node.kind](node, this)
		return r
	}
	function pair(node, w) { w.walk(node.kids[0]); w.walk(node.kids[1]) }
	function list(node, w) { for (var i = 0; i < node.kids.length; i++) w.walk(node.kids[i]) }
	function applied(node, w) { Reflect.apply(list, null, [node, w]) }
	function leaving(node, w) { var o = {}; o[node.kind + 'x'] = 1; with (o) { list(node, w) } }
	function tree(d) {
		if (d == 0) return { kind: 0, v: 1 }
		return d % 2 ? { kind: 0, v: d, kids: [tree(d - 1), tree(d - 1)] } : { kind: 1, v: -d, kids: [tree(d - 1), tree(d - 1), { kind: 0, v: 3 }] }
	}
	var t = tree(9)
	var visitors = [
		function (n) { sum += n.v },
		function (n) { sum += 2 * n.v },
		function (n) { sum -= n.v },
		function (n, w) { sum += w.n },
		function (n) { sum ^= n.v },
		function (n) { sum = (sum * 3 + n.v) | 0 },
	]
	var kinds = { native: [pair, list], applied: [applied, applied], leaving: [leaving, leaving] }
	function run(i, how) { sum = 0; var w = new Walker(visitors[i], kinds[how]); w.walk(t); return sum + ':' + w.n }
`

// A recursive function due to be compiled again for what it learned --
// its visitor's call meeting a new function -- is compiled again where it
// learned it, its runs further out going on from their frames once what
// they called returns, as V8 installs the new code while older frames are
// in the old, rather than refused until they have all returned: then its
// old code left at that call for every node of the tree, Typescript's
// AstWalker.walk 145,000 times a run. Past the functions the call calls
// natively, the visitors it calls through their links (ssa.CallSite's
// Generic) are compiled again once invalidated, though Go never enters
// them: each call of one had been Go's from its first deoptimization on.
// Each answer is the interpreter's, however the walk's runs nest --
// natively, through a call of Go, or by functions Go runs.
func TestJITRecursiveCompiledAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, how := range []string{"native", "applied", "leaving"} {
		t.Run(how, func(t *testing.T) {
			var rounds []string
			for range 4 {
				rounds = append(rounds, fmt.Sprintf("run(0, %q)", how))
			}
			for i := 1; i < 6; i++ {
				rounds = append(rounds, fmt.Sprintf("run(%d, %q)", i, how), fmt.Sprintf("run(%d, %q)", i, how))
			}
			jitAgreesForTest(t, recursiveSetup, rounds...)
			// Each visitor's first walk leaves native code a few times for
			// it, not at every node; its next walks, none.
			r := jitAgreesForTest(t, recursiveSetup, rounds[:4]...)
			for i := 1; i < 6; i++ {
				for k := range 2 {
					hosts := r.jit.hosts
					if _, err := r.Run(compileForTest(t, fmt.Sprintf("run(%d, %q)", i, how))); err != nil {
						t.Fatal(err)
					}
					if n, most := r.jit.hosts-hosts, [2]uint64{32, 2}[k]; n > most {
						t.Errorf("visitor %d's walk %d left native code %d times, more than %d, for a tree of 1,193 nodes", i, k, n, most)
					}
				}
			}
		})
	}
}
