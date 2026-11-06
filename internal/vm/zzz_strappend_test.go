package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// strAppendTestSrc appends to captured bindings every way that could show a
// string being grown in place, each long enough to be a rope: reads between
// appends, whose strings must stay as they were, by a closure, by the maker,
// and compiled before the appender; two appenders to one binding; an empty
// left side, which hands back the operand itself; a rope on the right, a
// string appended to itself and bindings appended to one another; a right
// side whose conversion appends to the binding, which the old value must not
// show; numbers, undefined, BigInts and symbols; surrogates' halves meeting
// at the join; every way of reading a binding; an append whose value is used,
// and short circuits; parameters, which arguments aliases; a loop's bindings,
// copied for each iteration; a generator, a throw and a
// finally; classes; a closure outliving its maker; and a string past the
// longest. Four rounds, so that the tree tier and native code meet them.
const strAppendTestSrc = `
const out = []
const long = 'x'.repeat(70)
function hash(s) { let h = 7; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0; return s.length + ':' + h }

// Reads between appends: what was read stays as it was.
function snapshots() {
  var s = long
  const add = x => { s += x }
  const read = () => s
  const got = []
  for (let i = 0; i < 40; i++) {
    add('<a>'); add('<b>')
    if (i % 3 === 0) got.push(read())
    add('<c>')
  }
  return got.map(hash).join() + hash(read())
}

// Read by its maker, as React's renderToString reads it, between appends.
function maker() {
  var s = ''
  var sink = { push: function (x) { return x !== null && (s += x), true } }
  for (let i = 0; i < 50; i++) sink.push(i % 7 ? '<x>' : null)
  const first = s
  for (let i = 0; i < 50; i++) sink.push('<y>')
  const second = s
  sink.push('<z>')
  return hash(first) + ' ' + hash(second) + ' ' + hash(s)
}

// A reader compiled before the appender: no append is owned.
function hoisted() {
  var s = long
  function get() { return s }
  const add = x => { s += x }
  const a = (add('1'), add('2'), get())
  add('3'); add('4')
  return hash(a) + hash(get())
}

// Two closures appending to one binding, each with reads between.
function two() {
  let s = long
  const a = x => { s += x }, b = x => { s += x }
  const seen = []
  for (let i = 0; i < 30; i++) { a('a'); b('b'); a('A'); if (i % 5 === 0) seen.push(s) }
  return seen.map(hash).join() + hash(s)
}

// Each call of the maker a binding of its own.
function many() {
  const sinks = []
  for (let k = 0; k < 4; k++) {
    let s = long + k
    sinks.push({ add(x) { s += x }, get() { return s } })
  }
  for (let i = 0; i < 100; i++) sinks[i & 3].add('chunk' + (i & 1))
  return sinks.map(k => hash(k.get())).join()
}

// What an empty left side hands back is the operand itself, which others hold.
function emptyLeft() {
  var s = ''
  const add = x => { s += x }
  const shared = long + 'shared'
  add(shared); add('a'); add('b'); add('c')
  const t = 'y'.repeat(100) + long
  var u = ''
  const addU = x => { u += x }
  addU(t); addU('1'); addU('2'); addU('3')
  add('')
  return hash(shared) + hash(s) + hash(t) + hash(u)
}

// A right side that is a rope, that others hold, and a string to itself.
function operands() {
  var s = long
  const add = x => { s += x }
  const dbl = () => { s += s }
  let rope = long
  for (let i = 0; i < 5; i++) rope += 'r' + i
  add(rope); add(rope); add('q')
  const before = rope
  rope += 'more'
  add(rope)
  const a = s
  dbl(); add('tail'); dbl()
  return hash(before) + hash(a) + hash(s)
}

// One binding's string appended to another, each way.
function crossing() {
  var a = long, b = long + '2'
  const toA = () => { a += b }, toB = () => { b += a }
  for (let i = 0; i < 4; i++) { toA(); toB() }
  return hash(a) + hash(b)
}

// The right side's conversion runs code, which appends to the binding: the old
// value was read first, and shows none of it.
function converting() {
  var s = long
  const o = { toString() { s += 'X'; return 'Y' } }
  const v = { valueOf() { s += 'V'; return 7 } }
  const add = x => { s += x }
  const res = []
  for (let i = 0; i < 10; i++) { add('a'); add('b'); add('e'); add('f'); add(o); res.push(hash(s)); add('c'); add('d'); add('g'); add('h'); add(v); res.push(hash(s)) }
  return res.join()
}

// Numbers and anything else, as additions are.
function others() {
  var n = 1, u, m = 'a', b = 1n, z = null
  const addN = x => { n += x }, addU = x => { u += x }, addM = x => { m += x }, addB = x => { b += x }, addZ = x => { z += x }
  addN(2); addN(0.5); addN('s'); addN(long); addN('t'); addN(1); addN('u'); addN(2)
  addU('a'); addU(1); addU(true); addU(long); addU(null)
  addM(1); addM(long); addM(undefined); addM(2n); addM(long)
  addB(2n)
  addZ(long); addZ(long)
  try { addM(Symbol('q')) } catch (e) { addM(e.constructor.name) }
  try { addB(1) } catch (e) { addM(e.constructor.name) }
  return [hash(n), hash(String(u)), hash(m), b, hash(z)].join()
}

// Surrogates' halves meeting at the join, and a string of more than ASCII,
// which reads its code units by index.
function surrogates() {
  var s = long
  const add = x => { s += x }
  add('\ud83d'); add('\ude00'); add('a'); add('b')
  add('é'); add('日本語'); add('\ud83d'); add('\ude00'); add('\ud83d')
  const a = s
  add('c'); add('\ude00'); add('d')
  const parts = []
  for (let i = 68; i < s.length; i++) parts.push(s.charCodeAt(i))
  return hash(a) + hash(s) + parts.join() + s.indexOf('日') + s.lastIndexOf('c') + a.length
}

// Every way of reading the binding.
function reading() {
  var s = long
  const add = x => { s += x }
  const seen = []
  const rd = {
    typ() { return typeof s }, len() { return s.length }, tpl() { return ` + "`" + `[${s}]` + "`" + `.length }, idx() { return s[70] },
    arr() { return [s][0].length }, obj() { return ({ s }).s.length }, call() { return String(s).length },
    eq() { const t = s; return t === s }, cmp() { return s < long }, sw() { switch (s) { case long: return 1; default: return 2 } },
    it() { let n = 0; for (const c of s) n++; return n }, spr() { return [...s].length }, keys() { let n = 0; for (const k in s) n++; return n },
    meth() { return s.charCodeAt(s.length - 1) }, slc() { return s.slice(68, 74) }, dst() { const { length } = s; return length },
    chain() { let a, b; a = b = s; return (a + b).length }, cond() { return (s ? s : '').length }, or() { return (s || '').length },
    plus() { return (s + 'z').length }, left() { return ('z' + s).length }, json() { return JSON.stringify(s).length },
  }
  for (let i = 0; i < 12; i++) {
    add('<' + (i % 10) + '>'); add('abc')
    for (const k in rd) seen.push(rd[k]())
    add('de')
  }
  return hash(seen.join())
}

// An append whose value is used, and the forms of a short circuit.
function values() {
  var s = long
  const a = x => (s += x)
  const b = x => { const r = x && (s += x); return r }
  const c = x => { x && (s += x); return 0 }
  const d = x => { x || (s += 'or'); return 1 }
  const e = x => { x ?? (s += 'nn'); return 2 }
  const f = x => { return x ? (s += x) : 0 }
  const g = x => { (s += x, s += 'y'); return s }
  const r = [a('1'), a('2'), a('3')]
  const held = r[1]
  a('4'); a('5')
  r.push(b('6'), b('7'), b(''), c('8'), c(''), c('9'), d(0), d(1), d(0), e(null), e(1), f('10'), f(''), g('11'))
  return hash(held) + r.map(x => hash(String(x))).join() + hash(s)
}

// Bindings that are not the closure's to grow.
function params(s) {
  const add = x => { s += x }
  add('a'); add('b'); add('c')
  return hash(arguments[0]) + hash(s)
}
function paramsStrict(s) {
  'use strict'
  const add = x => { s += x }
  add('a'); add('b'); add('c')
  return hash(arguments[0]) + hash(s)
}
function perIteration() {
  const fs = []
  for (let s = long; s.length < 90; s += 'i') {
    fs.push(() => s, x => { s += x })
    fs[fs.length - 1]('a'); fs[fs.length - 1]('b')
  }
  return fs.filter((f, i) => i % 2 === 0).map(f => hash(f())).join()
}
function forOf() {
  const fs = []
  for (let s of [long, long + 'a', long + 'b']) {
    fs.push(() => s)
    const add = x => { s += x }
    add('1'); add('2'); add('3')
  }
  return fs.map(f => hash(f())).join()
}
function blockLet() {
  const fs = []
  for (let i = 0; i < 4; i++) {
    let s = long
    const add = x => { s += x }
    add('a'); add('b'); add('c')
    fs.push(() => s)
    add('d'); add('e')
  }
  return fs.map(f => hash(f())).join()
}

// Across a yield, an await, a throw and a finally.
function generators() {
  let s = long
  function* g() { for (let i = 0; i < 6; i++) { add('a'); add('b'); const t = s; yield t; add('c') } }
  const add = x => { s += x }
  const r = []
  for (const t of g()) { add('z'); r.push(hash(t)) }
  return r.join() + hash(s)
}
function throwing() {
  let s = long
  const add = x => { s += x }
  const boom = () => { add('a'); add('b'); throw new Error('e') }
  let t
  try { boom() } catch (e) { t = s; add('c') } finally { add('d'); add('e') }
  return hash(t) + hash(s)
}

// A class's fields and methods, and a closure that outlives its maker.
class Sink {
  #s = long
  push(x) { this.#s += x; return this }
  get value() { return this.#s }
}
function classes() {
  const k = new Sink()
  for (let i = 0; i < 40; i++) k.push('a').push('b')
  let s = long
  class C { add(x) { s += x } static read() { return s } }
  const c = new C()
  c.add('1'); c.add('2')
  const a = C.read()
  c.add('3')
  return hash(k.value) + hash(a) + hash(C.read())
}
function outlives() {
  const mk = () => { let s = long; return { add: x => { s += x }, get: () => s } }
  const a = mk(), b = mk()
  for (let i = 0; i < 20; i++) { a.add('a'); a.add('A'); b.add('b') }
  const held = a.get()
  a.add('more')
  return hash(held) + hash(a.get()) + hash(b.get())
}

// One past the longest string: the binding keeps what it had.
function tooLong() {
  let s = 'x'.repeat(1 << 28)
  const add = x => { s += x }
  add('a'); add('b')
  const big = 'y'.repeat((1 << 29) - 30)
  try { add(big); return 'no' } catch (e) { return e.constructor.name + s.length }
}

// Held across an append, by what aliases the binding.
function heldArguments(s) {
  const add = x => { s += x }
  add('a'); add('b'); add('c')
  const held = arguments[0]
  add('d'); add('e')
  return hash(held) + hash(s) + hash(arguments[0])
}

// A loop's head copied for each iteration: a closure of one iteration keeps
// what its copy held, whatever the next appends.
function loopCopies() {
  const reads = []
  for (let s = long, i = 0; i < 5; i++) {
    const add = x => { s += x }
    const read = () => s
    add('a'); add('b'); add('c')
    reads.push(read)
  }
  return reads.map(r => hash(r())).join()
}

// A right side that runs code, which appends: the old value was read first.
function rightRuns() {
  var s = long
  const add = x => { s += x }
  const f = () => { s += 'X'; s += 'Z'; return 'Y' }
  const byCall = () => { s += f() }
  const bySequence = () => { s += (s += 'p', 'q') }
  const o = { get v() { s += 'G'; return 'H' } }
  const byGetter = () => { s += o.v }
  const r = []
  for (let i = 0; i < 6; i++) {
    add('a'); add('b'); byCall(); r.push(hash(s))
    add('c'); add('d'); bySequence(); r.push(hash(s))
    add('e'); add('f'); byGetter(); r.push(hash(s))
  }
  return r.join()
}

// What only comparison and a rope's own reads show: halves joined as one code
// point, and ASCII that stops being so past a right side grown in place.
function joined() {
  var s = long
  const add = x => { s += x }
  add('a'); add('b'); add('\ud83d')
  const one = s === long + 'ab\ud83d'
  add('\ude00'); add('c')
  const two = s === long + 'ab😀c'
  var t = long
  const addT = x => { t += x }
  addT('a'); addT('b'); addT('c'); addT('é'); addT('d'); addT('e')
  const held = t
  const code = held.charCodeAt(74)
  const code2 = held.charCodeAt(73)
  addT('f')
  const unit = held[73]
  var u = long
  const addU = x => { u += x }
  addU('a'); addU('b'); addU('\ud83d')
  const v = u
  addU('\ude00'); addU('z')
  return [one, two, code, code2, unit, hash(held), hash(t), t === long + 'abcédef', t.indexOf('é'),
    v === long + 'ab\ud83d', u === long + 'ab😀z', u.codePointAt(72), hash(u)].join()
}

// A reader only the interpreter runs: the tree tier builds no with.
function interpreted() {
  var s = long
  const add = x => { s += x }
  const read = o => { with (o) { return s } }
  add('a'); add('b')
  const held = read({})
  add('c'); add('d')
  const held2 = read({})
  add('e')
  return hash(held) + hash(held2) + hash(s)
}

// Halves that meet in appends with no read between them, and a right side
// taken apart again: a low surrogate's string, a high one put before it, is
// rebuilt from its pieces, which each say what they are.
function halves() {
  var w = long
  const addW = x => { w += x }
  addW('a'); addW('b'); addW('\ud83d'); addW('\ude00'); addW('c')
  var z = long
  const addZ = x => { z += x }
  addZ('a'); addZ('b'); addZ('\ud83d'); addZ('\ude00')
  var low = '\ude00' + long
  const addL = x => { low += x }
  addL('a'); addL('b'); addL('c'); addL('é'); addL('d')
  const held = low
  const joined = '\ud83d' + held
  // The same past the run's ropes, in the string's tail.
  var t = long
  const addT = x => { t += x }
  addT('a'); addT('b'); addT('c'); addT('d'); addT('\ud83d'); addT('\ude00'); addT('e')
  return [t === long + 'abcd😀e', t.length, held === '\ude00' + long + 'abcéd', joined === '😀' + long + 'abcéd',
    w === long + 'ab😀c', z === long + 'ab😀', w.length, w.codePointAt(72),
    joined.length, joined.charCodeAt(0), joined.charCodeAt(1), joined.charCodeAt(72), joined.charCodeAt(75),
    joined.charCodeAt(76), joined.indexOf('é'), hash(joined)].join()
}

// Reads hot enough for native code, each held across more appends: a closure
// called often first, inlined or not, and the maker's own, in its loop.
function nativeReads() {
  var s = long
  const add = x => { s += x }
  const read = () => s
  let warm = 0
  for (let i = 0; i < 3000; i++) warm += read().length
  const held = []
  for (let i = 0; i < 800; i++) {
    add('a'); add('b')
    held.push(read())
    add('c'); add('d')
    if ((i & 15) === 0) held.push(s)
    add('e')
  }
  return warm + ':' + hash(held.map(hash).join()) + hash(s)
}

// A local appended to, with a right side that runs code. esc reaches nothing
// of it; what is held stays as it was.
function esc(x) { return String(x).replace(/</g, '&lt;') }
function localRuns() {
  var out = '' + long
  const got = []
  for (let i = 0; i < 30; i++) {
    out += esc('<a' + i)
    out += '|'
    out += esc(i)
    out += esc('b')
    if (i % 4 === 0) got.push(out)
    out += '#'
  }
  return got.map(hash).join() + hash(out)
}
function localNoLoop(a, b) {
  var out = long + '<tr>'
  out += esc(a)
  out += '<td>'
  out += esc(b)
  out += '</td>'
  out += esc(a + b)
  return out
}
function localBranches(xs) {
  let out = ''
  for (const x of xs) {
    if (x === 1) out += 'one'
    else if (x === 2) { out += esc('two'); continue }
    else out += x
    x > 2 && (out += '>')
    x > 3 || (out += '<')
    switch (x) { case 1: out += 'a'; break; case 3: out += esc('c'); default: out += 'd' }
  }
  return hash(out)
}
// Reassigned, to a string others hold; the local's own appends leave it alone.
function localShared() {
  const shared = long + 'shared'
  var out = ''
  out = shared
  out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += 'e'
  const held = out
  out = held
  out += 'f'; out += 'g'; out += 'h'; out += 'i'; out += 'j'
  return hash(shared) + hash(held) + hash(out)
}
// What the right side's text can do to the local itself.
function localSelf() {
  var a = '' + long, b = '' + long, c = '' + long, d = '' + long
  for (let i = 0; i < 5; i++) {
    a += a.length
    b += (b = 'q' + i, 'r')
    c += (c += 'x', 'y')
    d += (d += 'x', d += 'z', d.length)
    a += 'A'; b += 'B'; c += 'C'; d += 'D'
  }
  return hash(a) + hash(b) + hash(c) + hash(d)
}
// A function or a class that mentions the local: it can run in the right side.
function localClosure() {
  var out = '' + long
  const f = () => { out += 'X'; return 'Y' }
  for (let i = 0; i < 6; i++) { out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += f() }
  return hash(out)
}
function localClosureLater() {
  var out = '' + long, f = () => 'n', r = []
  for (let i = 0; i < 6; i++) {
    out += 'a'; out += 'b'; out += 'c'; out += 'd'
    out += f()
    f = () => { out += 'X'; return 'Y' }
    r.push(hash(out))
  }
  return r.join()
}
function localClass() {
  var out = '' + long
  class C { f = (out += 'X', 'Y'); static g() { out += 'S'; return 'T' } get h() { return out } }
  for (let i = 0; i < 6; i++) { out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += new C().f; out += C.g() }
  return hash(out) + hash(new C().h)
}
// A throw out of the right side leaves what the local had.
function localStatic() {
  var out = '' + long
  for (let i = 0; i < 6; i++) { out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += (class { static { out += 'X' } }, 'Y') }
  return hash(out)
}
// A read earlier in the source than the appends, and later when it runs.
function localReadFirst() {
  var s = '' + long
  const held = []
  for (let i = 0; i < 6; i++) {
    const h = s
    s += 'a'; s += 'b'; s += 'c'; s += 'd'; s += 'e'
    held.push(h)
  }
  return held.map(hash).join() + hash(s)
}
function localThrows() {
  var out = '' + long
  const boom = () => { throw new RangeError('x') }
  let n = 0
  for (let i = 0; i < 8; i++) {
    out += 'a'; out += 'b'; out += 'c'; out += 'd'
    try { out += boom() } catch (e) { n += e.message.length }
    out += 'e'
  }
  return n + hash(out)
}
// Suspended in the right side; each generator's local its own.
function localGenerators() {
  function* g(k) { var out = long + k; for (let i = 0; i < 5; i++) { out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += yield out.length } yield out }
  const a = g('a'), b = g('b'), r = []
  let x = a.next(), y = b.next()
  for (let i = 0; i < 5; i++) { x = a.next('p' + i); y = b.next('q' + i); r.push(x.value, y.value) }
  return r.map(v => typeof v === 'string' ? hash(v) : v).join()
}
function localRecursive(n) {
  if (n === 0) return 'leaf'
  var out = '(' + n
  out += localRecursive(n - 1)
  out += ','
  out += localRecursive(n - 1)
  out += ')'
  return out
}
// Any right side, as an addition has it.
function localOperands() {
  var out = ''
  const o = { toString() { return 'obj' } }, v = { valueOf() { return 7 } }
  out += 1; out += 2.5; out += null; out += undefined; out += true; out += 2n; out += o; out += v; out += long
  out += -0; out += 1e21; out += NaN; out += [1, 2]; out += ` + "`" + `t${out.length}` + "`" + `
  try { out += Symbol('s') } catch (e) { out += e.constructor.name }
  var n = ''
  n = 1
  n += 2
  n += 'x'
  n += 3
  var u = 'a'
  u = undefined
  u += 1
  return hash(out) + n + u
}
// Reads of every kind between appends, in a function run hot.
function localReads() {
  var s = '' + long, t = 0
  for (let i = 0; i < 600; i++) {
    s += esc(i)
    if (i % 16 === 0) t += s.length
    s += 'a'
    const h = i % 64 === 0 ? s : ''
    s += 'b'
    if (h) t += hash(h).length
    s += 'c'
  }
  return t + hash(s)
}
function localHot(n) {
  const r = []
  for (let k = 0; k < n; k++) {
    var out = '<ul>' + long
    for (let i = 0; i < 8; i++) { out += '<li>'; out += esc(i); out += '</li>' }
    out += '</ul>'
    r.push(out)
    out += 'x'
  }
  return hash(r[0]) + hash(r[n - 1]) + r.length
}

// x.length is no read: the run of appends goes on past it, the answer right.
function lengthClosure() {
  var s = '' + long
  const len = () => s.length
  const add = x => { s += x }
  const get = () => s
  const held = []
  let t = 0
  for (let i = 0; i < 30; i++) {
    add('<a>'); t += len(); add('<b>'); t += len(); add('<c>'); t += s.length
    if (i % 7 === 0) held.push(get())
  }
  return t + held.map(hash).join() + hash(s)
}
function lengthLocal() {
  var out = '' + long, t = 0
  const held = []
  for (let i = 0; i < 30; i++) {
    out += esc(i); t += out.length; out += 'a'; out += 'b'
    if (i % 5 === 0) held.push(out)
    t += out.length; out += 'c'
    if (out.length > 100000) break
  }
  return t + held.map(hash).join() + hash(out)
}
function lengthWhile() {
  var out = '', n = 0
  while (out.length < 200) { out += '<x>'; n++ }
  return n + hash(out)
}
// A length that is no string's: a getter runs, and may do what it likes.
function lengthGetter() {
  var s = { get length() { s = long; s += 'g'; return 3 }, toString() { return 'o' } }
  const add = x => { s += x }
  const got = [s.length]
  add('a'); add('b'); add('c'); add('d')
  got.push(s.length, hash(s))
  var a = ['x', 'y'], u
  got.push(a.length)
  try { got.push(u.length) } catch (e) { got.push(e.name) }
  return got.join()
}
// A reader Go answers without a frame, held across a run that buys a tail.
function pureReader() {
  var s = long
  const sink = { push(x) { s += x }, get() { return s } }
  const held = []
  for (let i = 0; i < 300; i++) {
    sink.push('<a>'); sink.push('<b>'); sink.push('<c>'); sink.push('<d>'); sink.push('<e>')
    if (i % 50 === 0) held.push(sink.get())
  }
  return held.map(hash).join() + hash(sink.get())
}
// An addition of nothing gives back what an append owned, and the read after
// it must still end the owning.
function emptyAfterLocal(e) {
  var out = '' + long
  const held = []
  for (let i = 0; i < 12; i++) {
    out += 'a'; out += 'b'; out += 'c'; out += 'd'; out += e
    var h = out
    held.push(h)
    out += 'z'
  }
  return held.map(hash).join()
}
function emptyAfterClosure(e) {
  var s = '' + long
  const add = (x, y) => { s += x; s += y; return s }
  const go = x => { s += x }
  const held = []
  for (let i = 0; i < 12; i++) {
    go('a'); go('b'); go('c'); go('d')
    held.push(add(e, e))
    go('z')
  }
  return held.map(hash).join()
}
// Additions given back: of a number, what is not a string, and a read's.
function givenBack() {
  var out = 'a', got = []
  out += 1
  got.push(out)
  out = 2
  out += 3
  got.push(out)
  var t = '' + long
  t += { toString() { return 'o' } }
  got.push(hash(t))
  t += 1.5
  got.push(hash(t))
  t += null
  var r = t
  t += 'z'
  got.push(hash(r), hash(t))
  return got.join()
}

const tests = [snapshots, maker, hoisted, two, many, emptyLeft, operands, crossing, converting, others, surrogates, reading, values,
  () => params(long), () => paramsStrict(long), perIteration, forOf, blockLet, generators, throwing, classes, outlives,
  () => heldArguments(long), loopCopies, rightRuns, joined, halves, interpreted, nativeReads,
  localRuns, () => localNoLoop('<x', 'y'), () => localBranches([1, 2, 3, 4, 5, 1, 3]), localShared, localSelf, localClosure,
  localClosureLater, localClass, localStatic, localReadFirst, localThrows, localGenerators, () => hash(localRecursive(5)), localOperands, localReads, () => localHot(400),
  lengthClosure, lengthLocal, lengthWhile, lengthGetter, pureReader, () => emptyAfterLocal(''), () => emptyAfterClosure(''), givenBack]
for (let r = 0; r < 4; r++) for (const t of tests) out.push(t())
out.push(tooLong())
out.join('|')
`

// strAppendTestWant is Node's answer.
const strAppendTestWant = "76:-1591915514,103:239840560,130:-1972117050,157:1162182512,184:1814603654,211:1786978736,238:1672977734,265:-754412560,292:429703942,319:1759106608,346:-2125195066,373:-352655248,400:-1722179962,427:762301104430:-2074811193|126:1897913735 276:1251682215 279:-41135303|72:-192080175274:945455241|73:584735513,88:2104768519,103:-1912397479,118:-1822545465,133:1212118937,148:-22901881160:56928519|221:1217630772,221:528173710,221:-1630755658,221:1974754576|76:-975285279:1510658550170:802463879173:413498443|80:251795335315:-6709108811268:1565499303|2401:7020268913884:-1762580281|75:-709849936,80:1354628293,85:-129027470,90:-74801085,95:703794612,100:2109177409,105:-206094730,110:1646098111,115:256828600,120:-1577227843,125:-1996152710,130:-1023396037,135:1639780796,140:1966522169,145:-1326327682,150:-1323703881,155:1186370752,160:1435139765,165:-843684222,170:1171355187|78:121345370,89:1658004910,170:-1944680487,3,144:1543159566|81:-32975027184:-998260378120,120,55357,56832,97,98,233,26085,26412,35486,55357,56832,55357,99,56832,100758181|1061:2127171585|72:-192080175271:-1724529334,72:-1920801752,73:584687883,76:-1950578454,77:-338389875,0:7,1:265,1:265,1:265,1:266,1:266,1:266,1:267,1:267,87:1014783539,1:265,90:-85833746690:-858337466|73:58473554773:584735547|70:-102546130573:584735547|72:-1920800216,75:-709846238,78:1389793256,81:-153739934,84:-1631132248,87:199290786,90:1427106664|73:584687883,74:946884216,74:946914007|75:-709851908,75:-709851908,75:-709851908,75:-709851908|72:-1920800216,76:-529888462,80:1160156988,84:-1786147258,88:2116415568,92:50844399494:-1011589629|72:-192080021675:-709851908|150:144893540772:-192080175273:584687883|110:-640020281114:123270364490:1197716679|73:58473554775:-70985190875:-709851908|73:584735547,76:-530575545,79:-896315461,82:-322123065,85:-1411193797|73:584735537,76:-530871457,79:-1121891320,82:1171279696,85:1279209184,88:-323916569,91:993103377,94:1808067711,97:960419560,100:-1212917648,103:-469691968,106:410131975,109:-940193551,112:-1824242273,115:-1780258360,118:-1420535408,121:-857473888,124:1460979495|true,true,100,233,é,76:-530444785,77:736080951,true,73,true,true,128512,75:-654990257|true,77,true,true,true,true,75,128512,77,55357,56832,97,233,100,75,77:996209043|72:-192080021674:94693287375:-709851908|210000:13362:-7216164124070:-200329785|79:-270115893,119:664997363,159:266943483,205:272750813,253:-2043988743,301:-1134013643,349:203764689,397:-1052580243410:-807718721|xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx<tr>&lt;x<td>y</td>&lt;xy|29:-991559854|76:-975285281:4392370386:718245009|85:-171487618680:-16247380:151556725785:-1620183556|100:-1535504217|75:-709851899,80:988269170,85:-126623451,90:-1904672174,95:-1874160315,100:-884088270|106:1030628149107:1884701635|100:-1535504217|70:-1025461305,75:-709851908,80:730606823,85:-790380580,90:-1349861881,95:-1057824068100:-203528409|8110:-1022810041|81,81,87,87,93,93,99,99,101:740177166,101:1950424426|252:-1841443535|121:-19445110753x3NaN|665923560:-1194495795|159:-371085914159:-371085914400|1858579:273865649,142:-69078841,205:-1407545103,268:859345799,331:255497777340:-1805643641|822073:584688426,93:905074623,114:1126965742,139:-802160968,164:255722632,189:1093335805210:85090372|67201:-453413093|3,75,75:-704341566,2,TypeError|85:-21847216,835:2039117168,1585:102535056,2335:-991235664,3085:-1829266480,3835:7007472484570:-265115897|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|a1,5,71:-1724529272,74:947301168,78:2046908599,79:-970342749|76:-1591915514,103:239840560,130:-1972117050,157:1162182512,184:1814603654,211:1786978736,238:1672977734,265:-754412560,292:429703942,319:1759106608,346:-2125195066,373:-352655248,400:-1722179962,427:762301104430:-2074811193|126:1897913735 276:1251682215 279:-41135303|72:-192080175274:945455241|73:584735513,88:2104768519,103:-1912397479,118:-1822545465,133:1212118937,148:-22901881160:56928519|221:1217630772,221:528173710,221:-1630755658,221:1974754576|76:-975285279:1510658550170:802463879173:413498443|80:251795335315:-6709108811268:1565499303|2401:7020268913884:-1762580281|75:-709849936,80:1354628293,85:-129027470,90:-74801085,95:703794612,100:2109177409,105:-206094730,110:1646098111,115:256828600,120:-1577227843,125:-1996152710,130:-1023396037,135:1639780796,140:1966522169,145:-1326327682,150:-1323703881,155:1186370752,160:1435139765,165:-843684222,170:1171355187|78:121345370,89:1658004910,170:-1944680487,3,144:1543159566|81:-32975027184:-998260378120,120,55357,56832,97,98,233,26085,26412,35486,55357,56832,55357,99,56832,100758181|1061:2127171585|72:-192080175271:-1724529334,72:-1920801752,73:584687883,76:-1950578454,77:-338389875,0:7,1:265,1:265,1:265,1:266,1:266,1:266,1:267,1:267,87:1014783539,1:265,90:-85833746690:-858337466|73:58473554773:584735547|70:-102546130573:584735547|72:-1920800216,75:-709846238,78:1389793256,81:-153739934,84:-1631132248,87:199290786,90:1427106664|73:584687883,74:946884216,74:946914007|75:-709851908,75:-709851908,75:-709851908,75:-709851908|72:-1920800216,76:-529888462,80:1160156988,84:-1786147258,88:2116415568,92:50844399494:-1011589629|72:-192080021675:-709851908|150:144893540772:-192080175273:584687883|110:-640020281114:123270364490:1197716679|73:58473554775:-70985190875:-709851908|73:584735547,76:-530575545,79:-896315461,82:-322123065,85:-1411193797|73:584735537,76:-530871457,79:-1121891320,82:1171279696,85:1279209184,88:-323916569,91:993103377,94:1808067711,97:960419560,100:-1212917648,103:-469691968,106:410131975,109:-940193551,112:-1824242273,115:-1780258360,118:-1420535408,121:-857473888,124:1460979495|true,true,100,233,é,76:-530444785,77:736080951,true,73,true,true,128512,75:-654990257|true,77,true,true,true,true,75,128512,77,55357,56832,97,233,100,75,77:996209043|72:-192080021674:94693287375:-709851908|210000:13362:-7216164124070:-200329785|79:-270115893,119:664997363,159:266943483,205:272750813,253:-2043988743,301:-1134013643,349:203764689,397:-1052580243410:-807718721|xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx<tr>&lt;x<td>y</td>&lt;xy|29:-991559854|76:-975285281:4392370386:718245009|85:-171487618680:-16247380:151556725785:-1620183556|100:-1535504217|75:-709851899,80:988269170,85:-126623451,90:-1904672174,95:-1874160315,100:-884088270|106:1030628149107:1884701635|100:-1535504217|70:-1025461305,75:-709851908,80:730606823,85:-790380580,90:-1349861881,95:-1057824068100:-203528409|8110:-1022810041|81,81,87,87,93,93,99,99,101:740177166,101:1950424426|252:-1841443535|121:-19445110753x3NaN|665923560:-1194495795|159:-371085914159:-371085914400|1858579:273865649,142:-69078841,205:-1407545103,268:859345799,331:255497777340:-1805643641|822073:584688426,93:905074623,114:1126965742,139:-802160968,164:255722632,189:1093335805210:85090372|67201:-453413093|3,75,75:-704341566,2,TypeError|85:-21847216,835:2039117168,1585:102535056,2335:-991235664,3085:-1829266480,3835:7007472484570:-265115897|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|a1,5,71:-1724529272,74:947301168,78:2046908599,79:-970342749|76:-1591915514,103:239840560,130:-1972117050,157:1162182512,184:1814603654,211:1786978736,238:1672977734,265:-754412560,292:429703942,319:1759106608,346:-2125195066,373:-352655248,400:-1722179962,427:762301104430:-2074811193|126:1897913735 276:1251682215 279:-41135303|72:-192080175274:945455241|73:584735513,88:2104768519,103:-1912397479,118:-1822545465,133:1212118937,148:-22901881160:56928519|221:1217630772,221:528173710,221:-1630755658,221:1974754576|76:-975285279:1510658550170:802463879173:413498443|80:251795335315:-6709108811268:1565499303|2401:7020268913884:-1762580281|75:-709849936,80:1354628293,85:-129027470,90:-74801085,95:703794612,100:2109177409,105:-206094730,110:1646098111,115:256828600,120:-1577227843,125:-1996152710,130:-1023396037,135:1639780796,140:1966522169,145:-1326327682,150:-1323703881,155:1186370752,160:1435139765,165:-843684222,170:1171355187|78:121345370,89:1658004910,170:-1944680487,3,144:1543159566|81:-32975027184:-998260378120,120,55357,56832,97,98,233,26085,26412,35486,55357,56832,55357,99,56832,100758181|1061:2127171585|72:-192080175271:-1724529334,72:-1920801752,73:584687883,76:-1950578454,77:-338389875,0:7,1:265,1:265,1:265,1:266,1:266,1:266,1:267,1:267,87:1014783539,1:265,90:-85833746690:-858337466|73:58473554773:584735547|70:-102546130573:584735547|72:-1920800216,75:-709846238,78:1389793256,81:-153739934,84:-1631132248,87:199290786,90:1427106664|73:584687883,74:946884216,74:946914007|75:-709851908,75:-709851908,75:-709851908,75:-709851908|72:-1920800216,76:-529888462,80:1160156988,84:-1786147258,88:2116415568,92:50844399494:-1011589629|72:-192080021675:-709851908|150:144893540772:-192080175273:584687883|110:-640020281114:123270364490:1197716679|73:58473554775:-70985190875:-709851908|73:584735547,76:-530575545,79:-896315461,82:-322123065,85:-1411193797|73:584735537,76:-530871457,79:-1121891320,82:1171279696,85:1279209184,88:-323916569,91:993103377,94:1808067711,97:960419560,100:-1212917648,103:-469691968,106:410131975,109:-940193551,112:-1824242273,115:-1780258360,118:-1420535408,121:-857473888,124:1460979495|true,true,100,233,é,76:-530444785,77:736080951,true,73,true,true,128512,75:-654990257|true,77,true,true,true,true,75,128512,77,55357,56832,97,233,100,75,77:996209043|72:-192080021674:94693287375:-709851908|210000:13362:-7216164124070:-200329785|79:-270115893,119:664997363,159:266943483,205:272750813,253:-2043988743,301:-1134013643,349:203764689,397:-1052580243410:-807718721|xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx<tr>&lt;x<td>y</td>&lt;xy|29:-991559854|76:-975285281:4392370386:718245009|85:-171487618680:-16247380:151556725785:-1620183556|100:-1535504217|75:-709851899,80:988269170,85:-126623451,90:-1904672174,95:-1874160315,100:-884088270|106:1030628149107:1884701635|100:-1535504217|70:-1025461305,75:-709851908,80:730606823,85:-790380580,90:-1349861881,95:-1057824068100:-203528409|8110:-1022810041|81,81,87,87,93,93,99,99,101:740177166,101:1950424426|252:-1841443535|121:-19445110753x3NaN|665923560:-1194495795|159:-371085914159:-371085914400|1858579:273865649,142:-69078841,205:-1407545103,268:859345799,331:255497777340:-1805643641|822073:584688426,93:905074623,114:1126965742,139:-802160968,164:255722632,189:1093335805210:85090372|67201:-453413093|3,75,75:-704341566,2,TypeError|85:-21847216,835:2039117168,1585:102535056,2335:-991235664,3085:-1829266480,3835:7007472484570:-265115897|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|a1,5,71:-1724529272,74:947301168,78:2046908599,79:-970342749|76:-1591915514,103:239840560,130:-1972117050,157:1162182512,184:1814603654,211:1786978736,238:1672977734,265:-754412560,292:429703942,319:1759106608,346:-2125195066,373:-352655248,400:-1722179962,427:762301104430:-2074811193|126:1897913735 276:1251682215 279:-41135303|72:-192080175274:945455241|73:584735513,88:2104768519,103:-1912397479,118:-1822545465,133:1212118937,148:-22901881160:56928519|221:1217630772,221:528173710,221:-1630755658,221:1974754576|76:-975285279:1510658550170:802463879173:413498443|80:251795335315:-6709108811268:1565499303|2401:7020268913884:-1762580281|75:-709849936,80:1354628293,85:-129027470,90:-74801085,95:703794612,100:2109177409,105:-206094730,110:1646098111,115:256828600,120:-1577227843,125:-1996152710,130:-1023396037,135:1639780796,140:1966522169,145:-1326327682,150:-1323703881,155:1186370752,160:1435139765,165:-843684222,170:1171355187|78:121345370,89:1658004910,170:-1944680487,3,144:1543159566|81:-32975027184:-998260378120,120,55357,56832,97,98,233,26085,26412,35486,55357,56832,55357,99,56832,100758181|1061:2127171585|72:-192080175271:-1724529334,72:-1920801752,73:584687883,76:-1950578454,77:-338389875,0:7,1:265,1:265,1:265,1:266,1:266,1:266,1:267,1:267,87:1014783539,1:265,90:-85833746690:-858337466|73:58473554773:584735547|70:-102546130573:584735547|72:-1920800216,75:-709846238,78:1389793256,81:-153739934,84:-1631132248,87:199290786,90:1427106664|73:584687883,74:946884216,74:946914007|75:-709851908,75:-709851908,75:-709851908,75:-709851908|72:-1920800216,76:-529888462,80:1160156988,84:-1786147258,88:2116415568,92:50844399494:-1011589629|72:-192080021675:-709851908|150:144893540772:-192080175273:584687883|110:-640020281114:123270364490:1197716679|73:58473554775:-70985190875:-709851908|73:584735547,76:-530575545,79:-896315461,82:-322123065,85:-1411193797|73:584735537,76:-530871457,79:-1121891320,82:1171279696,85:1279209184,88:-323916569,91:993103377,94:1808067711,97:960419560,100:-1212917648,103:-469691968,106:410131975,109:-940193551,112:-1824242273,115:-1780258360,118:-1420535408,121:-857473888,124:1460979495|true,true,100,233,é,76:-530444785,77:736080951,true,73,true,true,128512,75:-654990257|true,77,true,true,true,true,75,128512,77,55357,56832,97,233,100,75,77:996209043|72:-192080021674:94693287375:-709851908|210000:13362:-7216164124070:-200329785|79:-270115893,119:664997363,159:266943483,205:272750813,253:-2043988743,301:-1134013643,349:203764689,397:-1052580243410:-807718721|xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx<tr>&lt;x<td>y</td>&lt;xy|29:-991559854|76:-975285281:4392370386:718245009|85:-171487618680:-16247380:151556725785:-1620183556|100:-1535504217|75:-709851899,80:988269170,85:-126623451,90:-1904672174,95:-1874160315,100:-884088270|106:1030628149107:1884701635|100:-1535504217|70:-1025461305,75:-709851908,80:730606823,85:-790380580,90:-1349861881,95:-1057824068100:-203528409|8110:-1022810041|81,81,87,87,93,93,99,99,101:740177166,101:1950424426|252:-1841443535|121:-19445110753x3NaN|665923560:-1194495795|159:-371085914159:-371085914400|1858579:273865649,142:-69078841,205:-1407545103,268:859345799,331:255497777340:-1805643641|822073:584688426,93:905074623,114:1126965742,139:-802160968,164:255722632,189:1093335805210:85090372|67201:-453413093|3,75,75:-704341566,2,TypeError|85:-21847216,835:2039117168,1585:102535056,2335:-991235664,3085:-1829266480,3835:7007472484570:-265115897|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|74:946932873,79:-788322093,84:162081481,89:-1587285357,94:-1157596919,99:-1247572909,104:-751110327,109:561903123,114:874292617,119:2108633043,124:1516864457,129:-1477232237|a1,5,71:-1724529272,74:947301168,78:2046908599,79:-970342749|RangeError268435458"

// Appends answer as Node does, in the tree tier as in the interpreter.
func TestStrAppend(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, strAppendTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != strAppendTestWant {
			t.Errorf("tree tier %v:\n%s", tree, firstDifference(got, strAppendTestWant))
		}
	}
	SetTreeTier(true)
}

// A declaration captures the binding just as a function expression does.
// Its append on the right must not change the left value already read.
const strAppendDeclarationSrc = `
function render() {
  var out = '' + 'x'.repeat(70)
  function emit() { out += 'X'; return 'Y' }
  out += 'a'; out += 'b'; out += 'c'; out += 'd'
  out += emit()
  return out.slice(70)
}
var result = ''
for (var round = 0; round < 2000; round++) result = render()
result
`

func TestStrAppendDeclaration(t *testing.T) {
	t.Cleanup(func() { SetTreeTier(true) })
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, strAppendDeclarationSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "abcdY" {
			t.Errorf("tree tier %v: got %q, want abcdY", tree, got)
		}
	}
}

// firstDifference is where got and want part, by the |-separated answer.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "|"), strings.Split(want, "|")
	for i := range min(len(g), len(w)) {
		if g[i] != w[i] {
			return fmt.Sprintf("answer %d: got %s want %s", i, g[i], w[i])
		}
	}
	return fmt.Sprintf("%d answers, want %d", len(g), len(w))
}

// A run of appends makes one string and its right side, not a node each.
func TestStrAppendAllocations(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function appends(n) {
			var s = ""
			var sink = { push(x) { let _k = 0; s += x } }
			for (var i = 0; i < n; i++) sink.push("<x>")
			return s.length
		}
		appends(100)`)); err != nil {
		t.Fatal(err)
	}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	v, err := r.Run(compileForTest(t, `appends(10000)`))
	runtime.ReadMemStats(&b)
	if err != nil || v.Number() != 30000 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := b.Mallocs - a.Mallocs; n > 1000 && allocsOrdinary() {
		t.Errorf("10,000 appends made %d allocations", n)
	}
}

func TestStrAppendTailGrowth(t *testing.T) {
	for _, chunk := range []string{"x", strings.Repeat("\u00e9", 100), strings.Repeat("z", strTailLongest)} {
		r := New(Config{})
		s := NewString(strings.Repeat("a", 70))
		piece := NewString(chunk)
		n := max(200, 32768/len(chunk))
		grew := false
		for range n {
			var room int
			if s.tailed {
				room = cap((*strTail)(s.aux).buf)
			}
			var err error
			s, err = strAppend[struct{}](r, s, piece)
			if err != nil {
				t.Fatal(err)
			}
			if s.tailed {
				if next := cap((*strTail)(s.aux).buf); room >= 16384 && next > room {
					grew = true
					if next < 2*room {
						t.Errorf("tail grew from %d to %d bytes, want at least double", room, next)
					}
					runtime.GC()
				}
			}
		}
		want := strings.Repeat("a", 70) + strings.Repeat(chunk, n)
		if s.Go() != want || s.Len() != NewString(want).Len() {
			t.Fatal("growing the tail changed its text or length")
		}
		if !grew {
			t.Fatal("no large tail grew")
		}
		r.Close()
	}
}

// A long right side stays what it is: copying it would cost what a node
// saves, with room for four times it, and a rope need not be written out for
// an append. The short ones after it go on to a tail, of their own bytes.
func TestStrAppendLongRightSide(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	long := NewString(strings.Repeat("z", strTailLongest+1))
	rope := NewString(strings.Repeat("r", 100)).Concat(NewString(strings.Repeat("s", 100)))
	if rope.left == nil {
		t.Fatal("want a rope")
	}
	s := NewString(strings.Repeat("a", 70))
	want := s.Go()
	add := func(b *String) {
		t.Helper()
		var err error
		if s, err = strAppend[struct{}](r, s, b); err != nil {
			t.Fatal(err)
		}
	}
	for range 8 {
		add(long)
		want += long.Go()
		if s.tailed || s.rightSide() != long {
			t.Fatalf("a long right side was copied: tailed %v", s.tailed)
		}
	}
	add(rope)
	if s.tailed || s.rightSide() != rope || rope.left == nil {
		t.Fatal("a rope on the right was written out")
	}
	want += strings.Repeat("r", 100) + strings.Repeat("s", 100)
	short := NewString("short")
	for range strAppendRopes + 3 {
		add(short)
		want += "short"
	}
	if !s.tailed {
		t.Fatal("short appends after long ones made no tail")
	}
	if room := cap((*strTail)(s.aux).buf); room > 4*strTailLongest {
		t.Fatalf("a tail of %d bytes", room)
	}
	// A tail stops at a long side, and starts again after it.
	add(long)
	want += long.Go()
	if s.tailed {
		t.Fatal("a long right side went into a tail")
	}
	if s.Go() != want || s.Len() != len(want) {
		t.Fatal("the text changed")
	}
}

// A marked string's fields say what an append has made of it, and a read's
// str_freeze ends it. The string is an ordinary one all the while.
func TestStrAppendStates(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	long := NewString(strings.Repeat("x", 100))
	s, err := strAppend[struct{}](r, long, NewString("a"))
	if err != nil {
		t.Fatal(err)
	}
	if s == long || !s.building || s.tailed || s.left == nil || long.building {
		t.Fatalf("the first append: new %v, marked %v, tailed %v, rope %v, its left marked %v", s != long, s.building, s.tailed, s.left != nil, long.building)
	}
	for i := 1; i < strAppendRopes; i++ {
		g, err := strAppend[struct{}](r, s, NewString("b"))
		if err != nil {
			t.Fatal(err)
		}
		if g == s || !g.building || g.tailed || s.tailed {
			t.Fatalf("append %d: new %v, marked %v, tailed %v", i+1, g != s, g.building, g.tailed)
		}
		s = g
	}
	g, err := strAppend[struct{}](r, s, NewString("c"))
	if err != nil {
		t.Fatal(err)
	}
	if g != s || !s.tailed {
		t.Fatalf("the run's tail: the same %v, tailed %v", g == s, s.tailed)
	}
	if g, _ = strAppend[struct{}](r, s, NewString("d")); g != s {
		t.Fatal("the append after it made a string")
	}
	strFreeze[struct{}](Str(s))
	if s.building {
		t.Fatal("str_freeze left the mark")
	}
	want := strings.Repeat("x", 100) + "a" + strings.Repeat("b", strAppendRopes-1) + "cd"
	n, err := strAppend[struct{}](r, s, NewString("e"))
	if err != nil {
		t.Fatal(err)
	}
	if n == s || !n.building || n.ropes != 0 {
		t.Fatalf("after a read: new %v, marked %v, %d ropes", n != s, n.building, n.ropes)
	}
	if s.Go() != want || n.Go() != want+"e" {
		t.Fatalf("after a read: %q and %q", s.Go(), n.Go())
	}
	// What an empty operand, or a short result, hands back is never marked.
	for _, tc := range [][2]*String{{NewString(""), long}, {long, NewString("")}, {NewString("a"), NewString("b")}} {
		if g, err := strAppend[struct{}](r, tc[0], tc[1]); err != nil || g.building {
			t.Fatalf("%q + %q marked: %v", tc[0].Go(), tc[1].Go(), err)
		}
	}
	if long.building {
		t.Fatal("an operand handed back was marked")
	}
}

// A rope's flags are its sides', each from the right side, and a tail's are
// kept as it grows, on the string and on the tail alike.
func TestStrAppendFlags(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	long := strings.Repeat("x", 100)
	low, high := "\xed\xb8\x80", "\xed\xa0\xbd"
	s, err := strAppend[struct{}](r, NewString(low+long), NewString("\u00e9"+high))
	if err != nil {
		t.Fatal(err)
	}
	if s.left == nil || !s.startsLow || !s.endsHigh || s.ascii {
		t.Fatalf("rope: left %v, starts low %v, ends high %v, ascii %v", s.left != nil, s.startsLow, s.endsHigh, s.ascii)
	}
	s, err = strAppend[struct{}](r, NewString(long), NewString(low+"x"))
	if err != nil {
		t.Fatal(err)
	}
	if s.startsLow || s.endsHigh || s.ascii {
		t.Fatalf("rope: starts low %v, ends high %v, ascii %v", s.startsLow, s.endsHigh, s.ascii)
	}
	s = NewString(long)
	for i := 0; i < strAppendRopes+1; i++ {
		if s, err = strAppend[struct{}](r, s, NewString("a")); err != nil {
			t.Fatal(err)
		}
	}
	if !s.tailed || !s.ascii {
		t.Fatalf("tailed %v, ascii %v", s.tailed, s.ascii)
	}
	tail := (*strTail)(s.aux)
	if s, err = strAppend[struct{}](r, s, NewString("\u00e9"+high)); err != nil {
		t.Fatal(err)
	}
	if !s.endsHigh || s.ascii || !tail.endsHigh || tail.ascii {
		t.Fatalf("tail: ends high %v and %v, ascii %v and %v", s.endsHigh, tail.endsHigh, s.ascii, tail.ascii)
	}
	if s, err = strAppend[struct{}](r, s, NewString("b")); err != nil {
		t.Fatal(err)
	}
	if s.endsHigh || tail.endsHigh {
		t.Fatalf("tail: ends high %v and %v after a b", s.endsHigh, tail.endsHigh)
	}
}

// A meter counts what a concatenation takes, so a metered runtime's strings
// are made as ever.
func TestStrAppendMetered(t *testing.T) {
	r := New(Config{MemoryLimit: 1 << 30})
	defer r.Close()
	long := NewString(strings.Repeat("x", 100))
	s, err := strAppend[struct{}](r, long, NewString("a"))
	if err != nil {
		t.Fatal(err)
	}
	if s.building {
		t.Fatal("a metered runtime's append was marked")
	}
}
