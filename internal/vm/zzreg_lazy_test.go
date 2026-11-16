package vm

import (
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// regLazySrc has functions called and never called, names, a string, a
// regular expression and property sites the stack form has none of, and a
// built-in's callback, which enters the code through a plan (regDirect).
// None is a leaf (bytecode.Function.Leaf), whose code is compiled as its
// closure is made (regMadeClosure).
const regLazySrc = `
function never() { return "never".length + /q+/.exec("qq")[0].length }
function used(o) {
  var s = 0
  for (var j = 0; j < 3; j++) s += o.alpha + o["be" + "ta"] + /a+/.exec("baa")[0].length
  return s + "x".repeat(2).length
}
function twice(v) { return v * 2 + ("k" + v).length }
var made = function () { var s = "m"; for (var i = 0; i < 2; i++) s += i; return s }
used({alpha: 1, beta: 2}) + [1, 2, 3].map(twice).join() + used({alpha: 3, beta: 4})
`

// TestRegCompiledLazily pins that a function's register code is compiled
// at its first call and only then: a function never called keeps none,
// and the code a call compiles, with the names, constants and sites it adds
// past its function's own, gives the answers the stack form gives.
func TestRegCompiledLazily(t *testing.T) {
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, regLazySrc)
	compiler.SetRegisterForm(false)
	byName := map[string]*bytecode.Function{}
	for _, c := range fn.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil {
			byName[c.Fn.Name] = c.Fn
		}
	}
	for _, name := range []string{"never", "used", "twice", "made"} {
		if f := byName[name]; f == nil || f.Reg == nil || f.RegCompiled() != nil {
			t.Fatalf("%s: want register code to compile later, not yet compiled", name)
		}
	}
	SetRegTier(true)
	defer SetRegTier(false)
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "174,6,829"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	for _, name := range []string{"never", "made"} {
		if byName[name].RegCompiled() != nil {
			t.Errorf("%s: never called, but its register code was compiled", name)
		}
	}
	for _, name := range []string{"used", "twice"} {
		rc := byName[name].RegCompiled()
		if rc == nil || rc.Names == nil {
			t.Fatalf("%s: called, but no register code compiled at its call", name)
		}
		if f := byName[name]; len(rc.Names) < len(f.Names) || len(rc.Constants) < len(f.Constants) || rc.PropSites < f.PropSites {
			t.Errorf("%s: the code's tables are shorter than its function's", name)
		}
	}
}

// TestRegCompiledLazilyConcurrently runs one program in runtimes on
// goroutines of their own, whose first calls compile its functions'
// register code at once: under the race detector, the compile is made
// once and adds nothing to the Function the runtimes share.
func TestRegCompiledLazilyConcurrently(t *testing.T) {
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, regLazySrc)
	compiler.SetRegisterForm(false)
	SetRegTier(true)
	defer SetRegTier(false)
	var wg sync.WaitGroup
	errs := make([]string, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(Config{})
			defer r.Close()
			v, err := r.Run(fn)
			if err != nil {
				errs[i] = err.Error()
			} else if got := v.String().Go(); got != "174,6,829" {
				errs[i] = got
			}
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Errorf("runtime %d: %s", i, e)
		}
	}
}
