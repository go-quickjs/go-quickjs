package vm

import "testing"

// TestForAwaitClose pins how a for await is left, on both tiers, against
// Node's answers: a break, a continue of an outer loop, a return and a
// throw close its iterator as AsyncIteratorClose does -- its return method
// called and what that returns awaited, which the ticker shows taking a
// turn, and checked to be an object but on a throw, which a failure in
// closing does not replace -- while a loop its iterator ends, or whose
// next, its rejection or its result's done fails, is not closed; a
// synchronous iterator driven asynchronously is closed through the
// wrapper the spec gives it, which answers even without a return method;
// an iterator of a for...of inside closes first; and a return or a throw
// injected into an async generator suspended in the loop's body closes it.
// Each case leaves a promise in p, and out is what that settles to.
func TestForAwaitClose(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct{ name, src, want string }{
		{"asyncBreak", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { tick(8); for await (const x of it) break; log.push('after') } var p = f().then(() => log.join(''))`, "tRtaftert"},
		{"asyncBreakNoReturn", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) } }; async function f() { tick(8); for await (const x of it) break; log.push('after') } var p = f().then(() => log.join(''))`, "taftert"},
		{"asyncReturnNonObject", `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { return 1 } }; async function f() { for await (const x of it) return 'r' } var p = f().then(v => v, e => e.name)`, "TypeError"},
		{"asyncThrowReturnFails", `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { throw 'R' } }; async function f() { for await (const x of it) throw 'T' } var p = f().then(v => v, e => e)`, "T"},
		{"asyncThrowReturnRejects", `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { return Promise.reject('R') } }; async function f() { for await (const x of it) throw 'T' } var p = f().then(v => v, e => e)`, "T"},
		{"asyncBreakReturnFails", `var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { throw 'R' } }; async function f() { try { for await (const x of it) break } catch (e) { return 'c' + e } } var p = f()`, "cR"},
		{"exhaustedNotClosed", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, i: 0, next() { return Promise.resolve({ value: this.i, done: this.i++ > 1 }) }, return() { log.push('R'); return {} } }; async function f() { let s = 0; for await (const x of it) s += x; return s + log.join('') } var p = f()`, "1"},
		{"nextThrowsNotClosed", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { throw 'N' }, return() { log.push('R'); return {} } }; async function f() { try { for await (const x of it); } catch (e) { return e + log.join('') } } var p = f()`, "N"},
		{"nextRejectsNotClosed", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.reject('N') }, return() { log.push('R'); return {} } }; async function f() { try { for await (const x of it); } catch (e) { return e + log.join('') } } var p = f()`, "N"},
		{"doneGetterThrowsNotClosed", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return { get done() { throw 'D' } } }, return() { log.push('R'); return {} } }; async function f() { try { for await (const x of it); } catch (e) { return e + log.join('') } } var p = f()`, "D"},
		{"syncBreak", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } function* g() { try { yield 1; yield 2 } finally { log.push('F') } } async function f() { tick(8); for await (const x of g()) break; log.push('after') } var p = f().then(() => log.join(''))`, "ttFttaftert"},
		{"syncBreakNoReturn", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } async function f() { tick(8); for await (const x of [1, 2]) break; log.push('after') } var p = f().then(() => log.join(''))`, "tttaftert"},
		{"syncReturnNonObject", `var it = { [Symbol.iterator]() { return this }, next() { return { value: 1, done: false } }, return() { return 1 } }; async function f() { for await (const x of it) break; return 'no' } var p = f().then(v => v, e => e.name)`, "TypeError"},
		{"nestedForOfBreak", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { outer: for await (const x of it) for (const y of g('in')) break outer; } var p = f().then(() => log.join(','))`, "close in,R"},
		{"nestedForOfReturn", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { for await (const x of it) for (const y of g('in')) return 'v'; } var p = f().then(v => v + ' ' + log.join(','))`, "v close in,R"},
		{"continueOuter", `var log = []; var it = () => ({ [Symbol.asyncIterator]() { return this }, i: 0, next() { return Promise.resolve({ value: this.i, done: this.i++ > 0 }) }, return() { log.push('R'); return Promise.resolve({}) } }); async function f() { outer: for (const a of [1, 2]) { for await (const x of it()) continue outer; } return log.join(',') } var p = f()`, "R,R"},
		{"continueInner", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, i: 0, next() { return Promise.resolve({ value: this.i, done: this.i++ > 2 }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { let s = 0; for await (const x of it) { if (x == 1) continue; s += x } return s + log.join('') } var p = f()`, "2"},
		{"finallyAroundLoop", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { try { for await (const x of it) return 'v' } finally { log.push('F') } } var p = f().then(v => v + log.join(''))`, "vRF"},
		{"asyncGeneratorReturnInBody", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function* f() { for await (const x of it) yield x } var g = f(); var p = g.next().then(() => g.return('q')).then(r => r.value + r.done + log.join(''))`, "qtrueR"},
		{"asyncGeneratorThrowInBody", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: 1, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function* f() { for await (const x of it) yield x } var g = f(); var p = g.next().then(() => g.throw('T')).then(r => 'no', e => e + log.join(''))`, "TR"},
		{"destructuringThrows", `var log = []; var it = { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve({ value: null, done: false }) }, return() { log.push('R'); return Promise.resolve({}) } }; async function f() { try { for await (const { a } of it); } catch (e) { return e.name + log.join('') } } var p = f()`, "TypeErrorR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				src := "var out = 'pending'; " + tt.src + "; Promise.resolve(p).then(v => out = String(v), e => out = 'E ' + e)"
				fn := compileForTest(t, src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err == nil {
					if err = r.runMicrotasks(); err == nil {
						v, err = r.Run(compileForTest(t, "out"))
					}
				}
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
				if on {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}
