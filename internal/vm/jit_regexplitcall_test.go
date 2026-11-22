//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// regexpLitCallSetup's loops call a RegExp literal's test and exec, which
// register code makes one instruction, regexp_lit, and native code a leaf's
// call of the realm's built-in, or, where the method is another function,
// a call of Go of it: its operands the method call's, the literal's RegExp
// its receiver (compile's regexpLit).
const regexpLitCallSetup = `
	var savedTest = RegExp.prototype.test, savedExec = RegExp.prototype.exec
	function t(n, s) { let c = 0; for (let i = 0; i < n; i++) if (/a/.test(s)) c++; return c }
	function e(n, s) { let c = 0; for (let i = 0; i < n; i++) { const m = /a(.)/.exec(s); c += m === null ? 0 : m.length } return c }
	function patch() {
		RegExp.prototype.test = function (s) { return typeof s === 'string' && s.length > 2 }
		RegExp.prototype.exec = function (s) { return s.length > 2 ? [s, this.source] : null }
	}
	function restore() { RegExp.prototype.test = savedTest; RegExp.prototype.exec = savedExec }
`

// regexpLitCallRounds runs the loops with the built-ins, patched -- after
// their first compiles -- and restored.
var regexpLitCallRounds = []string{
	`t(100, 'xa') + ':' + e(100, 'xab')`, `t(100, 'xa') + ':' + e(100, 'xab')`,
	`patch(), t(100, 'xa') + ':' + t(100, 'a') + ':' + e(100, 'xab') + ':' + e(100, 'a')`,
	`t(100, 'xa') + ':' + e(100, 'xab')`, `t(100, 'xa') + ':' + e(100, 'xab')`,
	`restore(), t(100, 'xa') + ':' + e(100, 'xab')`, `t(100, 'xa') + ':' + e(100, 'xab')`,
}

// A RegExp literal's test or exec that is not the realm's built-in --
// RegExp.prototype's replaced before the code is compiled or after -- is
// called with the literal's RegExp as its receiver and the literal call's
// argument, as the interpreter calls it: each answer is the interpreter's,
// at the first call's compile, at the runtime's own thresholds and under
// CI's stress setting, in a realm of the runtime's and in another. Go had
// read the call's operands as a plain call's, its receiver taken for the
// function: native code's call threw a TypeError, "value is not a
// function", where the interpreter's returned.
func TestJITRegExpLiteralCallOfAnother(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	patched := `patch(); ` + regexpLitCallSetup + `; patch()`
	t.Run("first call", func(t *testing.T) {
		jitAgreesForTest(t, regexpLitCallSetup, regexpLitCallRounds...)
		jitAgreesForTest(t, patched, regexpLitCallRounds[2:]...)
	})
	t.Run("stress", func(t *testing.T) {
		c := jitStressConfig{threshold: true, budget: 3, deopt: 7, background: true, checks: 3}
		jitAgreesForTestWith(t, c, regexpLitCallSetup, regexpLitCallRounds...)
		jitAgreesForTestWith(t, c, patched, regexpLitCallRounds[2:]...)
	})
	t.Run("thresholds", func(t *testing.T) {
		want := newTestRuntime(t, Config{})
		t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.SetJITWarmUp(jitInvocationsForMaglev, jitInvocationsAfterICUpdate)
		run := func(name string, rw, rr *Realm, src string) {
			t.Helper()
			wv, err := want.RunIn(rw, compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			gv, err := r.RunIn(rr, compileForTest(t, src))
			if err != nil {
				t.Fatalf("%s, %s: %v, interpreter's %v", name, src, err, wv)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("%s, %s: got %s, interpreter %s", name, src, got, want)
			}
		}
		for _, realms := range []struct {
			name   string
			rw, rr *Realm
		}{{"the runtime's realm", want.Realm, r.Realm}, {"another realm", want.NewRealm(), r.NewRealm()}} {
			if _, err := want.RunIn(realms.rw, compileForTest(t, regexpLitCallSetup)); err != nil {
				t.Fatal(err)
			}
			if _, err := r.RunIn(realms.rr, compileForTest(t, regexpLitCallSetup)); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				for _, src := range regexpLitCallRounds {
					run(realms.name, realms.rw, realms.rr, "''+("+src+")")
				}
			}
		}
		if r.jit == nil || r.jit.compiled == 0 {
			t.Fatal("nothing was compiled")
		}
	})
}
