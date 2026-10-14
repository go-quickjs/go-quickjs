package vm

import "testing"

// splice on a dense array moves its elements in one copy (spliceDense), and
// the spec's algorithm, written out below with nothing but property reads,
// writes and deletions, answers every case the same: the removed elements,
// holes as holes, the array after, its length -- for starts and counts of
// every kind, and for receivers the copy may not be used for, which the
// protocol makes: a subclass, a constructor or species replaced, a frozen,
// sealed or unextensible array, a length not writable, indices inherited,
// and arguments whose conversion runs code.
func TestSpliceDenseMatchesSpec(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function toInt(v) { v = Number(v); return v !== v ? 0 : Math.trunc(v) }
		function set(o, k, v) { if (!Reflect.set(o, k, v)) throw new TypeError('set ' + k) }
		function del(o, k) { if (!Reflect.deleteProperty(o, k)) throw new TypeError('delete ' + k) }
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
			let start = toInt(args[0])
			start = start < 0 ? Math.max(len + start, 0) : Math.min(start, len)
			let del_ = args.length === 0 ? 0 : args.length === 1 ? len - start : Math.min(Math.max(toInt(args[1]), 0), len - start)
			const items = args.slice(2), out = species(o, del_)
			for (let k = 0; k < del_; k++) if (start + k in o) Object.defineProperty(out, k, { value: o[start + k], writable: true, enumerable: true, configurable: true })
			set(out, 'length', del_)
			if (items.length < del_) {
				for (let k = start; k < len - del_; k++) {
					if (k + del_ in o) set(o, k + items.length, o[k + del_]); else del(o, k + items.length)
				}
				for (let k = len; k > len - del_ + items.length; k--) del(o, k - 1)
			} else if (items.length > del_) {
				for (let k = len - del_; k > start; k--) {
					if (k + del_ - 1 in o) set(o, k + items.length - 1, o[k + del_ - 1]); else del(o, k + items.length - 1)
				}
			}
			for (let i = 0; i < items.length; i++) set(o, start + i, items[i])
			set(o, 'length', len - del_ + items.length)
			return out
		}
		function show(a) {
			let s = '[' + a.length + ':'
			for (let i = 0; i < a.length; i++) s += (i in a ? String(a[i]) : '_') + ','
			return s + ']' + (Array.isArray(a) ? '' : 'obj') + (a.constructor === Array ? '' : a.constructor && a.constructor.name)
		}
		const make = () => { const a = [1, 2, , 4, 5, , 7, 8, 9]; a[9] = 10; return a }
		const argsList = [[0, 1], [2, 0, 'x'], [1, 2, 'x', 'y', 'z'], [-2, 1], [-20, 3, 'q'], [20, 1, 'w'], [3, 100], [1, -5, 'n'],
			[0, NaN, 'm'], [1.7, 1.2, 'f'], [Infinity, 1], [-Infinity, Infinity, 'i'], [5, 0], [2, 2, 'a', 'b'], [0, 10]]
		const bad = []
		const outcome = (f, a) => { let s; try { s = show(f()) } catch (e) { s = 'threw ' + e.constructor.name } return s + show(a) }
		const check = (name, mk, args) => {
			const a = mk(), b = mk()
			const g = outcome(() => Array.prototype.splice.apply(a, args), a), w = outcome(() => ref(b, args), b)
			if (g !== w) bad.push(name + ' ' + JSON.stringify(args) + ': ' + g + ' want ' + w)
		}
		for (const args of argsList) {
			check('dense', make, args)
			check('empty', () => [], args)
			check('unextensible', () => { const a = make(); Object.preventExtensions(a); return a }, args)
			check('sealed', () => Object.seal(make()), args)
			check('frozen', () => Object.freeze(make()), args)
		}
		class Sub extends Array {}
		const cases = {
			subclass: () => Sub.from(make()),
			ownCtor: () => { const a = make(); a.constructor = undefined; return a },
			lengthFixed: () => { const a = make(); Object.defineProperty(a, 'length', { writable: false }); return a },
		}
		for (const [name, mk] of Object.entries(cases)) for (const args of argsList) check(name, mk, args)
		const conv = f => {
			const a = make()
			let calls = 0
			const start = { valueOf() { calls++; a.length = 3; return 1 } }
			return outcome(() => f(a, [start, 1, 'v']), a) + calls
		}
		const g = conv((a, args) => Array.prototype.splice.apply(a, args)), w = conv(ref)
		if (g !== w) bad.push('valueOf: ' + g + ' want ' + w)
		Array.prototype[3] = 'inherited'
		check('inherited', make, [1, 2])
		check('inheritedHoles', () => [1, 2, , 4], [0, 1, 'z', 'y'])
		delete Array.prototype[3]
		const saved = Array.prototype.constructor
		Array.prototype.constructor = Sub
		check('protoCtor', make, [1, 1])
		Array.prototype.constructor = saved
		const sp = Object.getOwnPropertyDescriptor(Array, Symbol.species)
		Object.defineProperty(Array, Symbol.species, { get() { return Sub }, configurable: true })
		check('species', make, [1, 1])
		Object.defineProperty(Array, Symbol.species, sp)
		check('restored', make, [1, 1])
		bad.join('\n')`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "" {
		t.Fatalf("splice differs from the spec's:\n%s", s)
	}
}

// spliceDense takes a plain dense array with numbers for its start and
// count, and declines every receiver and argument the spec's steps could
// tell apart from a move of its elements.
func TestSpliceDenseTakes(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	for _, c := range []struct {
		src  string
		args []Value
		want bool
	}{
		{`[1, 2, , 4]`, []Value{Int(1), Int(1), Str(NewString("x"))}, true},
		{`[1, 2, 3]`, []Value{Float(-1.5), Float(9), Int(7), Int(8)}, true},
		{`[]`, []Value{Int(0), Int(0)}, true},
		{`[1, 2, 3]`, []Value{Int(1)}, false},
		{`[1, 2, 3]`, []Value{Str(NewString("1")), Int(1)}, false},
		{`class S extends Array {}; S.from([1, 2, 3])`, []Value{Int(1), Int(1)}, false},
		{`var a = [1, 2, 3]; a.constructor = Array; a`, []Value{Int(1), Int(1)}, false},
		{`Object.freeze([1, 2, 3])`, []Value{Int(1), Int(1)}, false},
		{`Object.preventExtensions([1, 2, 3])`, []Value{Int(1), Int(1)}, false},
		{`Object.defineProperty([1, 2, 3], 'length', { writable: false })`, []Value{Int(1), Int(1)}, false},
		{`var a = [1, 2, 3]; a.x = 1; Object.setPrototypeOf(a, Object.create(Array.prototype)); a`, []Value{Int(1), Int(1)}, false},
	} {
		v, err := r.Run(compileForTest(t, c.src))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := r.spliceDense(v, c.args); ok != c.want {
			t.Errorf("%s: spliceDense took it %v, want %v", c.src, ok, c.want)
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
		if _, ok := r.spliceDense(v, []Value{Int(1), Int(1)}); ok {
			t.Errorf("after %s: spliceDense took a plain array", src)
		}
	}
}
