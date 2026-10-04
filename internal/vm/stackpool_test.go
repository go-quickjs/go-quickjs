package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

func compileForTest(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := compiler.Compile(prog, compiler.Options{Source: "test", Text: src})
	if err != nil {
		t.Fatal(err)
	}
	return fn
}

// TestReleasedStacksAreClear pins that a stack a closed runtime gives back
// holds nothing of what ran on it -- the frames of functions, generators and
// all -- so that the next runtime to take it keeps nothing of the last one
// alive.
func TestReleasedStacksAreClear(t *testing.T) {
	r := New(Config{})
	if _, err := r.Run(compileForTest(t, `
		function deep(n, o) { return n ? deep(n - 1, {o}) : o }
		function* g() { let big = [1, 2, 3]; yield big; yield [big, big] }
		var kept = [deep(200, {}), ...g()];
		for (const x of g()) kept.push(x);
		kept.length`)); err != nil {
		t.Fatal(err)
	}
	stack := r.stack
	r.Close()
	r.ReleaseClosed()
	if r.stack != nil {
		t.Fatal("the stack was not released")
	}
	for i, v := range stack {
		if v != (Value{}) {
			t.Fatalf("slot %d of the released stack holds %v", i, v)
		}
	}
}

// TestReleaseStackWhileRunning pins that a runtime closed from inside its own
// script keeps its stack, which the script is still running on.
func TestReleaseStackWhileRunning(t *testing.T) {
	r := New(Config{})
	released := false
	r.global.setOwnRaw(r.atoms.intern("closeNow"), Obj(r.newNativeFunc("closeNow", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			rt.Close()
			rt.ReleaseClosed()
			released = rt.stack == nil
			return Undefined, nil
		})), propDefault)
	v, err := r.Run(compileForTest(t, `function f(x) { closeNow(); return x + 1 } f(41)`))
	if err != nil || v.Number() != 42 {
		t.Fatalf("f = %v, %v", v, err)
	}
	if released {
		t.Error("the stack was released while a script ran on it")
	}
}

// TestReturnedTreeFramesAreCleared pins that the sweep of returned frames
// clears the tree tier's context too, which a tree leaves set when it
// returns: the closure it ran, its locals and the value it returned would
// otherwise stay reachable until the frame was used again.
func TestReturnedTreeFramesAreCleared(t *testing.T) {
	defer SetTreeTier(true, false)
	SetTreeTier(true, true)
	r := New(Config{})
	defer r.Close()
	before := TreesBuilt()
	if _, err := r.Run(compileForTest(t, `
		function make(n) { var o = { list: [] }; for (var i = 0; i < n; i++) o.list.push(i); return o }
		function outer() { return make(3).list.length }
		outer()`)); err != nil {
		t.Fatal(err)
	}
	if TreesBuilt() == before {
		t.Fatal("nothing was built as a tree")
	}
	r.clearReturnedFrames()
	for i := r.frameDepth; i < len(r.cur)+r.frameBase && i < 16; i++ {
		f := r.frameAt(i)
		if f.tc.cl != nil || f.tc.locals != nil || f.tc.stack != nil || f.tc.ret != (Value{}) {
			t.Fatalf("frame %d still holds its tree's context", i)
		}
	}
}
