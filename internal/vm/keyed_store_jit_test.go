//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// TestJITKeyedStoreSemantics runs keyedStoreCases with the JIT, each
// function compiled at its first call, the collector idle and as while it
// marks: the code compiled for a keyed store's site, a branch for each key
// to its shapes' stores (ssa's keyedStores), answers as the interpreter
// and Node do, and deoptimizes where its site meets what it was not
// compiled for. While it marks, every store of a pointer word and every
// call of Go fills the registers native code may keep with junk first
// (jit.StoreRefClobber, jit.CallGoClobber): a value or an object the code
// kept across either without saving it gives another answer.
func TestJITKeyedStoreSemantics(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
				jitClobberStoresForTest(t)
				jitClobberCallsForTest(t)
			}
			for _, c := range keyedStoreCases {
				r := jitRuntimeForTest(t, Config{JIT: true})
				v, err := r.Run(compileForTest(t, c.src))
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if got := v.String().Go(); got != c.want {
					t.Errorf("%s: got\n%s\nwant\n%s", c.name, got, c.want)
				}
			}
		})
	}
}

// keyedStoreLoopsSrc stores four keys by a literal's computed key, an
// assignment and a class's computed field, into objects of a few shapes.
const keyedStoreLoopsSrc = `
	var keys = ['ka', 'kb', 'kc', 'kd']
	class F { [keys[0]] = 1; [keys[1]] = 2 }
	function lit(n) {
		var o, s = 0
		for (var j = 0; j < n; j++) { o = { [keys[j & 3]]: j, v: 1 }; s += j & 3 }
		return Object.keys(o).join() + s
	}
	function set(n) {
		var o, p = { x: 1 }
		for (var j = 0; j < n; j++) {
			o = (j & 1) ? {} : { y: 2 }
			o[keys[j & 3]] = j
			p[keys[j & 3]] = j
		}
		return Object.keys(o).join() + Object.keys(p).join() + p.kd
	}
	function fields(n) {
		var f
		for (var j = 0; j < n; j++) f = new F()
		return Object.keys(f).join()
	}
	function main(n) { return lit(n) + '|' + set(n) + '|' + fields(n) }`

// TestJITKeyedStoreNative checks that a keyed store whose site knows its
// keys and shapes stays native once compiled: no host exit and no call of
// Go at set_index or define_index, the answers the interpreter's.
func TestJITKeyedStoreNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, keyedStoreLoopsSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			const want = "kd,v600|kdx,ka,kb,kc,kd399|ka,kb"
			run := func() {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("main"))
					v, err := r.Call(fn, Undefined, []Value{Float(400)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] || got[0] != want {
					t.Fatalf("main: with the JIT %q, without %q, want %q", got[0], got[1], want)
				}
			}
			for range 4 {
				run()
			}
			r.jitStress = jitStressConfig{}
			r.jitState().reporting = true
			for range 4 {
				run()
			}
			before := r.RegNativeStats()
			run()
			after := r.RegNativeStats()
			for _, op := range []string{"set_index", "define_index"} {
				if n := after.HostOps[op] - before.HostOps[op]; n != 0 {
					t.Errorf("%d host exits at %s once warm", n, op)
				}
				for _, name := range []string{op, "whole " + op} {
					if n := after.GoCallOps[name] - before.GoCallOps[name]; n != 0 {
						t.Errorf("%d calls of Go at %s once warm", n, name)
					}
				}
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
			// Each loop's stores compiled by their keys.
			keys := keyedStoreKeyTests(t, r)
			for _, name := range []string{"lit", "set"} {
				if keys[name] < 4 {
					t.Errorf("%s's code tests %d keys, want each store's four", name, keys[name])
				}
			}
		})
	}
}

// keyedStoreKeyTests is, for each function r compiled since
// recordJITCompilesForTest, by its name, how many tests of a keyed
// store's key its code makes (ssa.OpKeyIs), built again as it was.
func keyedStoreKeyTests(t *testing.T, r *Runtime) map[string]int {
	t.Helper()
	jitCompileLog.Lock()
	seen := jitCompileLog.seen[r]
	jitCompileLog.Unlock()
	n := map[string]int{}
	for _, cl := range seen {
		e := r.jit.regCache[r.jitKeyOf(cl.fn)]
		if e == nil || e.p == nil || e.code == nil {
			continue
		}
		f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
		if err != nil {
			continue
		}
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == ssa.OpKeyIs {
					n[cl.fn.Name]++
				}
			}
		}
	}
	return n
}
