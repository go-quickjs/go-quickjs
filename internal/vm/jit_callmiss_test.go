//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// callMissCountsSource has a site of each kind that calls framed
// callees of several templates, so that the site's call cache misses:
// site's call, which each test of TestJITRegFramedMissCountedOnce runs
// with the callees its runner picks.
const callMissCountsSource = `
function f0(x) { var s = x; for (var i = 0; i < 2; i++) s += i; return s }
function f1(x) { var s = x; for (var i = 0; i < 2; i++) s -= i; return s }
function f2(x) { var s = x; for (var i = 0; i < 2; i++) s *= 1; return s + 1 }
function f3(x) { var s = x; for (var i = 0; i < 2; i++) s ^= 0; return s + 2 }
function f4(x) { var s = x; for (var i = 0; i < 2; i++) s |= 0; return s + 3 }
function f5(x) { var s = x; for (var i = 0; i < 2; i++) s &= -1; return s + 4 }
function f6(x) { var s = x; for (var i = 0; i < 2; i++) s += 0; return s + 5 }
function f7(x) { var s = x; for (var i = 0; i < 2; i++) s -= 0; return s + 6 }
var fs = [f0, f1, f2, f3, f4, f5, f6, f7];
function C0(x) { for (var i = 0; i < 2; i++) this.v = x }
function C1(x) { for (var i = 0; i < 2; i++) this.v = x + 1 }
var cs = [C0, C1];
var o0 = { m: f0 }, o1 = { m: f1, n: 0 }, o2 = { m: f2, n: 0, k: 0 };
var os = [o0, o1, o2];
function siteCall(f, x) { var v = f(x); return v }
function siteMethod(o, x) { var v = o.m(x); return v }
function siteVia(f, x) { var v = f.call(null, x); return v }
function siteApply(f, x) { var v = f.apply(null, [x]); return v }
function siteNew(C, x) { var o = new C(x); return o.v }
function run(site, n, kinds) {
	var sum = 0;
	for (var i = 0; i < n; i++) {
		var k = i % kinds;
		sum += site === siteMethod ? site(os[k], i) : site === siteNew ? site(cs[k], i) : site(fs[k], i);
	}
	return sum;
}`

// In a runtime that runs the JIT, a call whose framed callee misses the
// site's call cache is counted once by the feedback dispatcher: the miss
// (nativeCallMiss) fills the cache and has the handler make the call
// again, which the dispatcher's handler would count a second time,
// doubling the site's frequency (1,998 calls counted for 1,000 runs) --
// for a site that alternates two templates, rotates three, calls eight,
// a method's, a call through Function.prototype.call or apply, and a
// construction, which nothing counts, while the collector is idle and
// while it marks.
func TestJITRegFramedMissCountedOnce(t *testing.T) {
	const n = 1000
	for _, marking := range []bool{false, true} {
		for _, tc := range []struct {
			site  string
			kinds int
		}{
			{"siteCall", 1}, {"siteCall", 2}, {"siteCall", 3}, {"siteCall", 8},
			{"siteMethod", 3}, {"siteVia", 2}, {"siteApply", 2}, {"siteNew", 2},
		} {
			t.Run(fmt.Sprintf("marking=%v/%s/%d", marking, tc.site, tc.kinds), func(t *testing.T) {
				if marking {
					nativeMarking(t, true)
					jitMarkingForTest(t)
				}
				prog := compileForTest(t, callMissCountsSource)
				r := newTestRuntime(t, Config{JIT: true})
				t.Cleanup(r.Close)
				r.jitStress = jitStressConfig{}
				r.SetJITWarmUp(1<<16, 1<<16)
				if _, err := r.Run(prog); err != nil {
					t.Fatal(err)
				}
				// Never compiled: the interpreter runs every call of the
				// site and of its callees, and counts.
				for _, name := range []string{"f0", "f1", "f2", "f3", "f4", "f5", "f6", "f7", "C0", "C1", "run", tc.site} {
					fv, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					fv.Object().fn().closure.jitRegRefused = true
				}
				site, _ := r.GetProp(Obj(r.Global()), r.Intern(tc.site))
				run, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
				if _, err := r.Call(run, Undefined, []Value{site, Float(n), Float(float64(tc.kinds))}); err != nil {
					t.Fatal(err)
				}
				if !regNativeOn {
					return
				}
				c := site.Object().fn().closure.jitRegCounts
				if len(c) < 2 {
					t.Fatalf("counts %v: want the site's", c)
				}
				// n runs, and n calls less the first, made before the site
				// had its counts; a construction is not counted.
				want := uint64(n - 1)
				if tc.site == "siteNew" {
					want = 0
				}
				if c[0] != n || c[1] != want {
					t.Fatalf("counts %v: want [%d %d]", c, n, want)
				}
			})
		}
	}
}
