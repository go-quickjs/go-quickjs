//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// addAbsentSetup's ++this.count is add_prop_imm, whose read and store share
// one site: each new BP reads count where nothing has it, and the store
// adds it, then later calls read and write it where it is. Box2D's
// b2DynamicTreeBroadPhase counts its proxies so, never setting
// m_proxyCount in its constructor: once the JIT had compiled CreateProxy,
// a world's first proxy wrote NaN over m_tree, and the next call threw
// "undefined is not a function". The prototype's count, a getter's, and
// one each object has read-only are the other reads whose entries are no
// store's.
const addAbsentSetup = `function Tree() { this.root = null; }
	Tree.prototype.make = function (x) { return x; };
	function world(BP, n) {
		var bp = new BP(), s = 0;
		for (var i = 0; i < n; i++) {
			if (typeof bp.tree !== "object") return "call " + i + ": tree " + bp.tree;
			s += bp.create(i);
		}
		return s + "/" + Object.keys(bp).join() + "/" + bp.count;
	}
	function make(init) {
		function BP() { this.tree = new Tree(); this.mv = []; this.pb = []; this.pc = 0; init(this); }
		BP.prototype.create = function (x) { var f = this.tree.make(x); ++this.count; this.mv.push(f); return f; };
		return BP;
	}
	var absent = make(function (o) {});
	var inherited = make(function (o) {});
	inherited.prototype.count = 0;
	var getter = make(function (o) {});
	Object.defineProperty(getter.prototype, "count", { get: function () { return this.pc + 1; }, set: function (v) { this.pc = v; } });
	var readOnly = make(function (o) { Object.defineProperty(o, "count", { value: 7, writable: false, enumerable: true }); });
	function worlds(BP, n) {
		var out = "";
		for (var w = 0; w < n; w++) {
			var r = world(BP, 50);
			if (r.indexOf("call") === 0) return "world " + w + " " + r;
			if (w === n - 1) out = r;
		}
		return out;
	}`

// An addition by o.k += v, whose read fills nothing of its site but what
// its store would -- a key nothing has, a prototype's, a getter's, or one
// each object has read-only -- is made as the interpreter makes it once
// the JIT compiles the function, world after world.
func TestJITAddPropAbsent(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []struct{ ctor, want string }{
		// Node v26's answers.
		{"absent", "1225/tree,mv,pb,pc,count/NaN"},
		{"inherited", "1225/tree,mv,pb,pc,count/50"},
		{"getter", "1225/tree,mv,pb,pc/101"},
		{"readOnly", "1225/tree,mv,pb,pc,count/7"},
	} {
		t.Run(c.ctor, func(t *testing.T) {
			t.Parallel()
			src := "worlds(" + c.ctor + ", 10)"
			rounds := []string{src, src, src, src, src, src, src, src}
			r := jitAgreesForTest(t, addAbsentSetup, rounds...)
			v, err := r.Run(compileForTest(t, "worlds("+c.ctor+", 30)"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("worlds(%s, 30) = %s, want %s", c.ctor, got, c.want)
			}
		})
	}
}
