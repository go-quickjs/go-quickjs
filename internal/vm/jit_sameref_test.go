//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
)

// sameRefSrc stores pointers over themselves -- an object's property its
// own value again, an array's element its own, a binding the object it
// holds -- beside stores that change them, in a loop the JIT compiles;
// sameOnly stores none but array elements over themselves.
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
function sameOnly(arr, n) {
	for (let i = 0; i < n; i++) {
		arr[i & 3] = arr[i & 3];
	}
	return arr[0].v + arr[1].v;
}
function run(n) {
	const o = {p: a, q: null}, arr = [a, b, c, a];
	const s = same(o, arr, n);
	return s + ":" + (o.p === a) + (o.q === b) + (arr[0] === a) + (arr[1] === b) + (arr[2] === c) + (arr[3] === a);
}
function runOnly(n) {
	return sameOnly([c, a, b, c], n);
}`

// A pointer stored over itself goes through Go's write barrier while the
// collector marks, as any other pointer does: the JIT's barrier routine
// (refStoringCall) goes straight back from a nil over a nil alone. That a
// slot holds a pointer does not say the collector has shaded it -- native
// code writes some pointer words with no barrier by design, a native call's
// operands into its callee's frame among them, and an exit's records, a
// store into a slot holding the pointer already among them, are what shade
// what it holds where the collector does not look. With stores over
// themselves skipped, AWFY's Havlak, the collector run often, met an object
// the collector had freed in the VM's stack (GODEBUG=gccheckmark=1 found it
// unmarked there in a third of its runs).
//
// Native code storing the same pointers while the collector marks answers
// as the interpreter does, through collections, with its barrier calls
// clobbering every register they may (jitClobberStoresForTest); and every
// one of sameOnly's stores, each of an element over itself, reaches the
// barrier (jit.StoreRefCounting).
func TestJITSameRefStoreShades(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const n = 20000
	// s: o.p.v*10 + o.q.v + arr[i&3].v + x.v, where o.p is a throughout,
	// o.q c at even i and b at odd ones, arr's elements a, b, c, a in turn,
	// and x a throughout: 10n + 3n/2 + 2n/2 + 7n/4 + n.
	const want, wantOnly = "305000:truetruetruetruetruetrue", "4"
	for _, mode := range []string{"idle", "marking", "counted"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "marking":
				jitMarkingForTest(t)
				jitClobberStoresForTest(t)
			case "counted":
				jitMarkingForTest(t)
				jitTestSwitch(t, func(jc *jitTestConfig) { jc.storeRef = jit.StoreRefCounting() })
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
					fn, _ = r.GetProp(Obj(r.Global()), r.Intern("runOnly"))
					calls := jit.StoreRefCalls()
					v, err = r.Call(fn, Undefined, []Value{Float(n)})
					calls = jit.StoreRefCalls() - calls
					if got := regNativeShow(r, v, err); got != wantOnly {
						t.Fatalf("round %d, JIT %v: runOnly = %q, want %q", round, i == 0, got, wantOnly)
					}
					if mode == "counted" && i == 0 && round == 5 && calls < n {
						t.Errorf("sameOnly's %d stores of an element over itself, natively while the collector marks: %d reached Go's write barrier", n, calls)
					}
				}
				runtime.GC()
			}
			for _, name := range []string{"same", "sameOnly"} {
				if jitSSACodeForTest(rts[0], name) == nil {
					t.Fatalf("%s not compiled", name)
				}
			}
			if rts[0].RegNativeStats().Entries == 0 {
				t.Error("no native code entered")
			}
		})
	}
}
