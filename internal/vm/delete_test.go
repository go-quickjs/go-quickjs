package vm

import "testing"

// A delete of a primitive's property deletes from its wrapper, as ToObject
// makes it: a string's length and its indices are the String object's own,
// non-configurable -- false in sloppy code, a TypeError in strict code --
// and anything else is absent from the wrapper -- true -- as Node answers.
// Every property of a primitive had been deleted, true. In the interpreter
// and on the trees, a loop of each enough for the tree tier to take it.
func TestDeletePrimitiveProperty(t *testing.T) {
	const src = `
		var r = []
		r.push(delete 'ab'.length, delete 'ab'[0], delete 'ab'[1], delete 'ab'[5], delete 'ab'.x,
			delete (5).x, delete true.toString, delete Symbol().description, delete 10n.x)
		var k = 'length'
		r.push(delete 'xyz'[k])
		function strict(f) { try { return f() } catch (e) { return e.name } }
		r.push(strict(function () { 'use strict'; return delete 'ab'.length }),
			strict(function () { 'use strict'; return delete 'ab'[0] }),
			strict(function () { 'use strict'; return delete 'ab'[7] }),
			strict(function () { 'use strict'; return delete (1).x }))
		function loop(s) { var out = ''; for (var i = 0; i < 50; i++) out = String(delete s.length) + String(delete s[i & 3]); return out }
		function loopStrict(s) { 'use strict'; try { for (var i = 0; i < 50; i++) delete s[i & 3 | 8]; delete s[1] } catch (e) { return e.name } }
		for (var j = 0; j < 30; j++) { loop('ab'); loopStrict('ab') }
		r.push(loop('ab'), loopStrict('ab'))
		r.join()`
	const want = "false,false,false,true,true,true,true,true,true,false,TypeError,TypeError,true,true,falsefalse,TypeError"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
