//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// A pointer word stored natively while the collector marks, each way there is
// of it -- nothing over nothing, which the store routine goes straight back
// from, a pointer over nothing, nothing over a pointer, a pointer over a
// pointer -- in a property, an element and a literal, with every register
// native code keeps given junk across the stores that reach Go. Each answer
// is the interpreter's.
func TestJITStoreWords(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function step(o, a, i, box, other) {
			o.p = i
			o.q = i & 1 ? box : i
			o.r = i & 2 ? null : i
			o.s = i & 1 ? box : other
			a[0] = i
			a[1] = i & 1 ? box : i
			a[2] = i & 1 ? other : box
			var lit = { x: i, y: box, z: i + 1, w: 's' }
			return o.p + (typeof o.q === 'object' ? o.q.v : o.q) + (o.r === null ? 1 : o.r) + o.s.v +
				a[0] + (typeof a[1] === 'object' ? a[1].v : a[1]) + a[2].v + lit.x + lit.y.v + lit.z + lit.w.length
		}
		function run(n) {
			var o = { p: 0, q: 0, r: 0, s: null }, a = [0, 0, 0], box = { v: 3 }, other = { v: 5 }, s = 0
			for (var i = 0; i < n; i++) s = (s + step(o, a, i, box, other)) | 0
			last = o
			return s
		}
		var got = [], last
		for (var k = 0; k < 40; k++) got.push(run(2001))
		got.join()`
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	want := v.String().Go()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking %v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
				jitClobberStoresForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			v, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("%s", got)
			}
			if r.JITStats().SSAEntries == 0 {
				t.Errorf("no native entry")
			}
			// The last store over a pointer was a number's: the pointer's word
			// went, which no script sees, and with it the barrier that shades
			// what it held.
			o := r.global.getOwn(r.atoms.intern("last")).value().Object()
			if q := o.getOwn(r.atoms.intern("q")).value(); !q.IsNumber() || q.ref != nil {
				t.Errorf("a number over a pointer left its pointer word: %v, %v", q.num, q.ref)
			}
		})
	}
}
