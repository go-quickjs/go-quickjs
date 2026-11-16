//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regDeadCompile is src compiled to register code.
func regDeadCompile(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	compiler.SetRegisterForm(true)
	defer compiler.SetRegisterForm(false)
	return compileForTest(t, src)
}

// TestJITRegExpDeadExecLeaf runs regexpDeadKindsSrc's calls of exec whose
// value nothing reads but its truth -- a statement's, an if's, a while's,
// a not's, an &&'s, a ternary's -- and regexpDeadTestSrc's, in register
// code natively and with no JIT, the collector idle and marking: each
// answer, the statics and lastIndex included, is the interpreter's and
// Node's, and the register code's leaf call of exec is told nothing reads
// it (ssa.CallSite's Unused, by the program's liveness,
// windowResultUnused), so that it gives back exec's dummy rather than
// making an array, as the stack form's leaf does.
func TestJITRegExpDeadExecLeaf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// dead is whether nothing reads the call's value but its truth.
	cases := []struct {
		name, call, want string
		dead             bool
	}{
		{"kUp", "kUp('xab'), RegExp.lastMatch + RegExp.$2", "abb", true},
		{"kGlobal", "kgre.lastIndex = 0, kGlobal('bxbb'), kgre.lastIndex + RegExp.leftContext", "4bxb", true},
		{"kLocal", "kLocal('qab', /(q)(a)/), RegExp.$1 + RegExp.$2", "qa", true},
		{"kProp", "kProp('cab', { re: /(c)/ }), RegExp.$1", "c", true},
		{"kIf", "kIf('ab', /a/) + RegExp.lastMatch", "3a", true},
		{"kWhile", "kWhile('a1a2a3', /a(.)/g) + RegExp.$1", "33", true},
		{"kNot", "kNot('ab', /b/) + ',' + kNot('ab', /c/)", "0,3", true},
		{"kAnd", "kAnd('ab', /b/) + ',' + kAnd('ab', /c/)", "3,0", true},
		{"kTernary", "kTernary('ab', /b/) + ',' + kTernary('ab', /c/)", "6,3", true},
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regexpDeadKindsSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			run := func(call string) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					v, err := r.Run(regDeadCompile(t, "''+("+call+")"))
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", call, got[0], got[1])
				}
				return got[0]
			}
			for range 30 {
				for _, c := range cases {
					if got := run(c.call); got != c.want {
						t.Fatalf("%s = %s, want Node's %s", c.call, got, c.want)
					}
				}
			}
			r.jitStress = jitStressConfig{}
			exec := uintptr(unsafe.Pointer(r.regexpExecFn))
			for _, c := range cases {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
				e := fn.Object().fn().closure.closureCode.jitReg
				if e == nil || e.code == nil {
					t.Errorf("marking %v: %s not compiled", marking, c.name)
					continue
				}
				unused := 0
				for _, st := range e.code.GoCallStates() {
					if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == exec && st.Unused {
						unused++
					}
				}
				// A loop's test copied before it (the compile package's
				// loopTest) is a second call.
				if c.dead && unused == 0 || !c.dead && unused != 0 {
					t.Errorf("marking %v: %s's code has %d leaves' calls of exec whose result nothing reads, want them %v", marking, c.name, unused, c.dead)
				}
				regexpExecLeafForTest(r).dummy = nil
				entries := r.RegNativeStats().Entries
				if got := run(c.call); got != c.want {
					t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.call, got, c.want)
				}
				if r.RegNativeStats().Entries == entries {
					t.Errorf("marking %v: %s did not run natively", marking, c.call)
				}
				if c.dead && regexpExecLeafForTest(r).dummy == nil {
					t.Errorf("marking %v: %s's calls made exec's arrays", marking, c.call)
				}
				if regexpExecLeafForTest(r).dead != nil {
					t.Errorf("marking %v: %s left exec's leaf told of a call", marking, c.call)
				}
			}
		})
	}
	// The whole source's answer, its later runs native.
	rts := regNativeRuntimes(t, "var allSrc")
	for i, r := range rts {
		r.SetJITWarmUp(1, 1)
		v, err := r.Run(regDeadCompile(t, regexpDeadKindsSrc+"\n"+regexpDeadTestSrc))
		if got := regNativeShow(r, v, err); got != regexpDeadTestWant {
			t.Errorf("runtime %d (JIT %v):\ngot  %s\nwant %s", i, i == 0, got, regexpDeadTestWant)
		}
	}
}
