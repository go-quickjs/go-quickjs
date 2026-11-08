//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// A direct eval in a strict closure assigns what its maker shares with it,
// as one in a sloppy closure does: the JIT's closureAssigned counted only
// HasDirectEval, which marks sloppy functions alone, so the maker's native
// code took the let as no closure's to assign, kept what it had read of it
// across the closure's call, and added 1 three times where the eval had
// made it 2 and 3. Each answer the interpreter's and the specification's,
// idle and while the collector marks.
func TestJITStrictEvalAssigns(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			both := jitBoth{want: New(Config{}), jit: jitRuntimeForTest(t, Config{JIT: true})}
			t.Cleanup(func() { both.want.Close(); both.want.ReleaseClosed() })
			for _, rt := range []*Runtime{both.want, both.jit} {
				rt.SetEvaluator(func(source string, req EvalRequest) (*bytecode.Function, error) {
					prog, err := parser.Parse(source, parser.Options{Strict: req.Direct && req.Scope.Strict})
					if err != nil {
						return nil, err
					}
					return compiler.Compile(prog, compiler.Options{Source: "<eval>", Text: source,
						EvalConfigurable: true, EvalOwnVarScope: true, EvalScope: req.Scope.Bindings, EvalWithDepth: req.Scope.WithDepth})
				})
			}
			both.script(t, `'use strict'
				function f() { let x = 1; const g = () => eval('x = x + 1'); let s = 0; for (let i = 0; i < 3; i++) { s += x; g() } return s * 10 + x }
				function p(x) { const g = () => eval('x += 1'); let s = 0; for (let i = 0; i < 3; i++) { s += x; g() } return s * 10 + x }`)
			for range 5 {
				both.agree(t, `f()`, `p(1)`)
				if v, err := both.jit.Run(compileForTest(t, `f() + ',' + p(1)`)); err != nil || v.String().Go() != "64,64" {
					t.Fatalf("got %v, %v; want 64,64", v, err)
				}
			}
			if e := both.jit.jit.hint(both.jit.global.getOwn(both.jit.atoms.intern("f")).value().Object().fn().closure); e == nil || e.ssa == nil {
				t.Fatalf("f is not native: %+v", e)
			}
		})
	}
}
