package vm

import "testing"

// Object.create's objects on a shared prototype are made with the room the
// ones before them came to have (newCreated): one property in room for one
// once the first has been given its property, at most maxSlack however
// many an object of the prototype came to have; and they answer as objects
// grown property by property do -- their values, their keys' order,
// deletions and the prototype's properties.
func TestObjectCreateRoom(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var proto = { x: 1 }, big = { z: 0 }, out = [], o, b
		for (var j = 0; j < 5; j++) { o = Object.create(proto); o.y = j; out.push(o.x + o.y) }
		var one = o
		for (var j = 0; j < 5; j++) {
			b = Object.create(big)
			b.k0 = 0; b.k1 = 1; b.k2 = 2; b.k3 = 3; b.k4 = 4; b.k5 = 5; b.k6 = 6; b.k7 = 7; b.k8 = 8; b.k9 = 9
			b.k10 = 10; b.k11 = 11; b.k12 = 12; b.k13 = 13; b.k14 = 14; b.k15 = 15; b.k16 = 16; b.k17 = 17; b.k18 = 18; b.k19 = 19
		}
		var wide = b, fresh = Object.create(big)
		fresh.a = 1; fresh.b = 2; delete fresh.a; fresh.c = 3; fresh.a = 4
		out.push(Object.keys(fresh).join(), fresh.z, Object.keys(wide).length, wide.k19, Object.getPrototypeOf(fresh) === big)
		var d = Object.create(proto, { p: { value: 7, enumerable: true }, q: { get() { return 8 } } })
		out.push(d.p, d.q, Object.keys(d).join(), d.x)
		out.join()`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "1,2,3,4,5,b,c,a,0,20,19,true,7,8,p,1"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	one := globalObject(r, "one")
	if int(one.valn) != 1 || int(one.valc) != 1 {
		t.Errorf("Object.create's fifth object: %d properties in room for %d, want 1 in 1", one.valn, one.valc)
	}
	// d, given two properties, taught it two.
	if root := one.shape.root(); !root.generic || root.slack != 2 {
		t.Errorf("the prototype's root notes room for %d (generic %v), want 2", root.slack, root.generic)
	}
	if root := globalObject(r, "wide").shape.root(); root.slack != maxSlack {
		t.Errorf("the root of objects of 20 properties notes room for %d, want %d", root.slack, maxSlack)
	}
}

// An object pattern's rest of a plain object of a shared layout is copied
// by its layout (restShaped): the same keys, in the same order, as the
// property-by-property copy makes of sources it declines -- accessors,
// symbols, elements, non-enumerable keys, computed keys that are not
// strings -- and a rest object of its own, made with room for them.
func TestObjectRestShaped(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const show = o => Object.keys(o).map(k => k + '=' + String(o[k])).join(' ') + (Object.getPrototypeOf(o) === Object.prototype ? '' : ' !proto')
		const out = []
		const ref = { a: 1, b: 2, c: 3, d: 4, e: 5 }
		let r, a, c
		;({ a, ...r } = ref); out.push(show(r))
		;({ a, c, ...r } = ref); out.push(show(r))
		;({ ...r } = ref); out.push(show(r))
		;({ x: a, ...r } = ref); out.push(show(r))
		;({ ['b']: a, [3]: c, ...r } = { 3: 'three', b: 1, z: 2 }); out.push(show(r))
		const s = Symbol('s')
		;({ a, ...r } = { a: 1, [s]: 2, b: 3 }); out.push(show(r), r[s])
		;({ a, ...r } = { a: 1, get g() { return 'got' }, b: 3 }); out.push(show(r), Object.getOwnPropertyDescriptor(r, 'g').value)
		const ne = { a: 1, b: 2 }; Object.defineProperty(ne, 'h', { value: 9, enumerable: false })
		;({ a, ...r } = ne); out.push(show(r))
		;({ a, ...r } = Object.create({ inherited: 1 }, { a: { value: 1, enumerable: true }, own: { value: 2, enumerable: true } })); out.push(show(r))
		r.extra = 1; ref.f = 6; out.push(show(r), 'f' in r)
		const kobj = { toString() { return 'b' } }
		;({ [kobj]: a, ...r } = ref); out.push(show(r))
		out.join(' | ')`))
	if err != nil {
		t.Fatal(err)
	}
	want := "b=2 c=3 d=4 e=5 | b=2 d=4 e=5 | a=1 b=2 c=3 d=4 e=5 | a=1 b=2 c=3 d=4 e=5 | z=2 | b=3 | 2 | " +
		"g=got b=3 | got | b=2 | own=2 | own=2 extra=1 | false | a=1 c=3 d=4 e=5 f=6"
	if got := v.String().Go(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	o, err := r.Run(compileForTest(t, `({ a: 1, b: 2, c: 3, d: 4, e: 5, f: 6 })`))
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := r.restShaped(o.Object(), []Value{Str(NewString("a"))})
	if !ok || int(rest.valn) != 5 || int(rest.valc) != 5 {
		t.Errorf("restShaped: took it %v, %d properties in room for %d, want 5 in 5", ok, rest.valn, rest.valc)
	}
}
