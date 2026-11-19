//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"
)

// sameRefSrc stores pointers over themselves -- an object's property its
// own value again, an array's element its own, a binding the object it
// holds -- beside stores that change them, in a loop the JIT compiles.
const sameRefSrc = `
var a = {v: 1}, b = {v: 2}, c = {v: 3};
function same(o, arr, n) {
	let s = 0, x = a;
	for (let i = 0; i < n; i++) {
		o.p = o.p;
		arr[i & 3] = arr[i & 3];
		o.q = (i & 1) ? b : c;
		x = (i & 7) ? x : o.p;
		s += o.p.v * 10 + o.q.v + arr[i & 3].v + x.v;
	}
	return s;
}
function run(n) {
	const o = {p: a, q: null}, arr = [a, b, c, a];
	const s = same(o, arr, n);
	return s + ":" + (o.p === a) + (o.q === b) + (arr[0] === a) + (arr[1] === b) + (arr[2] === c) + (arr[3] === a);
}`

// The JIT's barrier routine goes straight back from a pointer stored over
// itself (refStoringCall), the collector marking or not: the slot holds the
// same pointer before and after, so no edge of the heap is made or broken,
// and nothing needs shading. Native code storing the same pointers while
// the collector marks -- its barrier calls clobbering every register they
// may (jitClobberStoresForTest) -- answers as the interpreter does, and
// leaves every object where it was, through collections.
func TestJITSameRefStoreChangesNoEdge(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const n = 20000
	// s: o.p.v*10 + o.q.v + arr[i&3].v + x.v, where o.p is a throughout,
	// o.q c at even i and b at odd ones, arr's elements a, b, c, a in turn,
	// and x a throughout: 10n + 3n/2 + 2n/2 + 7n/4 + n.
	const want = "305000:truetruetruetruetruetrue"
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
				jitClobberStoresForTest(t)
			}
			rts := regNativeRuntimes(t, sameRefSrc)
			rts[0].SetJITWarmUp(1, 1)
			for round := range 6 {
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
					v, err := r.Call(fn, Undefined, []Value{Float(n)})
					if got := regNativeShow(r, v, err); got != want {
						t.Fatalf("round %d, JIT %v: run = %q, want %q", round, i == 0, got, want)
					}
				}
				runtime.GC()
			}
			if jitSSACodeForTest(rts[0], "same") == nil {
				t.Fatal("same not compiled")
			}
			if rts[0].RegNativeStats().Entries == 0 {
				t.Error("no native code entered")
			}
		})
	}
}
