//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

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
