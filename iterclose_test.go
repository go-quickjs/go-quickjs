package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// IteratorClose on a normal completion: a return that is not callable, or
// that gives no object, is a TypeError wherever an iterator is closed, and a
// nullish one is no return at all. Every answer is what Node and Chrome 158
// give.
func TestIteratorClose(t *testing.T) {
	const setup = `
		function mk(r) { return {next() { return {done: false, value: 1} }, return: r, __proto__: Iterator.prototype} }
		function iterable(r) { return {[Symbol.iterator]() { return mk(r) }} }
		function tryIt(f) { try { return String(f()) } catch (e) { return e.name } }
		function close(f, r) { const i = f(r); i.next(); return JSON.stringify(i.return()) }`
	rt := quickjs.New()
	defer rt.Close()
	evalString(t, rt, setup+`; "ready"`)
	for _, c := range []struct{ name, call, nullish string }{
		{"map", `close(r => mk(r).map(x => x), R)`, `{"done":true}`},
		{"drop", `close(r => mk(r).drop(1), R)`, `{"done":true}`},
		{"flatMap", `close(r => mk(r).flatMap(x => [1, 2]), R)`, `{"done":true}`},
		{"concat", `close(r => Iterator.concat(iterable(r)), R)`, `{"done":true}`},
		{"take", `JSON.stringify(mk(R).take(0).next())`, `{"done":true}`},
		{"find", `mk(R).find(x => true)`, `1`},
		{"some", `mk(R).some(x => true)`, `true`},
		{"every", `mk(R).every(x => false)`, `false`},
	} {
		for _, r := range []struct{ value, want string }{
			{`1`, "TypeError"},
			{`function() { return 1 }`, "TypeError"},
			{`null`, c.nullish},
			{`undefined`, c.nullish},
		} {
			code := `tryIt(() => { const R = ` + r.value + `; return ` + c.call + ` })`
			if got := evalString(t, rt, code); got != r.want {
				t.Errorf("%s, return %s: %q, want %q", c.name, r.value, got, r.want)
			}
		}
	}
}

// A helper is finished once closing what is beneath it failed, whether or
// not the close threw.
func TestIteratorConcatFinishedAfterFailedClose(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	const code = `(function() {
		const c = Iterator.concat({[Symbol.iterator]() { return {next() { return {done: false, value: 1} }, return: 1} }});
		c.next();
		let threw = false;
		try { c.return() } catch (e) { threw = e instanceof TypeError }
		return JSON.stringify([threw, c.next(), c.return()]);
	})()`
	if got, want := evalString(t, rt, code), `[true,{"done":true},{"done":true}]`; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// When stepping flatMap's inner iterator throws, that iterator is finished:
// only the one beneath is closed.
func TestFlatMapInnerThrowClosesOnlyTheOuter(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	const code = `(function() {
		let outer = 0, inner = 0;
		const i = {next() { throw new Error("step") }, return() { inner++; return {} }, [Symbol.iterator]() { return this }};
		const o = {next() { return {done: false, value: 1} }, return() { outer++; return {} }, __proto__: Iterator.prototype};
		const f = o.flatMap(() => i);
		let message = "";
		try { f.next() } catch (e) { message = e.message }
		return [message, outer, inner].join();
	})()`
	if got, want := evalString(t, rt, code), "step,1,0"; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// DataView's byteLength and byteOffset answer 0 for a view out of bounds of
// its buffer, where V8 throws a TypeError, as its other methods do on both
// sides.
func TestDataViewOutOfBoundsGetters(t *testing.T) {
	const fixed = `(function() {
		const b = new ArrayBuffer(8, {maxByteLength: 16});
		const d = new DataView(b, 2, 4);
		b.resize(4);
		return d;
	})()`
	const tracking = `(function() {
		const b = new ArrayBuffer(8, {maxByteLength: 16});
		const d = new DataView(b, 4);
		b.resize(2);
		return d;
	})()`
	const detached = `(function() {
		const b = new ArrayBuffer(8);
		const d = new DataView(b, 2);
		b.transfer();
		return d;
	})()`
	const live = `(function() {
		const b = new ArrayBuffer(8, {maxByteLength: 16});
		const d = new DataView(b, 2);
		b.resize(4);
		return d;
	})()`
	for _, c := range []struct {
		quirks bool
		want   string
	}{{false, "0"}, {true, "TypeError"}} {
		var opts []quickjs.Option
		if c.quirks {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		rt := quickjs.New(opts...)
		for _, view := range []string{fixed, tracking, detached} {
			for _, getter := range []string{"byteLength", "byteOffset"} {
				code := `(function() { const d = ` + view + `; try { return String(d.` + getter + `) } catch (e) { return e.name } })()`
				if got := evalString(t, rt, code); got != c.want {
					t.Errorf("quirks %v: %s of %s: %q, want %q", c.quirks, getter, view, got, c.want)
				}
			}
		}
		// A method that reads still throws, and a view in bounds still answers.
		code := `(function() { const d = ` + fixed + `; try { return String(d.getInt8(0)) } catch (e) { return e.name } })()`
		if got := evalString(t, rt, code); got != "TypeError" {
			t.Errorf("quirks %v: getInt8: %q", c.quirks, got)
		}
		code = `(function() { const d = ` + live + `; return d.byteLength + "," + d.byteOffset })()`
		if got := evalString(t, rt, code); got != "2,2" {
			t.Errorf("quirks %v: in bounds: %q", c.quirks, got)
		}
		rt.Close()
	}
}
