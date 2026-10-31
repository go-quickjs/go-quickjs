package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A property of this or of a local given a local or a constant (setPropAt)
// is stored as setPropStmt stores it, in strict and in sloppy code: to an
// own property of one shape and then of others, a new one, one a setter of
// the prototype takes, a non-writable one, a frozen object's, a proxy's, an
// array's length, a string's and a number's, and null's, which throws a
// TypeError. Each function has a local of its own, so that it is run on
// its tree rather than by pureCall.
func TestSetPropAt(t *testing.T) {
	const src = `
		let log = ''
		class S { set p(v) { log += 'set' + v + ';' } }
		function mk() {
		  return [{ p: 1 }, { a: 0, p: 2 }, {}, new S(), Object.defineProperty({}, 'p', { value: 7, writable: false }),
		    Object.freeze({ p: 3 }), new Proxy({}, { set(t, k, v) { log += 'trap' + String(k) + v + ';'; return true } }),
		    [1, 2, 3], 'str', 5, null]
		}
		function strictLocal(o, v) { 'use strict'; let _k = 0; o.p = v; o.length = 1; return o.p }
		function strictConst(o) { 'use strict'; let _k = 0; o.p = null; o.p = 4; return o.p }
		function sloppyLocal(o, v) { let _k = 0; o.p = v; o.length = 1; return o.p }
		function sloppyConst(o) { let _k = 0; o.p = undefined; o.p = true; return o.p }
		class T {
		  constructor() { this.p = 0 }
		  set(v) { 'use strict'; let _k = 0; this.p = v; this.q = 'k'; this.p = 9; this.p = v; return this.p + this.q }
		}
		const out = []
		for (let round = 0; round < 3; round++) {
		  log = ''
		  const r = []
		  for (const f of [strictLocal, strictConst, sloppyLocal, sloppyConst]) {
		    for (const o of mk()) {
		      let k
		      try { k = String(f(o, 'v')) } catch (err) { k = err.constructor.name }
		      r.push(k + (o !== null && typeof o === 'object' && !Array.isArray(o) ? ':' + JSON.stringify(Object.keys(o)) : '') + (Array.isArray(o) ? ':' + o.length : ''))
		    }
		  }
		  const t = new T()
		  r.push(t.set('w'), T.prototype.set.call(new T(), 8), log)
		  out.push(r.join(','))
		}
		const res = out[0] + ' | ' + (out[0] === out[1] && out[1] === out[2])
		res`
	const want = "v:[\"p\",\"length\"],v:[\"a\",\"p\",\"length\"],v:[\"p\",\"length\"],undefined:[\"length\"],TypeError:[],TypeError:[\"p\"],undefined:[],v:1,TypeError,TypeError,TypeError,4:[\"p\"],4:[\"a\",\"p\"],4:[\"p\"],undefined:[],TypeError:[],TypeError:[\"p\"],undefined:[],4:3,TypeError,TypeError,TypeError,v:[\"p\",\"length\"],v:[\"a\",\"p\",\"length\"],v:[\"p\",\"length\"],undefined:[\"length\"],7:[\"length\"],3:[\"p\"],undefined:[],v:1,undefined,undefined,TypeError,true:[\"p\"],true:[\"a\",\"p\"],true:[\"p\"],undefined:[],7:[],3:[\"p\"],undefined:[],true:3,undefined,undefined,TypeError,wk,8k,setv;trappv;traplength1;setnull;set4;trappnull;trapp4;setv;trappv;traplength1;setundefined;settrue;trappundefined;trapptrue; | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		switch name := c.Fn.Name; name {
		case "strictLocal", "strictConst", "sloppyLocal", "sloppyConst":
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", name)
			}
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
