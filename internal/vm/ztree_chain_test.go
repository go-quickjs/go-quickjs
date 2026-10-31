package vm

import "testing"

// A property read of a property of this, a local or an upvalue, and a
// write to one, are one node (propChain, propSetChain): an own property, a
// prototype's, a getter's and a setter's at either step, a primitive and
// null at the first -- whose read throws a TypeError -- a string's length
// at the second, and objects of more shapes than a site's cache holds, so
// that the sites give up their caches and the runtime's shared cache
// answers them (ownScan, cachedProp). A write to a frozen object is
// ignored in sloppy code and throws in strict code. Each answer is the
// interpreter's, and Node's.
func TestTreePropertyChains(t *testing.T) {
	src := `
		function Box(o) { this.inner = o; }
		Box.prototype.read = function () { return this.inner.v + ',' + this.inner.w; };
		Box.prototype.write = function (x) { this.inner.v = x; return this.inner.v; };
		function viaLocal(b) { return b.inner.v + ':' + b.inner.len.length; }
		function setLocal(b, x) { b.inner.v = x; return b.inner.v; }
		function strictSet(b, x) { 'use strict'; b.inner.v = x; return b.inner.v; }
		function make(base) { return function () { return base.inner.v; }; }
		var out = [], shapes = [];
		var proto = { w: 'pw', get g() { return 'gv'; } };
		for (var i = 0; i < 24; i++) {
			var o = Object.create(proto); o['k' + i] = i; o.v = i; o.len = 'ab' + i;
			if (i % 5 == 0) Object.defineProperty(o, 'w', { get: function () { return 'getter' }, configurable: true });
			shapes.push(new Box(o));
		}
		for (var r = 0; r < 3; r++)
			for (var i = 0; i < shapes.length; i++) out.push(shapes[i].read() + '|' + viaLocal(shapes[i]));
		for (var i = 0; i < shapes.length; i++) out.push(shapes[i].write(i * 2) + '/' + setLocal(shapes[i], i * 3));
		var withSetter = new Box(Object.defineProperty({}, 'v', { get: function () { return this._v }, set: function (x) { this._v = 'set' + x }, configurable: true }));
		out.push(withSetter.write(7), setLocal(withSetter, 8));
		var frozen = new Box(Object.freeze({ v: 'ice' }));
		out.push(setLocal(frozen, 1));
		try { strictSet(frozen, 2) } catch (e) { out.push(e.constructor.name) }
		out.push(viaLocal({ inner: { v: 'x', len: 'four' } }));
		out.push(make({ inner: { v: 'up' } })(), make({ inner: Object.create({ v: 'upproto' }) })());
		try { viaLocal({ inner: null }) } catch (e) { out.push(e.constructor.name) }
		try { viaLocal(5) } catch (e) { out.push(e.constructor.name) }
		try { viaLocal({ inner: 'str' }) } catch (e) { out.push(e.constructor.name) }
		out.push(make({ inner: 'str' })());
		try { make('abc')() } catch (e) { out.push(e.constructor.name) }
		try { setLocal({ inner: undefined }, 1) } catch (e) { out.push(e.constructor.name) }
		out.push(setLocal({ inner: 7 }, 1));
		function setter(base) { return function (x) { base.inner.v = x; return base.inner.v; }; }
		var same = [], sum = 0, up = setter({ inner: { v: 0 } }), upMiss = setter({ inner: 9 });
		function Box2(o) { this.inner = o; }
		Box2.prototype.write = function (x) { this.inner.v = x; return this.inner.v; };
		function setLocal2(b, x) { b.inner.v = x; return b.inner.v; }
		for (var i = 0; i < 6; i++) same.push(new Box2({ v: 0, w: 1 }));
		for (var r = 0; r < 4; r++) for (var i = 0; i < same.length; i++) { sum += same[i].write(r + i) + setLocal2(same[i], r * i) + up(r * 10 + i); }
		out.push(sum, up('last'), upMiss(3));
		try { setter({ inner: null })(1) } catch (e) { out.push(e.constructor.name) }
		out.join(' ')`
	var want string
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := v.String().Go()
		if !tree {
			want = got
			continue
		}
		if got != want {
			t.Errorf("the trees gave\n%s\nthe interpreter\n%s", got, want)
		}
	}
	t.Log(want)
}
