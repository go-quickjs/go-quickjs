//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"strings"
	"testing"
)

// jitEndedForTest is how many runs of r's register code ended in the
// interpreter for a reason starting with prefix (jitState's regEnded).
func jitEndedForTest(r *Runtime, prefix string) uint64 {
	n := uint64(0)
	for k, c := range r.jit.regEnded {
		if strings.HasPrefix(k, prefix) {
			n += c
		}
	}
	return n
}

// A callee's object literal, inlined in a loop, defines its fields natively
// as the root's does (ssa's define): its definitions were asked of the
// root's frame alone, so that the callee inlined left native code at its
// first field at every call -- microbench's args_read, whose loop's first
// code inlined extra_args_list, left at each of its turns until the
// inlining was given up. The loop runs natively from its one entry, by
// on-stack replacement, with no exit. Each answer is Node's.
func TestJITInlinedLiteralDefines(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	r, want := jitAnswersForTest(t, `
		function extra_args_list() {
			var r = null, i;
			for (i = arguments.length - 1; i >= 0; i--)
				r = {car: arguments[i], cdr: r};
			return r;
		}
		function args_read(n) {
			var j, s = 0;
			for (j = 0; j < n; j++)
				s = (s + extra_args_list(j, 1, 2).cdr.car) | 0;
			return s;
		}`)
	r.jit.reporting = true
	// Each answer is found twice (jitTierRuntimeForTest): two calls.
	want(`args_read(300000)`, "300000")
	e := jitNativeForTest(t, r, "args_read")
	if st := e.ssaStats; st.entries != 2 || st.hosts != 0 || st.guards != 0 {
		t.Fatalf("args_read's loop entered %d times in two calls, %d host exits, %d guard failures; want one entry a call and no exit", st.entries, st.hosts, st.guards)
	}
	if n := jitEndedForTest(r, "inlined callee left"); n != 0 {
		t.Fatalf("the inlined extra_args_list left native code %d times", n)
	}
	if n := jitEndedForTest(r, "slow loop"); n != 0 {
		t.Fatalf("args_read's loop judged slow %d times", n)
	}
	// The literal's fields as the interpreter defines them: their order,
	// and an object of the literal's shape whatever made it.
	want(`JSON.stringify(extra_args_list(1, 2))`, `{"car":1,"cdr":{"car":2,"cdr":null}}`)
	want(`Object.keys(extra_args_list(7)).join()`, "car,cdr")
}
