//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regSuspendSrc has a generator, an async function and top-level code,
// each with a loop: register code the JIT compiles and enters by on-stack
// replacement at the loop, a suspended frame's code entered where it
// resumes (regGenRun), and the yields, awaits and returns made by the
// interpreter.
const regSuspendSrc = `
function* gen(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		s += i * 2;
		if (i % 64 == 0) {
			const got = yield s;
			if (got) s += got;
		}
	}
	return s;
}
function driveGen(n) {
	let t = 0;
	for (const v of gen(n)) t = (t + v) % 1000003;
	const g = gen(n);
	let r = g.next();
	while (!r.done) r = g.next(r.value % 7);
	return t + ":" + r.value;
}
var asyncOut = [];
async function af(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		s += i;
		if (i % 50 == 0) s += await (i % 3);
	}
	return s;
}
function driveAsync(n) { af(n).then(function (v) { asyncOut.push(v) }) }
var top = 0;
for (var k = 0; k < 30000; k++) top = (top + k * 3) % 65521;
`

// TestJITRegSuspendedAndTopLevel runs a generator, an async function and
// top-level code compiled from their register code, with the JIT and
// without, and checks the answers alike and that each function's code was
// compiled and entered: at its loop and, for those that suspend, where
// they resume.
func TestJITRegSuspendedAndTopLevel(t *testing.T) {
	t.Parallel()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, regSuspendSrc)
			var got [2]string
			var rts [2]*Runtime
			for i := range rts {
				r := newTestRuntime(t, Config{JIT: i == 0})
				t.Cleanup(r.Close)
				rts[i] = r
				r.SetJITWarmUp(1, 1)
				if _, err := r.Run(prog); err != nil {
					t.Fatal(err)
				}
				var out string
				for _, call := range []string{"driveGen", "driveAsync"} {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(call))
					for range 4 {
						v, err := r.Call(fn, Undefined, []Value{Float(3000)})
						out += regNativeShow(r, v, err) + ";"
					}
				}
				if err := r.DrainJobs(); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"asyncOut", "top"} {
					v, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					out += regNativeShow(r, v, nil) + ";"
				}
				got[i] = out
			}
			if got[0] != got[1] {
				t.Fatalf("with the JIT %q, without %q", got[0], got[1])
			}
			r := rts[0]
			compiled := map[string]bool{}
			for key, e := range r.jit.regCache {
				if fn := key.Value(); fn != nil && e.code != nil {
					switch {
					case fn.Generator:
						compiled["generator"] = true
					case fn.Async:
						compiled["async"] = true
					case fn.TopLevel:
						compiled["top-level"] = len(e.loops) != 0
					}
				}
			}
			for _, k := range []string{"generator", "async", "top-level"} {
				if !compiled[k] {
					t.Errorf("no %s code compiled with a loop's entry: %v", k, compiled)
				}
			}
			if r.jit.regResumes == 0 {
				t.Error("no suspended frame resumed in native code")
			}
		})
	}
}

// regThrownSrc's async functions await, in a try statement, a promise that
// rejects at every fourth turn, and one that rejects with no handler; its
// generator is thrown into at a yield in a try statement. Each throw
// resumes the frame where it was suspended.
const regThrownSrc = `
async function fails(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		try { s += await (i % 4 ? i : Promise.reject(i)) } catch (e) { s -= e }
	}
	return s;
}
async function finished(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		try { s += await (i % 4 ? i : Promise.reject(i)) } finally { s++ }
	}
	return s;
}
function* catches(n) {
	let s = 0;
	for (let i = 0; i < n; i++) { try { s += yield i } catch (e) { s += 1000 } }
	return s;
}
function injected(n) {
	const g = catches(n); g.next(); let s = 0;
	for (let i = 0; i < n; i++) { const r = i % 3 ? g.next(i) : g.throw(i); if (!r.done) s += r.value }
	return s + ':' + g.return(7).value;
}
var out = [];
function drive(n) { fails(n).then(v => out.push(v)); finished(n).then(v => out.push(v), e => out.push('e' + e)) }
function take() { const o = out.join(); out = []; return o }
`

// A throw injected into a suspended frame of native code, which a handler
// of the frame's takes -- a rejected await, a generator's throw -- resumes
// the frame natively at the handler, the value thrown in its register, as
// the interpreter's gen_entry throws it there: each answer the
// interpreter's, with the collector idle and marking, and, once the code
// is compiled, each such resumption an entry of it. Left to gen_entry, the
// interpreter had caught it and run the rest of the frame: of the 21
// throws a round injects, 7 resumptions had entered native code.
func TestJITThrowResumesAtHandler(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			var rounds []string
			for range 6 {
				rounds = append(rounds, "(drive(40), 0)", "take()", "injected(30)")
			}
			r := jitAgreesForTest(t, regThrownSrc, rounds...)
			for _, name := range []string{"fails", "finished", "catches"} {
				jitNativeForTest(t, r, name)
			}
			// A round of a runtime whose code is compiled and not yet
			// judged (jitRegStretchWork): every throw's resumption, fails'
			// 10, finished's one and catches' 10, enters the code.
			r = jitAgreesForTest(t, regThrownSrc, rounds[:3]...)
			resumes := r.jit.regResumes
			for _, rd := range [][2]string{{"(drive(40), 0)", "0"}, {"take()", "e0,420"}, {"injected(30)", "435:7"}} {
				v, err := r.Run(compileForTest(t, "''+"+rd[0]))
				if err == nil {
					err = r.runMicrotasks()
				}
				if err != nil {
					t.Fatalf("%s: %v", rd[0], err)
				}
				if got := v.String().Go(); got != rd[1] {
					t.Errorf("%s = %q, want %q", rd[0], got, rd[1])
				}
			}
			if got := r.jit.regResumes - resumes; got < 21 {
				t.Errorf("%d resumptions entered native code, want the round's 21 throws' at least", got)
			}
		})
	}
}
