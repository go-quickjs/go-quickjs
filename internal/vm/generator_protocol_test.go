package vm

import (
	"reflect"
	"testing"
	"unsafe"
)

// generatorCases are generators, iterators and the loops, spreads,
// destructurings and helpers that step them, each a script and its
// completion value, Node v26.10.0's (vm.runInContext, as a classic
// script). One whose source starts "var out = 'pending'" is async: its
// answer is out once the jobs it queued have run. They cover what a
// generator's resumption, a loop's cursor and a helper's step do without
// a result object or a lookup: return and throw, and before the body
// starts; a for-of left by break, continue, return and a throw, which
// calls return, and a generator that throws, which is not; finally
// clauses that yield, override or run; a generator resumed while it
// runs; yield* with its sent values, throws and results handed on;
// next, return, Symbol.iterator and prototypes a script replaces, on an
// instance and on the intrinsics; with scopes and direct evals across
// suspensions; and for-in, whose snapshot a cursor keeps.
var generatorCases = []struct{ name, want, src string }{
	{"breakReturns", `1,2,fin`,
		`var log = []; function* g() { try { yield 1; yield 2; yield 3 } finally { log.push('fin') } } for (var x of g()) { log.push(x); if (x == 2) break } log.join()`},
	{"continueThenEnd", `2,fin`,
		`var log = []; function* g() { try { yield 1; yield 2 } finally { log.push('fin') } } for (var x of g()) { if (x == 1) continue; log.push(x) } log.join()`},
	{"returnInLoop", `1,fin,r`,
		`var log = []; function* g() { try { yield 1; yield 2 } finally { log.push('fin') } } function f() { for (var x of g()) { log.push(x); return 'r' } } log.push(f()); log.join()`},
	{"bodyThrows", `1,fin,boom`,
		`var log = []; function* g() { try { yield 1; yield 2 } finally { log.push('fin') } } try { for (var x of g()) { log.push(x); throw 'boom' } } catch (e) { log.push(e) } log.join()`},
	{"genThrows", `1,RangeError,{"value":1,"done":false}`,
		`var log = []; function* g() { yield 1; throw new RangeError('x') } var it = g(); it.return = function () { log.push('return called'); return {} }; try { for (var x of it) log.push(x) } catch (e) { log.push(e.name) } log.push(JSON.stringify(g().next())); log.join()`},
	{"genThrowsThenDone", `["e",{"done":true},{"value":4,"done":true}]`,
		`function* g() { yield 1; throw 'e' } var it = g(); it.next(); var a; try { it.next() } catch (e) { a = e } JSON.stringify([a, it.next(), it.return(4)])`},
	{"instanceReturn", `1,own return undefined 0`,
		`var log = []; function* g() { yield 1; yield 2 } var it = g(); it.return = function (v) { log.push('own return ' + v + ' ' + arguments.length); return {} }; for (var x of it) { log.push(x); break } log.join()`},
	{"returnFinallyYields", `[{"value":"f","done":false},{"value":"f2","done":false},{"value":5,"done":true},{"done":true}]`,
		`function* g() { try { yield 1 } finally { yield 'f'; yield 'f2' } } var it = g(); it.next(); JSON.stringify([it.return(5), it.next(), it.next(), it.next()])`},
	{"returnFinallyOverrides", `[{"value":"f","done":true},{"done":true}]`,
		`function* g() { try { yield 1 } finally { return 'f' } } var it = g(); it.next(); JSON.stringify([it.return(5), it.next()])`},
	{"returnNotStarted", `[{"value":3,"done":true},{"done":true}]0`,
		`var log = []; function* g() { log.push('ran'); yield 1 } var it = g(); JSON.stringify([it.return(3), it.next()]) + log.length`},
	{"returnNoFinally", `[{"value":7,"done":true},{"done":true},{"value":8,"done":true},false]false`,
		`function* g() { yield 1; yield 2 } var it = g(); it.next(); var a = it.return(7); JSON.stringify([a, it.next(), it.return(8), it.throw === undefined]) + (a === it.return())`},
	{"returnClosesCaptures", `1,2,{"done":true}`,
		`function* g() { var x = 1; var f = () => x++; yield f; x = 10; yield 2 } var it = g(); var f = it.next().value; it.return(); [f(), f(), JSON.stringify(it.next())].join()`},
	{"breakClosesCaptures", `0,1,2|{"value":3,"done":true}`,
		`var fs = []; function* g() { for (let i = 0; i < 5; i++) { fs.push(() => i); yield i } } for (var v of g()) { if (v == 2) break } fs.map(f => f()).join() + '|' + JSON.stringify(g().return(3))`},
	{"destructureCloses", `[1,1,2,{"done":true}]`,
		`function* g() { yield 1; yield 2; yield 3 } var it = g(); var [a] = it; var [b, c] = g(); JSON.stringify([a, b, c, it.next()])`},
	{"patchedReturn", `patched true 0,patched true 0|TypeError`,
		`var log = []; function* g() { yield 1; yield 2 } var GP = Object.getPrototypeOf(g).prototype; var orig = GP.return; GP.return = function (v) { log.push('patched ' + (this instanceof g) + ' ' + arguments.length); return orig.call(this, v) }; for (var x of g()) break; var [y] = g(); GP.return = function () { return 5 }; var e; try { for (var x of g()) break } catch (err) { e = err.constructor.name } GP.return = orig; log.join() + '|' + e`},
	{"returnGetter", `get,call`,
		`var log = []; function* g() { yield 1 } var it = g(); Object.defineProperty(it, 'return', { get() { log.push('get'); return function () { log.push('call'); return {} } } }); for (var x of it) break; log.join()`},
	{"returnTakeHelper", `0,1{"done":true}`,
		`var log = []; function* g() { var i = 0; while (true) yield i++ } var it = g(); var r = it.take(2).toArray(); r.join() + JSON.stringify(it.next())`},
	{"returnInsideCatch", `{"value":4,"done":true}0`,
		`var log = []; function* g() { try { yield 1 } catch (e) { log.push('caught') } yield 2 } var it = g(); it.next(); JSON.stringify(it.return(4)) + log.length`},
	{"returnArgumentsLeft", `[{"value":9,"done":true},{"done":true}]`,
		`function* g(a) { var args = arguments; yield 1; yield args[0] } var it = g(5); it.next(); JSON.stringify([it.return(9), it.next()])`},
	{"returnRunning", `TypeError{"done":true}`,
		`var it; function* g() { yield it.return(1) } it = g(); var r; try { it.next() } catch (e) { r = e.constructor.name } r + JSON.stringify(it.next())`},
	{"throwNotStarted", `t{"done":true}`,
		`function* g() { try { yield 1 } catch (e) { yield 'caught' } } var it = g(); var r; try { it.throw(new Error('t')) } catch (e) { r = e.message } r + JSON.stringify(it.next())`},
	{"throwCaught", `[{"value":"c7","done":false},{"value":1,"done":false},{"value":"c8","done":false},{"value":3,"done":true},{"done":true}]`,
		`function* g() { while (true) { try { yield 1 } catch (e) { yield 'c' + e } } } var it = g(); it.next(); JSON.stringify([it.throw(7), it.next(), it.throw(8), it.return(3), it.next()])`},
	{"throwFinally", `fin,out x,{"done":true}`,
		`var log = []; function* g() { try { yield 1 } finally { log.push('fin') } } var it = g(); it.next(); try { it.throw('x') } catch (e) { log.push('out ' + e) } log.push(JSON.stringify(it.next())); log.join()`},
	{"sentValues", `1,10,12,true`,
		`function* g() { var x = yield 1; var y = yield x * 2; return x + y } var it = g(); [it.next('ignored').value, it.next(5).value, it.next(7).value, it.next().done].join()`},
	{"reentrantNext", `TypeError`,
		`var it; function* g() { try { it.next() } catch (e) { yield e.constructor.name } } it = g(); it.next().value`},
	{"reentrantReturn", `TypeError`,
		`var it; function* g() { try { it.return(1) } catch (e) { yield e.constructor.name } } it = g(); it.next().value`},
	{"reentrantThrow", `TypeError`,
		`var it; function* g() { try { it.throw(1) } catch (e) { yield e.constructor.name } } it = g(); it.next().value`},
	{"reentrantForOf", `TypeError`,
		`var it; function* g() { try { for (var x of it) { } } catch (e) { yield e.constructor.name } } it = g(); [...it].join()`},
	{"wrongReceiver", `TypeError,TypeError,TypeError`,
		`var GP = Object.getPrototypeOf(function* () { }).prototype; var out = []; for (var m of ['next', 'return', 'throw']) { try { GP[m].call({}) } catch (e) { out.push(e.constructor.name) } } out.join()`},
	{"yieldStar", `1,2,r`,
		`function* i() { yield 1; yield 2; return 'r' } function* o() { var r = yield* i(); yield r } [...o()].join()`},
	{"yieldStarBreak", `a,b,inner fin,outer fin`,
		`var log = []; function* inner() { try { yield 'a'; yield 'b' } finally { log.push('inner fin') } } function* outer() { try { yield* inner(); yield 'c' } finally { log.push('outer fin') } } for (var v of outer()) { log.push(v); if (v == 'b') break } log.join()`},
	{"yieldStarBreakNoFinally", `1,inner fin,plain return 1,{"done":true}`,
		`var log = []; function* inner() { try { yield 1; yield 2 } finally { log.push('inner fin') } } function* outer() { yield* inner() } for (var v of outer()) { log.push(v); break } var plain = { [Symbol.iterator]() { return this }, next() { return { value: 'p', done: false } }, return(v) { log.push('plain return ' + arguments.length); return { done: true } } }; function* outer2() { yield* plain } var it = outer2(); it.next(); log.push(JSON.stringify(it.return(6))); log.join()`},
	{"yieldStarThrow", `[{"value":"inner caught x","done":false},{"done":true}]`,
		`function* inner() { try { yield 1 } catch (e) { yield 'inner caught ' + e } } function* outer() { yield* inner() } var it = outer(); it.next(); JSON.stringify([it.throw('x'), it.next()])`},
	{"yieldStarNoThrow", `closed,TypeError`,
		`var log = []; var inner = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } }, return() { log.push('closed'); return {} } }; function* outer() { yield* inner } var it = outer(); it.next(); try { it.throw('x') } catch (e) { log.push(e.constructor.name) } log.join()`},
	{"yieldStarSent", `1,11,{"value":30,"done":true}`,
		`function* inner() { var a = yield 1; var b = yield a + 1; return a + b } function* outer() { return yield* inner() } var it = outer(); [it.next().value, it.next(10).value, JSON.stringify(it.next(20))].join()`},
	{"yieldStarRaw", `true,value,done,extra`,
		`var res = { value: 9, done: false, extra: 1 }; var inner = { [Symbol.iterator]() { return this }, next() { return res } }; function* outer() { yield* inner } var r = outer().next(); (r === res) + ',' + Object.keys(r).join()`},
	{"yieldStarDeep", `64`,
		`function* t(d) { if (d == 0) { yield 1; return } yield* t(d - 1); yield* t(d - 1) } var s = 0; for (var v of t(6)) s += v; s`},
	{"nestedForOf", `10,20,a fin`,
		`var log = []; function* a() { try { yield 1; yield 2; yield 3 } finally { log.push('a fin') } } function* b() { for (var x of a()) yield x * 10 } for (var v of b()) { log.push(v); if (v == 20) break } log.join()`},
	{"returnFromInnerLoop", `a fin,[{"value":1,"done":false},{"value":"r","done":true},{"done":true}]`,
		`var log = []; function* a() { try { yield 1; yield 2 } finally { log.push('a fin') } } function* b() { for (var x of a()) { if (x == 2) return 'r'; yield x } } var it = b(); log.push(JSON.stringify([it.next(), it.next(), it.next()])); log.join()`},
	{"spread", `0,1,2,3|4|0,2,4`,
		`function* g(n) { for (var i = 0; i < n; i++) yield i } [...g(4)].join() + '|' + Math.max(...g(5)) + '|' + Array.from(g(3), x => x * 2).join()`},
	{"destructure", `1,1,3,1,2+3,fin,fin,fin`,
		`var log = []; function* g() { try { yield 1; yield 2; yield 3 } finally { log.push('fin') } } var [a] = g(); var [b, , c] = g(); var [d, ...rest] = g(); [a, b, c, d, rest.join('+'), log.join()].join()`},
	{"destructureEmpty", ``,
		`var log = []; function* g() { log.push('started'); try { yield 1 } finally { log.push('fin') } } var [] = g(); log.join()`},
	{"twiceIterated", `2,0`,
		`function* g() { yield 1; yield 2 } var it = g(); var a = [...it], b = [...it]; a.length + ',' + b.length`},
	{"prototypeChain", `true,true,true,[object Generator],function`,
		`function* g() { yield 1 } var it = g(); var GP = Object.getPrototypeOf(g).prototype; [Object.getPrototypeOf(it) === g.prototype, Object.getPrototypeOf(g.prototype) === GP, it[Symbol.iterator]() === it, Object.prototype.toString.call(it), typeof it.next].join()`},
	{"customPrototype", `hi,TypeError,true,1`,
		`function* g() { yield 1 } g.prototype = { hello() { return 'hi' } }; var it = g(); var a = it.hello(); var b; try { it.next() } catch (e) { b = e.constructor.name } g.prototype = 7; var it2 = g(); var GP = Object.getPrototypeOf(function* () { }).prototype; [a, b, Object.getPrototypeOf(it2) === GP, it2.next().value].join()`},
	{"patchedNext", `6,3,8`,
		`function* g() { yield 1; yield 2; yield 3 } var GP = Object.getPrototypeOf(g).prototype; var orig = GP.next; var count = 0; GP.next = function (v) { count++; return orig.call(this, v) }; var s = 0; for (var x of g()) s += x; var sp = [...g()].length; GP.next = orig; s + ',' + sp + ',' + count`},
	{"patchedIterator", `7,8;7,8;7,8;7;7,8|1`,
		`function* g() { yield 1 } function* o() { yield* g() } var GP = Object.getPrototypeOf(g).prototype; var IP = Object.getPrototypeOf(GP); var orig = IP[Symbol.iterator]; IP[Symbol.iterator] = function () { return [7, 8][Symbol.iterator]() }; var f = []; for (var x of g()) f.push(x); var [d] = g(); var r = [[...g()].join(), Array.from(g()).join(), f.join(), d, [...o()].join()].join(';'); IP[Symbol.iterator] = orig; r + '|' + [...g()].join()`},
	{"generatorProtoOwnKeys", `false,true,true,1`,
		`var GP = Object.getPrototypeOf(function* () { }).prototype; var IP = Object.getPrototypeOf(GP); [GP.hasOwnProperty(Symbol.iterator), IP.hasOwnProperty(Symbol.iterator), GP[Symbol.iterator] === IP[Symbol.iterator], Object.getOwnPropertySymbols(GP).length].join()`},
	{"ownNext", `0,0`,
		`function* g() { yield 1; yield 2 } var it = g(); it.next = function () { return { done: true, value: 'x' } }; var r = [...it].length; var n = 0; for (var x of it) n++; r + ',' + n`},
	{"nextGetter", `3,1,4`,
		`function* g() { yield 1 } var it = g(); var reads = 0, calls = 0; Object.defineProperty(it, 'next', { get() { reads++; return function () { calls++; return { done: calls > 3 } } } }); var n = 0; for (var x of it) n++; n + ',' + reads + ',' + calls`},
	{"resultObjects", `true,value+done,true,{"done":true},true`,
		`function* g() { yield 1; yield 2 } var it = g(); var a = it.next(), b = it.next(), c = it.next(); [a !== b, Object.keys(a).join('+'), Object.getPrototypeOf(a) === Object.prototype, JSON.stringify(c), c.hasOwnProperty('value')].join()`},
	{"helpers", `0,6,12,18|10|true|true|3|0,0,1,0,1,2|2,3`,
		`function* g(n) { for (var i = 0; i < n; i++) yield i } [g(10).map(x => x * 2).filter(x => x % 3 == 0).toArray().join(), g(5).reduce((a, b) => a + b, 0), g(5).some(x => x == 3), g(5).every(x => x < 5), g(5).find(x => x > 2), g(4).flatMap(x => g(x)).toArray().join(), g(6).drop(2).take(2).toArray().join()].join('|')`},
	{"helpersClose", `0,1,2|4|true|closed,closed,closed`,
		`var log = []; function* nat() { try { var i = 0; while (true) yield i++ } finally { log.push('closed') } } var r = nat().take(3).toArray(); var s = nat().find(x => x == 4); var t = nat().map(x => x * 2).some(x => x > 5); r.join() + '|' + s + '|' + t + '|' + log.join()`},
	{"helpersThrow", `mf|closed,closed`,
		`var log = []; function* nat() { try { var i = 0; while (true) yield i++ } finally { log.push('closed') } } var e1, e2; try { nat().map(x => { if (x == 2) throw 'm'; return x }).toArray() } catch (e) { e1 = e } try { nat().forEach(x => { if (x == 3) throw 'f' }) } catch (e) { e2 = e } e1 + e2 + '|' + log.join()`},
	{"helperReentrant", `TypeError`,
		`var h; function* g() { yield 1; yield 2 } h = g().map(x => { try { h.next() } catch (e) { return e.constructor.name } }); h.next().value`},
	{"helperOverHelper", `2,4|{"done":true}{"done":true}`,
		`function* g() { yield 1; yield 2; yield 3 } var h = g().map(x => x + 1); var h2 = h.filter(x => x != 3); var out = []; for (var v of h2) out.push(v); out.join() + '|' + JSON.stringify(h.next()) + JSON.stringify(h2.next())`},
	{"helperReturn", `[{"value":3,"done":false},{"done":true},{"done":true}]fin`,
		`var log = []; function* g() { try { yield 1; yield 2 } finally { log.push('fin') } } var h = g().map(x => x * 3); var a = h.next(); var b = h.return(); var c = h.next(); JSON.stringify([a, b, c]) + log.join()`},
	{"helperPatchedNext", `2,4,3`,
		`function* g() { yield 1; yield 2 } var GP = Object.getPrototypeOf(g).prototype; var orig = GP.next; var count = 0; GP.next = function (v) { count++; return orig.call(this, v) }; var r = g().map(x => x * 2).toArray().join(); GP.next = orig; r + ',' + count`},
	{"helperNextCachedOnce", `1,2,3`,
		`function* g() { yield 1; yield 2; yield 3 } var it = g(); var h = it.map(x => x); it.next = function () { return { done: true } }; h.toArray().join()`},
	{"helperBadResult", `TypeError`,
		`var it = { next() { return 5 } }; var r; try { Iterator.prototype.map.call(it, x => x).next() } catch (e) { r = e.constructor.name } r`},
	{"helperStackDepth", `6`,
		`function* g() { yield 1 } var h = g(); for (var i = 0; i < 5; i++) h = h.map(x => x + 1); h.toArray().join()`},
	{"helperFrames", `3,1,1,1,1`,
		`function* g() { yield new Error('x').stack } function* h() { yield 1 } function count(s, re) { return s.split('\n').filter(l => re.test(l)).length } var a = g().map(x => x).filter(x => true).toArray()[0]; var b = h().map(x => new Error('y').stack).reduce((p, c) => c, 0); var c; for (var v of h().map(x => new Error('z').stack)) c = v; [count(a, /\.next \(/), count(a, /\.toArray \(/), count(b, /\.next \(/), count(b, /\.reduce \(/), count(c, /\.next \(/)].join()`},
	{"helperBigPipeline", `810|1,1,1|11,21,31|3|0,1,5`,
		`function* nat() { var i = 0; while (true) yield i++ } var s = 0; for (var v of nat().filter(x => x % 3 == 0).map(x => x * x).drop(2).take(5)) s += v; var arr = Iterator.from({ next() { return { value: 1, done: false } } }).take(3).toArray(); var it = [10, 20, 30].values().map(x => x + 1); var m = new Map([[1, 2]]).entries().map(([k, v]) => k + v).toArray(); s + '|' + arr.join() + '|' + [...it].join() + '|' + m.join() + '|' + Iterator.concat(nat().take(2), [5]).toArray().join()`},
	{"helperReentrantForOf", `TypeError,TypeError`,
		`var h; function* g() { yield 1; yield 2 } h = g().map(x => { try { for (var y of h) { } return 'no' } catch (e) { return e.constructor.name } }); var r = []; for (var v of h) r.push(v); r.join()`},
	{"helperReentrantStep", `1TypeError`,
		`var h; function* g() { yield 1 } h = g().filter(x => { try { [...h]; return true } catch (e) { r = e.constructor.name; return true } }); var r; [...h].length + r`},
	{"iteratorReadOnce", `1,2:1 3,6:1 1,2:1 1,2:1 1,2:1 2:1 2:1`,
		`var n = 0; var o = { get [Symbol.iterator]() { n++; return function* () { yield 1; yield 2 } } }; var out = []; for (var f of [() => Array.from(o).join(), () => Array.from(o, x => x * 3).join(), () => new Uint8Array(o).join(), () => Uint8Array.from(o).join(), () => [...o].join(), () => new Set(o).size, () => Math.max(...o)]) { n = 0; var r = f(); out.push(r + ':' + n) } out.join(' ')`},
	{"arrayFromNotCallable", `TypeError,TypeError,TypeError,a,a,TypeError`,
		`var out = []; for (var m of [5, 'x', {}, null, undefined]) { try { out.push(Array.from({ [Symbol.iterator]: m, length: 1, 0: 'a' }).join()) } catch (e) { out.push(e.constructor.name) } } try { Array.from(null) } catch (e) { out.push(e.constructor.name) } out.join()`},
	{"spreadPatched", `0;1;7;7;c,c,c,c;2;1,3;[]`,
		`function* g() { yield 1; yield 2 } var reads = 0; var a = g(); Object.defineProperty(a, 'next', { get() { reads++; return function () { return { done: true } } } }); var b = g(); b[Symbol.iterator] = function () { return [7][Symbol.iterator]() }; var c = g(); Object.setPrototypeOf(c, { next() { return { value: 'c', done: reads++ > 4 } }, [Symbol.iterator]() { return this } }); var d = g(); [[...a].length, reads, [...b].join(), Math.max(...b), [...c].join(), new Set(g()).size, Array.from(d, (x, i) => x + i).join(), JSON.stringify([...d])].join(';')`},
	{"deepStepRange", `RangeError1`,
		`function* g() { yield 1 } function f(n) { for (var x of g()) return f(n + 1) } var r; try { f(0) } catch (e) { r = e.constructor.name } var s = 0; for (var v of g()) s += v; r + s`},
	{"genFrames", `1,1,1,1`,
		`function* g() { yield new Error('x').stack; yield new Error('y').stack } function count(s, re) { return s.split('\n').filter(l => re.test(l)).length } var out = []; for (var v of g()) out.push(count(v, /\.next \(/)); var it = g(); out.push(count(it.next().value, /\.next \(/)); out.push(count([...g()][1], /\.next \(/)); out.join()`},
	{"iterationChanges", `3/0 3/0 0/0 3/0 0/0 3/0 0/0 3/0 5/1 TypeError 3/0 3/0 3/0 3/0`,
		`function* g() { yield 1; yield 2 } var GP = Object.getPrototypeOf(g).prototype, IP = Object.getPrototypeOf(GP); var orig = GP.next, origIter = IP[Symbol.iterator]; function sum(it) { var s = 0; for (var v of it) s += v; return s + '/' + [...it].length } var out = [sum(g()), sum(g())]; GP.next = function () { return { done: this.k = (this.k | 0) + 1, value: 'p' } }; out.push(sum(g())); GP.next = orig; out.push(sum(g())); g.prototype.next = function () { return { done: true } }; out.push(sum(g())); delete g.prototype.next; out.push(sum(g())); Object.setPrototypeOf(g.prototype, { next() { return { done: true } }, [Symbol.iterator]() { return this } }); out.push(sum(g())); Object.setPrototypeOf(g.prototype, GP); out.push(sum(g())); IP[Symbol.iterator] = function () { return [5][Symbol.iterator]() }; out.push(sum(g())); delete IP[Symbol.iterator]; try { sum(g()) } catch (e) { out.push(e.constructor.name) } IP[Symbol.iterator] = origIter; out.push(sum(g())); Object.defineProperty(GP, 'next', { get() { return orig } }); out.push(sum(g())); Object.defineProperty(GP, 'next', { value: orig, writable: true, enumerable: false, configurable: true }); var it = g(); it.extra = 1; out.push(sum(it), sum(g())); out.join(' ')`},
	{"iterationShadowed", `3/0 3/0 9/1 3/0 3/0 0/0`,
		`function* g() { yield 1; yield 2 } var GP = Object.getPrototypeOf(g).prototype; function sum(it) { var s = 0; for (var v of it) s += v; return s + '/' + [...it].length } var out = [sum(g()), sum(g())]; GP[Symbol.iterator] = function () { return [9][Symbol.iterator]() }; out.push(sum(g())); delete GP[Symbol.iterator]; out.push(sum(g())); var o = Object.getPrototypeOf(GP); var mid = Object.create(GP); Object.setPrototypeOf(g.prototype, mid); out.push(sum(g())); mid.next = function () { return { done: true } }; out.push(sum(g())); out.join(' ')`},
	{"withScope", `1,5,5`,
		`function* g(o) { with (o) { yield x; x = 5; yield x } } var o = { x: 1 }; var it = g(o); [it.next().value, it.next().value, o.x].join()`},
	{"withDefined", `3,4,4`,
		`var o = { y: 3 }; with (o) { var gf = function* () { yield y; y = 4; yield y } } var r = [...gf()].join(); r + ',' + o.y`},
	{"withYieldInside", `inner,outer`,
		`function* g() { var x = 'outer'; with ({ x: 'inner' }) { yield x } yield x } [...g()].join()`},
	{"argumentsKept", `3,5,2`,
		`function* g(a, b) { yield arguments.length; a = 5; yield arguments[0]; yield b } [...g(1, 2, 3)].join()`},
	{"closureAcross", `1,2,3,333`,
		`function* g() { var fs = []; for (var i = 0; i < 3; i++) { fs.push(() => i); yield fs.length } yield fs.map(f => f()).join('') } [...g()].join()`},
	{"letClosureAcross", `1,2,3,012`,
		`function* g() { var fs = []; for (let i = 0; i < 3; i++) { fs.push(() => i); yield fs.length } yield fs.map(f => f()).join('') } [...g()].join()`},
	{"forInSnapshot", `a,c`,
		`var o = { a: 1, b: 2, c: 3 }; var out = []; for (var k in o) { out.push(k); delete o.b; o.d = 4 } out.join()`},
	{"forInShadow", `y,z`,
		`var p = { x: 1, y: 2 }; var o = Object.create(p); o.y = 3; o.z = 4; Object.defineProperty(o, 'x', { value: 0, enumerable: false }); var out = []; for (var k in o) out.push(k); out.join()`},
	{"forInArray", `0,1`,
		`var a = [1, 2, 3]; var out = []; for (var k in a) { out.push(k); if (k === '0') a.length = 2 } out.join()`},
	{"forInArrayExtra", `0,1,p`,
		`var a = [1, 2]; a.p = 'q'; var out = []; for (var k in a) out.push(k); out.join()`},
	{"forInNullish", `20`,
		`var n = 0; for (var k in null) n++; for (var k in undefined) n++; for (var k in 5) n++; for (var k in 'ab') n += 10; n`},
	{"forInGenerator", `p,q,r`,
		`function* g(o) { for (var k in o) yield k } [...g({ p: 1, q: 2, r: 3 })].join()`},
	{"forInGeneratorDelete", `pr`,
		`var o = { p: 1, q: 2, r: 3 }; function* g() { for (var k in o) yield k } var it = g(); var a = it.next().value; delete o.q; a + [...it].join()`},
	{"forInBreakNoClose", `a,a,b`,
		`var o = { a: 1, b: 2 }; var out = []; for (var k in o) { out.push(k); break } for (var k in o) out.push(k); out.join()`},
	{"mapForOf", `1a,2b,3c`,
		`var m = new Map([[1, 'a'], [2, 'b']]); var out = []; for (var [k, v] of m) { out.push(k + v); if (k == 1) m.set(3, 'c') } out.join()`},
	{"arrayForOfGrow", `1,2,3,4,5`,
		`var a = [1, 2]; var out = []; for (var x of a) { out.push(x); if (x < 4) a.push(x + 2) } out.join()`},
	{"stringForOf", `1,2,1`,
		`var out = []; for (var c of 'a' + String.fromCharCode(0xd83d, 0xde00) + 'b') out.push(c.length); out.join()`},
	{"asyncAwaitInTry", `1,x,f`,
		`var out = 'pending'; async function f() { var log = []; try { log.push(await 1); throw 'x' } catch (e) { log.push(e) } finally { log.push(await 'f') } return log.join() } f().then(v => out = v); out`},
	{"asyncGenYieldStar", `1,2,3`,
		`var out = 'pending'; async function* ag() { yield* [1, 2]; yield await Promise.resolve(3) } (async () => { var r = []; for await (var v of ag()) r.push(v); out = r.join() })(); out`},
	{"asyncGenReturn", `fin!`,
		`var out = 'pending'; async function* ag() { try { yield 1; yield 2 } finally { out = 'fin' } } (async () => { for await (var v of ag()) break; out += '!' })(); out`},
}

// runGeneratorCases runs generatorCases, each in a runtime of its own that
// mk makes, rounds times in it.
func runGeneratorCases(t *testing.T, mk func() *Runtime, rounds int) {
	t.Helper()
	for _, tt := range generatorCases {
		r := mk()
		p := compileForTest(t, tt.src)
		for round := 0; round < rounds; round++ {
			v, err := r.Run(p)
			if err == nil && len(tt.src) > 19 && tt.src[:19] == "var out = 'pending'" {
				if err = r.runMicrotasks(); err == nil {
					v, err = r.Run(compileForTest(t, "out"))
				}
			}
			if err != nil {
				t.Errorf("%s round %d: %v", tt.name, round, err)
				break
			}
			s, err := r.ToString(v)
			var got string
			if s != nil {
				got = s.Go()
			}
			if err != nil || got != tt.want {
				t.Errorf("%s round %d: got %q, %v; want %q", tt.name, round, got, err, tt.want)
				break
			}
		}
		r.Close()
	}
}

// TestGeneratorProtocol runs generatorCases in the interpreter and in the
// tree tier.
func TestGeneratorProtocol(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		t.Run(map[bool]string{false: "interpreter", true: "tree"}[on], func(t *testing.T) {
			runGeneratorCases(t, func() *Runtime { return New(Config{}) }, 3)
		})
	}
}

// TestResumeResultInRegisters pins that a resumption's result is a struct
// the compiler keeps in registers -- at most four fields and four words,
// Go's limit for one -- rather than in memory, where each resumption had
// stored its fields one by one and read them back whole, a load the
// stores could not be forwarded to.
func TestResumeResultInRegisters(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the limit is a 64-bit platform's: a Value is three words of four bytes")
	}
	typ := reflect.TypeOf(resumeResult{})
	if n, size := typ.NumField(), typ.Size(); n > 4 || size > 4*unsafe.Sizeof(uintptr(0)) {
		t.Errorf("resumeResult has %d fields, %d bytes", n, size)
	}
}

// TestHelperStepsAllocate pins that a pipeline of iterator helpers over a
// generator makes no result object at a step, at any level: a helper's
// step and a generator's are applied directly, by each helper and by the
// loop that drains the last (stepIter), so a pipeline allocates what it
// makes once -- its generator, its helpers, its cursor -- whatever its
// length. Each step had made two result objects a level.
func TestHelperStepsAllocate(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function* count(n) { for (var i = 0; i < n; i++) yield i }
		function pipe() { return count(100).map(x => x * 2).filter(x => x & 2).reduce((a, b) => a + b, 0) }
		function take() { var s = 0; for (var v of count(1000).take(100)) s += v; return s }`)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want Value
		max  float64
	}{
		{"pipe", Int(5000), 8},
		{"take", Int(4950), 6},
	} {
		fn, err := r.Run(compileForTest(t, c.name))
		if err != nil {
			t.Fatal(err)
		}
		n := testing.AllocsPerRun(20, func() {
			v, err := r.call(fn, Undefined, nil)
			if err != nil || !v.SameValue(c.want) {
				t.Fatalf("%s: %v, %v", c.name, v, err)
			}
		})
		if n > c.max {
			t.Errorf("%s: %.0f allocations a call, past %.0f", c.name, n, c.max)
		}
	}
}

// TestGeneratorCloseAllocates pins that closing a generator a loop or a
// destructuring leaves before its end makes nothing: its return is applied
// without the result object it would make (iterReturn), and a return at a
// yield nothing between it and the top of the body sees finishes the
// generator without a frame or the signal a frame is given
// (returnsAtOnce). A loop or a destructuring allocates its generator and
// its cursor; each had made a result object and a return signal as well.
func TestGeneratorCloseAllocates(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function* count(n) { for (var i = 0; i < n; i++) yield i }
		function* pair(a, b) { yield a; yield b }
		function destructure() { var [a] = pair(1, 2); return a }
		function brk() { var s = 0; for (var v of count(10)) { if (v == 3) break; s += v } return s }`)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want Value
	}{
		{"destructure", Int(1)},
		{"brk", Int(3)},
	} {
		fn, err := r.Run(compileForTest(t, c.name))
		if err != nil {
			t.Fatal(err)
		}
		n := testing.AllocsPerRun(20, func() {
			v, err := r.call(fn, Undefined, nil)
			if err != nil || !v.SameValue(c.want) {
				t.Fatalf("%s: %v, %v", c.name, v, err)
			}
		})
		if n > 2 {
			t.Errorf("%s: %.0f allocations a call, past 2", c.name, n)
		}
	}
}

// TestGeneratorReturnClosesCaptures pins that a generator a return
// finishes at a yield, without a frame, detaches the captures of its
// locals as its frame's end would: a closure that outlives it holds the
// binding, not the generator's values.
func TestGeneratorReturnClosesCaptures(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `function* g() { var x = 1; yield () => x++ } var it = g(); var f = it.next().value; it.return(); f`))
	if err != nil {
		t.Fatal(err)
	}
	ups := v.Object().fn().closure.upvalues
	if len(ups) != 1 || ups[0].slot != &ups[0].closed {
		t.Fatalf("the capture is not closed: %+v", ups)
	}
	if got, err := r.call(v, Undefined, nil); err != nil || !got.SameValue(Int(1)) {
		t.Errorf("got %v, %v; want 1", got, err)
	}
}
