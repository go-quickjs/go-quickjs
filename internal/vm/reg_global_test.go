package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
	fns := make([]*bytecode.Function, len(steps))
	for i, s := range steps {
		fns[i] = compileForTest(t, s.src)
	}
	before := RegsBuilt()
	r := newTestRuntime(t, Config{})
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

// TestRegGlobalLex pins the global sites of a script-level lexical
// binding, which read and write it in globalLex once it is initialized
// (regNoteGlobalLex): a read in its dead zone throws and one after reads
// it, a let written and read back, a const's write throwing every time, a
// class read by a loop, a later script's let of another name, and a
// global property a later script's let shadows, natively and by the Go
// table alike.
func TestRegGlobalLex(t *testing.T) {
	steps := []struct{ src, want string }{
		{`function readA() { var s = 0; for (var j = 0; j < 3; j++) s += a; return s }
		  function writeB(v) { for (var j = 0; j < 3; j++) b = v + j; return b }
		  function writeC() { var n = 0; for (var j = 0; j < 3; j++) try { c = j } catch (e) { n++ } return n + ":" + c }
		  function useK() { var s = 0; for (var j = 0; j < 4; j++) s += new K(j).v + (K.prototype.m() ? 1 : 0); return s }
		  function readG() { var s = 0; for (var j = 0; j < 3; j++) s += g; return s }
		  globalThis.g = 2
		  var tdz; try { readA() } catch (e) { tdz = e.name }
		  let a = 5, b = 1; const c = 9
		  class K { constructor(v) { this.v = v } m() { return true } }
		  [tdz, readA(), readA(), writeB(10), writeB(20), b, writeC(), writeC(), useK(), useK(), readG(), readG()].join()`,
			"ReferenceError,15,15,12,22,22,3:9,3:9,10,10,6,6"},
		{`let g = 7; let z = 1; a = 100; [readG(), readG(), readA(), writeB(1), b].join()`, "21,21,300,3,3"},
	}
	for _, native := range []bool{false, true} {
		fns := make([]*bytecode.Function, len(steps))
		for i, s := range steps {
			fns[i] = compileForTest(t, s.src)
		}
		r := newTestRuntime(t, nativeConfig(native))
		for i, s := range steps {
			v, err := r.Run(fns[i])
			if err != nil {
				t.Fatalf("native %v step %d: %v", native, i, err)
			}
			if got := v.String().Go(); got != s.want {
				t.Errorf("native %v step %d: got %s, want %s", native, i, got, s.want)
			}
		}
		r.Close()
	}
}
