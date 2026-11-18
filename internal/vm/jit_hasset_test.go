//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// The in operator's cache (hasCache) holds four answers to a set, the
// newest first, so that the four keys of one layout prop_in_keyed asks in
// turn keep their entries wherever their addresses put them: with two to a
// set, three of them had shared one in a run -- by the strings' and the
// layout's addresses -- and every visit of them called Go, 72 calls of 96
// turns, and none in the run beside it while the collector marked
// (TestJITSameWorkWhileMarking). Its four answers each have an entry its
// code finds: no call of Go, and the same exits, while the collector marks
// or not.
func TestJITInKeyedSameWorkWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `var global_res;
function prop_in_keyed(n)
{
    var obj = { a: 1, b: 2, c: 3 }, keys = ["a", "z", "c", "y"], j, s = 0;
    for (j = 0; j < n; j++) {
        if (keys[j & 3] in obj) s++;
    }
    global_res = s;
    return n;
}`
	var work [2]jitMarkingWork
	for i, marking := range []bool{false, true} {
		func() {
			defer jitMarkingHeldForTest()()
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			fn, err := r.GetProp(Obj(r.Global()), r.Intern("prop_in_keyed"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			jitGoCallWork = func() { calls++ }
			defer func() { jitGoCallWork = nil }()
			for range 2 {
				if res := r.RegNativeCall(fn, []Value{Float(32)}, false); res.Err != nil {
					t.Fatal(res.Err)
				}
			}
			calls = 0
			st0 := r.RegNativeStats()
			if res := r.RegNativeCall(fn, []Value{Float(96)}, false); res.Err != nil {
				t.Fatal(res.Err)
			}
			st := r.RegNativeStats()
			got, err := r.GetProp(Obj(r.Global()), r.Intern("global_res"))
			if err != nil || got.Number() != 48 {
				t.Fatalf("marking %v: global_res %v, %v; want 48", marking, got, err)
			}
			if st.Entries == st0.Entries {
				t.Fatalf("marking %v: prop_in_keyed did not run natively", marking)
			}
			work[i] = jitMarkingWork{calls: uint64(calls), hosts: st.Hosts - st0.Hosts, interpreted: st.Interpreted - st0.Interpreted}
			if calls != 0 {
				t.Errorf("marking %v: %d calls of Go in 96 turns, want none", marking, calls)
			}
		}()
	}
	if work[0] != work[1] {
		t.Errorf("%+v, and while marking %+v", work[0], work[1])
	}
}

// Keys made by concatenation -- of 64 bytes or more (ropeThreshold) -- are
// ropes until something reads their text. A key's first in reaches Go
// (hasQuick), which atomizes it -- toPropertyName flattens it in place --
// before the answer is noted, and the next in of the same string finds it
// there, by its pointer, in native code and in the interpreter's handler
// alike.
// (A key made anew at each in is a new string, which no entry, noted by
// the string's pointer, ever answers.) Twenty such keys of one layout:
// none called Go once each had been asked.
func TestJITInRopeKeys(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `var global_res, keys = [], obj = {}
var prefix = "a_property_name_prefix_long_enough_that_its_concatenations_are_ropes_"
for (var i = 0; i < 20; i++) { keys.push(prefix + i); if (i % 3) obj[prefix + i] = i }
function inRopes(n) { var s = 0; for (var j = 0; j < n; j++) if (keys[j % 20] in obj) s++; global_res = s; return n }`)); err != nil {
		t.Fatal(err)
	}
	ka, err := r.GetProp(Obj(r.Global()), r.Intern("keys"))
	if err != nil {
		t.Fatal(err)
	}
	ropes := 0
	for _, k := range ka.Object().elemsOf() {
		if k.String().left != nil {
			ropes++
		}
	}
	if ropes == 0 {
		t.Fatal("no key is a rope: the test asks nothing of ropes")
	}
	fn, err := r.GetProp(Obj(r.Global()), r.Intern("inRopes"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	for range 2 {
		if res := r.RegNativeCall(fn, []Value{Float(40)}, false); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	calls = 0
	if res := r.RegNativeCall(fn, []Value{Float(200)}, false); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, err := r.GetProp(Obj(r.Global()), r.Intern("global_res")); err != nil || got.Number() != 130 {
		t.Fatalf("global_res %v, %v; want 130", got, err)
	}
	if calls != 0 {
		t.Errorf("%d calls of Go in 200 turns over %d keys made as ropes, want none", calls, ropes)
	}
}

// Four keys of one layout whose addresses put them in one set of hasCache
// each keep their entry: asked in turn, none calls Go once each has been
// asked -- a loop of 400 turns over them calls it as often as one over
// one key. A fifth key
// of the set takes the newest place, and the oldest of the four -- the
// first noted -- leaves.
func TestJITHasCacheFourWays(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `var global_res, obj = { a: 1, b: 2, c: 3 }, keys = []
for (var i = 0; i < 4000; i++) keys.push("key" + i)
function inSome(ks, n) { var s = 0; for (var j = 0; j < n; j++) if (ks[j % ks.length] in obj) s++; global_res = s; return n }`)); err != nil {
		t.Fatal(err)
	}
	global := func(name string) Value {
		v, err := r.GetProp(Obj(r.Global()), r.Intern(name))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	sh := r.ensureShape(global("obj").Object())
	keys := global("keys").Object().elemsOf()
	sets := map[uint64][]int{}
	var five []int
	for i, k := range keys {
		set := hasSetOf(k.ref, sh)
		if sets[set] = append(sets[set], i); len(sets[set]) == 5 {
			five = sets[set]
			break
		}
	}
	if five == nil {
		t.Fatal("no five keys share a set")
	}
	idx := make([]string, len(five))
	for i, k := range five {
		idx[i] = fmt.Sprintf("keys[%d]", k)
	}
	if _, err := r.Run(compileForTest(t, `var pick1 = [`+idx[0]+`], pick4 = [`+strings.Join(idx[:4], ", ")+`], pick5 = [`+strings.Join(idx, ", ")+`]`)); err != nil {
		t.Fatal(err)
	}
	fn := global("inSome")
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	run := func(pick string, n int) {
		t.Helper()
		if res := r.RegNativeCall(fn, []Value{global(pick), Float(float64(n))}, false); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	for range 2 {
		run("pick4", 40)
		run("pick1", 40)
	}
	// What a run of the loop makes besides its ins, which one key, whose
	// entry nothing displaces, measures: the four keys' ins make no more.
	calls = 0
	run("pick1", 400)
	base := calls
	calls = 0
	run("pick4", 400)
	if calls != base {
		t.Errorf("four keys of one set: %d calls of Go in 400 turns, one key %d, want as many", calls, base)
	}
	set := hasSetOf(keys[five[0]].ref, sh)
	ways := r.has[set*hasCacheWays : (set+1)*hasCacheWays]
	for w, i := range []int{3, 2, 1, 0} {
		if ways[w].key != keys[five[i]].ref {
			t.Fatalf("way %d holds another key than the %dth noted", w, i+1)
		}
	}
	run("pick5", 5)
	if ways[0].key != keys[five[4]].ref {
		t.Error("the fifth key is not the newest of its set")
	}
	for w := range ways {
		if ways[w].key == keys[five[0]].ref {
			t.Errorf("the oldest key is still in way %d", w)
		}
	}
	for w, i := range []int{3, 2, 1} {
		if ways[w+1].key != keys[five[i]].ref {
			t.Errorf("way %d holds another key than the %dth noted", w+1, i+1)
		}
	}
}
