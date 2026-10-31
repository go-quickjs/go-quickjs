package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An arrow's this is read in place by the tree tier only where the compiler
// has found it a value (ArrowThisValue): an arrow in a function, a method, a
// class's field initializer or static block, or a script's top level, and
// one nested in such an arrow, but not one in a derived constructor, in an
// arrow inside one, or in eval code, whose call site may be inside one.
// Each case gives what the interpreter gives: this, a sloppy function's
// boxed primitive, the property reads and updates and the method calls of
// this, and inside a derived constructor the ReferenceError before super()
// and the object after it. Eval code's arrows are among treeScripts'.
func TestArrowThis(t *testing.T) {
	const src = `
		function plain() { let _k = 0; return () => { let _j = 0; return typeof this + ':' + (this === undefined) } }
		function sloppy() { let _k = 0; const f = () => { let _j = 0; return typeof this + this.valueOf() }; return f() }
		class Base {
		  constructor() { this.n = 1; this.field = () => { let _j = 0; return this.n } }
		  get() { return this.n }
		  method() { let _k = 0; const f = () => { let _j = 0; this.n += 1; return this.get() + this.n }; return f() + f() }
		  nested() { let _k = 0; return (() => () => { let _j = 0; return this.n })()() }
		  static s = (() => { let _j = 0; return this.name })()
		  static { this.block = (() => { let _j = 0; return this.name + '!' })() }
		}
		class Derived extends Base {
		  inner = () => { let _j = 0; return this.n * 10 }
		  constructor(how) {
		    let _k = 0
		    const before = () => { let _j = 0; return this.n }
		    if (how === 'early') before()
		    super()
		    this.n = 5
		    this.out = [before(), this.inner(), (() => () => { let _j = 0; return this.n + 1 })()()].join('/')
		  }
		}
		const r = []
		for (let round = 0; round < 3; round++) {
		  const b = new Base()
		  const s = [plain()(), plain.call(7)(), sloppy.call(3), b.field(), b.method(), b.nested(), Base.s, Base.block,
		    new Derived('late').out, (() => this === globalThis)()]
		  for (const how of ['early']) { try { new Derived(how); s.push('no error') } catch (err) { s.push(err.constructor.name) } }
		  r.push(s.join(','))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "object:false,object:false,object3,1,10,3,Base,Base!,5/50/6,true,ReferenceError | true"
	// Which arrows the compiler finds a value's this in, by the function
	// their first statement's local is in: _j in all of them.
	var walk func(fn *bytecode.Function, in string)
	got := map[string]int{}
	walk = func(fn *bytecode.Function, in string) {
		for _, c := range fn.Constants {
			if c.Kind != bytecode.ConstFunction || c.Fn == nil {
				continue
			}
			name := in + "/" + c.Fn.Name
			if c.Fn.Kind == bytecode.KindArrow {
				if c.Fn.ArrowThisValue {
					got[in]++
				} else {
					got[in+"!"]++
				}
			}
			walk(c.Fn, name)
		}
	}
	walk(compileForTest(t, src), "")
	// The script's own, Base's constructor's, method's, nested's and
	// nested's arrow's, plain's and sloppy's, and those of the field
	// initializers and the static block (anonymous functions of the
	// classes'), have a value's this; Derived's constructor's two and the
	// one nested in its arrow may not.
	wantArrows := map[string]int{"": 1, "/": 3, "/Base": 1, "/Derived!": 2, "/Derived/!": 1,
		"/method": 1, "/nested": 1, "/nested/": 1, "/plain": 1, "/sloppy": 1}
	if len(got) != len(wantArrows) {
		t.Errorf("arrows by maker (! where this may be a binding): got %v, want %v", got, wantArrows)
	}
	for k, n := range wantArrows {
		if got[k] != n {
			t.Errorf("arrows by maker (! where this may be a binding): got %v, want %v", got, wantArrows)
			break
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
