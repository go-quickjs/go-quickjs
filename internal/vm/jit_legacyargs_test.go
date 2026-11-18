//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

func TestJITLegacyArgumentsParameterState(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		for _, inline := range []bool{false, true} {
			name := map[bool]string{false: "idle", true: "marking"}[marking] + "/" + map[bool]string{false: "native", true: "inline"}[inline]
			t.Run(name, func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				if !inline {
					jitNoInlineForTest(t, "method")
				}
				r := jitRuntimeForTest(t, Config{JIT: true, NodeQuirks: true})
				if _, err := r.Run(compileForTest(t, `
      function method(x){return method.arguments.length+":"+method.arguments[0]}
      var o={custom:method};
      function legacy(n){let result;for(let i=0;i<20;i++)result=o.custom(n+0);return ''+result}`)); err != nil {
					t.Fatal(err)
				}
				p := compileForTest(t, `legacy(42)`)
				for range 12 {
					v, err := r.Run(p)
					if err != nil || v.String().Go() != "1:42" {
						t.Fatalf("got %q, %v; want 1:42", v.String().Go(), err)
					}
				}
				e := jitNativeForTest(t, r, "legacy")
				if inline {
					found := false
					for _, call := range e.inlines {
						found = found || call.cl != nil && call.cl.fn.Name == "method"
					}
					if !found {
						t.Fatal("legacy did not inline method")
					}
				} else {
					jitNativeForTest(t, r, "method")
				}
			})
		}
	}
}
