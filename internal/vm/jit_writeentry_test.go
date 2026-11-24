//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// jitEntriesForTest counts the register pcs of the global function name's
// code that native code may be entered at.
func jitEntriesForTest(t *testing.T, r *Runtime, name string) int {
	t.Helper()
	cl := jitGlobalClosureForTest(r, name)
	e := jitRegOfForTest(r, cl)
	if e == nil || e.code == nil || e.invalid {
		t.Fatalf("%s has no valid code", name)
	}
	n := 0
	for rpc := range cl.reg.rc.Code {
		if ipc := e.ipc(rpc); ipc >= 0 && e.code.HasEntry(ipc) {
			n++
		}
	}
	return n
}

// An element's write has no entry after it until Go has gone on there, as
// a call of a function has none (ssa.Func's Unresumed): a loop of element
// updates, tab[k] += j ten times a turn, is one block from its header to
// its back edge, entered at its start and at its header alone, and leaves
// nowhere. A write native code does not make -- to an element with a
// setter -- leaves for Go, which goes on after it in code compiled again
// with the entry there (jitRegResumeSeen), and runs end in the interpreter
// no more. Each answer is the interpreter's.
func TestJITElementWritesHaveNoEntryUntilResumed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	holdCollector(t)
	both := jitBothForTest(t, `
		var calls = 0
		function update(n) {
			var tab = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]
			for (var j = 0; j < n; j++) {
				tab[0] += j; tab[1] += j; tab[2] += j; tab[3] += j; tab[4] += j
				tab[5] += j; tab[6] += j; tab[7] += j; tab[8] += j; tab[9] += j
			}
			return tab.join()
		}
		function setter(n, tab) {
			var s = 0
			for (var j = 0; j < n; j++) {
				tab[0] = j
				tab[1] = j
				s += tab[0]
			}
			return s + ':' + calls
		}
		function plain() { return [0, 0] }
		function trapped() {
			var a = [0, 0]
			Object.defineProperty(a, 1, { set(v) { calls++ }, get() { return 7 } })
			return a
		}`)
	for range 3 {
		both.agree(t, `update(1000)`)
	}
	r := both.jit
	e := jitNativeForTest(t, r, "update")
	hosts, guards, ended := e.ssaStats.hosts, e.ssaStats.guards, e.ssaStats.ended
	both.agree(t, `update(1000)`)
	if n := jitEntriesForTest(t, r, "update"); n != 2 {
		t.Errorf("update's code has %d entries, want 2: its start and its loop's header", n)
	}
	if h, g, x := e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.ssaStats.ended-ended; h != 0 || g != 0 || x != 0 {
		t.Errorf("update left native code: %d host exits, %d guards failed, %d runs ended in the interpreter", h, g, x)
	}

	for range 3 {
		both.agree(t, `setter(200, plain())`)
	}
	before := jitEntriesForTest(t, r, "setter")
	both.agree(t, `setter(200, trapped())`)
	if n := jitEntriesForTest(t, r, "setter"); n != before+1 {
		t.Errorf("setter's code has %d entries after Go went on after its write, want %d", n, before+1)
	}
	// The site learns the setter it met, and its code is compiled again
	// for it; from then on no run ends in the interpreter.
	both.agree(t, `setter(200, trapped())`)
	interpreted := r.jit.interpreted
	both.agree(t, `setter(200, trapped())`, `setter(200, trapped())`)
	if x := r.jit.interpreted - interpreted; x != 0 {
		t.Errorf("setter's runs ended in the interpreter %d times once its write had an entry after it", x)
	}
	both.agree(t, `setter(200, plain())`, `setter(50, trapped())`)
}
