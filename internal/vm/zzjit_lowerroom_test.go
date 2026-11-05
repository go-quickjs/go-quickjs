//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// jitHotProgram is a program of n hot functions, each run in turn.
func jitHotProgram(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "function f%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", i, i, i)
	}
	b.WriteString("var fs=[")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "f%d,", i)
	}
	b.WriteString("];let t=0;for(let r=0;r<20;r++)for(const f of fs)t=(t+f(200))|0;t")
	return b.String()
}

// Asking whether a function is lowered makes it no lowering: refused the
// room for its compile, a function holds nothing. Two checks in the
// background's path asked through the call that makes one, before they
// asked for room, so that at a budget too tight for more code every
// function hot enough left a lowering of no program behind it, which no
// compile came to settle: 300 of them, 88 KB past a budget of 160 KB. At
// every budget the JIT holds no more than it, compiling in the background
// or not.
func TestJITLoweringsOnlyInRoom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := jitHotProgram(300)
	configs := []jitStressConfig{{threshold: true, budget: 3, deopt: 7, background: true, checks: 3}, {threshold: true, background: true}, {}}
	for _, c := range configs {
		for limit := int64(96 << 10); limit <= 512<<10; limit += 16 << 10 {
			r := New(Config{JIT: true, JITMemoryLimit: limit})
			r.SetJITWarmUp(8, 8)
			if r.jitStress = c; c.threshold {
				// As QJS_JIT_STRESS's threshold has a runtime made.
				r.jitCallThreshold, r.jitICThreshold = 1, 1
			}
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			if s := r.jit; s != nil && s.held() > s.budget {
				t.Errorf("%+v, limit %d KB: held %d, %d entries, %d lowerings, %d bytes of them", c, limit>>10, s.held(), len(s.cache), len(s.lowered), s.lowerBytes)
			}
			r.Close()
			r.ReleaseClosed()
		}
	}
}
