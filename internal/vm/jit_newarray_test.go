//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// new Array(n) is made natively, as V8's code allocates a holey array of a
// length it checks: an array from its call's pool (abi.ObjectPool) with
// room for n elements, holes, its length made n. Lengths up to the room
// the pool's arrays have take one -- past abi.PoolSmallRoom, more than
// half of it; another, up to abi.PoolRoomLimit, has Go fill the pool
// again with arrays of its size class (Want), and the call is made again;
// any other -- longer, negative, not an integer, not a number -- is the
// Array constructor's, made by Go. Each
// answer is the interpreter's, with the collector marking too (Go takes
// the array then, and native code makes its length); and the lengths the
// pool has room for are made without a call of Go. AWFY's Storage, whose
// new Array(4) and new Array(1..10) had each been a call of Go, which
// left its recursion slower than the interpreter's, had been judged
// not to pay.
func TestJITNewArrayLength(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function sizes(k, lim){let out='';for(let i=0;i<k;i++){const n=i%lim;const a=new Array(n);a[n>>1]=i;a.push(-i);out+=a.length+','+Object.keys(a).length+','+a.indexOf(i)+';'}return out}
		function fill(k, lim){let s=0;for(let i=0;i<k;i++){const a=new Array(i%lim);s+=a.length}return s}
		function big(k){let s=0;for(let i=0;i<k;i++){s+=new Array(100+i%3).length}return s}
		function odd(x){let out='';for(let i=0;i<3;i++){try{const a=new Array(x);out+=a.length+':'+a[0]+':'+Object.keys(a).length+' '}catch(e){out+=e.name+' '}}return out}`
	cases := []string{`sizes(40, 5)`, `sizes(60, 17)`, `sizes(60, 17)`, `sizes(60, 18)`, `String(fill(100, 17))`, `String(big(50))`,
		`odd(-1)`, `odd(1.5)`, `odd(NaN)`, `odd(2**32)`, `odd("3")`, `odd(-0)`, `odd(100)`, `odd(16)`, `odd(0)`}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		want := newTestRuntime(t, Config{})
		r := jitRuntimeForTest(t, Config{JIT: true})
		for _, rt := range []*Runtime{want, r} {
			if _, err := rt.Run(compileForTest(t, setup)); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 3; round++ {
			for _, c := range cases {
				wv, err := want.Run(compileForTest(t, c))
				if err != nil {
					t.Fatal(err)
				}
				gv, err := r.Run(compileForTest(t, c))
				if err != nil {
					t.Fatalf("marking %v, %s: %v", marking, c, err)
				}
				if got, w := gv.String().Go(), wv.String().Go(); got != w {
					t.Fatalf("marking %v, round %d, %s: got %s, interpreter %s", marking, round, c, got, w)
				}
			}
		}
		want.Close()
		want.ReleaseClosed()
		cl := r.global.getOwn(r.atoms.intern("sizes")).value().Object().fn().closure
		cv, ok := jitViewForTest(r, cl)
		if !ok {
			t.Fatalf("marking %v: sizes was not compiled: %+v", marking, cv.e)
		}
		e := cv.e
		var pool *abi.ObjectPool
		for _, x := range jitTargets(e) {
			if x.obj == r.proto.arrayCtor && x.pool != nil {
				pool = x.pool
			}
		}
		// The room of the last length sizes(60, 18) made, 59 % 18: its
		// pool's arrays had room for 32 since its first 17, and were
		// made again for 0, at most half of it.
		if want := uint64(jitArrayRoom(59 % 18)); pool == nil || pool.Room != want {
			t.Fatalf("marking %v: new Array(n)'s pool %+v, want room %d", marking, pool, want)
		}
		// Lengths up to the room, made without Go: fill's calls of Go are
		// the pool's refills.
		cl = r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
		if cv, ok = jitViewForTest(r, cl); !ok {
			t.Fatalf("marking %v: fill was not compiled: %+v", marking, cv.e)
		}
		e = cv.e
		before := e.ssaStats.goCalls
		v, err := r.Run(compileForTest(t, `fill(1000, 17)`))
		if err != nil || v.Number() != 7979 {
			t.Fatal(v, err)
		}
		if calls := e.ssaStats.goCalls - before; !marking && calls > 1000/abi.PoolSize {
			t.Errorf("new Array(n) of lengths in the pool's room made %d calls of Go in 1000 iterations", calls)
		}
		// A length past 16, of a room of 128 (jitArrayRoom), from its
		// pool too, where it had been the Array constructor's, made by Go
		// at every construction: its calls of Go are the pool's refills.
		cl = r.global.getOwn(r.atoms.intern("big")).value().Object().fn().closure
		if cv, ok = jitViewForTest(r, cl); !ok {
			t.Fatalf("marking %v: big was not compiled: %+v", marking, cv.e)
		}
		e = cv.e
		hosts, before := e.ssaStats.hosts, e.ssaStats.goCalls
		if v, err := r.Run(compileForTest(t, `big(300)`)); err != nil || v.Number() != 30300 {
			t.Fatal(v, err)
		}
		if left := e.ssaStats.hosts - hosts; !marking && left > 2 {
			t.Errorf("new Array(100) left native code %d times in 300 iterations", left)
		}
		if calls := e.ssaStats.goCalls - before; !marking && calls > 300/abi.PoolSize {
			t.Errorf("new Array(100..102) made %d calls of Go in 300 iterations", calls)
		}
		_ = unsafe.Pointer(pool)
	}
}

// `new Array(n)` of a length past what a pool's arrays have room for
// (abi.PoolRoomLimit) is Go's construction, called from native code, which
// goes on after it: it had been an exit at every such construction. A
// negative or too large length throws the interpreter's RangeError there,
// caught. One site's lengths of every size class in turn -- i % 40, 5000,
// 17 -- have its pool filled again for each (Want). Each answer is the
// interpreter's, at every stress setting, and a loop of such
// constructions does not leave native code.
func TestJITNewArrayPastPoolRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function mk(n) { const a = new Array(n); return a.length * 10 + (a[0] === undefined ? 1 : 0) + (1 in a ? 100 : 0) }
		function run(k) { let s = 0; for (let i = 0; i < k; i++) s += mk(i % 40) + mk(5000) + mk(17); return s }
		function bad(n) { try { return new Array(n).length } catch (e) { return e.constructor.name } }
		function runBad(k) { let r = ''; for (let i = 0; i < k; i++) r += bad(i % 3 === 0 ? -1 : i % 3 === 1 ? 2 ** 32 : 20); return r }`
	r := jitAgreesForTest(t, setup, `run(200)`, `run(200)`, `run(200)`, `runBad(30)`, `runBad(30)`)
	m := jitCodeNamedForTest(t, r, "run")
	_, mkLink := jitLinkedForTest(t, r, "mk")
	hosts, unwound, in := r.jit.hosts, r.jit.unwound, mkLink.in
	if v, err := r.Run(compileForTest(t, `''+run(200)`)); err != nil || v.String().Go() != "10073600" {
		t.Fatalf("run(200) = %s, %v; want 10073600", v.String().Go(), err)
	}
	if n, u := r.jit.hosts-hosts, r.jit.unwound-unwound; n != 0 || u != 0 {
		t.Errorf("run left native code %d times, Go finished %d levels", n, u)
	}
	if d := mkLink.in - in; m.code == nil || d != 600 {
		t.Errorf("mk was called natively %d times, want 600", d)
	}
	jitDifferential(t, setup+`; [run(50), runBad(20), run(20)].join()`)
}
