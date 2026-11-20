//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regPlainSrc's post writes two properties of objects of forty layouts,
// each past the eight a property search takes (abi.MaxScan): megamorphic
// sites, whose misses native code leaves to a call of Go that writes
// without a frame (jitGoCallPlain, jitPlainWrite). Its register code's
// write of lim is at register pc 2, where its stack form's bytecode has
// the write of flag: the call of Go had read the stack form's instruction
// at the register pc and written v to flag. Octane's Typescript, whose
// parsePostfixOperators does so with ast.limChar, printed its AST with
// parentheses around terms and reported parse errors.
const regPlainSrc = `
function post(o, v) { o.flag = false; o.lim = v; return o }
function caller(o, v) { return post(o, v) }
function make(i) {
	var o = {};
	o["k" + (i % 40)] = 0;
	o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8;
	o.flag = true; o.lim = -1;
	return o;
}
function run(n) {
	var bad = 0;
	for (var i = 0; i < n; i++) {
		var o = make(i);
		caller(o, i);
		if (o.flag !== false || o.lim !== i) bad++;
	}
	return bad;
}
`

// TestJITRegPlainWrite runs regPlainSrc's run with post called natively
// and inlined, the collector idle and marking: no write is lost or made to
// another property.
func TestJITRegPlainWrite(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, inline := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("inline=%v,marking=%v", inline, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				if !inline {
					regNoInline(t)
				}
				rts := regNativeRuntimes(t, regPlainSrc)
				rts[0].SetJITWarmUp(1, 1)
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
					for k := range 6 {
						v, err := r.Call(fn, Undefined, []Value{Float(400)})
						if got := regNativeShow(r, v, err); got != "0" {
							t.Fatalf("runtime %d, call %d: %s objects written wrong, want 0", i, k, got)
						}
					}
				}
				if !inline && rts[0].RegNativeStats().Entries == 0 {
					t.Error("no register code entered natively")
				}
			})
		}
	}
}
