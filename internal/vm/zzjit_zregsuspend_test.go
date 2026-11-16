//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
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
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			compiler.SetRegisterForm(true)
			prog := compileForTest(t, regSuspendSrc)
			compiler.SetRegisterForm(false)
			SetRegTier(true)
			was := SetJITRegTier(true)
			t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); SetJITRegTier(was) })
			var got [2]string
			var rts [2]*Runtime
			for i := range rts {
				r := New(Config{JIT: i == 0})
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
