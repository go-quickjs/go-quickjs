//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// jitLinksHeld reports a function's link whose entry -- what native callers
// call -- is not in the code of the entry the cache holds for the function:
// a link no entry owns, or whose owner the cache does not hold, or whose
// owner has other code. Native code calls through it, and nothing would
// keep that code from being released.
func jitLinksHeld(s *jitState) error {
	for key, l := range s.links {
		if l.entry == 0 {
			continue
		}
		switch {
		case l.owner == nil:
			return fmt.Errorf("a link no entry owns calls %#x", l.entry)
		case s.cache[key] != l.owner:
			return fmt.Errorf("a link's owner is not the cache's entry")
		case l.owner.ssa == nil || !l.owner.ssa.Contains(l.entry):
			return fmt.Errorf("a link calls %#x, outside its owner's code", l.entry)
		}
	}
	return nil
}

// jitEntriesFound reports an entry the cache holds that entryFor does not
// find by its function, or one entryFor keeps that the cache does not hold.
func jitEntriesFound(s *jitState) error {
	for key, e := range s.cache {
		if fn := key.Value(); fn != nil && s.entryFor(fn) != e {
			return fmt.Errorf("entryFor does not find %s's entry", fn.Name)
		}
	}
	for _, x := range s.byFn {
		if s.cache[x.key] != x.e {
			return fmt.Errorf("byFn keeps an entry the cache does not hold")
		}
	}
	return nil
}

// A function compiled for native callers is not the room its own compile
// makes: its entry, a refusal the cache holds, is busy before the cache
// looks for what to drop. Before, the cache could drop it, and the compile
// went on for an entry no longer the function's, whose code the function's
// link then called with nothing to keep it.
func TestJITNativeCalleeKeepsItsEntry(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function g(o) { return o.a * 3 + o.b }
		function warm(n) { let s = 0; for (let i = 0; i < n; i++) s += i % 7; return s }
		warm(1000); warm(1000); g({ a: 1, b: 2 })`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	e := r.jitFor(cl)
	s := r.jit
	if e == nil || e.ssa != nil || !e.owned() || s.perInstr == 0 {
		t.Fatalf("g is not a refusal the cache holds: %+v", e)
	}
	// One byte short of the room its compile asks for: the cache must drop
	// something, and the refusal is all there is to drop.
	now := jitEpochNow
	var tick uint64
	jitEpochNow = func() uint64 { tick += 10; return tick }
	t.Cleanup(func() { jitEpochNow = now })
	r.jitMemoryLimit = int64(s.roomHeld() + len(cl.fn.Code)*s.perInstr - 1)
	ce := r.jitNativeCallee(cl)
	if ce != nil && !ce.owned() {
		t.Fatal("compiled for an entry the cache dropped")
	}
	if err := jitLinksHeld(s); err != nil {
		t.Fatal(err)
	}
}

// The entry a compile returns is the cache's: the room its compile's
// workspaces then make is not made of it. Before, a compile that left the
// JIT past its budget could drop the very entry it had compiled, to be run
// as it was returned -- which, once a dropped entry was no longer run,
// left top-level code trying at every loop and never running natively.
func TestJITForKeepsWhatItCompiled(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += (i * 7) % 13 + (i * 5) % 11 + (i * 3) % 7; return s }`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	now := jitEpochNow
	var tick uint64
	jitEpochNow = func() uint64 { tick += 10; return tick }
	t.Cleanup(func() { jitEpochNow = now })
	// As little as compiling it takes: placed, it leaves the JIT past its
	// budget, and the room the compile then makes may only be its own.
	r.jitMemoryLimit = int64(len(cl.fn.Code)*ir.MaxSlots*8 + 32<<10)
	e := r.jitFor(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("not compiled in the budget: %+v", e)
	}
	if !e.owned() || r.jit.cache[e.link.key] != e {
		t.Fatal("the compile dropped the entry it returns")
	}
}

// The room the cache makes is the workspaces' before it is code's: they
// spare the next compile its allocations, code a compile. Before, they
// went last, and a program whose compiles grew them past half the budget
// had its hot code dropped and compiled again on every pass, as Octane's
// zlib did hundreds of times a function.
func TestJITDropsWorkspacesBeforeCode(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += (i * 7) % 13; return s }`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	now := jitEpochNow
	var tick uint64
	jitEpochNow = func() uint64 { tick += 10; return tick }
	t.Cleanup(func() { jitEpochNow = now })
	e := r.jitFor(cl)
	s := r.jit
	if e == nil || e.ssa == nil || s.workBytes() == 0 {
		t.Fatalf("f has no code, or the compile kept no workspaces: %+v", e)
	}
	// Old by now, and either would make the room.
	s.budget = s.held() - 1
	if !s.makeRoom(0) {
		t.Fatal("no room made")
	}
	if !e.owned() || e.ssa == nil {
		t.Error("dropped code for the workspaces")
	}
	if s.workBytes() != 0 {
		t.Error("kept the workspaces")
	}
}

// An entry the cache dropped since Go found it is not run: Go runs the
// frame in the existing tiers. Before, a compile Go made between finding an
// entry and running it could drop it, and its code ran though nothing held
// what it calls through.
func TestJITRunsNoDroppedEntry(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i % 7; return s }
		f(1000); f(1000)`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	e := r.jitFor(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("f has no code: %+v", e)
	}
	if !r.jit.dropEntry(e.link.key, e) {
		t.Fatal("the entry was not dropped")
	}
	// No frame: a run would read it.
	if _, _, native := r.runSSAIn(nil, e, 0, 0, 0, 0); native {
		t.Fatal("ran a dropped entry")
	}
}

// While a native call of a function's code has not returned -- its context
// marked live, its closure the function's -- the cache neither drops the
// code nor compiles the function again: the call returns into the code,
// and Go reads its frame by the code it left. Before, only Go's own runs of
// the code counted.
func TestJITKeepsCodeANativeCallIsIn(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i % 7; return s }
		f(1000); f(1000)`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	e := r.jitFor(cl)
	s := r.jit
	if e == nil || e.ssa == nil || len(s.ssaCtxs) < 2 {
		t.Fatalf("f has no code, or no contexts: %+v", e)
	}
	c := s.ssaCtxs[1]
	c.Live, c.Closure = abi.LiveCall, unsafe.Pointer(cl)
	defer func() { c.Live, c.Closure = 0, nil }()
	if !s.inUse(e) {
		t.Error("the cache may drop code a native call is in")
	}
	code := e.ssa
	e.reopt = true
	r.jitReoptimize(cl, e)
	if e.ssa != code {
		t.Error("compiled again while a native call is in its code")
	}
}

// Code the cache drops to make room is code nothing runs, calls or returns
// to: compiled in a budget far too small for a program's hot functions,
// each calling others natively and compiled again as it learns, a run
// gives what the interpreter gives, and every function's link calls the
// code of the entry the cache holds. Before, the cache dropped the entry a
// compile for native callers was being made for (jitNativeCallee made room
// before it was busy), whose code its link then called with nothing to keep
// it; Go ran code the cache had dropped since it was found (runSSAIn); a
// function was compiled again while a native call of it had not returned,
// which then returned into the new code's place, or had its frame read by
// the new code's entry (otherwiseInUse); and a native callee's caller went
// on at the place the old code had returned to in new code put there
// (jitUnwindNative): Octane's Typescript failed, a different way each run.
func TestJITEvictionKeepsRunningCode(t *testing.T) {
	// Leaves the JIT refuses on their own -- property reads, no loop --
	// which it compiles for the native callers it then meets: each
	// caller's loop calls several, and is compiled again as it learns.
	var b strings.Builder
	const leaves, callers = 120, 60
	for i := 0; i < leaves; i++ {
		fmt.Fprintf(&b, "function g%d(o) { return o.a * %d + o.b }\n", i, i%7+1)
	}
	for i := 0; i < callers; i++ {
		fmt.Fprintf(&b, "function c%d(os, d) { let s = 0; for (let k = 0; k < os.length; k++) { s = (s + g%d(os[k]) + g%d(os[k]) + g%d(os[k])) %% 1000003",
			i, i%leaves, (i*7+1)%leaves, (i*11+2)%leaves)
		fmt.Fprintf(&b, "; if (d > 0 && k === 0) s = (s + c%d(os, d - 1)) %% 1000003 } return s }\n", (i*5+3)%callers)
	}
	b.WriteString("const os = []; for (let i = 0; i < 20; i++) os.push({a: i, b: i * 3});\n")
	fmt.Fprintf(&b, "let r = 0; for (let k = 0; k < 30; k++) { for (let i = 0; i < %d; i++) r = (r + this[\"c\" + i](os, 2)) %% 1000003; check() } r", callers)
	src := b.String()
	plain := New(Config{})
	plain.global.setOwnRaw(plain.atoms.intern("check"), Obj(plain.newNativeFunc("check", 0, func(*Runtime, Value, []Value) (Value, error) {
		return Undefined, nil
	})), propDefault)
	want, err := plain.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	// The cache's clock a tick at every look: code is old at once, and the
	// cache drops it to make room for whatever is compiled next.
	now := jitEpochNow
	var tick uint64
	jitEpochNow = func() uint64 { tick++; return tick }
	t.Cleanup(func() { jitEpochNow = now })
	r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: 1 << 20})
	// Between rounds, every link holds, and the collector runs, releasing
	// the code nothing holds -- which native code then must not run.
	var broken error
	r.global.setOwnRaw(r.atoms.intern("check"), Obj(r.newNativeFunc("check", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
		if err := jitLinksHeld(rt.jit); err != nil && broken == nil {
			broken = err
		}
		if err := jitEntriesFound(rt.jit); err != nil && broken == nil {
			broken = err
		}
		runtime.GC()
		return Undefined, nil
	})), propDefault)
	got, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if !got.sameBits(want) {
		t.Fatalf("jit: %v, interpreter: %v", got, want)
	}
	if r.jit == nil || r.jit.evicted == 0 || r.jit.compiled < 20 {
		t.Fatalf("the cache dropped no code: %+v", r.JITStats())
	}
	if broken != nil {
		t.Fatal(broken)
	}
}
