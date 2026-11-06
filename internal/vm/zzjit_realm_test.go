//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// jitRealmProgram makes each kind of value native code makes from a pool or
// asks the realm for, in hot loops, and says which came out of another
// realm's, as a list of names. first is what a loop makes first, from what
// the pool holds when the realm's turn comes, and last what it makes after
// the pool has been filled again. __tag is the realm's own.
const jitRealmProgram = `
function object(n) { var f, o; for (var i = 0; i < n; i++) { o = {a: i}; f = f || o; } return [f, o]; }
function array(n) { var f, o; for (var i = 0; i < n; i++) { o = [i, 2]; f = f || o; } return [f, o]; }
function empty(n) { var f, o; for (var i = 0; i < n; i++) { o = []; f = f || o; } return [f, o]; }
function func(n) { var f, o; for (var i = 0; i < n; i++) { o = function () { return i; }; f = f || o; } return [f, o]; }
function arrow(n) { var f, o; for (var i = 0; i < n; i++) { o = () => i; f = f || o; } return [f, o]; }
function regexp(n) { var f, o; for (var i = 0; i < n; i++) { o = /a/g; f = f || o; } return [f, o]; }
function args() { return arguments; }
function argsLoop(n) { var f, o; for (var i = 0; i < n; i++) { o = args(i); f = f || o; } return [f, o]; }
function make(i) { return {b: i}; }
function called(n) { var f, o; for (var i = 0; i < n; i++) { o = make(i); f = f || o; } return [f, o]; }
function* gen() { yield 1; }
function genResult(n) { var f, o; for (var i = 0; i < n; i++) { o = gen().next(); f = f || o; } return [f, o]; }
function construct(n) { function C(v) { this.v = v; } var f, o; for (var i = 0; i < n; i++) { o = new C(i); f = f || o; } return [Object.getPrototypeOf(f), Object.getPrototypeOf(o)]; }
function newArray(n) { var f, o; for (var i = 0; i < n; i++) { o = new Array(2); f = f || o; } return [f, o]; }
function tag(n) { var o; for (var i = 0; i < n; i++) o = __tag; return o; }
function global(n) { var o; for (var i = 0; i < n; i++) o = Math; return o; }
function abs(n) { var o = 0; for (var i = 0; i < n; i++) o = Math.abs(-i); return o; }
function thrown(n) { var o; for (var i = 0; i < n; i++) { try { null.x; } catch (e) { o = e; } } return o; }
function check(n) {
	var bad = [];
	function own(name, pair, proto) {
		if (Object.getPrototypeOf(pair[0]) !== proto) bad.push(name + ":first");
		if (Object.getPrototypeOf(pair[1]) !== proto) bad.push(name + ":last");
	}
	own("object", object(n), Object.prototype);
	own("array", array(n), Array.prototype);
	own("empty", empty(n), Array.prototype);
	own("func", func(n), Function.prototype);
	own("arrow", arrow(n), Function.prototype);
	own("regexp", regexp(n), RegExp.prototype);
	own("arguments", argsLoop(n), Object.prototype);
	own("called", called(n), Object.prototype);
	own("generator", genResult(n), Object.prototype);
	own("construct", construct(n), Object.prototype);
	own("newArray", newArray(n), Array.prototype);
	if (tag(n) !== __tag) bad.push("tag");
	if (global(n) !== Math) bad.push("global");
	if (abs(n) !== n - 1) bad.push("abs");
	if (!(thrown(n >> 4) instanceof TypeError)) bad.push("thrown");
	return bad.join(",");
}
`

// jitRealmsForTest makes n realms of r, each with __tag its own.
func jitRealmsForTest(t *testing.T, r *Runtime, n int) []*Realm {
	t.Helper()
	realms := make([]*Realm, n)
	for i := range realms {
		realms[i] = r.NewRealm()
		r.InRealm(realms[i], func() {
			r.global.setOwnRaw(r.atoms.intern("__tag"), Float(float64(i)), propDefault)
		})
	}
	return realms
}

// jitRealmCheck runs check(n) in re, which must find nothing of another
// realm's.
func jitRealmCheck(t *testing.T, r *Runtime, re *Realm, fn *bytecode.Function, n int, say string) {
	t.Helper()
	v, err := r.RunIn(re, fn)
	if err != nil {
		t.Fatalf("%s: %v", say, err)
	}
	if s := v.String().Go(); s != "" {
		t.Errorf("%s: of another realm: %s", say, s)
	}
}

// A compiled program run in several realms -- as a host runs one Program in
// a context for each tenant, or node:vm's scripts -- makes each realm's
// values, not those of the realm that was first to make its code hot: a
// function's code is its realm's (jitKey). Each realm's code is native, the
// realms taking turns, and what a pool held when another realm's turn came
// is no other realm's either: it was the second realm's objects, literals
// and arrays alike, that had the first's prototypes.
func TestJITRealmsHaveTheirOwnCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	fn := compileForTest(t, jitRealmProgram+"check(4000);")
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7},
		{background: true, checks: 2}} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitStress = c
		realms := jitRealmsForTest(t, r, 3)
		for round := range 12 {
			for i, re := range realms {
				jitRealmCheck(t, r, re, fn, 4000, jitRealmSay(c, round, i))
			}
		}
		if t.Failed() {
			return
		}
		if c != (jitStressConfig{}) {
			continue
		}
		// Each realm compiled for itself: the same bytecode, code of each.
		var object *bytecode.Function
		for _, k := range fn.Constants {
			if k.Kind == bytecode.ConstFunction && k.Fn.Name == "object" {
				object = k.Fn
			}
		}
		if object == nil {
			t.Fatal("no function object")
		}
		seen := map[*jitEntry]bool{}
		for _, re := range realms {
			r.InRealm(re, func() {
				if e := r.jit.entryFor(object); e != nil && e.ssa != nil && e.ssaStats.entries != 0 {
					seen[e] = true
				}
			})
		}
		if len(seen) != len(realms) {
			t.Errorf("%d of %d realms ran code of their own", len(seen), len(realms))
		}
	}
}

func jitRealmSay(c jitStressConfig, round, i int) string {
	return "stress " + string(rune('0'+c.budget)) + " round " + string(rune('a'+round)) + " realm " + string(rune('0'+i))
}

// A call of another realm's function makes that realm's values, in native
// callers too: a function's realm is its closure's, and a caller's code
// leaves for Go to switch it.
func TestJITCallsAcrossRealms(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	realms := jitRealmsForTest(t, r, 2)
	fn := compileForTest(t, `
function make(i) { return {a: i, array: [i]}; }
function drive(f, n) { var o; for (var i = 0; i < n; i++) o = f(i); return o; }
globalThis.make = make; globalThis.drive = drive;`)
	for _, re := range realms {
		if _, err := r.RunIn(re, fn); err != nil {
			t.Fatal(err)
		}
	}
	read := func(re *Realm, name string) Value {
		var v Value
		r.InRealm(re, func() { v = r.global.getOwn(r.atoms.intern(name)).value() })
		return v
	}
	var protos [2][2]*Object
	for i, re := range realms {
		r.InRealm(re, func() { protos[i] = [2]*Object{r.proto.object, r.proto.array} })
	}
	for round := range 40 {
		for i, re := range realms {
			// Each realm's drive, called with the other's make and its own.
			for j := range realms {
				v, err := r.Call(read(re, "drive"), Undefined, []Value{read(realms[j], "make"), Float(2000)})
				if err != nil {
					t.Fatal(err)
				}
				o := v.Object()
				if o.proto() != protos[j][0] {
					t.Fatalf("round %d: realm %d's drive of realm %d's make: an object of the wrong realm", round, i, j)
				}
				if a := o.getOwn(r.atoms.intern("array")).value().Object(); a.proto() != protos[j][1] {
					t.Fatalf("round %d: realm %d's drive of realm %d's make: an array of the wrong realm", round, i, j)
				}
			}
		}
	}
}

// A host that runs one compiled program in a realm for each request makes
// a function hot in each: what the realms' code holds stays within the
// JIT's budget, as any other code does, the realms' entries going as old
// entries do.
func TestJITRealmsWithinTheBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	fn := compileForTest(t, jitHotProgram(40))
	for _, limit := range []int64{96 << 10, 256 << 10} {
		r := New(Config{JIT: true, JITMemoryLimit: limit})
		r.SetJITWarmUp(8, 8)
		for i := range 60 {
			if _, err := r.RunIn(r.NewRealm(), fn); err != nil {
				t.Fatal(err)
			}
			if s := r.jit; s != nil && s.held() > s.budget {
				t.Fatalf("limit %d KB, realm %d: held %d, %d entries", limit>>10, i, s.held(), len(s.cache))
			}
		}
		r.Close()
		r.ReleaseClosed()
	}
}

// jitRealmFlushed has the cache look for code to flush, its clock moved
// past the last look each time, a turn in the current realm between looks,
// until it holds no entry of a realm that is not current or a look finds
// nothing more to drop.
func jitRealmFlushed(t *testing.T, r *Runtime, setEpoch func(uint64), keep map[weak.Pointer[Realm]]bool) int {
	t.Helper()
	cur := r.jitRealmKey()
	idle := compileForTest(t, `0`)
	left := 0
	for i := uint64(1); i <= 8; i++ {
		setEpoch(1000 + i*jitFlushAge)
		if _, err := r.Run(idle); err != nil {
			t.Fatal(err)
		}
		// A look is at a check, which installs what is done first.
		r.jitDrain()
		r.jit.flush()
		left = 0
		for key := range r.jit.cache {
			if key.realm != cur && !keep[key.realm] {
				left++
			}
		}
		if left == 0 && i >= 2 {
			break
		}
	}
	return left
}

// Entries of realms a host has dropped leave the cache, and the realms with
// them. An entry's key holds its realm weakly, but its code's pools hold the
// realm's objects, and its feedback the closures it saw, so that a key was
// never gone: the cache lets go of a realm none of whose entries has run
// between two of its looks, its entries with no code as well, and of what it
// keeps of the realm it last ran code of, which had kept the last realm and,
// through the megamorphic cache, more. One that has only gone quiet runs on,
// compiling again, and the runtime's first realm runs on as it was.
func TestJITDroppedRealmsLeaveTheCache(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	fn := compileForTest(t, jitRealmProgram+"check(1000);")
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3, deopt: 7}, {background: true, checks: 2}} {
		setEpoch(1000)
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitStress = c
		r.global.setOwnRaw(r.atoms.intern("__tag"), Int(-1), propDefault)
		quiet := jitRealmsForTest(t, r, 1)[0]
		jitRealmCheck(t, r, quiet, fn, 1000, "the quiet realm")
		var refs []weak.Pointer[Realm]
		for i, re := range jitRealmsForTest(t, r, 3) {
			refs = append(refs, weak.Make(re))
			for round := range 3 {
				jitRealmCheck(t, r, re, fn, 1000, jitRealmSay(c, round, i))
			}
		}
		s := r.jit
		native := 0
		for key, e := range s.cache {
			if key.realm.Value() != nil && e.ssa != nil && e.ssaStats.entries != 0 {
				native++
			}
		}
		if native == 0 {
			t.Fatalf("%+v: no realm ran native code", c)
		}
		if n := jitRealmFlushed(t, r, setEpoch, nil); n != 0 {
			t.Errorf("%+v: %d entries of dropped realms left", c, n)
		}
		if err := jitLinksHeld(s); err != nil {
			t.Error(err)
		}
		if err := jitEntriesFound(s); err != nil {
			t.Error(err)
		}
		if n := realmsAlive(r, refs); n != 0 {
			t.Errorf("%+v: %d of %d realms alive with %d entries in the cache", c, n, len(refs), len(s.cache))
		}
		for round := range 3 {
			jitRealmCheck(t, r, quiet, fn, 1000, jitRealmSay(c, round, 9))
		}
		if v, err := r.Run(compileForTest(t, jitRealmProgram+"check(1000);")); err != nil || v.String().Go() != "" {
			t.Errorf("%+v: in the first realm: %v, %v", c, v, err)
		}
		if t.Failed() {
			return
		}
	}
}

// What a realm dropped has run, each program of realmPrograms made hot, is
// no reason to keep it: what native code made and met -- suspended
// generators' frames in the contexts, the megamorphic cache's shapes, the
// closures an entry warming has seen -- is let go of with its entries.
func TestJITDroppedRealmsAreCollected(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	for _, p := range realmPrograms {
		t.Run(p.name, func(t *testing.T) {
			setEpoch(1000)
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.SetJITWarmUp(8, 8)
			fn := compileForTest(t, "function hot() { "+p.src+" }\nfor (var i_ = 0; i_ < 20; i_++) hot();")
			var refs []weak.Pointer[Realm]
			for range 4 {
				re := r.NewRealm()
				refs = append(refs, weak.Make(re))
				for range 3 {
					if _, err := r.RunIn(re, fn); err != nil {
						t.Fatal(err)
					}
					if err := r.DrainJobs(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if r.jit == nil {
				return
			}
			if n := jitRealmFlushed(t, r, setEpoch, nil); n != 0 {
				t.Errorf("%d entries of dropped realms left", n)
			}
			if n := realmsAlive(r, refs); n != 0 {
				t.Errorf("%d of %d realms alive", n, len(refs))
			}
		})
	}
}

// The entries of the realm that runs stay, those warming up as well as those
// with code, while another's go: warming, an entry's uses are not counted,
// and what it is compiled for needs the invocations it has.
func TestJITFlushKeepsTheRunningRealm(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITWarmUp(1<<15, 1<<15)
	fn := compileForTest(t, "{"+jitHotProgram(4)+"}")
	re := r.NewRealm()
	for range 3 {
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RunIn(re, fn); err != nil {
			t.Fatal(err)
		}
	}
	s := r.jit
	count := func() (own, other, warming int) {
		cur := r.jitRealmKey()
		for key, e := range s.cache {
			if key.realm == cur {
				own++
				if !e.holdsCode() {
					warming++
				}
			} else {
				other++
			}
		}
		return
	}
	own, other, warming := count()
	if own == 0 || other == 0 || warming == 0 {
		t.Fatalf("%d entries of the first realm, %d warming, %d of the other", own, warming, other)
	}
	setEpoch(1000 + jitFlushAge)
	s.flush()
	setEpoch(1000 + 2*jitFlushAge)
	s.flush()
	own2, other2, warming2 := count()
	if own2 != own || warming2 != warming {
		t.Errorf("the running realm's entries: %d, %d warming, of %d, %d", own2, warming2, own, warming)
	}
	if other2 != 0 {
		t.Errorf("%d entries of %d left of a realm with none used", other2, other)
	}
	// The other realm lives, and warms up again.
	if _, err := r.RunIn(re, fn); err != nil {
		t.Fatal(err)
	}
}
