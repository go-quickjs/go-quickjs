//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regCellsWholeSrc's loops each make a closure of a binding of their turn
// -- a let of the loop's head, of its body, of an inner block, a const of
// a for...of -- which native code keeps in a cell of its own and gives a
// new cell at each turn's end (mir's cellsClose), and read the binding,
// or assign it, around an instruction a call of Go runs whole on the
// frame (jitRegWhole) or makes on its operands: a literal's prototype
// (set_proto_of), field, method, getter, setter, computed key and name,
// spread; an array's spread; a class; a spread whose getter assigns the
// binding; a generator's yields; a throw; a template and an in.
const regCellsWholeSrc = `
function protoNull(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {__proto__: null, a: i};
		s += o.a * 3 + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
var base = {b: 7};
function protoObj(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {__proto__: base, a: i, c: i * 2};
		s += o.a + o.b + o.c + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function method(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {m() { return i * 10 }, a: i};
		s += o.a + o.m() + f();
		fs.push(o.m);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function getter(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {get g() { return i * 5 }, a: i};
		s += o.a + o.g + f();
		fs.push(o);
	}
	return s + ":" + fs[n - 1].g + ":" + fs[n >> 1].a;
}
function setter(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {set p(v) { this.q = v + i }, a: i};
		o.p = 1;
		s += o.a + o.q + f();
		fs.push(o);
	}
	return s + ":" + fs[n - 1].q + ":" + fs[n >> 1].a;
}
function accessors(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {get g() { return i }, set g(v) { this.q = v * i }, a: i};
		o.g = 2;
		s += o.a + o.g + o.q + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function computed(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const k = "k" + (i & 3);
		const o = {[k]: i, a: i, [k + "f"]: () => i};
		s += o.a + o[k] + o[k + "f"]() + f() + o[k + "f"].name.length;
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
var src = {x: 1, y: 2};
function spread(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {...src, a: i};
		s += o.a + o.x + o.y + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
var arr = [1, 2, 3];
function arrSpread(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const a = [...arr, i, ...arr];
		s += a[3] + a.length + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function classes(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		class C { m() { return i + 1 } }
		s += new C().m() + i + f();
		fs.push(C);
	}
	return s + ":" + new fs[n - 1]().m() + ":" + new fs[n >> 1]().m();
}
function assigned(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		let c = i;
		const g = () => c;
		const o = {__proto__: null, a: c};
		c = o.a + 1;
		const p = {__proto__: null, b: c};
		s += p.b + g();
		fs.push(g);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function spreadGetter(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		let c = i;
		const g = () => c;
		const from = {get x() { c += 1000; return 1 }};
		const o = {...from, a: c};
		s += o.a + o.x + c + g();
		fs.push(g);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function blocks(n) {
	var s = 0, fs = [];
	for (var j = 0; j < n; j++) {
		let i = j * 2;
		{
			let k = i + 1;
			const h = () => k + i;
			const o = {__proto__: null, a: k, b: i};
			s += o.a + o.b + h();
			fs.push(h);
		}
		const o2 = {__proto__: null, c: i};
		s += o2.c;
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function forOf(n) {
	var s = 0, fs = [], xs = [];
	for (var j = 0; j < n; j++) xs.push(j * 3);
	for (const x of xs) {
		const f = () => x;
		const o = {__proto__: null, a: x};
		s += o.a + f();
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}

function* gen(n) {
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {__proto__: null, a: i};
		yield o.a + f();
		yield i;
	}
}
function gens(n) {
	var s = 0, k = 0;
	for (const v of gen(n)) { s += v * (k & 1 ? 1 : 3); k++ }
	return s + ":" + k;
}
function thrower(i) { if (i % 7 == 3) throw i; return i }
function throws(n) {
	var s = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {__proto__: null, a: i};
		try {
			if (i % 5 == 2) throw o.a + 1000;
			s += thrower(i);
		} catch (e) {
			s += e * 2;
		}
		fs.push(f);
	}
	return s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
var keys = {k3: 1, 5: 2};
function strs(n) {
	var s = "", c = 0, fs = [];
	for (let i = 0; i < n; i++) {
		const f = () => i;
		const o = {__proto__: null, a: i};
		s = ` + "`${i}-${o.a}`" + `;
		if (i in keys) c += 100;
		c += s.length + o.a;
		fs.push(f);
	}
	return c + ":" + s + ":" + fs[n - 1]() + ":" + fs[n >> 1]();
}
function main(n) { return protoNull(n) + ";" + protoObj(n) + ";" + method(n) + ";" + getter(n) + ";" + setter(n) + ";" + accessors(n) + ";" + computed(n) + ";" + spread(n) + ";" + arrSpread(n) + ";" + classes(n) + ";" + assigned(n) + ";" + spreadGetter(n) + ";" + blocks(n) + ";" + forOf(n) + ";" + gens(n) + ";" + throws(n) + ";" + strs(n) }
`

// TestJITRegCellsWhole checks that an instruction Go runs whole on the
// frame of native code whose captured registers are in cells of their
// own reads each captured register's value now: the register itself held
// what it held when native code entered, as a cell native code gave it at
// a scope's end is open on no register. A let of a loop's turn read so
// was stale -- {__proto__: null, a: i} had a 14 for every i past 14, and
// TestJITSSAObjectLiterals's accessors and TestJITDefinitionCallees's
// methods met it too. Each function is run natively from its start and
// by on-stack replacement, with the collector idle and marking
// (regWrongCheck), and main, which runs them all, as the JIT tiers it,
// whose literals' members -- getters and setters among them -- are calls
// of Go, not host exits.
func TestJITRegCellsWhole(t *testing.T) {
	regWrongCheck(t, regCellsWholeSrc, []regWrongCase{
		{"protoNull", []float64{300}, 2, "179400:299:150"},
		{"protoNull", []float64{7}, 0, "84:6:3"},
		{"protoObj", []float64{300}, 2, "181500:299:150"},
		{"protoObj", []float64{7}, 0, "133:6:3"},
		{"method", []float64{300}, 2, "538200:2990:1500"},
		{"method", []float64{7}, 0, "252:60:30"},
		{"getter", []float64{300}, 2, "313950:1495:150"},
		{"getter", []float64{7}, 0, "147:30:3"},
		{"setter", []float64{300}, 2, "134850:300:150"},
		{"setter", []float64{7}, 0, "70:7:3"},
		{"accessors", []float64{300}, 2, "224250:299:150"},
		{"accessors", []float64{7}, 0, "105:6:3"},
		{"computed", []float64{300}, 2, "180300:299:150"},
		{"computed", []float64{7}, 0, "105:6:3"},
		{"spread", []float64{300}, 2, "90600:299:150"},
		{"spread", []float64{7}, 0, "63:6:3"},
		{"arrSpread", []float64{300}, 2, "91800:299:150"},
		{"arrSpread", []float64{7}, 0, "91:6:3"},
		{"classes", []float64{300}, 2, "134850:300:151"},
		{"classes", []float64{7}, 0, "70:7:4"},
		{"assigned", []float64{300}, 2, "90300:300:151"},
		{"assigned", []float64{7}, 0, "56:7:4"},
		{"spreadGetter", []float64{300}, 2, "1034850:1299:1150"},
		{"spreadGetter", []float64{7}, 0, "21070:1006:1003"},
		{"blocks", []float64{300}, 2, "449100:1197:601"},
		{"blocks", []float64{7}, 0, "224:25:13"},
		{"forOf", []float64{300}, 2, "269100:897:450"},
		{"forOf", []float64{7}, 0, "126:18:9"},
		{"gens", []float64{300}, 2, "313950:600"},
		{"gens", []float64{7}, 0, "147:14"},
		{"throws", []float64{300}, 2, "178857:299:150"},
		{"throws", []float64{7}, 0, "2026:6:3"},
		{"strs", []float64{300}, 2, "46830:299-299:299:150"},
		{"strs", []float64{7}, 0, "142:6-6:6:3"},
	})
	const want = "179400:299:150;181500:299:150;538200:2990:1500;313950:1495:150;134850:300:150;224250:299:150;180300:299:150;90600:299:150;91800:299:150;134850:300:151;90300:300:151;1034850:1299:1150;449100:1197:601;269100:897:450;313950:600;178857:299:150;46830:299-299:299:150"
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("tiered/marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			regClosRun(t, regCellsWholeSrc, 300, 20, want, false)
			r := regClosRun(t, regCellsWholeSrc, 300, 20, want, true)
			st := r.RegNativeStats()
			t.Logf("entries %d, hosts %v, calls of Go %d", st.Entries, st.HostOps, st.GoCalls)
			// A literal's prototype, field, method and accessors are calls
			// of Go: none leaves native code.
			for _, op := range []string{"set_proto_of", "define_field", "define_method", "define_getter", "define_setter", "close_regs"} {
				if n := st.HostOps[op]; n > 50 {
					t.Errorf("%s left to Go at %d host exits", op, n)
				}
			}
		})
	}
}
