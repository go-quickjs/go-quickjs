package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// A frame given to its callee in place by a call in tail position
// (regTailTo) closes the upvalues its closures captured and starts with
// none open, as a frame made anew does: the list they were in had grown by
// one a call, each closure made after scanning it and each tail call
// closing every one again, so that test262's tco-call-args, a hundred
// thousand calls each making a closure, took five seconds where the old
// tiers take a fiftieth. The bottom of the recursion asks how many are
// open in a frame at most -- two, k and f -- and each closure answers its
// own k.
func TestRegTailToClosesUpvalues(t *testing.T) {
	const src = `
		"use strict"
		var fs = [], open = -1
		;(function f(n) {
			if (n === 0) { open = maxOpen(); return }
			var k = n
			function g() { return k }
			k += 0
			if (n % 500 === 0) fs.push(g)
			function getF() { return f }
			return getF()(n - 1)
		})(3000)
		open + " " + fs.map(function (g) { return g() }).join()`
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	r := New(Config{})
	defer r.Close()
	r.global.setOwnRaw(r.atoms.intern("maxOpen"), r.NewFunction("maxOpen", 0, func(r *Runtime, _ Value, _ []Value) (Value, error) {
		most := 0
		for d := 0; d < r.frameDepth; d++ {
			most = max(most, len(r.frameAt(d).openUpvalues))
		}
		return Float(float64(most)), nil
	}), propDefault)
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2 3000,2500,2000,1500,1000,500"
	if got := v.String().Go(); got != want {
		t.Fatalf("got %q, want %q: a frame given to its callee in place keeps the upvalues of the calls before open", got, want)
	}
}
