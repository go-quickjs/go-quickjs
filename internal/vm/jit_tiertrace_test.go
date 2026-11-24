//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// jitTierTraceForTest records the tier trace (JITTierHook) of the runtimes
// t makes from now on but g's, the tests' leaf, which the Go table's calls
// (QJS_NONATIVE) run with a frame and compile.
func jitTierTraceForTest(t *testing.T) *[]JITTierEvent {
	var events []JITTierEvent
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.tierHook = func(e JITTierEvent) {
			if e.Func.Name != "g" {
				events = append(events, e)
			}
		}
	})
	return &events
}

// jitTierLines is events as lines of the kind, the function's name and
// the reason.
func jitTierLines(events []JITTierEvent) string {
	var b strings.Builder
	for _, e := range events {
		kind := "C"
		if e.Deopt {
			kind = "D"
		}
		fmt.Fprintf(&b, "%s %s %s\n", kind, e.Func.Name, e.Reason)
	}
	return b.String()
}

// The tier trace says each compile placed and each deoptimization that
// invalidates code, with what it was for: f, compiled for P's objects, is
// called with a Q's, a shape its read's cache had not met, deoptimizes
// there and is compiled again once the interpreter has spent a fresh
// budget, its answers the interpreter's throughout.
func TestJITTierTrace(t *testing.T) {
	events := jitTierTraceForTest(t)
	r, run := jitTierRuntimeForTest(t, 8, `function P(x){this.x=x} function Q(x){this.y=0;this.x=x}
		var p=new P(1), q=new Q(2);
		function g(v){return v} function f(o){return g(o.x)+1}`)
	r.jitStress = jitStressConfig{}
	for range 20 {
		run("f(p)")
	}
	for range 20 {
		run("f(q)")
	}
	if got, want := jitTierLines(*events), "C f first\nD f read/mono\nC f deopt\n"; got != want {
		t.Fatalf("tier trace:\n%swant\n%s", got, want)
	}
	first, deopt, again := (*events)[0], (*events)[1], (*events)[2]
	if first.Deopts != 0 || first.Compiles != 0 || first.Length == 0 || first.Func.Line != 3 {
		t.Errorf("first compile: %+v", first)
	}
	// Compiled at its ninth call, eight runs having spent the budget; run
	// natively for the twelve calls after, up to the thirteenth's failure:
	// their invocations and the work its native stretches counted, fewer
	// where the Go table (QJS_NONATIVE) calls g with a frame of its own.
	if deopt.Deopts != 1 || deopt.Compiles != 1 || deopt.Age < 1 || deopt.Age > 20 || deopt.Budget != 8 {
		t.Errorf("deoptimization: %+v", deopt)
	}
	if again.Deopts != 1 || again.Compiles != 1 {
		t.Errorf("compile after it: %+v", again)
	}
}
