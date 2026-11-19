package vm

import (
	"context"
	"errors"
	"testing"
)

// slice on a dense array copies its range at once (sliceDense), and the
// spec's algorithm, written out below with nothing but property reads and
// definitions, answers every case the same: the elements, holes as holes,
// the length -- for starts and ends of every kind, and for receivers the
// copy may not be used for, which the protocol makes: a subclass, a
// constructor or species replaced, indices inherited, and arguments whose
// conversion runs code.
func TestSliceDenseMatchesSpec(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function toInt(v) { v = Number(v); return v !== v ? 0 : Math.trunc(v) }
		function species(o, n) {
			let C
			if (Array.isArray(o)) {
				C = o.constructor
				if (C !== null && (typeof C === 'object' || typeof C === 'function')) {
					C = C[Symbol.species]
					if (C === null) C = undefined
				}
			}
			return C === undefined ? new Array(n) : new C(n)
		}
		function ref(o, args) {
			const len = o.length
			let k = toInt(args[0])
			k = k < 0 ? Math.max(len + k, 0) : Math.min(k, len)
			let end = args[1] === undefined ? len : toInt(args[1])
			end = end < 0 ? Math.max(len + end, 0) : Math.min(end, len)
			const out = species(o, Math.max(end - k, 0))
			let n = 0
			for (; k < end; k++, n++) if (k in o) Object.defineProperty(out, n, { value: o[k], writable: true, enumerable: true, configurable: true })
			out.length = n
			return out
		}
		function show(a) {
			let s = '[' + a.length + ':'
			for (let i = 0; i < a.length; i++) s += (i in a ? String(a[i]) : '_') + ','
			return s + ']' + (Array.isArray(a) ? '' : 'obj') + (a.constructor === Array ? '' : a.constructor && a.constructor.name)
		}
		const make = () => { const a = [1, 2, , 4, 5, , 7, 8, 9]; a[9] = 10; return a }
		const argsList = [[], [0], [2], [-3], [1, 4], [2, -1], [-20, 3], [20], [3, 100], [5, 2], [1.7, 6.2],
			[NaN, NaN], [Infinity], [-Infinity, Infinity], [0, undefined], [undefined, 4], [-0, -0]]
		const bad = []
		const outcome = (f, a) => { let s; try { s = show(f()) } catch (e) { s = 'threw ' + e.constructor.name } return s + show(a) }
		const check = (name, mk, args) => {
			const a = mk(), b = mk()
			const g = outcome(() => Array.prototype.slice.apply(a, args), a), w = outcome(() => ref(b, args), b)
			if (g !== w) bad.push(name + ' ' + JSON.stringify(args) + ': ' + g + ' want ' + w)
		}
		class Sub extends Array {}
		const cases = {
			dense: make,
			empty: () => [],
			frozen: () => Object.freeze(make()),
			trailingHoles: () => { const a = [1, 2, 3]; a.length = 6; return a },
			subclass: () => Sub.from(make()),
			ownCtor: () => { const a = make(); a.constructor = undefined; return a },
		}
		for (const [name, mk] of Object.entries(cases)) for (const args of argsList) check(name, mk, args)
		const conv = f => {
			const a = make()
			let calls = 0
			const end = { valueOf() { calls++; a.length = 3; return 8 } }
			return outcome(() => f(a, [1, end]), a) + calls
		}
		const g = conv((a, args) => Array.prototype.slice.apply(a, args)), w = conv(ref)
		if (g !== w) bad.push('valueOf: ' + g + ' want ' + w)
		Array.prototype[3] = 'inherited'
		check('inherited', make, [1, 6])
		delete Array.prototype[3]
		Object.prototype[2] = 'object'
		check('inheritedObject', make, [0])
		delete Object.prototype[2]
		const saved = Array.prototype.constructor
		Array.prototype.constructor = Sub
		check('protoCtor', make, [1, 3])
		Array.prototype.constructor = saved
		const sp = Object.getOwnPropertyDescriptor(Array, Symbol.species)
		Object.defineProperty(Array, Symbol.species, { get() { return Sub }, configurable: true })
		check('species', make, [1, 3])
		Object.defineProperty(Array, Symbol.species, sp)
		check('restored', make, [1, 3])
		const src = make(), copy = src.slice()
		copy[0] = 'changed'
		if (src[0] !== 1) bad.push('the copy shares the source: ' + show(src))
		bad.join('\n')`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "" {
		t.Fatalf("slice differs from the spec's:\n%s", s)
	}
}

// sliceDense takes a plain dense array with numbers or nothing for its
// start and end, and declines every receiver and argument the spec's steps
// could tell apart from a copy of its elements.
func TestSliceDenseTakes(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	for _, c := range []struct {
		src  string
		args []Value
		want bool
	}{
		{`[1, 2, , 4]`, nil, true},
		{`[1, 2, 3]`, []Value{Float(-1.5), Float(9)}, true},
		{`[1, 2, 3]`, []Value{Undefined, Int(2)}, true},
		{`[]`, []Value{Int(0)}, true},
		{`[1, 2, 3]`, []Value{Str(NewString("1"))}, false},
		{`[1, 2, 3]`, []Value{Int(0), Null}, false},
		{`class S extends Array {}; S.from([1, 2, 3])`, nil, false},
		{`var a = [1, 2, 3]; a.constructor = Array; a`, nil, false},
		{`var a = []; a[100000] = 1; a`, nil, false},
		{`({length: 2, 0: 1, 1: 2})`, nil, false},
	} {
		v, err := r.Run(compileForTest(t, c.src))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := r.sliceDense(v, c.args); ok != c.want {
			t.Errorf("%s: sliceDense took it %v, want %v", c.src, ok, c.want)
		}
	}
	for _, src := range []string{
		`Array.prototype[5] = 1`,
		`delete Array.prototype[5]; Array.prototype.constructor = Object`,
		`Array.prototype.constructor = Array; Object.defineProperty(Array, Symbol.species, { get() { return Array }, configurable: true })`,
	} {
		v, err := r.Run(compileForTest(t, src+`; [1, 2, 3]`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := r.sliceDense(v, nil); ok {
			t.Errorf("after %s: sliceDense took a plain array", src)
		}
	}
}

// The elements slice and splice copy or move at once on a dense array
// (sliceDense, spliceDense) count towards the next check for an interrupt,
// as the protocol's walk counts each one (arrayLike's at): a stop the
// counted checks alone notice -- as in a build with the race detector, and
// here, where nothing watches the context -- stops a loop of them within
// the first call after it, not a thousand calls on at the loop's back
// edges. Counted as one step each, a.slice() of 100,000 elements in a loop
// ran seconds past its deadline (TestInterruptIsNotAValue under -race).
func TestDenseCopiesCountTowardsTheCheck(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"big.slice()", "big.slice(1, -1)", "big.splice(0, 0, 1)", "big.splice(1, 1)"} {
		r := New(Config{})
		ctx, cancel := context.WithCancel(context.Background())
		r.global.setOwnRaw(r.atoms.intern("stop"), r.NewFunction("stop", 0, func(*Runtime, Value, []Value) (Value, error) {
			cancel()
			return Undefined, nil
		}), propWritable|propConfigurable)
		r.SetContext(ctx)
		_, err := r.Run(compileForTest(t, `var big = new Array(100000).fill(1), n = 0;
			while (true) { `+op+`; if (++n == 3) stop() }`))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: %v, want the stop", op, err)
		}
		if n := r.global.getOwn(r.atoms.intern("n")).value().Number(); n != 3 {
			t.Errorf("%s: %v calls made, want 3: the stop came %v calls late", op, n, n-3)
		}
		r.Close()
		cancel()
	}
}
