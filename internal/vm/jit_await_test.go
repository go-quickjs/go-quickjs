//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// jitAwaitSrc awaits every kind of value in async functions' loops and
// straight lines, with other jobs queued among them and without: what the
// interpreter's await goes on past in place -- a primitive, a fulfilled
// promise of the realm's constructor, where the frame's await job resumed
// it and no job is queued -- and what it suspends at -- a pending, rejected
// or subclassed promise, one with a constructor of its own or whose
// prototype's constructor changed, a thenable, an object, a function --
// and the order the queue then runs them in. all() leaves JSON of what
// each gave in res.
const jitAwaitSrc = `var log = [];
async function collect(n, f) { var out = []; for (var i = 0; i < n; i++) out.push(String(await f(i))); return out.join(","); }
class P extends Promise {}
var thenable = i => ({ then(r) { log.push("then" + i); r(i * 2) } });
var own = i => { var p = Promise.resolve(i); p.constructor = Object; return p };
var kinds = {
  number: i => i, string: i => "s" + i, undef: i => undefined, nul: i => null, bool: i => i % 2 == 0,
  symbol: i => Symbol("y" + i), bigint: i => BigInt(i) * 10n, object: i => ({ v: i, toString() { return "o" + this.v } }),
  func: i => function f() {}, fulfilled: i => Promise.resolve(i), pending: i => Promise.resolve().then(() => i * 3),
  thenable: thenable, sub: i => P.resolve(i), own: own,
  rejected: i => (i % 3 ? i : Promise.reject(new Error("r" + i))),
};
async function guarded(n, f) { var out = []; for (var i = 0; i < n; i++) { try { out.push(String(await f(i))) } catch (e) { out.push("caught " + e.message) } } return out.join(","); }
async function tagged(tag, n, f) { for (var i = 0; i < n; i++) { log.push(tag + i); await f(i) } log.push(tag + "!") }
function mix() {
  log = [];
  tagged("a", 6, kinds.number); tagged("b", 4, kinds.fulfilled); tagged("c", 3, kinds.pending);
  Promise.resolve().then(() => log.push("p")); tagged("d", 3, kinds.thenable);
}
async function inner(n) { var s = 0; for (var i = 0; i < n; i++) { if (i == 2) Promise.resolve().then(() => log.push("in")); s += await i; log.push("i" + i) } return s }
async function line() { log.push(1); await 1; log.push(2); await "x"; log.push(3); await null; log.push(4); return 5 }
async function adder(x) { return x + 1 }
async function calls(n) { var s = 0; for (var j = 0; j < n; j++) s += await adder(j); return s }
async function deep(x) { return (await adder(x)) * 2 }
async function settle(n) { var s = 0; for (var i = 0; i < n; i++) s += await (i & 1 ? i : Promise.resolve(i)); return s }
async function chain(n) { var s = 0; for (var j = 0; j < n; j++) s += await deep(j); return s }
var out = {};
async function all() {
  for (var k in kinds) {
    if (k == "rejected") out[k] = await guarded(8, kinds[k]); else out[k] = await collect(6, kinds[k]);
  }
  out.thenlog = log.join(",");
  mix(); await null; await null; await null; await null; await null; await null; await null; await null; await null; await null;
  out.mix = log.join(",");
  log = []; out.inner = await inner(5) + " " + log.join(",");
  log = []; var p = line(); log.push("after"); out.line = await p + " " + log.join(",");
  out.calls = await calls(50); out.chain = await chain(20); out.settle = await settle(300);
  Promise.prototype.constructor = Object; out.changed = await collect(4, kinds.fulfilled); Promise.prototype.constructor = Promise;
  return JSON.stringify(out);
}
var res;
function run() { res = undefined; log = []; all().then(function (v) { res = v }) }
`

// jitAwaitWant is jitAwaitSrc's res, Node 26's.
const jitAwaitWant = `{"number":"0,1,2,3,4,5","string":"s0,s1,s2,s3,s4,s5","undef":"undefined,undefined,undefined,undefined,undefined,undefined","nul":"null,null,null,null,null,null","bool":"true,false,true,false,true,false","symbol":"Symbol(y0),Symbol(y1),Symbol(y2),Symbol(y3),Symbol(y4),Symbol(y5)","bigint":"0,10,20,30,40,50","object":"o0,o1,o2,o3,o4,o5","func":"function f() {},function f() {},function f() {},function f() {},function f() {},function f() {}","fulfilled":"0,1,2,3,4,5","pending":"0,3,6,9,12,15","thenable":"0,2,4,6,8,10","sub":"0,1,2,3,4,5","own":"0,1,2,3,4,5","rejected":"caught r0,1,2,caught r3,4,5,caught r6,7","thenlog":"then0,then1,then2,then3,then4,then5","mix":"a0,b0,c0,d0,a1,b1,p,then0,a2,b2,c1,d1,a3,b3,then1,a4,b!,c2,d2,a5,then2,a!,c!,d!","inner":"10 i0,i1,in,i2,i3,i4","line":"5 1,after,2,3,4","calls":1275,"chain":420,"settle":44850,"changed":"0,1,2,3"}`

// jitAwaitRun runs jitAwaitSrc's all() rounds times on r, set up already,
// draining its jobs after each, and returns its last res.
func jitAwaitRun(t *testing.T, r *Runtime, rounds int) string {
	t.Helper()
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
	var got string
	for range rounds {
		if _, err := r.Call(fn, Undefined, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.DrainJobs(); err != nil {
			t.Fatal(err)
		}
		v, _ := r.GetProp(Obj(r.Global()), r.Intern("res"))
		got = v.String().Go()
	}
	return got
}

// TestJITAwaitOrder runs jitAwaitSrc with the interpreter and with the JIT
// -- compiled at its budget, at the first call, polled every few back
// edges, deoptimized at random, compiled in the background and taken back,
// in the runtime's default mode, idle and while the collector marks -- and checks each gives Node's answer: native code
// goes on past an await in place exactly where the interpreter does, and
// suspends where it does, the queue running the same jobs in the same
// order. Where its functions are compiled at their first calls, the loops
// that await settled values run natively, never judged slow for exits at
// their awaits.
func TestJITAwaitOrder(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, jitAwaitSrc)); err != nil {
		t.Fatal(err)
	}
	if got := jitAwaitRun(t, want, 2); got != jitAwaitWant {
		t.Fatalf("the interpreter gave %s, want %s", got, jitAwaitWant)
	}
	configs := []jitStressConfig{
		{},
		{threshold: true},
		{threshold: true, budget: 3},
		{threshold: true, deopt: 5},
		{threshold: true, miss: true},
		{threshold: true, background: true, checks: 2},
		{threshold: true, background: true, checks: 1, budget: 2},
		{threshold: true, background: true, checks: 2, takeBack: true},
	}
	regBothMarking(t, func(t *testing.T) {
		for i := range len(configs) + 1 {
			r := jitRuntimeForTest(t, Config{JIT: true})
			var c jitStressConfig
			if i < len(configs) {
				c = configs[i]
			} else {
				// The runtime's default, WithJIT's: in the background
				// where GOMAXPROCS is above 1.
				r.SetJITBackgroundAuto()
			}
			r.jitStress = c
			r.SetJITReport(true)
			if _, err := r.Run(compileForTest(t, jitAwaitSrc)); err != nil {
				t.Fatal(err)
			}
			if got := jitAwaitRun(t, r, 3); got != jitAwaitWant {
				t.Fatalf("%+v: the JIT gave %s, want %s", c, got, jitAwaitWant)
			}
			if i < len(configs) && c == (jitStressConfig{threshold: true}) {
				// settle's awaits after its first, of numbers and
				// fulfilled promises, go on in place natively: no exit at
				// each, for which its loop had been judged slow and left to
				// the interpreter.
				for _, name := range []string{"settle"} {
					if e := jitNativeForTest(t, r, name); e.loopSlow || e.entrySlow {
						t.Errorf("%s judged slow: %+v", name, e.ssaStats)
					}
				}
			}
			r.Close()
			r.ReleaseClosed()
		}
	})
}

// TestJITAwaitInPlaceStops runs an async function that awaits a number for
// ever, natively once compiled -- each await going on in place, the queue
// empty -- and checks that a deadline stops it: native code's loop polls
// at its back edge as the interpreter's await checks its budget.
func TestJITAwaitInPlaceStops(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
var turns = 0;
async function spin(n) { for (var i = 0; ; i++) { turns++; await i; if (n > 0 && i >= n) return i } }
`
	for _, jit := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: jit})
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("spin"))
		for range 4 {
			// Warm: compiled, its loop native.
			if _, err := r.Call(fn, Undefined, []Value{Float(20000)}); err != nil {
				t.Fatal(err)
			}
			if err := r.DrainJobs(); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		if jit {
			jitNativeForTest(t, r, "spin")
		}
		r.SetContext(ctx)
		start := time.Now()
		_, err := r.Call(fn, Undefined, []Value{Float(0)})
		if err == nil {
			err = r.DrainJobs()
		}
		cancel()
		r.SetContext(context.Background())
		r.ClearStop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("JIT %v: spin stopped with %v, want the deadline's error", jit, err)
		}
		if took := time.Since(start); took > 20*time.Second {
			t.Errorf("JIT %v: spin stopped after %v", jit, took)
		}
		r.Close()
		r.ReleaseClosed()
	}
}

// TestJITJobQueueWords holds the words native code reads of the job queue
// (abi.Context's JobHead and JobCount) to the runtime's: its next job's
// index and its length, through every context, as jobs are queued and run.
func TestJITJobQueueWords(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = jitStressConfig{threshold: true}
	// A run of native code, which shares the runtime's words with the
	// contexts (jitShareContexts).
	if _, err := r.Run(compileForTest(t, `function f(n) { var s = 0; for (var i = 0; i < n; i++) s += i; return s } f(100); f(100)`)); err != nil {
		t.Fatal(err)
	}
	if r.jit == nil || len(r.jit.ssaCtxs) == 0 || r.jit.ssaCtxs[0].JobHead == nil {
		t.Fatal("no context was given the job queue's words")
	}
	check := func(when string) {
		t.Helper()
		for i, c := range r.jit.ssaCtxs {
			if c.JobHead == nil {
				continue
			}
			if c.JobHead != &r.jobHead || *c.JobCount != len(r.microtasks) || *c.JobHead != r.jobHead {
				t.Fatalf("%s: context %d's queue words %d of %d, the runtime's %d of %d", when, i, *c.JobHead, *c.JobCount, r.jobHead, len(r.microtasks))
			}
		}
	}
	check("empty")
	if _, err := r.Run(compileForTest(t, `for (var i = 0; i < 5; i++) Promise.resolve(i).then(function () {})`)); err != nil {
		t.Fatal(err)
	}
	check("queued")
	if err := r.DrainJobs(); err != nil {
		t.Fatal(err)
	}
	check("drained")
}

// TestJITAwaitCallsSettled runs an async function whose loop calls another
// async function at each turn and awaits its fulfilled promise, in place,
// and checks that the calls are judged as the calls of Go they stay
// (jitRegGoSettled): native code never calls an async function natively,
// so each costs the loop a call of Go, and a loop that does little else
// does not pay (jitRegRunGoProfit) and is left to the interpreter, whose
// call of it is the cheaper. Judged as calls of script, which code compiled
// again might make natively, the loop ran natively once its awaits went on
// in place there, at 1.8 times the interpreter's time.
func TestJITAwaitCallsSettled(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
async function addOne(x) { return x + 1 }
async function calls(n) { var s = 0; for (var j = 0; j < n; j++) s += await addOne(j); return s }
var res;
function run(n) { calls(n).then(function (v) { res = v }) }
`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
	for range 4 {
		if _, err := r.Call(fn, Undefined, []Value{Float(5000)}); err != nil {
			t.Fatal(err)
		}
		if err := r.DrainJobs(); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := r.GetProp(Obj(r.Global()), r.Intern("res")); !v.IsNumber() || v.Number() != 12502500 {
		t.Fatalf("calls gave %v, want 12502500", v)
	}
	e := jitNativeForTest(t, r, "calls")
	if e.ssaStats.runGoCalls == 0 || !e.loopSlow {
		t.Errorf("calls' calls of addOne judged %d times, its loop slow %v: want them judged, and the loop slow", e.ssaStats.runGoCalls, e.loopSlow)
	}
}
