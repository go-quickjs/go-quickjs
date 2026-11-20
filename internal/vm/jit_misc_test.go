//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regFramelessSrc runs operations register code has Go make without a frame
// (jitRegFrameless, jitForInPlain, jitForOfPlain, jitStepPlain): an object
// pattern's rest, of a plain object and of one whose getter the copy calls;
// computed keys of strings, numbers, symbols and an object whose toString
// counts its calls; a literal's __proto__; for...in over a plain object, a
// dictionary and a proxy; for...of over a dense array, one with a hole and
// one that grows while it is walked.
const regFramelessSrc = `
var gets = 0, keys = 0, sym = Symbol("s");
function rest(n) {
	var plain = { a: 1, b: 2, c: 3, d: 4 }, getter = { a: 1, get b() { gets++; return 5 }, c: 3 }, s = 0, r;
	for (var j = 0; j < n; j++) {
		var a, b;
		({ a, ...r } = j % 50 == 49 ? getter : plain);
		s += r.b + r.c + (r.d | 0);
		({ a, b, ...r } = plain);
		s += r.c;
	}
	return s + ":" + gets;
}
function computed(n) {
	var ks = ["x", "y", 3, sym], s = 0, kobj = { toString() { keys++; return "w" } };
	for (var j = 0; j < n; j++) {
		var k = ks[j & 3], o = { [k]: j, v: 1 };
		s += o[k] + o.v;
		if (j % 100 == 99) s += { [kobj]: 2 }.w;
	}
	return s + ":" + keys;
}
function protoLit(n) {
	var s = 0;
	for (var j = 0; j < n; j++) {
		var o = { __proto__: j % 7 == 6 ? null : { z: j }, w: 1 };
		s += (o.z | 0) + o.w;
	}
	return s;
}
function forin(n) {
	var plain = { a: 1, b: 2, c: 3 }, dict = { z: 0, a: 1, b: 2, c: 3 }, s = 0, k;
	var px = new Proxy({ p: 1, q: 2 }, { ownKeys(t) { s += 1000; return Reflect.ownKeys(t) } });
	delete dict.z;
	for (var j = 0; j < n; j++) {
		for (k in plain) s += plain[k];
		for (k in dict) s += dict[k] * 2;
		if (j % 100 == 0) for (k in px) s += 7;
	}
	return s;
}
function forof(n) {
	var a = [1, 2, 3, 4], h = [1, , 3], s = 0, x;
	for (var j = 0; j < n; j++) {
		for (x of a) s += x;
		for (x of h) s += x === undefined ? 100 : x;
		if (j % 100 == 0) {
			var g = [1];
			for (x of g) if (g.length < 5) g.push(x + 1);
			s += g.length * 1000;
		}
	}
	return s;
}
`

// TestJITRegFrameless runs regFramelessSrc's functions in a runtime whose
// register code makes those operations by Go without a frame and in one
// with no JIT, the collector idle and marking: the answers equal -- the
// getter and the toString called as often -- and none of the operations
// that runs no script left to Go whole or at an exit but the few that do
// run one.
func TestJITRegFrameless(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	wants := map[string]string{
		"rest":     "11980:120",
		"computed": "500520:60",
		"protoLit": "429571",
		"forin":    "28140",
		"forof":    "164000",
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regFramelessSrc)
			rts[0].SetJITWarmUp(1, 1)
			rts[0].jitStress = jitStressConfig{}
			rts[0].SetJITReport(true)
			for _, name := range []string{"rest", "computed", "protoLit", "forin", "forof"} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 6 {
						v, err := r.Call(fn, Undefined, []Value{Float(1000)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
				if got[1] != wants[name] {
					t.Errorf("%s = %q, want %q", name, got[1], wants[name])
				}
			}
			st := rts[0].RegNativeStats()
			t.Logf("calls of Go %v, hosts %v", st.GoCallOps, st.HostOps)
			// Made whole only where they may run script: the getter's rest,
			// the key whose toString runs, the proxy's cursor.
			for op, most := range map[string]uint64{
				"whole object_rest": 200, "whole to_property_key": 200, "whole define_index": 200,
				"whole set_proto_of": 0, "whole for_in_start": 200, "whole for_of_start": 0,
			} {
				if n := st.GoCallOps[op]; n > most {
					t.Errorf("%s: %d calls of Go with a frame, want at most %d", op, n, most)
				}
			}
			for _, op := range []string{"object_rest", "to_property_key", "define_index", "set_proto_of", "for_in_start", "for_of_start"} {
				if n := st.HostOps[op]; n > 100 {
					t.Errorf("%s left to Go %d times", op, n)
				}
			}
		})
	}
}

// regBigFirstSrc is BigInt arithmetic the interpreter marks for the first
// compile (regBigOf): xorshift's wrapped shifts, int64 arithmetic and
// division, and numbers past 64 bits.
const regBigFirstSrc = `
function xorshift(n) {
	var x = 88172645463325252n;
	for (var i = 0; i < n; i++) {
		x ^= BigInt.asUintN(64, x << 13n);
		x ^= x >> 7n;
		x ^= BigInt.asUintN(64, x << 17n);
	}
	return String(x);
}
function arith(n) {
	var s = 0n, d = 0n;
	for (var i = 0; i < n; i++) {
		var b = BigInt(i);
		s += b * 3n - 1n;
		d = (d + s / 7n) % 1000003n;
	}
	return s + ":" + d;
}
function wide(n) {
	var a = 1n << 70n, s = 0n;
	for (var i = 0; i < n; i++) s = (s + a * BigInt(i)) % 1000000007n;
	return String(s);
}
`

// TestJITRegBigIntFirstCompile runs regBigFirstSrc's functions in a
// runtime whose register code knows from its first compile the BigInts the
// interpreter met (regBigOf), and in one with no JIT, the collector idle
// and marking: the answers equal, xorshift's code compiled with no guard
// of it failing, and arith's division and remainder marked.
func TestJITRegBigIntFirstCompile(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	wants := map[string]string{
		"xorshift": "14004281193724023956",
		"arith":    "5995000:140002",
		"wide":     "485968666",
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regBigFirstSrc)
			rts[0].jitStress = jitStressConfig{}
			jitDefaultsForTest(rts[0])
			for _, name := range []string{"xorshift", "arith", "wide"} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 20 {
						v, err := r.Call(fn, Undefined, []Value{Float(2000)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] || got[1] != wants[name] {
					t.Errorf("%s: with the JIT %q, without %q, want %q", name, got[0], got[1], wants[name])
				}
				r := rts[0]
				cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
				e := r.jit.regCache[r.jitKeyOf(cl.fn)]
				if e == nil || e.code == nil || e.invalid {
					t.Fatalf("%s has no register code", name)
				}
				switch name {
				case "xorshift":
					// Its shifts wrapped and its xors of Uint64s from the
					// first compile: no guard failed.
					if l := r.jit.regLearnedOf(r.jitRegKeyOf(cl)); len(l.failed) != 0 {
						t.Errorf("%s: guards failed at register pcs %v", name, l.failed)
					}
				case "arith":
					// Its division and remainder, which the interpreter makes
					// in Go, marked as of BigInts in the int64 range.
					marked := 0
					for pc, w := range cl.reg.rc.Code {
						if big, wide, heap := regBigSeen(cl, cl.reg.rc, pc); w.Op() == bytecode.RBin && big && !wide && !heap {
							marked++
						}
					}
					if marked != 2 {
						t.Errorf("%s: %d of its two bin operators marked as BigInts'", name, marked)
					}
				}
			}
		})
	}
}

// regLoopSlowSrc is loops whose turns leave native code: using
// declarations' resources (made whole now, never left at) and a downward
// fill of an array past its length, whose stores to the sparse array
// leave at each turn, which the interpreter then runs (loopSlow).
const regLoopSlowSrc = `
var res = { count: 0 };
res[Symbol.dispose] = function () { this.count++ };
function using(n) {
	var i, s = 0;
	res.count = 0;
	for (i = 0; i < n; i++) {
		using u = res;
		s = (s + i) | 0;
	}
	return s + ":" + res.count;
}
function fillDown(n) {
	var i, j, a, s = 0, len = 4096;
	for (j = 0; j < n; j += len) {
		a = [];
		for (i = len - 1; i >= 0; i--) a[i] = i;
		for (i = 0; i < len; i++) s += a[i];
	}
	return s + ":" + a.length;
}
`

// TestJITRegLoopsLeaving runs regLoopSlowSrc's functions in a runtime with
// register code and in one with no JIT, the collector idle and marking:
// the answers equal; a using declaration's operations are calls of Go
// that never leave; and the downward fill's loop, its stores left to Go,
// is left to the interpreter once judged not to pay, not gone on in
// natively after each store.
func TestJITRegLoopsLeaving(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	wants := map[string]string{"using": "33550336:8192", "fillDown": "16773120:4096"}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regLoopSlowSrc)
			rts[0].jitStress = jitStressConfig{}
			jitDefaultsForTest(rts[0])
			rts[0].SetJITReport(true)
			for _, name := range []string{"using", "fillDown"} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 20 {
						v, err := r.Call(fn, Undefined, []Value{Float(8192)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] || got[1] != wants[name] {
					t.Errorf("%s: with the JIT %q, without %q, want %q", name, got[0], got[1], wants[name])
				}
			}
			st := rts[0].RegNativeStats()
			t.Logf("hosts %d %v, entries %d, interpreted %d, ended %v", st.Hosts, st.HostOps, st.Entries, st.Interpreted, st.Ended)
			for _, op := range []string{"new_dispose_capability", "add_disposable", "dispose"} {
				if n := st.HostOps[op]; n > 100 {
					t.Errorf("%s left to Go %d times", op, n)
				}
			}
			// 20 calls of 2 fills of 4096 stores, those to a sparse array
			// Go's: a run's worth of them, 64, left at for each probe
			// before the loop is judged not to pay -- the work the code did
			// natively before, in the fills' other loops, wears off at 12 a
			// store -- which the run then leaves (Ended's slow loop), and
			// as many again each time a loop's checks of the code retry it
			// (jitRegRetrySlow, counting them as calls): 2624 with the loop
			// compiled at a sample, 3072 compiled where its count of back
			// edges spent its budget (jitRegEdges), which then ran natively
			// longer before; less than a fill's stores, against 108,863
			// where the run went on natively after each, never judged.
			slow := uint64(0)
			for k, n := range st.Ended {
				if strings.HasPrefix(k, "slow loop at ") && strings.HasSuffix(k, " in fillDown") {
					slow += n
				}
			}
			if slow == 0 {
				t.Errorf("fillDown's loop never left for the interpreter once judged slow: %v", st.Ended)
			}
			if n := st.HostOps["set_index"]; n >= 4096 {
				t.Errorf("set_index left to Go %d times, the loop going on natively after each", n)
			}
		})
	}
}

// TestJITRegCallSettles runs a loop of a built-in's calls of Go in a
// runtime with register code: the call is settled as one Go makes
// (jitRegCallSettle) after jitCallsToInline of them, its learning asked
// no more, and the answers are the interpreter's.
func TestJITRegCallSettles(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function keys(n) {
	var o = { a: 1, b: 2, c: 3 }, s = 0;
	for (var j = 0; j < n; j++) s += Object.keys(o).length + JSON.stringify(j).length;
	return s;
}`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, src)
			rts[0].SetJITWarmUp(1, 1)
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("keys"))
				for range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(1000)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] || got[1] != "5890" {
				t.Fatalf("keys: with the JIT %q, without %q, want 5890", got[0], got[1])
			}
			r := rts[0]
			cl := r.global.getOwn(r.atoms.intern("keys")).value().Object().fn().closure
			e := r.jit.regCache[r.jitKeyOf(cl.fn)]
			if e == nil || e.life == nil {
				t.Fatal("keys has no register code")
			}
			done := 0
			for _, st := range e.life.callSites {
				if st == jitCallDone {
					done++
				}
			}
			if done < 2 {
				t.Errorf("%d calls settled, want the two built-ins' (call sites %v)", done, e.life.callSites)
			}
		})
	}
}
