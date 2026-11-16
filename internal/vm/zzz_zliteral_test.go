package vm

// literalTestSrc makes object literals of every kind of field a literal
// made at once (new_object_lit) takes or leaves to define_field:
// constants, locals, upvalues, this, arguments, calls whose order shows,
// a value that throws midway, a key that repeats, a spread, accessors,
// __proto__, index keys, nested literals, more fields than a table scans,
// a literal grown after it is made, shorthands and a method with super, a
// computed key, an assignment and conditionals among the values, a
// literal among a call's arguments, a setter on Object.prototype a field
// does not call, and a yield among the values. Four rounds: the first
// defines the fields, the second learns the shape, the rest make the
// objects with it. Each function has a local of its own, so that it is
// run as register code rather than by pureCall.
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
