//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// RegExp.prototype's exec and test, native leaves (zzz_regexpleaf.go), are
// leaves' calls of native code (ssa.GoLeaf) where their receiver is no
// literal's: loop's exec of a RegExp it holds and its test of one neither
// global nor sticky, and esc's exec, from a native callee of escs's loop,
// are made through the VM's routine for leaves, with the collector idle
// and while it marks, each answer the interpreter's and Node's
// (regexpLeafTestWant). A global pattern's test, which writes lastIndex,
// and a literal's, whose RegExp its call of Go gives back to the literal's
// pool (jitRegExpLitPool), stay calls of Go.
func TestJITRegExpLeaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			rounds = append(rounds, "loop(200)", "escs(200)")
		}
		r := jitAgreesForTest(t, regexpLeafTestSrc, rounds...)
		exec, test := uintptr(unsafe.Pointer(r.regexpExecFn)), uintptr(unsafe.Pointer(r.regexpTestFn))
		for _, c := range []struct {
			name        string
			execs, test int
		}{{"loop", 1, 1}, {"esc", 1, 0}} {
			e := jitNativeForTest(t, r, c.name)
			execs, tests := 0, 0
			for _, st := range e.ssa.GoCallStates() {
				if ssa.GoOp(st.Op&0xff) != ssa.GoLeaf {
					continue
				}
				switch st.Leaf {
				case exec:
					execs++
				case test:
					tests++
				}
			}
			if execs != c.execs || tests != c.test {
				t.Errorf("marking %v: %s's code has %d leaves' calls of exec and %d of test, want %d and %d", marking, c.name, execs, tests, c.execs, c.test)
			}
		}
		for _, c := range []struct {
			src, want string
			leaves    uint64
		}{{"loop(200)", "70347", 400}, {"escs(200)", "q&#x27;s11", 150}} {
			fast := r.jit.leafFast
			v, err := r.Run(compileForTest(t, "''+"+c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.src, got, c.want)
			}
			if got := r.jit.leafFast - fast; got < c.leaves {
				t.Errorf("marking %v: %s made %d leaves' calls through the routine, want at least %d", marking, c.src, got, c.leaves)
			}
		}
		debug.SetGCPercent(gc)
	}
}

// A RegExp the matcher matches is a leaf's call of native code too, for a
// string: find's exec of a pattern with groups is made through the VM's
// routine for leaves; and where the match fails, spending the step budget,
// the leaf's call errs with nothing made, the code leaves the call to Go,
// which makes it with a frame and throws the interpreter's SyntaxError.
func TestJITRegExpLeafMatcher(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var re = /(\w+)@(\w+)\.com/, bad = /(a+)+b/;
function find(n) { var c = 0; for (var i = 0; i < n; i++) { var m = re.exec('mail bob' + (i % 10) + '@example.com now'); c += m[2].length + m.index } return c }
function boom(s) { try { return String(bad.exec(s)) } catch (e) { return e.name + ': ' + e.message } }
function booms(n) { var out = []; for (var i = 0; i < n; i++) out.push(boom(i === n - 1 ? 'a'.repeat(32) + 'cb' : 'aab')); return out[0] + '|' + out[n - 1] + '|' + out.length }`
	var rounds []string
	for range 4 {
		rounds = append(rounds, "find(200)", "booms(100)")
	}
	r := jitAgreesForTest(t, setup, rounds...)
	exec := uintptr(unsafe.Pointer(r.regexpExecFn))
	leaves := 0
	for _, st := range jitNativeForTest(t, r, "find").ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == exec {
			leaves++
		}
	}
	if leaves != 1 {
		t.Fatalf("find's code has %d leaves' calls of exec, want 1", leaves)
	}
	fast := r.jit.leafFast
	v, err := r.Run(compileForTest(t, `''+find(200) + ' ' + booms(100)`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "2400 aab,aa|SyntaxError: regular expression is too complex|100"; got != want {
		t.Errorf("= %s, want %s", got, want)
	}
	if got := r.jit.leafFast - fast; got < 200 {
		t.Errorf("%d leaves' calls through the routine, want at least 200", got)
	}

	// find's exec, of a global's RegExp, is compiled a leaf's call from
	// the first (jitRegExpLeafGuess), not learned at a call of Go and
	// compiled again for.
	r = jitAgreesForTest(t, setup, "find(200)", "find(200)", "find(200)")
	leaves = 0
	for _, st := range jitNativeForTest(t, r, "find").ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == uintptr(unsafe.Pointer(r.regexpExecFn)) {
			leaves++
		}
	}
	if leaves != 1 || r.jit.compiled != 1 {
		t.Errorf("find compiled %d times, its code with %d leaves' calls of exec; want once, with 1", r.jit.compiled, leaves)
	}
}
