//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regFwdPolySrc is microbench's forward_poly: forwarders a factory made,
// each handing its arguments to a function of its own, f.apply(this,
// arguments) of a captured f, called by one call.
const regFwdPolySrc = `
function wrap(f) { return function () { return f.apply(this, arguments) } }
var fs = [wrap(function (a, b) { return a + b }), wrap(function (a, b) { return a - b }),
	wrap(function (a, b) { return a * b }), wrap(function (a, b) { return a | b })];
function poly(n) { var s = 0; for (var j = 0; j < n; j++) s += fs[j & 3](j, 3); return s }
`

// TestJITRegForwardPolySettles calls forwarders whose forwards meet four
// functions: the forwarder's forward settles on all four together, which
// its code calls natively (jitRegForwardSeen), and no call of it is inlined
// for a guess at its f (jitRegForwardTarget). Inlined for the f of the
// closure the call's cache held, the call had left native code three turns
// in four. Exact answers, idle and marking; once warm, no guard fails and
// no forward is a call of Go.
func TestJITRegForwardPolySettles(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regFwdPolySrc)
			r := rts[0]
			r.jitStress = jitStressConfig{}
			for range 6 {
				regCarriedCall(t, rts, "1874975000", "poly", 50000)
			}
			before := r.RegNativeStats()
			for range 4 {
				regCarriedCall(t, rts, "1874975000", "poly", 50000)
			}
			after := r.RegNativeStats()
			if n := after.Guards - before.Guards; n != 0 {
				t.Errorf("%d guards failed once warm", n)
			}
			if n := after.GoCallOps["apply_args->function with code"] - before.GoCallOps["apply_args->function with code"]; n != 0 {
				t.Errorf("%d forwards made by Go once warm: %v", n, after.GoCallOps)
			}
			fsv, _ := r.GetProp(Obj(r.Global()), r.Intern("fs"))
			fw, _ := r.GetProp(fsv, r.Intern("0"))
			cl := fw.Object().fn().closure
			l := r.jit.regLearned[r.jitRegKeyOf(cl)]
			rpc := -1
			for i, w := range cl.reg.rc.Code {
				if w.Op() == bytecode.RApplyArgs {
					rpc = i
				}
			}
			if l == nil || rpc < 0 || rpc >= len(l.callSites) || l.callSites[rpc] != jitCallNative {
				t.Fatalf("the forward at %d is not settled as native", rpc)
			}
			if n := jitCountAt(l.nativeCalls, rpc); n != 4 {
				t.Errorf("the forward calls %d functions natively, want 4", n)
			}
		})
	}
}

// regFwdMonoSrc is microbench's forward_call2: a forwarder of one
// function, called by a loop compiled before the forwarder's forward is
// decided.
const regFwdMonoSrc = `
function target(a, b) { return a + b }
function fwd() { return target.apply(this, arguments) }
function mono(n) { var s = 0; for (var j = 0; j < n; j++) s += fwd(j, 1); return s }
`

// TestJITRegForwardMonoInlined has a forwarder's forward settle on one
// function after its caller was compiled to call it: the caller is compiled
// again to inline it (jitRegCallersReopt), and once warm no call of it, nor
// its forward, leaves native code or is made by Go. Before its forward is
// decided a plain call of it is not inlined (jitRegForwardTarget): left at
// that, forward_call2 ran 4 times as long.
func TestJITRegForwardMonoInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regFwdMonoSrc)
			r := rts[0]
			r.jitStress = jitStressConfig{}
			for range 6 {
				regCarriedCall(t, rts, "1250025000", "mono", 50000)
			}
			before, inlined := r.RegNativeStats(), r.JITStats().Inlined
			regCarriedCall(t, rts, "1250025000", "mono", 50000)
			after := r.RegNativeStats()
			if after.GoCalls != before.GoCalls || after.Hosts != before.Hosts {
				t.Errorf("once warm: %d calls of Go, %d exits: %v", after.GoCalls-before.GoCalls, after.Hosts-before.Hosts, after.GoCallOps)
			}
			if inlined == 0 {
				t.Errorf("the forwarder was not inlined: %+v", r.JITStats())
			}
		})
	}
}
