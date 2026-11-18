//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// asmRetSrc is an asm.js module whose functions convert the results of
// their calls, each of its declared type: a signed int's (sig, and neg
// and sig through a table), a double's and a float's, over their edges
// -- MinInt32, -0, NaN, the infinities, the denormals, a float's rounding
// -- and an import's, which may be anything: a string, an object whose
// valueOf runs.
const asmRetSrc = `function RetModule(stdlib, foreign, heap) {
    "use asm";
    var I32 = new stdlib.Int32Array(heap);
    var F32 = new stdlib.Float32Array(heap);
    var F64 = new stdlib.Float64Array(heap);
    var fround = stdlib.Math.fround;
    var imp = foreign.imp;
    function sig(k) {
        k = k | 0;
        return I32[k << 2 >> 2] | 0;
    }
    function neg(k) {
        k = k | 0;
        return (0 - (I32[k << 2 >> 2] | 0)) | 0;
    }
    function dbl(k) {
        k = k | 0;
        return +F64[(k << 3) + 256 >> 3];
    }
    function flt(k) {
        k = k | 0;
        return fround(F32[(k << 2) + 1024 >> 2]);
    }
    function viaI(k) {
        k = k | 0;
        return sig(k) | 0;
    }
    function viaD(k) {
        k = k | 0;
        return +dbl(k);
    }
    function viaF(k) {
        k = k | 0;
        return fround(flt(k));
    }
    function viaT(k) {
        k = k | 0;
        return tab[k & 1](k) | 0;
    }
    function viaImp(k) {
        k = k | 0;
        return imp(k | 0) | 0;
    }
    var tab = [sig, neg];
    return { viaI: viaI, viaD: viaD, viaF: viaF, viaT: viaT, viaImp: viaImp };
}
var retHeap = new ArrayBuffer(65536);
var retModule = RetModule(globalThis, { imp: function (k) { return k % 3 ? String(k * 7) : { valueOf: function () { return k - 0.5; } }; } }, retHeap);
function retRun() {
    var i32 = new Int32Array(retHeap), f64 = new Float64Array(retHeap, 256), f32 = new Float32Array(retHeap, 1024);
    var ints = [0, 1, -1, 2147483647, -2147483648, 65536, -7];
    var dbls = [0, -0, 1.5, -2.5, NaN, Infinity, -Infinity, 1e300, 5e-324, 9007199254740993, 0.1];
    var flts = [0, -0, 1.5, NaN, Infinity, 3.4028234663852886e38, 1.401298464324817e-45, 0.1, -16777217];
    for (var i = 0; i < ints.length; i++) i32[i] = ints[i];
    for (var i = 0; i < dbls.length; i++) f64[i] = dbls[i];
    for (var i = 0; i < flts.length; i++) f32[i] = flts[i];
    var fmt = function (x) { return Object.is(x, -0) ? "-0" : String(x); };
    var m = retModule, out = [];
    for (var round = 0; round < 3; round++) {
        var row = [];
        for (var k = 0; k < 11; k++)
            row.push(m.viaI(k), m.viaT(k), fmt(m.viaD(k)), fmt(m.viaF(k)), m.viaImp(k));
        out.push(row.join(","));
    }
    return out.join("\n");
}
`

// asmRetWant is what Node gives for asmRetSrc's retRun().
const asmRetWant = "0,0,0,0,0,1,-1,-0,-0,7,-1,-1,1.5,1.5,14,2147483647,-2147483647,-2.5,NaN,2,-2147483648,-2147483648,NaN,Infinity,28,65536,-65536,Infinity,3.4028234663852886e+38,35,-7,-7,-Infinity,1.401298464324817e-45,5,0,0,1e+300,0.10000000149011612,49,0,0,5e-324,-16777216,56,0,0,9007199254740992,0,8,0,0,0.1,0,70"

// TestAsmJITCallResults runs asmRetSrc with the JIT -- its callees called
// natively, not inlined -- under its stress settings and with the
// collector marking: each answer Node's and the typed interpreter's; and
// the conversion of a call's result of a function of the module, which
// validation proves of its declared type (ir.RawNumber), its number word
// unchecked (ssa.OpNumberF64), with no conversion of Go's at all
// (OpOddballF64), but the import's and the parameters'. Converted as an import's, a call of
// fib(k - 1) in asm_fib had a number check and a call of Go for a string
// at each.
func TestAsmJITCallResults(t *testing.T) {
	want := strings.Repeat(asmRetWant+"\n", 3)[:3*len(asmRetWant)+2]
	typed, ran := asmRun(t, asmRetSrc, "retRun()", true, true, "retModule", "viaI")
	if !ran {
		t.Fatal("the module did not run typed")
	}
	if typed != want {
		t.Fatalf("typed code differs from Node:\n%s", firstDiff(typed, want))
	}
	for _, c := range []struct {
		name   string
		stress jitStressConfig
	}{{"plain", jitStressConfig{}}, {"stress", jitStressConfig{threshold: true, budget: 3, deopt: 7}}, {"marking", jitStressConfig{}}} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "marking" {
				jitMarkingForTest(t)
			}
			jitNoInline = func(fn *bytecode.Function) bool { return fn.Name != "retRun" }
			t.Cleanup(func() { jitNoInline = nil })
			ops := map[string][2]int{}
			jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
				var n [2]int
				for _, b := range f.Blocks {
					for _, v := range b.Values {
						switch v.Op {
						case ssa.OpNumberF64:
							n[0]++
						case ssa.OpOddballF64:
							n[1]++
						}
					}
				}
				ops[fn.Name] = n
			}
			t.Cleanup(func() { jitSSAOptimized = nil })
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = c.stress
			v, err := r.Run(compileForTest(t, asmRetSrc+"\nretRun()"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != want {
				t.Errorf("native code differs from Node:\n%s", firstDiff(got, want))
			}
			for name, how := range asmJITNative(r, "retModule", []string{"viaI", "viaD", "viaF", "viaT", "viaImp"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
			if c.name == "stress" {
				return
			}
			// Each caller's conversion of its call's result: numbers'
			// unchecked, the import's Go's for what is no number; and of
			// its parameter, which its caller, ordinary code, may give
			// anything.
			for name, n := range map[string][2]int{"viaI": {1, 1}, "viaT": {1, 1}, "viaD": {1, 1}, "viaF": {1, 1}, "viaImp": {0, 2}} {
				if got := ops[name]; got != n {
					t.Errorf("%s: %d unchecked numbers and %d conversions by Go, want %d and %d", name, got[0], got[1], n[0], n[1])
				}
			}
		})
	}
}
