//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

func init() { nativeTestTables["feedback"] = nativeFeedback.addr }

// TestRegNativeFeedbackSelected checks that each runtime picks its native
// dispatcher, through its contexts' table: the feedback one where it runs
// the JIT, plain where it does not; and that code runs through the
// feedback table as through plain's, which it is but for the handlers it
// overrides, none yet.
func TestRegNativeFeedbackSelected(t *testing.T) {
	if nativeFeedback == nativePlain || nativeFeedback.addr == nativePlain.addr {
		t.Fatal("a build with the JIT has no feedback dispatcher of its own")
	}
	if nativeFeedback.has != nativePlain.has {
		t.Error("the feedback dispatcher's operations with a handler are not plain's")
	}
	withJIT, without := New(Config{JIT: true}), New(Config{})
	defer withJIT.Close()
	defer without.Close()
	for _, c := range []struct {
		r    *Runtime
		want *nativeTable
		name string
	}{{withJIT, nativeFeedback, "feedback"}, {without, nativePlain, "plain"}} {
		if d := c.r.nativeDispatch(); d != c.want {
			t.Errorf("a runtime with the JIT %v dispatches by another table than %s's", c.r.jitOn(), c.name)
		}
		ctx := c.r.newNativeCtx(&regState{})
		if ctx.disp != c.want || ctx.table != c.want.addr {
			t.Errorf("a runtime with the JIT %v makes a context of another table than %s's", c.r.jitOn(), c.name)
		}
	}

	// A runtime without the JIT runs register code: its contexts given
	// the feedback table, the code runs through it.
	src := `function f(n) { var s = 0, a = [1, 2, 3]; for (var i = 0; i < n; i++) { var o = {a: i, b: 1}; s += o.a + o.b + a[i % 3] } return s }
		function g(n) { return n < 2 ? n : g(n - 1) + g(n - 2) }
		String(f(300)) + " " + String(g(15))`
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	regNativeOn = true
	defer func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was }()
	run := func() string {
		v, err := without.Run(fn)
		if err != nil {
			t.Fatal(err)
		}
		return v.String().Go()
	}
	want := run()
	var ctxs []*nativeCtx
	for st := without.regFree; st != nil; st = st.next {
		if st.native != nil {
			st.native.disp, st.native.table = nativeFeedback, nativeFeedback.addr
			ctxs = append(ctxs, st.native)
		}
	}
	if len(ctxs) == 0 {
		t.Fatal("no context of a native run to give the feedback table")
	}
	if got := run(); got != want {
		t.Fatalf("through the feedback table %q, through plain's %q", got, want)
	}
	for _, ctx := range ctxs {
		if ctx.table != nativeFeedback.addr {
			t.Fatal("a context's table changed in the run")
		}
	}
}
