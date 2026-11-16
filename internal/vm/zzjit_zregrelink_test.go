//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regRelinkSrc's drivers call callees of host operations alone natively,
// none inlined: short, at each turn of its loop, which is compiled for it
// at its first call of it (LowerRegInline); and long, past what a callee
// inlined may have (ssa.MaxInline), in one of twenty-five of its calls, too
// seldom to be compiled for it before it has warmed up
// (jitRegCalledOften).
func regRelinkSrc() string {
	var b strings.Builder
	b.WriteString("function short(o) { return o.k * 3 + o.j }\nfunction long(o) { var s = o.k;")
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, " s = s * %d + o.j - %d;", i%5+2, i)
	}
	b.WriteString(" return s % 1000003 }\n")
	b.WriteString("var O = { k: 5, j: 2 };\n")
	b.WriteString("function driveShort(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + short(O) + i) | 0; return s }\n")
	b.WriteString("function driveLong(n) { var t = 0; for (var i = 0; i < 30; i++) t = (t + i * n) | 0; return n % 25 == 0 ? t + long(O) : t }\n")
	return b.String()
}

// regRelinkRuntimes are regNativeRuntimes of regRelinkSrc, its functions
// compiled once four invocations' worth of their budgets are spent, none
// inlined; call(name, n) calls name(n) in both and fails the test on a
// difference, or on an answer other than want where want is given.
func regRelinkRuntimes(t *testing.T) (*Runtime, func(name string, n int, want string)) {
	t.Helper()
	regNoInline(t)
	rts := regNativeRuntimes(t, regRelinkSrc())
	r := rts[0]
	r.jitStress = jitStressConfig{}
	r.SetJITWarmUp(4, 4)
	r.SetJITReport(true)
	return r, func(name string, n int, want string) {
		t.Helper()
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(name))
			v, err := rt.Call(fn, Undefined, []Value{Float(float64(n))})
			got[i] = regNativeShow(rt, v, err)
		}
		if got[0] != got[1] || want != "" && got[0] != want {
			t.Fatalf("%s(%d): with the JIT %q, without %q, want %q", name, n, got[0], got[1], want)
		}
	}
}

// regCallsOfGo is how many calls of Go of functions native code made.
func regCallsOfGo(r *Runtime) uint64 {
	var n uint64
	for k, v := range r.RegNativeStats().GoCallOps {
		if strings.HasPrefix(k, "call_w") {
			n += v
		}
	}
	return n
}

// TestJITRegReleasedCalleeRelinked compiles a callee for its native caller,
// has the cache let go of it while a run still holds its code (regClose's
// retired), and has the caller call it again: it is compiled for it again
// at its next call of Go and called natively from then on, the answers the
// interpreter's, the collector idle and marking. Before, the template held
// the code the cache had let go of (jitReg), which Go entered, and the
// caller's calls of Go never compiled it again (jitRegNativeCalleeN):
// AWFY's Havlak made 4.2 million calls of som.js's hashFn through Go, let
// go of during CD, each putting off the levels of the recursion below it.
func TestJITRegReleasedCalleeRelinked(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	regBothMarking(t, func(t *testing.T) {
		r, call := regRelinkRuntimes(t)
		// Node: driveShort(200).
		const want = "23300"
		for range 40 {
			call("driveShort", 200, want)
		}
		cl := jitGlobalClosure(r, "short")
		key := r.jitRegKeyOf(cl)
		s := r.jit
		e := s.regCache[key]
		if e == nil || e.code == nil || cl.closureCode.jitReg != e {
			t.Fatalf("short not compiled: %+v", e)
		}
		// Let go of while a run is in its code: closed once the run is
		// done, which it is not until the end.
		e.runs++
		s.regLetGo(key, e)
		if e.code == nil || s.regCache[key] != nil {
			t.Fatalf("short's code closed, or still the cache's")
		}
		calls := regCallsOfGo(r)
		for range 40 {
			call("driveShort", 200, want)
		}
		ne := s.regCache[key]
		if ne == nil || ne == e || ne.code == nil {
			t.Fatalf("short not compiled again: %+v", ne)
		}
		if cl.closureCode.jitReg != ne {
			t.Errorf("short's template holds other code than the cache's")
		}
		t.Logf("calls of Go after it was let go of: %d", regCallsOfGo(r)-calls)
		l := s.regLinks[key]
		if k := s.regLinkFor(ne); k != l {
			// The link stays what the function learned (regForget), read
			// without its key at host exits and spends (regLinkFor).
			t.Errorf("short's code compiled again reads link %p, its key's is %p", k, l)
		}
		if k := s.regLinkFor(&jitRegEntry{key: key}); k != l {
			// An entry with no life reads the link by its key.
			t.Errorf("an entry with no life reads link %p, its key's is %p", k, l)
		}
		in, calls := l.in, regCallsOfGo(r)
		for range 3 {
			call("driveShort", 200, want)
		}
		if n := regCallsOfGo(r) - calls; n != 0 {
			t.Errorf("%d calls of Go in 600 calls of short, compiled again", n)
		}
		if l.in == in {
			t.Errorf("no native call of short through its link")
		}
		e.runs--
		s.jitRegCloseRetired()
		if e.code != nil {
			t.Errorf("short's code let go of not closed once no run was in it")
		}
	})
}

// TestJITRegReleasedLongCallee has the cache let go of a long callee its
// native caller calls seldom, compiled for it once warmed up, and checks it
// is compiled for it again at its next call of Go, rather than warmed up
// anew; and so where its budget, spent by Go's calls of it, ended in a
// refusal for host operations alone and began again. Before,
// either was taken for one warming up, its native caller's calls left to
// Go: AWFY's CD's isInVoxel, 3.2 million of them.
func TestJITRegReleasedLongCallee(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused=%v", refused), func(t *testing.T) {
			regBothMarking(t, func(t *testing.T) {
				r, call := regRelinkRuntimes(t)
				cl := jitGlobalClosure(r, "long")
				key := r.jitRegKeyOf(cl)
				s := r.jit
				n := 0
				for ; n < 2000 && (s.regCache[key] == nil || s.regCache[key].code == nil); n++ {
					call("driveLong", n, "")
				}
				e := s.regCache[key]
				if e == nil || e.code == nil {
					t.Fatalf("long not compiled for driveLong: %+v", e)
				}
				if m := jitRegInlineSize(cl.reg); m <= ssa.MaxInline || m > ssa.MaxInlineTotal {
					t.Fatalf("long is %d instructions long, want past %d, within %d", m, ssa.MaxInline, ssa.MaxInlineTotal)
				}
				s.regLetGo(key, e)
				if refused {
					// Go's calls of it spend its budget to a refusal for host
					// operations alone, and begin it again.
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("long"))
					o, _ := r.GetProp(Obj(r.Global()), r.Intern("O"))
					life := r.jitRegLifeOf(cl)
					k := 0
					for ; k < 40; k++ {
						if re := s.regCache[key]; re != nil && re.code == nil && re.hostOnly && life.warming {
							break
						}
						v, err := r.Call(fn, Undefined, []Value{o})
						// Node: long(O).
						if got := regNativeShow(r, v, err); got != "865877" {
							t.Fatalf("long(O) = %s, want 865877", got)
						}
					}
					if k == 40 {
						t.Fatalf("long not refused for host operations alone and warming again: %+v", s.regCache[key])
					}
				}
				// driveLong's next call of it.
				for n%25 != 0 {
					n++
				}
				calls := regCallsOfGo(r)
				call("driveLong", n, "")
				if regCallsOfGo(r) == calls {
					t.Fatalf("driveLong(%d) made no call of Go of long", n)
				}
				if ne := s.regCache[key]; ne == nil || ne.code == nil || cl.closureCode.jitReg != ne {
					t.Fatalf("long not compiled again at driveLong's call of Go: %+v", ne)
				}
				calls = regCallsOfGo(r)
				for k := 1; k <= 3; k++ {
					call("driveLong", n+20*k, "")
				}
				if d := regCallsOfGo(r) - calls; d != 0 {
					t.Errorf("%d calls of Go of long, compiled again", d)
				}
			})
		})
	}
}
