package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestMapUpsert(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var m = new Map([[1, "a"]]); [m.getOrInsert(1, "b"), m.getOrInsert(2, "c"), [...m].join(";")].join("|")`,
			"a|c|1,a;2,c"},
		// The key is stored as +0 whichever zero it came as.
		{`var m = new Map(); m.getOrInsert(-0, 1); String(Object.is([...m.keys()][0], 0))`, "true"},
		{`var m = new Map(); var k; m.getOrInsertComputed(-0, x => { k = x; return 1 }); String(Object.is(k, 0))`,
			"true"},
		// A present key never runs the callback.
		{`var m = new Map([[1, "a"]]); var n = 0; [m.getOrInsertComputed(1, () => { n++ }), n].join()`, "a,0"},
		// What the callback returns wins over what it set, and keeps the
		// position the callback's own set gave the entry.
		{`var m = new Map(); m.getOrInsertComputed("x", () => { m.set("y", 1); m.set("x", 0); return 3 });
		  [...m].join(";")`, "y,1;x,3"},
		// The callback is checked before anything else happens to the key.
		{`try { new Map().getOrInsertComputed(1, 2) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { Map.prototype.getOrInsert.call(new WeakMap(), {}, 1) } catch (e) { e.constructor.name }`, "TypeError"},

		{`var w = new WeakMap(), k = {}; [w.getOrInsert(k, 1), w.getOrInsert(k, 2), w.get(k)].join()`, "1,1,1"},
		{`var w = new WeakMap(), k = Symbol(); w.getOrInsertComputed(k, x => typeof x)`, "symbol"},
		// WeakMap refuses the key before it looks at the callback.
		{`var n = 0; try { new WeakMap().getOrInsertComputed(1, () => n++) } catch (e) { e.constructor.name + n }`,
			"TypeError0"},
		{`try { new WeakMap().getOrInsertComputed({}, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { new WeakMap().getOrInsert(Symbol.for("r"), 1) } catch (e) { e.constructor.name }`, "TypeError"},

		{`[Map.prototype.getOrInsert.length, Map.prototype.getOrInsertComputed.length,
		   WeakMap.prototype.getOrInsert.length, WeakMap.prototype.getOrInsertComputed.length].join()`, "2,2,2,2"},
	}
	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}
