//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// regElemSrc reads and writes elements native code learns the kinds of at
// its exits (zzjit_zregelem.go): a typed array's, a BigInt64Array's, an
// object's property by its name, a string's character, an object's
// property written by its name, and additions of an array's strings.
const regElemSrc = `
var mem = new Uint8Array(256);
for (var i = 0; i < 256; i++) mem[i] = i;
var big = new BigInt64Array(16);
for (var i = 0; i < 16; i++) big[i] = BigInt(i * 3);
var dict = { add: 1, sub: 2, mul: 3, div: 4 };
var names = ["add", "sub", "mul", "div"];
function readTyped(a, n) { var s = 0; for (var i = 0; i < n; i++) { var v = a[i & 255]; s += v === 7 ? 1 : 0 } return s }
function readBig(a, n) { var s = 0n; for (var i = 0; i < n; i++) s += a[i & 15]; return s }
function keyed(n) { var s = 0; for (var i = 0; i < n; i++) { var k = names[i & 3]; var v = dict[k]; s += v === 3 ? 1 : 0 } return s }
function chars(str, n) { var s = 0; for (var i = 0; i < n; i++) { var c = str[i % str.length]; if (c === "a") s++ } return s }
function writes(n) { var o = {}; for (var i = 0; i < n; i++) o[names[i & 3]] = i; return o.add + o.div }
function adds(n) { var t = "", parts = ["x", "y", "z"]; for (var i = 0; i < n; i++) { t = t + parts[i % 3]; if (t.length > 50) t = "" } return t }
function counts(n) { var c = {}; for (var i = 0; i < n; i++) { var k = names[i & 3]; c[k] = (c[k] | 0) + 1 } return c.add + c.mul }
function main(n) {
	return readTyped(mem, n) + "," + readBig(big, n) + "," + keyed(n) + "," + chars("banana", n) + "," + writes(n) + "," + adds(n) + "," + counts(n);
}
`

// TestJITRegElementKinds runs regElemSrc's main natively and in a runtime
// without the JIT and compares their answers: each element access, which
// had left native code at every turn -- the read of a typed array's, a
// string's or an object's element by its cell, the write of an object's
// property by its name -- is made natively, or by Go from native code,
// once its code is compiled again for what it met (jitRegElemSeen).
func TestJITRegElementKinds(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regElemSrc)
			var got [2]string
			var before map[string]uint64
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("main"))
				for k := range 20 {
					if i == 0 && k == 10 {
						before = r.RegNativeStats().HostOps
					}
					v, err := r.Call(fn, Undefined, []Value{Float(3000)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if want := "12,67404,750,1500,5995,xyzxyzxyzxyzxyzxyzxyzxyzxyzxyzxyzxyzxyzxyz,1500"; got[0] != got[1] || got[1] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			st := rts[0].RegNativeStats()
			t.Logf("hosts %v, entries %d", st.HostOps, st.Entries)
			// Of 20 runs of 21,000 accesses each: left to Go only before the
			// code learned them and was compiled again for them, once it
			// had spent a budget since it learned them (jitRegLearnedDue):
			// none in the last ten.
			for _, op := range []string{"get_index", "set_index", "add_index"} {
				if n := st.HostOps[op] - before[op]; n != 0 {
					t.Errorf("%s left to Go at %d host exits in the last ten runs", op, n)
				}
			}
		})
	}
}

// regTypedKindSrc reads and writes typed arrays of each element type, each
// type through functions of its own, whose sites meet that type alone.
var regTypedKindSrc = func() string {
	var b strings.Builder
	b.WriteString("var values = [0, -1, 1.5, 255.5, 256, -129, 32768, 65536, 2147483648, -2147483649, 4294967296, 1e10, NaN, Infinity, 0.1, 1/3];\n")
	for _, n := range typedKindNames {
		fmt.Fprintf(&b, "function fill%[1]s(a) { for (var i = 0; i < a.length; i++) a[i] = values[i %% values.length] * 3 - i; return a }\n", n)
		fmt.Fprintf(&b, "function sum%[1]s(a) { var s = 0; for (var i = 0; i < a.length; i++) s += a[i]; return s }\n", n)
		fmt.Fprintf(&b, "function main%[1]s() { return sum%[1]s(fill%[1]s(new %[1]s(64))) }\n", n)
	}
	return b.String()
}()

// TestJITRegTypedKinds runs, for each element type, register code reading
// and writing typed arrays of that type, and compares its answers with a
// runtime's without the JIT. What a site's typed array met native code
// learns as it runs (jitTypedSeen) has the register code compiled again for
// it: learned in a cell the code was compiled without, it had stayed the
// code that reads every element through its cold path, a typed array an
// Array's read's miss.
func TestJITRegTypedKinds(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regTypedKindSrc)
			// The compiles counted as the code asks them, not as stress
			// configures them.
			rts[0].jitStress = jitStressConfig{}
			for i, n := range typedKindNames {
				var got [2]string
				for k, r := range rts {
					fill, _ := r.GetProp(Obj(r.Global()), r.Intern("fill"+n))
					sum, _ := r.GetProp(Obj(r.Global()), r.Intern("sum"+n))
					ctor, _ := r.GetProp(Obj(r.Global()), r.Intern(n))
					for range 30 {
						a, err := r.Construct(ctor, []Value{Float(1000)})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := r.Call(fill, Undefined, []Value{a}); err != nil {
							t.Fatal(err)
						}
						v, err := r.Call(sum, Undefined, []Value{a})
						got[k] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("sum%s: with the JIT %q, without %q", n, got[0], got[1])
				}
				k := []int{abi.ElemInt8, abi.ElemUint8, abi.ElemUint8Clamped, abi.ElemInt16, abi.ElemUint16, abi.ElemInt32, abi.ElemUint32, abi.ElemFloat32, abi.ElemFloat64}[i]
				r := rts[0]
				for _, f := range []string{"sum", "fill"} {
					cl := r.global.getOwn(r.atoms.intern(f + n)).value().Object().fn().closure
					e := r.jit.regCache[r.jitKeyOf(cl.fn)]
					if e == nil || e.code == nil || e.life == nil {
						t.Errorf("%s%s has no register code", f, n)
						continue
					}
					if kinds, _ := typedKindCells(t, r, f+n); kinds != 1<<k {
						t.Errorf("%s%s learned element types %#x, want %#x", f, n, kinds, 1<<k)
					}
					if e.life.reopts == 0 {
						t.Errorf("%s%s never compiled again for the element type it learned", f, n)
					}
				}
			}
		})
	}
}
