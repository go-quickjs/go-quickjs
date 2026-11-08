//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

const feedbackPCSetup = `
	function code(s, i) {
		return s.charCodeAt(i % s.length) + s.substring(i % 3, i % 3 + 1).length +
			String.fromCharCode(65 + i % 3).charCodeAt(0) + s.indexOf('b') + parseInt('1' + (i % 7))
	}
	function twice(s, n) {
		let t = 0
		for (let i = 0; i < n; i++) t += code(s, i) + code(s + 'x', i + 1) + s.charCodeAt(0)
		return t
	}
	function each(a, n) { let t = 0; for (let k = 0; k < n; k++) a.forEach((v, i) => { t += code(v, i + k) }); return t }
	var words = ['abc', 'xbz', 'bbb', 'qrs']
`

// A call site's feedback is found by its PC (jitFeedback's targetsAt):
// a callee whose built-ins' calls are made natively, inlined at two sites
// of one function and inside a loop's callback, in the runtime's realm
// and another's, gives the interpreter's answers, also once the built-ins
// are replaced and once its code has been flushed and compiled again.
func TestJITFeedbackByPCInlinedCopies(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	setClock := jitFlushForTest(t)
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	rts := []*Runtime{want, r}
	realms := []*Realm{want.NewRealm(), r.NewRealm()}
	run := func(inRealm bool, src string) {
		t.Helper()
		var got [2]string
		for i, rt := range rts {
			fn := compileForTest(t, "''+("+src+")")
			var v Value
			var err error
			if inRealm {
				v, err = rt.RunIn(realms[i], fn)
			} else {
				v, err = rt.Run(fn)
			}
			if err != nil {
				got[i] = "error " + err.Error()
			} else {
				got[i] = v.String().Go()
			}
		}
		if got[0] != got[1] {
			t.Fatalf("%s (realm %v): got %s, interpreter %s", src, inRealm, got[1], got[0])
		}
	}
	for i, rt := range rts {
		if _, err := rt.Run(compileForTest(t, feedbackPCSetup)); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunIn(realms[i], compileForTest(t, feedbackPCSetup)); err != nil {
			t.Fatal(err)
		}
	}
	rounds := []string{`twice('abc', 100)`, `each(words, 20)`, `twice('bq', 60)`}
	clock := uint64(1000)
	for k := range 4 {
		for range 4 {
			for _, src := range rounds {
				run(false, src)
				run(true, src)
			}
		}
		switch k {
		case 0:
			run(false, `(String.prototype.charCodeAt = function (i) { return 7 + i }, twice('abc', 30))`)
			run(true, `(String.fromCharCode = function () { return 'zz' }, twice('abc', 30) + each(words, 20))`)
		case 1:
			// The code left unused long enough to be flushed, and compiled
			// again from what was remembered of its sites.
			clock += 1 << 20
			setClock(clock)
			run(false, `each(words, 20)`)
			clock += 1 << 20
			setClock(clock)
		case 2:
			run(false, `(String.prototype.substring = function () { return 'long' }, twice('abc', 30))`)
		}
	}
	jitNativeForTest(t, r, "twice")
	jitNativeForTest(t, r, "each")
}
