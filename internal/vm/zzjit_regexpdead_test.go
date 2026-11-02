//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Native code's calls of exec whose result nothing reads (zzz_regexpdead.go)
// tell exec's leaf so: a leaf's call by its description, which marks it
// (ssa.CallSite's Unused) -- a statement's, an if's, a while's, a not's, an
// &&'s, a ternary's -- and a literal's call of Go by the instruction after
// it. Each answer is the interpreter's and Node's, with the collector idle
// and while it marks, and the calls, made natively, gave back exec's dummy
// for the array.
func TestJITRegExpDeadExec(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []struct {
		name, call, want string
		leaf             bool
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
		{"kLit", "kLit('zab'), RegExp.$1 + RegExp.leftContext", "az", false},
		{"kLitG", "kLitG('zab'), RegExp.$2 + RegExp.rightContext", "b", false},
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			for _, c := range cases {
				rounds = append(rounds, c.call)
			}
			rounds = append(rounds, "all()")
		}
		r := jitAgreesForTest(t, regexpDeadKindsSrc+"\n"+regexpDeadTestSrc, rounds...)
		exec := uintptr(unsafe.Pointer(r.regexpExecFn))
		for _, c := range cases {
			e := jitNativeForTest(t, r, c.name)
			unused := 0
			for _, st := range e.ssa.GoCallStates() {
				if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == exec && st.Unused {
					unused++
				}
			}
			if c.leaf && unused != 1 {
				t.Errorf("marking %v: %s's code has %d leaves' calls of exec whose result nothing reads, want 1", marking, c.name, unused)
			}
			regexpExecLeafForTest(r).dummy = nil
			fast, entries := r.jit.leafFast, e.ssaStats.entries
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.call, got, c.want)
			}
			if e.ssaStats.entries == entries {
				t.Errorf("marking %v: %s did not run natively", marking, c.call)
			}
			if c.leaf && r.jit.leafFast == fast {
				t.Errorf("marking %v: %s made no leaf's call through the routine", marking, c.call)
			}
			if regexpExecLeafForTest(r).dummy == nil {
				t.Errorf("marking %v: %s's calls made exec's arrays", marking, c.call)
			}
			if regexpExecLeafForTest(r).dead != nil {
				t.Errorf("marking %v: %s left exec's leaf told of a call", marking, c.call)
			}
		}
		// The source's own answer, three runs of all(), the later two
		// natively, from a runtime of its own.
		r = jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, regexpDeadKindsSrc+"\n"+regexpDeadTestSrc))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpDeadTestWant {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, regexpDeadTestWant)
		}
		if jitNativeForTest(t, r, "blk"); regexpExecLeafForTest(r).dummy == nil {
			t.Errorf("marking %v: all()'s calls made exec's arrays", marking)
		}
		debug.SetGCPercent(gc)
	}
}

// Native code's calls of Go of a string's replace whose result nothing
// reads (zzz_regexpdead.go) tell it so by the bytecode after the call:
// each answer is the interpreter's and Node's, with the collector idle and
// while it marks, the calls made natively; and jitCallMadeDead's call of
// replace, a drop after it, is replace's without its string -- the result
// the subject -- with the matches, the statics and lastIndex as replace's,
// where any other instruction after it has the string made.
func TestJITRegExpDeadReplace(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	names := []string{"rUp", "rGlobal", "rLocal", "rProp", "rLit", "rIf", "rSticky", "rTpl"}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			for _, c := range regexpDeadReplaceKinds {
				rounds = append(rounds, c.call)
			}
		}
		r := jitAgreesForTest(t, regexpDeadReplaceKindsSrc, rounds...)
		for i, c := range regexpDeadReplaceKinds {
			e := jitNativeForTest(t, r, names[i])
			entries := e.ssaStats.entries
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.call, got, c.want)
			}
			if e.ssaStats.entries == entries {
				t.Errorf("marking %v: %s did not run natively", marking, c.call)
			}
			if *r.strReplaceDead != nil {
				t.Errorf("marking %v: %s left replace told of a call", marking, c.call)
			}
		}
		// The source's own answer, three runs of all(), the later two
		// natively, from a runtime of its own.
		r = jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, regexpDeadReplaceTestSrc))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpDeadReplaceTestWant {
			t.Errorf("marking %v: all() differs from Node's:\ngot  %s\nwant %s", marking, got, regexpDeadReplaceTestWant)
		}
		jitNativeForTest(t, r, "blk")
		debug.SetGCPercent(gc)
	}

	r := jitRuntimeForTest(t, Config{JIT: true})
	re, err := r.Run(compileForTest(t, `var re = /(b)/g; re.lastIndex = 2; re`))
	if err != nil {
		t.Fatal(err)
	}
	call := bytecode.Instr{Op: bytecode.OpCallMethod, A: 2}
	for _, c := range []struct {
		next bytecode.Op
		want string
	}{{bytecode.OpDrop, "abcb"}, {bytecode.OpJumpIfFalse, "abcb"}, {bytecode.OpReturn, "ac"}} {
		f := &frame{cl: &closure{fn: &bytecode.Function{Code: []bytecode.Instr{call, {Op: c.next}}}}}
		sp := r.stackTop
		r.stack[sp], r.stack[sp+1], r.stack[sp+2], r.stack[sp+3] = Str(NewString("abcb")), Obj(r.strReplaceFn), re, Str(emptyString)
		v, err := jitCallMadeDead[struct{}](r, f, 0, call, sp+4)
		clear(r.stack[sp : sp+4])
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != c.want {
			t.Errorf("replace followed by %v = %s, want %s", c.next, got, c.want)
		}
		if st, err := r.Run(compileForTest(t, `re.lastIndex + RegExp.leftContext + RegExp.$1`)); err != nil || st.String().Go() != "0abcb" {
			t.Errorf("replace followed by %v left %v, %v, want 0abcb", c.next, st, err)
		}
		if *r.strReplaceDead != nil {
			t.Errorf("replace followed by %v left replace told of a call", c.next)
		}
	}
}

// Native code's replace whose result is tested for truth alone is as
// truthy as the string it would have made (zzz_regexpdead.go's
// regexpDeadTruth): each answer is the interpreter's and Node's, with the
// collector idle and while it marks, the call made natively -- each from a
// runtime of its own, as code that met them all would be compiled for none.
func TestJITRegExpDeadReplaceTruth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		for _, c := range regexpDeadTruth {
			r := jitAgreesForTest(t, regexpDeadTruthSrc, c.call, c.call, c.call, c.call, c.call, c.call)
			e := jitNativeForTest(t, r, c.call[:strings.IndexByte(c.call, '(')])
			entries := e.ssaStats.entries
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.call, got, c.want)
			}
			if e.ssaStats.entries == entries {
				t.Errorf("marking %v: %s did not run natively", marking, c.call)
			}
		}
		debug.SetGCPercent(gc)
	}
}
