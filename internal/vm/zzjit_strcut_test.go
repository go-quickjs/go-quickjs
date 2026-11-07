//go:build quickjs_jit

package vm

import (
	"fmt"
	"testing"
)

// Native code makes no cut of its own: its substring, slice, substr and charAt
// make a one-character string of the runtime's or call the built-in, as its
// leaves do, which under a limit are refused (leafChecks' callers), and a
// regexp's native forms are refused outright. So a loop run natively under a
// limit cuts by the rule, with the collector idle and marking.
func TestJITCutsUnderALimit(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 256 << 20})
			v, err := r.Run(compileForTest(t, `
				var big = "  ab,cd " + "x".repeat(1 << 16) + ",ef  "
				var han = "漢".repeat(1 << 14)
				function cuts(p, q) {
					var a
					for (var i = 0; i < 20000; i++) {
						a = [p.slice(2, 6), p.substring(2, 6), p.substr(2, 4), q.charAt(3), q.substring(3, 4),
							q[3], p.split(",", 1)[0], /ab/.exec(p)[0], /(a)b/.exec(p)[1], p.match(/cd/)[0],
							p.slice(100), p.substring(100), /x+/.exec(p)[0]]
						if (i < 19000) i += 100
					}
					return a
				}
				var out
				for (var k = 0; k < 40; k++) out = cuts(big, han)
				out`))
			if err != nil {
				t.Fatal(err)
			}
			if r.JITStats().SSAEntries == 0 {
				t.Fatal("the loop never ran native code")
			}
			big := r.global.getOwn(r.atoms.intern("big")).value().String()
			han := r.global.getOwn(r.atoms.intern("han")).value().String()
			elems := v.Object().elemsOf()
			for i, e := range elems {
				s := e.String()
				shared := strWithin(s.s, big.s) || strWithin(s.s, han.s)
				if long := i >= len(elems)-3; shared != long || long != (s.ropes&strRooted != 0) {
					t.Errorf("cut %d of %d bytes: shared %v, ropes %#x", i, len(s.s), shared, s.ropes)
				}
			}
		})
	}
}
