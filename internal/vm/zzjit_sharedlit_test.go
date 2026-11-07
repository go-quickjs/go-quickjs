//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
)

// Native code writes no element an array literal's arrays share
// (objSharedElems, abi.Encoding's FlagShared): its stores, of any value
// (OpElemStore) and of a number (OpElemWrite), its read-modify-writes, push,
// pop and length's write each leave the array to Go, which gives it a copy
// of its own, and the literal's other arrays are as they were. Each
// operation has literals of its own, each used six times, so that their
// arrays are shared at every round, never given up (sharedGiveUp). Each
// answer is the interpreter's, with the collector idle and marking.
func TestJITWritesSharedLiteral(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	ops := map[string]string{
		"storeAny":  `a[i] = 'x'`,
		"storeNum":  `a[i] = i * 10`,
		"addTo":     `a[i] += 1`,
		"pushIt":    `a.push(9)`,
		"popIt":     `a.pop(); a.push(0)`,
		"trunc":     `a.length = 3; a.push(7)`,
		"fillLoop":  `for (var k = 0; k < a.length; k++) a[k] = k * 3`,
		"sortCmp":   `a.sort(function (x, y) { return y - x; })`,
		"sortPlain": `a.sort()`,
		"sortUp":    `a.sort(function (x, y) { return x - y; })`,
		"appendAt":  `a[a.length] = i`,
	}
	const sites = 60
	var src strings.Builder
	var names []string
	for name, body := range ops {
		names = append(names, name)
		fmt.Fprintf(&src, "function %s(a, i) { %s; }\n", name, body)
		fmt.Fprintf(&src, "var %s_lits = [", name)
		for k := range sites {
			fmt.Fprintf(&src, "function () { return [1, 2, 3, 4, 5, 6, 7, %d]; },", k)
		}
		src.WriteString("];\n")
	}
	src.WriteString(`
		function run(op, lits) {
			var out = [];
			for (var pass = 0; pass < 2; pass++) {
				for (var j = 0; j < lits.length; j++) {
					var a = lits[j]();
					op(a, j & 7);
					out.push(a.join() + '/' + lits[j]().join());
				}
			}
			return out.join(';');
		}
		function probe() { return [1, 2, 3]; }`)
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		both := jitBothForTest(t, src.String())
		for _, r := range []*Runtime{both.want, both.jit} {
			v, err := r.Run(compileForTest(t, `probe()`))
			if err != nil {
				t.Fatal(err)
			}
			if s, _ := sharedElemsForTest(v); !s {
				t.Fatalf("a literal's array is not shared in %p", r)
			}
		}
		for range 3 {
			for _, name := range names {
				both.agree(t, fmt.Sprintf("run(%s, %s_lits)", name, name))
			}
		}
	}
}

// Native code makes a literal of constants' arrays as the tree tier does,
// over its shared storage, taking them whole from a pool (ssa's
// LiteralSite.Shared): reads of an array that does not escape, its
// length and elements, are what the literal holds; writes give the array
// a copy, and the site, once it has given sharing up, gets arrays of its
// own from the pool, as whole. Each answer is the interpreter's, with the
// collector idle and marking.
func TestJITMakesSharedLiteral(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	const setup = `
		function mk() { return [1, 2.5, 'x', true, null, 7]; }
		function reads() { var s = 0; for (var i = 0; i < 3000; i++) { var a = mk(); s += a.length + a[5] + (a[i % 6] === null ? 1 : 0); } return s; }
		function local() { var s = 0; for (var i = 0; i < 3000; i++) { var a = [4, 5, 6]; s += a[0] + a[2] + a.length; } return s; }
		function lenOnly() { var s = 0; for (var i = 0; i < 3000; i++) s += [9, 8, 7, 6].length; return s; }
		function writes() {
			var out = [];
			for (var i = 0; i < 300; i++) {
				var a = mk();
				if (i % 50 == 0) { a[i % 6] = i; a.push(i); }
				out.push(a.join());
			}
			return out.join(';') + '/' + mk().join();
		}
		function kept() { var all = []; for (var i = 0; i < 500; i++) all.push(mk()); all[3][0] = 'w'; return all[2].join() + '/' + all[3].join() + '/' + all[499].join(); }`
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		both := jitBothForTest(t, setup)
		for range 4 {
			both.agree(t, `reads()`, `local()`, `lenOnly()`, `kept()`)
		}
		// The arrays native code made, before any is written.
		r := both.jit
		v, err := r.Run(compileForTest(t, `mk()`))
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := sharedElemsForTest(v); !s {
			t.Error("native code's array of a literal of constants does not share them")
		}
		var native bool
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == "reads" && x.ssa != nil {
				for _, l := range x.literals {
					native = native || l.shared
				}
			}
		}
		if !native {
			t.Error("reads' code takes no shared literal's arrays")
		}
		for range 4 {
			both.agree(t, `writes()`, `kept()`)
		}
	}
}

// A sort native code makes of an array literal's shared elements (ssa's
// sortCall, jitSortCopy) writes them back to a copy of the array's own,
// and the literal's other arrays are as they were. Each answer is the
// interpreter's.
func TestJITSortsSharedLiteral(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	var src strings.Builder
	src.WriteString("var lits = [")
	for k := range 200 {
		fmt.Fprintf(&src, "function () { return [5, 3, 8, 1, 9, 2, 7, %d]; },", k)
	}
	src.WriteString(`];
		function sortLit(a) { a.sort((x, y) => x - y); return a }
		function drive() {
			var out = [];
			for (var j = 0; j < lits.length; j++) {
				var a = lits[j]();
				sortLit(a);
				out.push(a.join() + '/' + lits[j]().join());
			}
			return out.join(';');
		}`)
	both := jitBothForTest(t, src.String())
	for range 6 {
		both.agree(t, `drive()`)
	}
	r := both.jit
	native := false
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && (fn.Name == "sortLit" || fn.Name == "drive") && x.ssa != nil && x.loops {
			native = true
		}
	}
	if !native {
		t.Fatal("no sort of a shared literal's array made natively")
	}
}

// A keyed store whose site has met an ordinary object (GoKeyedWrite
// feedback, the backend's keyedStore) stores an array's element natively,
// and an array literal's shared elements not: the array gets a copy of its
// own, and the literal's next arrays are as they were. arm64's keyedStore
// had no test of the flag. Each answer is the interpreter's, with the
// collector idle and marking.
func TestJITKeyedStoreSharedLiteral(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	var src strings.Builder
	src.WriteString("var lits = [")
	for k := range 60 {
		fmt.Fprintf(&src, "function () { return [1, 2, 3, %d]; },", k)
	}
	src.WriteString(`];
		function set(o, i, v) { o[i] = v; }
		function drive() {
			var plain = {};
			for (var n = 0; n < 5000; n++) set(plain, n & 3, n);
			var out = [];
			for (var j = 0; j < lits.length; j++) {
				var a = lits[j]();
				set(a, j & 3, 'x');
				out.push(a.join() + '/' + lits[j]().join());
			}
			return out.join(';');
		}`)
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		both := jitBothForTest(t, src.String())
		for range 6 {
			both.agree(t, `drive()`)
		}
	}
}
