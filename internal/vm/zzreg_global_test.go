package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestRegGlobalEpoch pins the register code's global sites, which read and
// write a global's slot while the global epoch is the one they were filled
// at: a script-level lexical binding declared later shadows the global, an
// accessor defined over it is called, a binding made read-only is not
// written, and a binding deleted and made again is found where it now is.
func TestRegGlobalEpoch(t *testing.T) {
	steps := []struct{ src, want string }{
		{`globalThis.g = 1; globalThis.h = 2; globalThis.w = 3; globalThis.d = 4;
		  function readG() { var s = 0; for (var j = 0; j < 3; j++) s += g; return s }
		  function readH() { var s = 0; for (var j = 0; j < 3; j++) s += h; return s }
		  function writeW(v) { for (var j = 0; j < 3; j++) w = v + j; return w }
		  function readD() { return d }
		  [readG(), readG(), readH(), readH(), writeW(5), writeW(6), readD(), readD()].join()`,
			"3,3,6,6,7,8,4,4"},
		{`let g = 10; readG() + ""`, "30"},
		{`Object.defineProperty(globalThis, "h", { get() { return 7 } }); readH() + ""`, "21"},
		{`Object.defineProperty(globalThis, "w", { writable: false }); [writeW(20), w].join()`, "8,8"},
		{`delete globalThis.d; var e1 = 1, e2 = 2; globalThis.d = 9; [readD(), readD()].join()`, "9,9"},
	}
	compiler.SetRegisterForm(true)
	fns := make([]*bytecode.Function, len(steps))
	for i, s := range steps {
		fns[i] = compileForTest(t, s.src)
	}
	compiler.SetRegisterForm(false)
	SetRegTier(true)
	defer SetRegTier(false)
	before := RegsBuilt()
	r := New(Config{})
	defer r.Close()
	for i, s := range steps {
		v, err := r.Run(fns[i])
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := v.String().Go(); got != s.want {
			t.Errorf("step %d: got %s, want %s", i, got, s.want)
		}
	}
	if built := RegsBuilt() - before; built < 4 {
		t.Errorf("only %d functions ran as register code", built)
	}
}
