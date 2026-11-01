package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// literalTestSrc makes object literals of every kind of field the tree
// tier's literal node takes or leaves to define_field (zzz_literal.go):
// constants, locals, upvalues, this, arguments, calls whose order shows,
// a value that throws midway, a key that repeats, a spread, accessors,
// __proto__, index keys, nested literals, more fields than a table scans,
// a literal grown after it is made, shorthands and a method with super, a
// computed key, an assignment and conditionals among the values, a
// literal among a call's arguments, a setter on Object.prototype a field
// does not call, and a yield among the values. Four rounds: the first
// defines the fields, the second learns the shape, the rest make the
// objects with it. Each function has a local of its own, so that it is
// run on its tree rather than by pureCall.
const literalTestSrc = `
const log = []
function side(x) { log.push(x); return x }
function keys(o) { return Object.keys(o).map(k => k + '=' + (typeof o[k] === 'object' && o[k] !== null ? '{' + keys(o[k]) + '}' : String(o[k]))).join(',') }
function mixed(j) { let _k = 0; return { a: j, b: j + 1, c: "key", d: true, e: null, f: j, g: false, h: "end" } }
function order(j) { let _k = 0; return { x: side('x' + j), y: side('y' + j), z: j } }
function throws(j) { let _k = 0; try { return { p: side('p'), q: (() => { throw new Error('t' + j) })(), r: side('r') } } catch (e) { return e.message } }
function dup(j) { let _k = 0; return { a: j, b: 2, a: j + 10 } }
function spread(j, o) { let _k = 0; return { a: j, ...o, b: j, c: 3 } }
function acc(j) { let _k = 0; return { a: j, get b() { return j * 2 }, c: 3, set d(v) { log.push('set' + v) } } }
function proto(j, p) { let _k = 0; return { a: j, __proto__: p, b: 2 } }
function idx(j) { let _k = 0; return { 1: 'one', a: j, 0: 'zero', b: 1 } }
function nested(j) { let _k = 0; return { a: { b: j, c: { d: j + 1 } }, e: [j, { f: 1 }] } }
function big(j) { let _k = 0; return { k0: j, k1: 1, k2: 2, k3: 3, k4: 4, k5: 5, k6: 6, k7: 7, k8: 8, k9: 9, k10: 10, k11: j + 11 } }
function grown(j) { let _k = 0; const o = { a: j, b: 1 }; o.c = 2; o.d = 3; o.e = 4; return o }
function short(j) { let _k = 0; let x = j, y = 'y'; return { x, y, m() { return super.toString === Object.prototype.toString } } }
function computed(j, k) { let _k = 0; return { a: 1, [k]: j, b: 2 } }
function args(j) { let _k = 0; return { n: arguments.length, t: typeof this, a: j } }
let up = 7
function upv(j) { let _k = 0; return { u: up, j, w: up + j } }
function* gen(j) { let _k = 0; const o = { a: j, b: yield 'y', c: 3 }; return o }
function assign(j) { let _k = 0; let x = 1; const o = { a: x, b: (x = j), c: x }; return o }
function cond(j) { let _k = 0; return { a: j > 2 ? 'big' : 'small', b: j && 'and', c: j || 'or', d: j ?? 'n' } }
function args2(j) { let _k = 0; return [side('first'), { a: side('second'), b: side('third') }, side('fourth')] }
Object.defineProperty(Object.prototype, 'setterOnProto', { set(v) { log.push('protoSetter') }, configurable: true })
function protoSetter(j) { let _k = 0; return { setterOnProto: j, after: 1 } }
const P = { inherited: 'yes' }
const out = []
for (let r = 0; r < 4; r++) {
  log.length = 0
  const row = [keys(mixed(r)), keys(order(r)), throws(r), keys(dup(r)), keys(spread(r, { s: 1, a: 'o' })),
    keys(acc(r)), acc(r).b, keys(proto(r, P)), proto(r, P).inherited, keys(idx(r)), keys(nested(r)), keys(big(r)),
    big(r).k11, big(r).k5, keys(grown(r)), keys(short(r)), short(r).m(), keys(computed(r, 'k' + (r % 2))), keys(args(r)),
    keys(upv(r)), keys(assign(r)), keys(cond(r)), keys(args2(r)[1]), keys(protoSetter(r)), Object.getOwnPropertyNames(protoSetter(r)).join()]
  const g = gen(r); g.next(); row.push(keys(g.next('B' + r).value))
  row.push(log.join(';'))
  out.push(row.join(' | '))
}
delete Object.prototype.setterOnProto
const res = out.join('\n')
res
`

// literalTestWant is Node's answer.
const literalTestWant = "a=0,b=1,c=key,d=true,e=null,f=0,g=false,h=end | x=x0,y=y0,z=0 | t0 | a=10,b=2 | a=o,s=1,b=0,c=3 | a=0,b=0,c=3,d=undefined | 0 | a=0,b=2 | yes | 0=zero,1=one,a=0,b=1 | a={b=0,c={d=1}},e={0=0,1={f=1}} | k0=0,k1=1,k2=2,k3=3,k4=4,k5=5,k6=6,k7=7,k8=8,k9=9,k10=10,k11=11 | 11 | 5 | a=0,b=1,c=2,d=3,e=4 | x=0,y=y,m=m() { return super.toString === Object.prototype.toString } | true | a=1,k0=0,b=2 | n=1,t=object,a=0 | u=7,j=0,w=7 | a=1,b=0,c=0 | a=small,b=0,c=or,d=0 | a=second,b=third | setterOnProto=0,after=1 | setterOnProto,after | a=0,b=B0,c=3 | x0;y0;p;first;second;third;fourth" +
	"\n" + "a=1,b=2,c=key,d=true,e=null,f=1,g=false,h=end | x=x1,y=y1,z=1 | t1 | a=11,b=2 | a=o,s=1,b=1,c=3 | a=1,b=2,c=3,d=undefined | 2 | a=1,b=2 | yes | 0=zero,1=one,a=1,b=1 | a={b=1,c={d=2}},e={0=1,1={f=1}} | k0=1,k1=1,k2=2,k3=3,k4=4,k5=5,k6=6,k7=7,k8=8,k9=9,k10=10,k11=12 | 12 | 5 | a=1,b=1,c=2,d=3,e=4 | x=1,y=y,m=m() { return super.toString === Object.prototype.toString } | true | a=1,k1=1,b=2 | n=1,t=object,a=1 | u=7,j=1,w=8 | a=1,b=1,c=1 | a=small,b=and,c=1,d=1 | a=second,b=third | setterOnProto=1,after=1 | setterOnProto,after | a=1,b=B1,c=3 | x1;y1;p;first;second;third;fourth" +
	"\n" + "a=2,b=3,c=key,d=true,e=null,f=2,g=false,h=end | x=x2,y=y2,z=2 | t2 | a=12,b=2 | a=o,s=1,b=2,c=3 | a=2,b=4,c=3,d=undefined | 4 | a=2,b=2 | yes | 0=zero,1=one,a=2,b=1 | a={b=2,c={d=3}},e={0=2,1={f=1}} | k0=2,k1=1,k2=2,k3=3,k4=4,k5=5,k6=6,k7=7,k8=8,k9=9,k10=10,k11=13 | 13 | 5 | a=2,b=1,c=2,d=3,e=4 | x=2,y=y,m=m() { return super.toString === Object.prototype.toString } | true | a=1,k0=2,b=2 | n=1,t=object,a=2 | u=7,j=2,w=9 | a=1,b=2,c=2 | a=small,b=and,c=2,d=2 | a=second,b=third | setterOnProto=2,after=1 | setterOnProto,after | a=2,b=B2,c=3 | x2;y2;p;first;second;third;fourth" +
	"\n" + "a=3,b=4,c=key,d=true,e=null,f=3,g=false,h=end | x=x3,y=y3,z=3 | t3 | a=13,b=2 | a=o,s=1,b=3,c=3 | a=3,b=6,c=3,d=undefined | 6 | a=3,b=2 | yes | 0=zero,1=one,a=3,b=1 | a={b=3,c={d=4}},e={0=3,1={f=1}} | k0=3,k1=1,k2=2,k3=3,k4=4,k5=5,k6=6,k7=7,k8=8,k9=9,k10=10,k11=14 | 14 | 5 | a=3,b=1,c=2,d=3,e=4 | x=3,y=y,m=m() { return super.toString === Object.prototype.toString } | true | a=1,k1=3,b=2 | n=1,t=object,a=3 | u=7,j=3,w=10 | a=1,b=3,c=3 | a=big,b=and,c=3,d=3 | a=second,b=third | setterOnProto=3,after=1 | setterOnProto,after | a=3,b=B3,c=3 | x3;y3;p;first;second;third;fourth"

// An object literal's node (zzz_literal.go) makes the object the
// define_fields made, field for field, in the same order, with the same
// side effects, in the tree tier as in the interpreter, as Node does.
func TestTreeObjectLiterals(t *testing.T) {
	for _, c := range compileForTest(t, literalTestSrc).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		switch c.Fn.Name {
		case "mixed", "order", "dup", "spread", "acc", "proto", "idx", "nested", "big", "grown", "upv", "assign", "cond", "args2":
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", c.Fn.Name)
			}
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, literalTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != literalTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, literalTestWant)
		}
	}
	SetTreeTier(true)
	// Past the first two objects, the site knows the shape the fields lead
	// to, which its objects are made with.
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `function mixed(j) { let _k = 0; return { a: j, b: j + 1, c: "key", d: true, e: null, f: j, g: false, h: "end" } }
		let o; for (let i = 0; i < 4; i++) o = mixed(i); o`))
	if err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("mixed")).value().Object().fn().closure
	site := -1
	for _, in := range cl.fn.Code {
		if in.Op == bytecode.OpNewObject {
			site = int(in.B)
		}
	}
	if site < 0 || cl.ic[site].next == nil || v.Object().shape != cl.ic[site].next || cl.ic[site].s1 != cl.ic[site].shape {
		t.Fatalf("the literal's site does not know its shape: %+v", cl.ic[site])
	}
}
