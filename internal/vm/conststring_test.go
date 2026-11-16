package vm

import (
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestConstStringShared pins string constants of one text as one string
// of the runtime's, which === then finds equal by identity: two functions'
// literals, and a script's run later, share it while it lives; one no
// constant holds any more is swept out of the table.
func TestConstStringShared(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	a, b := r.constString("forward"), r.constString("forward")
	if a != b {
		t.Fatal("two constants of one text made two strings")
	}
	if c := r.constString("backward"); c == a || c.Go() != "backward" {
		t.Fatal("a constant of another text shares its string")
	}
	for i := 0; i < 2000; i++ {
		r.constString("x" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)))
		if i%500 == 0 {
			runtime.GC()
		}
	}
	runtime.GC()
	r.constString("last")
	if n := len(r.strConsts); n > 1500 {
		t.Errorf("%d strings kept of constants no value holds", n)
	}
	runtime.KeepAlive(a)
}

// TestConstStringEmpty pins the empty string's constant, the runtime's
// one empty string (emptyString), static data: it is never held weakly --
// Go 1.24's weak.Make faults on a pointer to no heap object ("getWeakHandle
// on invalid pointer") -- and a script's empty literals are it.
func TestConstStringEmpty(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if s := r.constString(""); s != emptyString {
		t.Fatal("the empty constant is not the runtime's empty string")
	}
	if _, ok := r.strConsts[""]; ok {
		t.Error("the empty string is held in the constants' table")
	}
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, `var a = "", b = ""; function f() { return "" } [a === b, f() === a, typeof f(), ("" + "x").length].join()`)
	compiler.SetRegisterForm(bytecode.RegDefault())
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "true,true,string,1" {
		t.Errorf("got %s, want true,true,string,1", got)
	}
}
