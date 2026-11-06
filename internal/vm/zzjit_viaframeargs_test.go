//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime/debug"
	"slices"
	"testing"
)

// viaFrameArgsSetup's functions are called through Function.prototype.call
// and read their arguments past their parameters, so that the call writes
// them to the callee's context (ssa.CallSite's FrameArgs): each reads its
// receiver too, but named's, whose receiver only its frame shows. Target
// tells a construction from a plain call by this instanceof Target.
const viaFrameArgsSetup = `
	function Target() {
		if (this instanceof Target) return arguments.length
		return -1 - arguments.length
	}
	function viaTarget(n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t * 3 + Target.call(Object.create(Target.prototype))) | 0
		return t
	}
	function reads(a) { return this.k * 10 + arguments.length + (a | 0) + (arguments[1] | 0) }
	function viaReads(o, n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t + reads.call(o) + reads.call(o, 1) + reads.call(o, 1, i)) | 0
		return t
	}
	function strict() { 'use strict'; return typeof this + arguments.length }
	function viaStrict(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = strict.call(i) + strict.call('s', 1) + strict.call(undefined, 1, 2)
		return s
	}
	function sloppy() { return typeof this + (this === globalThis) + arguments.length }
	function viaSloppy(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = sloppy.call(i) + sloppy.call('s', 1) + sloppy.call(undefined, 1, 2) + sloppy.call({}, 1)
		return s
	}
	function Shape() {}
	function named() { if (arguments.length > 1) throw new Error('stop'); return arguments.length }
	function viaNamed(o, n) {
		let t = 0
		for (let i = 0; i < n; i++) t += named.call(o, i)
		try { named.call(o, 1, 2) } catch (e) { return t + e.stack.split('\n')[1].trim().split(' ')[1] }
		return t
	}
	var o = { k: 3 }, shaped = new Shape();`

// A call through Function.prototype.call of a function that reads its
// arguments past its parameters writes the receiver from where the state
// keeps the call's operands, not straight to the callee's context. That
// receiver is the call's first argument; the slot under the call's callee,
// which a method call's receiver is in, holds the function. Native code
// gave the callee that function as its this, so Target's instanceof failed,
// reads' this.k was undefined with nothing thrown, strict's typeof this was
// function, and a callee that did not read its this had none, which a
// stack trace shows (viaNamed). Each is checked
// with the collector marking too: the receiver is the context's, written
// by the same code while it marks.
func TestJITCallThroughCallFrameArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		for name, src := range map[string]string{
			"viaTarget": `viaTarget(300)`,
			"viaReads":  `viaReads(o, 300)`,
			"viaStrict": `viaStrict(300)`,
			"viaSloppy": `viaSloppy(300)`,
			"viaNamed":  `viaNamed(shaped, 300)`,
		} {
			t.Run(fmt.Sprintf("%s/marking=%v", name, marking), func(t *testing.T) {
				r := jitAgreesForTest(t, viaFrameArgsSetup, src, src, src, src, src, src)
				e := jitNativeForTest(t, r, name)
				if !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.via }) {
					t.Errorf("%s does not call natively through call", name)
				}
			})
		}
	}
}

// applyFrameArgsSetup calls the same kinds of function by .apply, in each
// form it has: of an array, which is the built-in's, and of the caller's own
// arguments, which has an instruction of its own (OpApplyArguments), run by
// Go in a function's code and inlined where a constructor only forwards its
// arguments (forwardCall).
const applyFrameArgsSetup = viaFrameArgsSetup + `
	function viaApplyTarget(n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t * 3 + Target.apply(Object.create(Target.prototype), [1, i])) | 0
		return t
	}
	function viaApplyReads(o, n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t + reads.apply(o) + reads.apply(o, [1]) + reads.apply(o, [1, i])) | 0
		return t
	}
	function viaApplyStrict(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = strict.apply(i) + strict.apply('s', [1]) + strict.apply(undefined, [1, 2])
		return s
	}
	function viaApplySloppy(n) {
		let s = ''
		for (let i = 0; i < n; i++) s = sloppy.apply(i) + sloppy.apply(undefined, [1, 2]) + sloppy.apply({}, [1])
		return s
	}
	function viaReflect(o, n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t + Reflect.apply(reads, o, [1, i])) | 0
		return t
	}
	function forwarded() { return reads.apply(this, arguments) }
	function viaForwarded(o, n) {
		let t = 0
		for (let i = 0; i < n; i++) t = (t + forwarded.call(o, 1, i)) | 0
		return t
	}
	function Base(a) { this.a = a; this.n = arguments.length; this.isBase = this instanceof Derived }
	function Derived() { Base.apply(this, arguments) }
	Derived.prototype = Object.create(Base.prototype)
	function viaForwardedCtor(n) {
		let t = 0
		for (let i = 0; i < n; i++) { const d = new Derived(i, 2); t = (t + d.a + d.n + (d.isBase ? 1 : 0)) | 0 }
		return t
	}
	function Named() { named.apply(this, arguments) }
	function viaApplyNamed(n) {
		for (let i = 0; i < n; i++) new Named(i)
		try { new Named(1, 2) } catch (e) { return e.stack.split('\n').slice(1, 3).map(l => l.trim().split(' ')[1]).join() }
	}`

// .apply takes none of the path .call's fix was for: each of its forms
// gives its callee the receiver it was given, with the collector idle and
// marking, the stack's frames included, as the interpreter does.
func TestJITApplyFrameArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		for name, src := range map[string]string{
			"viaApplyTarget":   `viaApplyTarget(300)`,
			"viaApplyReads":    `viaApplyReads(o, 300)`,
			"viaApplyStrict":   `viaApplyStrict(300)`,
			"viaApplySloppy":   `viaApplySloppy(300)`,
			"viaReflect":       `viaReflect(o, 300)`,
			"viaForwarded":     `viaForwarded(o, 300)`,
			"viaForwardedCtor": `viaForwardedCtor(300)`,
			"viaApplyNamed":    `viaApplyNamed(300)`,
		} {
			t.Run(fmt.Sprintf("%s/marking=%v", name, marking), func(t *testing.T) {
				r := jitAgreesForTest(t, applyFrameArgsSetup, src, src, src, src, src, src)
				jitNativeForTest(t, r, name)
			})
		}
	}
}
