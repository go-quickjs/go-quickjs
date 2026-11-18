//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

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
	t.Parallel()
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
	t.Parallel()
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

// A megamorphic site's write to a receiver that is no object -- a number,
// a string, a boolean -- is Go's on the write's branch of its own, as a
// miss of the megamorphic cache is, native code going on after it
// (ssa.ObjectOfTry): it had left native code at every such write, with no
// entry after it, the interpreter running the rest of the invocation --
// microbench's carried_every_2, a number on every other turn, ran 3.6
// times as long as without the JIT. Each answer is the interpreter's;
// warm, the site leaves native code no more and nothing deoptimizes.
func TestJITMegamorphicWriteOfPrimitive(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		const obj = {a: 1}
		function write(k) {
			let s = 0
			for (let i = 0; i < k; i++) {
				const o = i % 2 === 0 ? i : obj
				o.a = i
				s += obj.a
			}
			return s
		}`
	r, run := jitTierRuntimeForTest(t, 1, setup)
	r.jitStress = jitStressConfig{}
	for range 3 {
		run(`write(600)`)
	}
	st := r.JITStats()
	run(`write(600)`)
	after := r.JITStats()
	if after.SSAEntries == st.SSAEntries {
		t.Errorf("warm, write(600) never entered native code")
	}
	if hosts := after.Hosts - st.Hosts; hosts != 0 {
		t.Errorf("warm, 300 writes of a number's property left native code %d times", hosts)
	}
	if guards := after.Guards - st.Guards; guards != 0 {
		t.Errorf("warm, the writes deoptimized %d times", guards)
	}
	if after.Interpreted != st.Interpreted {
		t.Errorf("warm, the interpreter ran %d invocations", after.Interpreted-st.Interpreted)
	}
}

// What the write of a primitive's property does, made by Go from native
// code (TestJITMegamorphicWriteOfPrimitive): a setter on the primitive's
// prototype called with the primitive as this -- itself in strict code, a
// wrapper in sloppy code -- nothing in sloppy code otherwise, a TypeError
// in strict code, and a TypeError for undefined and null in either. The
// answers are Node's.
func TestJITMegamorphicWriteOfPrimitiveSemantics(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		let log = ""
		Object.defineProperty(Number.prototype, "tagS", { set(v) { "use strict"; log += typeof this + v + ";" }, configurable: true })
		Object.defineProperty(Boolean.prototype, "tagS", { set(v) { log += typeof this + v + ";" }, configurable: true })
		const obj = {tagS: 0, a: 0}
		const recv = [obj, 3, "s", obj, true, 2.5, obj, false, "t"]
		function sloppy(k) {
			let n = 0
			for (let i = 0; i < k; i++) {
				const o = recv[i % recv.length]
				o.tagS = i % 7
				o.a = i
				n += o.a === i ? 1 : 0
			}
			return n + ":" + obj.tagS + ":" + obj.a
		}
		function strict(k) {
			"use strict"
			let n = 0, e = ""
			for (let i = 0; i < k; i++) {
				const o = recv[i % recv.length]
				try {
					o.tagS = i % 7
					o.a = i
				} catch (x) {
					n++
					e = x.constructor.name
				}
			}
			return n + ":" + e + ":" + obj.a
		}
		const nul = [obj, 1, obj, null, obj, undefined]
		function nullish(k) {
			let n = 0, e = ""
			for (let i = 0; i < k; i++) {
				try { nul[i % nul.length].a = i } catch (x) { n++; e = x.constructor.name }
			}
			return n + ":" + e + ":" + obj.a
		}`
	r, run := jitTierRuntimeForTest(t, 1, setup)
	for range 3 {
		run(`sloppy(900)`)
		run(`strict(900)`)
		run(`nullish(600)`)
	}
	for _, c := range []struct{ src, want string }{
		{`(log = "", sloppy(9), log)`, "number1;object4;number5;object0;"},
		{`sloppy(9)`, "3:6:6"},
		{`(log = "", strict(9), log)`, "number1;object4;number5;object0;"},
		{`strict(9)`, "6:TypeError:6"},
		{`nullish(6)`, "2:TypeError:4"},
	} {
		v, err := r.Run(compileForTest(t, c.src))
		if err != nil || v.String().Go() != c.want {
			t.Errorf("%s = %v, %v; want Node's %q", c.src, v, err, c.want)
		}
	}
}
