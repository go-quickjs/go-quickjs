//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// regNativeSrc is the program TestJITRegNative's functions are of: loops,
// property reads and writes, globals, captured bindings, a throw caught and
// one not, strings, a call of another function, a closure made.
const regNativeSrc = `
var total = 0;
function Point(x, y) { this.x = x; this.y = y }
Point.prototype.norm = function () { return this.x * this.x + this.y * this.y };
function sum(n) { var s = 0; for (var i = 0; i < n; i++) s += i; return s }
function nested(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < 3; j++) s += i * j; return s }
function props(n) { var p = new Point(1, 2); for (var i = 0; i < n; i++) { p.x += 1; total = total + p.norm() } return p.x + total }
function strs(n) { var s = ""; for (var i = 0; i < n; i++) s += String.fromCharCode(97 + i % 26); return s.length + s }
var counter = (function () { var k = 0; return function (n) { for (var i = 0; i < n; i++) k += 2; return k } })();
function thrower(n) { var s = 0; for (var i = 0; i < n; i++) s += i; try { if (n > 3) throw new Error("big " + s); return s } catch (e) { return e.message + i } }
function uncaught(n) { var s = 0; for (var i = 0; i < n; i++) s += i; if (n > 3) throw new TypeError("no " + s); return s * 2 }
function maker(n) { var a = []; for (var i = 0; i < n; i++) a.push(function () { return i }); return a.length + a[0]() }
function arr(n) { var a = [1, 2, 3]; for (var i = 0; i < n; i++) a[i % 3] = a[(i + 1) % 3] + i; return a.join() }
function forin(n) { var k = n, s = 0; for (var i = 0; i < n; i++) s += i; for (k in {}) ; for (k of []) ; return k + s }
function holes(n) { var s = 0; for (var i = 0; i < n; i++) s += i; var t = [1, , 3, s]; t[1] += 1; return JSON.stringify(t) }
function argsLen() { return arguments.length }
function mixed(n) { var x = 0; for (var i = 0; i < n; i++) { x = i % 7 == 0 ? x + "a" : x + 1 } return x }
function add2(a, b) { return a + b }
function calls(n) { var s = 0; for (var i = 0; i < n; i++) { s = add2(s, i); s += Math.max(i, 1) } return s }
function methods(n) { var p = new Point(1, 2), s = 0; for (var i = 0; i < n; i++) s += p.norm() + new Point(i, 1).x; return s }
function boom(i) { if (i > 4) throw new RangeError("at " + i); return i }
function callThrows(n) { var s = 0; try { for (var i = 0; i < n; i++) s += boom(i) } catch (e) { return e.message + s } return s }
function callUncaught(n) { var s = 0; for (var i = 0; i < n; i++) s += boom(i); return s }
var Odd = { [Symbol.hasInstance](x) { if (x > 5) throw new Error("odd " + x); return x % 2 == 1 } };
function inst(n) { var p = new Point(1, 2), s = 0; for (var i = 0; i < n; i++) if (p instanceof Point && typeof p.x == typeof i) s += typeof s == "number" ? 1 : 0; return s + typeof p }
function instThrows(n) { var s = 0; try { for (var i = 0; i < n; i++) if (i instanceof Odd) s++ } catch (e) { return e.message + s } return s }
`

// regNativeRuntimes are two runtimes that have run regNativeSrc in register
// form: the first runs the calls natively, the second as ordinary calls.
func regNativeRuntimes(t *testing.T, src string) [2]*Runtime {
	t.Helper()
	compiler.SetRegisterForm(true)
	prog := compileForTest(t, src)
	compiler.SetRegisterForm(false)
	SetRegTier(true)
	was := SetJITRegTier(true)
	t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); SetJITRegTier(was) })
	var rts [2]*Runtime
	for i := range rts {
		rts[i] = New(Config{JIT: i == 0})
		t.Cleanup(rts[i].Close)
		// Compiled on the spot: the tests count native work done once
		// the code is asked for, as the stack form's do
		// (jitRuntimeForTest); one of compiles in the background sets it.
		rts[i].jitStress.background = false
		if _, err := rts[i].Run(prog); err != nil {
			t.Fatal(err)
		}
	}
	return rts
}

// regNativeShow is v, or what err threw, as text.
func regNativeShow(r *Runtime, v Value, err error) string {
	text := func(v Value) string {
		s, err := r.toString(v)
		if err != nil {
			return "not shown: " + err.Error()
		}
		return s.Go()
	}
	var th *Thrown
	if errors.As(err, &th) {
		v = th.Value
		if v.IsObject() {
			m, _ := r.GetProp(v, r.Intern("message"))
			return "threw " + text(m)
		}
		return "threw " + text(v)
	}
	if err != nil {
		return "error " + err.Error()
	}
	return text(v)
}

// TestJITRegNative runs functions natively from their register code, from
// their first call (RegNativeCall), at their start and by on-stack
// replacement at a loop, and compares each answer with an ordinary call's
// in a runtime that has made the same calls: the code's host exits run the
// register instruction whole, its deoptimizations have runReg go on, and
// a deoptimized function is compiled again at its next call.
func TestJITRegNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regNativeSrc)
			natives, osrs := 0, 0
			for _, name := range []string{"sum", "nested", "props", "strs", "counter", "thrower", "uncaught", "maker", "arr", "forin", "holes", "argsLen", "mixed",
				"calls", "methods", "callThrows", "callUncaught", "inst", "instThrows"} {
				for i, n := range []float64{3, 5, 40, 2, 300, 7} {
					var got, want string
					for k, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						if k == 0 {
							res := r.RegNativeCall(fn, []Value{Float(n)}, i%2 == 1)
							if res.Skipped != "" {
								t.Errorf("%s(%v): not native: %s", name, n, res.Skipped)
							}
							if res.Entered >= 0 {
								natives++
							}
							if res.OSR {
								osrs++
							}
							got = regNativeShow(r, res.Value, res.Err)
						} else {
							v, err := r.Call(fn, Undefined, []Value{Float(n)})
							want = regNativeShow(r, v, err)
						}
					}
					if got != want {
						t.Errorf("%s(%v): natively %q, called %q", name, n, got, want)
					}
				}
			}
			st := rts[0].RegNativeStats()
			t.Logf("%d calls entered natively, %d by OSR; %+v", natives, osrs, st)
			if natives == 0 || osrs == 0 || st.Entries == 0 {
				t.Errorf("not run natively: %d calls, %d by OSR, %d entries", natives, osrs, st.Entries)
			}
			if st.GoCalls == 0 {
				t.Errorf("no call made by Go from native code")
			}
		})
	}
}
