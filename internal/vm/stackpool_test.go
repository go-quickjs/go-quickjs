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
	r.ReleaseStack()
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
			rt.ReleaseStack()
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
