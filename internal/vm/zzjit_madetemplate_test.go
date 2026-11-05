//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"
)

// A closure native code does not make stands in a frame as its template,
// which an exit read from the slot of the constants the code was compiled
// from: those of the run of the program that made the code hot, which it did
// not keep. Code that went on past that run, as a host's next run of a
// program does, read whatever the memory had become once they were collected
// -- here 1.5, the test's own values, made where they were; in the wild half
// a pointer, 2.8e-310 is not a function, or a number where the collector found
// a pointer -- and the slot of a closure kept it, for the interpreter to call.
// The code keeps a cell of its own (jitTemplateCell), as it does a string
// constant's.
func TestJITMadeTemplateOutlivesItsRun(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	setEpoch(1000)
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITWarmUp(8, 8)
	fn := compileForTest(t, `
		function hot() {
			function rd(o) { return o.x; }
			var os = [];
			for (var i = 0; i < 40; i++) { var o = {}; o["k" + i] = 1; o.x = i; os.push(o); }
			var s = 0;
			for (var j = 0; j < 400; j++) s += rd(os[j % 40]);
			function has(o) { return "x" in o; }
			for (var j = 0; j < 400; j++) has(os[j % 40]);
			function st(o) { o.y = 1; }
			for (var j = 0; j < 400; j++) st(Object.create(os[j % 40]));
			return s;
		}
		var t = 0;
		for (var i_ = 0; i_ < 20; i_++) t += hot();
		t;`)
	const want = 7800 * 20
	var junk [][]Value
	// Runs in the runtime's own realm, and in realms that each make the code
	// hot anew.
	for k := range 8 {
		re := r.Realm
		if k >= 4 {
			re = r.NewRealm()
		}
		for round := range 3 {
			v, err := r.RunIn(re, fn)
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if v.Number() != want {
				t.Fatalf("round %d: %v", round, v)
			}
			r.InRealm(re, func() {
				size := len(r.global.getOwn(r.atoms.intern("hot")).value().Object().fn().closure.consts)
				// The constants of the runs before, once nothing holds
				// them, go, and what is made next is made where they were.
				junk = junk[:0]
				runtime.GC()
				runtime.GC()
				for range 1 << 12 {
					s := make([]Value, size)
					for i := range s {
						s[i] = Float(1.5)
					}
					junk = append(junk, s)
				}
			})
		}
	}
	runtime.KeepAlive(junk)
}
