//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitCreateSrc's mixed calls Object.create of a prototype the pool's
// objects are made of, of null, of an array, with properties, of a
// prototype made anew, the prototype given a getter and another prototype
// of its own halfway; shared makes the objects of one prototype a call
// makes. jitCreateWant is Node's answer (vm.runInThisContext), mixed's and
// shared's.
const jitCreateSrc = `
function mixed(n) {
	var p = { x: 1 }, q = [5], s = 0, o;
	for (var i = 0; i < n; i++) {
		switch (i % 6) {
		case 0: o = Object.create(p); break;
		case 1: o = Object.create(null); break;
		case 2: o = Object.create(q); break;
		case 3: o = Object.create(p, { z: { value: i, enumerable: true } }); break;
		case 4: o = Object.create(i % 12 == 4 ? p : { x: 3 }); break;
		default: o = Object.create(p);
		}
		if (i == n >> 1) {
			Object.defineProperty(p, "y", { get() { return 100 }, configurable: true });
			Object.setPrototypeOf(p, { w: 7 });
		}
		o.k = i;
		s += (o.x | 0) + (o.y | 0) + (o.z | 0) + (o.w | 0) + (o.length | 0) + o.k + (Object.getPrototypeOf(o) === p ? 1 : 0);
	}
	delete p.y;
	Object.setPrototypeOf(p, Object.prototype);
	return s + ":" + Object.keys(o).join();
}
function shared(n) { var proto = { x: 1 }, s = 0, o; for (var j = 0; j < n; j++) { o = Object.create(proto); o.y = j; s += o.x + o.y } return s }
var protos = [];
for (var i = 0; i < 8; i++) protos.push({ x: i });
function rotate(n) { var s = 0, o; for (var j = 0; j < n; j++) { o = Object.create(protos[j & 7]); o.y = j; s += o.x + o.y } return s }
`

const jitCreateWant = "106311:z,k 80200 81200"

// TestJITObjectCreatePool runs jitCreateSrc's functions with the JIT and
// without, the collector idle and as while it marks: Node's answers, and
// once warm, shared's objects made natively from its call's pool
// (jit_create.go), the leaf called only to give the pool the call's
// prototype, to learn its objects' room, and to fill it each time it runs
// out -- PoolSize objects, twice as many each time after, up to
// PoolCapacity -- where each had been the leaf's. rotate's objects, of
// eight prototypes in turn, are the leaf's, its site's pool given up:
// empty, of no prototype.
func TestJITObjectCreatePool(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const n = 400
	// The leaf's calls in a call of shared once warm: the first gives the
	// pool the call's prototype, the second's object teaches its room, and
	// each filling after makes what the turns left need.
	leaves, made, size := 2, 0, abi.PoolSize/2
	for made < n-leaves+1 {
		size = min(2*size, abi.PoolCapacity)
		made += size
		leaves++
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, jitCreateSrc)
			rts[0].SetJITWarmUp(1, 1)
			call := func(r *Runtime, name string) string {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				v, err := r.Call(fn, Undefined, []Value{Float(n)})
				return regNativeShow(r, v, err)
			}
			for k := range 12 {
				for i, r := range rts {
					if got := call(r, "mixed") + " " + call(r, "shared") + " " + call(r, "rotate"); got != jitCreateWant {
						t.Fatalf("runtime %d, call %d: %s, want %s", i, k, got, jitCreateWant)
					}
				}
			}
			r := rts[0]
			before, entries := r.jit.leafFast, r.RegNativeStats().Entries
			if got := call(r, "shared"); got != "80200" {
				t.Fatalf("shared: %s, want 80200", got)
			}
			if r.RegNativeStats().Entries == entries {
				t.Fatal("shared not run natively once warm")
			}
			if got := r.jit.leafFast - before; got != uint64(leaves) {
				t.Errorf("Object.create's leaf called %d times in %d turns, want %d", got, n, leaves)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("rotate"))
			e := jitEntryOfForTest(r, fn.Object().fn().closure)
			if e == nil || len(e.creates) != 1 {
				t.Fatalf("rotate: no pool of its Object.create's (%v)", e != nil)
			}
			if p := e.creates[0].pool; p.Proto != nil || p.Count != 0 {
				t.Errorf("rotate's pool kept: %d objects of %p", p.Count, p.Proto)
			}
		})
	}
}
