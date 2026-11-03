//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A megamorphic site's read of a receiver that is no object -- a string, a
// number, a boolean -- is Go's on the read's branch of its own, as a miss
// of the megamorphic cache is, native code going on after it
// (ssa.ObjectOfTry): React's createElement reads a.defaultProps of an
// element type that is a string ("div") as often as a function, and had
// left native code at every element. Each answer is the interpreter's;
// warm, the site leaves native code no more.
func TestJITMegamorphicReadOfPrimitive(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function C0(){} C0.defaultProps = {a: 1}
		function C1(){} C1.x = 1; C1.defaultProps = {b: 2}
		function C2(){} C2.y = 1; C2.z = 2
		function C3(){} C3.w = 1; C3.defaultProps = null
		function C4(){} C4.q = 1; C4.r = 2; C4.s = 3
		function C5(){} C5.t = 1
		String.prototype.shared = 7
		const types = [C0, "div", C1, "span", C2, 3, C3, true, C4, "p", C5, "li"]
		function props(a) {
			let n = 0
			if (a && a.defaultProps) n += 1
			n += a.shared === 7 ? 10 : 0
			return n
		}
		function run(k) {
			let s = 0
			for (let i = 0; i < k; i++) s += props(types[i % types.length]) * (i % 5 + 1)
			return s
		}`
	r, run := jitTierRuntimeForTest(t, 1, setup)
	for range 3 {
		run(`run(600)`)
	}
	before := r.JITStats().Hosts
	run(`run(600)`)
	if hosts := r.JITStats().Hosts - before; hosts > 20 {
		t.Errorf("warm, 600 reads left native code %d times", hosts)
	}
}

// A getter the megamorphic read of a primitive finds on its prototype is
// called with the primitive as this: itself in strict code, a wrapper in
// sloppy code (OrdinaryCallBindThis). The answers are Node's.
func TestJITMegamorphicReadOfPrimitiveThis(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		for (const P of [String.prototype, Number.prototype, Boolean.prototype]) {
			Object.defineProperty(P, "kindS", { get() { "use strict"; return typeof this }, configurable: true })
			Object.defineProperty(P, "kindL", { get() { return typeof this }, configurable: true })
		}
		function O0(){ this.kindS = "o0" } function O1(){ this.a = 1; this.kindS = "o1" }
		function O2(){ this.b = 1; this.c = 2; this.kindS = "o2" } function O3(){ this.d = 1; this.kindS = "o3" }
		const recv = [new O0, "div", new O1, 3, new O2, true, new O3, "p", 2.5, {e: 1, kindS: "e"}, {f: 1, g: 2, kindS: "f"}, false]
		function kinds(k) {
			let out = ""
			for (let i = 0; i < k; i++) {
				const a = recv[i % recv.length]
				out += a.kindS + (typeof a === "object" ? "" : "," + a.kindL) + ";"
			}
			return out
		}`
	r, run := jitTierRuntimeForTest(t, 1, setup)
	for range 3 {
		run(`kinds(1200).length`)
	}
	v, err := r.Run(compileForTest(t, `kinds(12)`))
	const want = "o0;string,object;o1;number,object;o2;boolean,object;o3;string,object;number,object;e;f;boolean,object;"
	if err != nil || v.String().Go() != want {
		t.Fatalf("kinds(12) = %v, %v; want Node's %q", v, err, want)
	}
}
