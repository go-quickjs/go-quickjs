//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"reflect"
	"testing"
)

// jitSnapAskedSrc has a little of everything the snapshot is asked of:
// properties read, written and added, accessors, methods, globals,
// closures and inlined callees, literals, leaves (Object.create, a
// RegExp's exec), forEach's callbacks, strings, typed arrays and BigInts.
const jitSnapAskedSrc = `
function Point(x, y) { this.x = x; this.y = y }
Point.prototype.norm = function () { return this.x * this.x + this.y * this.y };
Object.defineProperty(Point.prototype, "sum", { get() { return this.x + this.y } });
var total = 0;
function add(a, b) { return a + b }
function make(i) { return i & 1 ? { a: i, b: [i, i + 1] } : Object.create(Point.prototype) }
var re = /(\d+)-(\d+)/;
function work(n) {
	let s = 0;
	const xs = [];
	const ta = new Int32Array(8);
	let big = 1n;
	for (let i = 0; i < n; i++) {
		const p = new Point(i, i + 1);
		s = add(s, p.norm() + p.sum) % 1000003;
		const o = make(i);
		if (o.a !== undefined) { o.c = o.a + 1; xs.push(o) }
		ta[i & 7] = ta[(i + 1) & 7] + i;
		const m = re.exec("a" + i + "-" + (i * 2));
		if (m) s += m[1].length + parseInt(m[2]);
		s += String.fromCharCode(65 + (i % 26)).charCodeAt(0) + "abcdef".indexOf("d");
		big = (big * 3n) % 1000003n;
		total += i;
	}
	xs.forEach((o, k) => { s = (s + o.b[0] + k) | 0 });
	const f = (k) => k + s;
	return s + f(1) + Number(big) + ta[3];
}
for (let k = 0; k < 20; k++) work(64);
`

// TestJITSnapshotAnswersAsAsked holds the snapshot of a compile in the
// background (jitRegSnapshotOf) to the feedback's answers as it asked them
// ahead -- what the snapshot had kept of each, boxed in a map by question,
// before its answers were kept as their methods' results by argument: for
// every question it asked -- at every PC whose op asks each method, of no
// argument, of a leaf's call, a leaf's kind, a pool's root, by a state's
// PC -- in the function and in every callee it asked to the inliner's
// depth, the build's question has the answer the feedback gave, the zero
// values where it answered nothing, the last answer where it was asked
// twice; and it keeps no answer it was not given. The functions are
// jitSnapAskedSrc's, and with QUICKJS_JIT_CORPUS, Octane's after a warm-up
// (TestJITRegCompileCorpus).
func TestJITSnapshotAnswersAsAsked(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	type question struct {
		s   *jitSnapshot
		m   jitSnapMethod
		arg int64
	}
	type heard struct {
		answered bool
		results  []any
	}
	asked := map[question]heard{}
	var order []question
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.snapAsked = func(s *jitSnapshot, m jitSnapMethod, arg int64, answered bool, results []any) {
			q := question{s, m, arg}
			h, seen := asked[q]
			if !seen {
				order = append(order, q)
			}
			// As a map by question kept them: the last answer.
			if answered || !seen {
				h = heard{answered, results}
			}
			asked[q] = h
		}
	})
	var r *Runtime
	var cls []*closure
	if dir := os.Getenv("QUICKJS_JIT_CORPUS"); dir != "" {
		r, cls = jitOctaneCorpusForTest(t, dir)
	} else {
		r = jitRuntimeForTest(t, Config{JIT: true})
		r.jitState().unavailable = true
		if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"work", "make", "add", "Point"} {
			cls = append(cls, jitGlobalClosureForTest(r, name))
		}
	}
	r.jitState().unavailable = false
	compared, functions := 0, 0
	unbound := map[jitSnapMethod]int{}
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
		if err != nil {
			if p, err = r.jitRegLower(cl, c.rc, jitLowerInline); err != nil {
				continue
			}
		}
		clear(asked)
		order = order[:0]
		r.jitRegSeenFeedback(cl, c)
		snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
		// The answers the worker derives, as it does before its build.
		snap.s.derive()
		functions++
		// The register sites asked by register pc (gatherPropertyAsked)
		// that no PC of the program asks the method at, by method: an ask
		// the feedback's asking at the PCs had not made, which may have
		// remembered a shape (primitiveSiteOf, accessorOf).
		for _, s := range append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...) {
			for _, a := range s.d.prop.accessors {
				if !a.bound {
					unbound[snapAccessor]++
				}
			}
			for _, a := range s.d.prop.strings {
				if !a.bound {
					unbound[snapStringProperty]++
				}
			}
			// What the calls' gather made by register pc (jitTargetsBind)
			// where no PC asks NativeCalls: a pool made early.
			tg := &s.d.calls.tg
			for _, x := range tg.lits {
				if !x.bound {
					unbound[snapNativeCalls]++
				}
			}
			for _, x := range tg.gens {
				if !x.bound {
					unbound[snapNativeCalls]++
				}
			}
			for _, x := range tg.genMakes {
				if !x.bound {
					unbound[snapNativeCalls]++
				}
			}
			for _, x := range tg.hidden {
				if !x.bound {
					unbound[snapNativeCalls]++
				}
			}
		}
		answered := map[*jitSnapshot]int{}
		for _, q := range order {
			h := asked[q]
			if h.answered {
				answered[q.s]++
			}
			reg := reflect.ValueOf(q.s.reg)
			method := reg.MethodByName(jitSnapNames[q.m])
			var in []reflect.Value
			switch mt := method.Type(); mt.NumIn() {
			case 0:
			case 1:
				in = []reflect.Value{reflect.ValueOf(q.arg).Convert(mt.In(0))}
			case 2:
				// A leaf's call's, by the leaf its PC's Leaf answered.
				leaf := q.s.a.Leaf.find(q.arg)
				if leaf == nil {
					t.Fatalf("%s(%d) asked at no leaf", jitSnapNames[q.m], q.arg)
				}
				in = []reflect.Value{reflect.ValueOf(int(q.arg)), reflect.ValueOf(leaf.v.r0)}
			}
			got := method.Call(in)
			for i, g := range got {
				want := reflect.Zero(g.Type()).Interface()
				if h.answered {
					want = h.results[i]
				}
				if !reflect.DeepEqual(g.Interface(), want) {
					t.Errorf("%s, %d callees deep: %s(%d) result %d: %+v, asked %+v", cl.fn.Name, q.s.depth, jitSnapNames[q.m], q.arg, i, g.Interface(), want)
				}
			}
			compared++
		}
		for _, s := range append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...) {
			if got, want := s.a.answered(), answered[s]; got != want {
				t.Errorf("%s, %d callees deep: %d answers kept, %d given", cl.fn.Name, s.depth, got, want)
			}
		}
		if t.Failed() {
			return
		}
	}
	if functions == 0 || compared == 0 {
		t.Fatalf("%d functions, %d answers compared", functions, compared)
	}
	t.Logf("%d functions, %d questions compared; asked by register pc at no PC: %d Accessor, %d StringProperty; made by register pc at no PC: %d NativeCalls",
		functions, compared, unbound[snapAccessor], unbound[snapStringProperty], unbound[snapNativeCalls])
}
