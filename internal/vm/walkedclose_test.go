package vm

import (
	"strings"
	"testing"
)

// An array a loop or a destructuring walks in place is closed as its
// iterator would be, when it is left before its end: a return method
// %ArrayIteratorPrototype% or what it inherits from has -- none of the
// intrinsics gives it one, a script may -- is looked up, a getter called,
// and the method called with an Array Iterator where the walk had got to;
// a yield* of an array looks its throw and return up as any delegate's.
// Before, a walked array closed nothing, which V8 and the specification's
// IteratorClose do not. Each answer is Node's, on both tiers.
func TestWalkedArrayClose(t *testing.T) {
	defer SetTreeTier(true)
	src := `
		var log = [], out = []
		var AIP = Object.getPrototypeOf([][Symbol.iterator]())
		var IP = Object.getPrototypeOf(AIP)
		function run(label, f) { log = []; try { f() } catch (e) { log.push('threw ' + e.constructor.name) } out.push(label + ': ' + log.join(' ')) }
		run('none', () => { for (const x of [1, 2, 3]) { if (x === 2) break } })
		IP.return = function (...a) { log.push('ret:' + Object.prototype.toString.call(this) + ':' + a.length + ':' + this.next().value); return {} }
		run('break', () => { for (const x of [1, 2, 3]) { if (x === 2) break } })
		run('done', () => { for (const x of [1, 2, 3]) { } })
		run('destructure', () => { const [a] = [1, 2, 3]; log.push('a=' + a) })
		run('destructure all', () => { const [a, b, c] = [1, 2, 3] })
		run('destructure rest', () => { const [a, ...b] = [1, 2, 3] })
		run('throw', () => { for (const x of [1, 2]) throw new Error('t') })
		run('return', () => { (function () { for (const x of [7, 8]) return x })() })
		run('continue outer', () => { outer: for (const y of [1, 2]) { for (const x of [1, 2, 3]) continue outer } })
		run('yield* return', () => { function* g() { yield* [1, 2, 3] } const it = g(); it.next(); log.push(JSON.stringify(it.return(5))) })
		run('yield* throw', () => { function* g() { yield* [1, 2, 3] } const it = g(); it.next(); it.throw(new Error('x')) })
		IP.return = function () { log.push('nonobj'); return 1 }
		run('break nonobj', () => { for (const x of [1, 2, 3]) { break } })
		IP.return = 5
		run('break notcallable', () => { for (const x of [1, 2, 3]) { break } })
		delete IP.return
		Object.defineProperty(Object.prototype, 'return', { get() { log.push('getter'); return undefined }, configurable: true })
		run('getter', () => { for (const x of [1, 2, 3]) { break } })
		delete Object.prototype.return
		AIP.return = function () { log.push('own'); return {} }
		run('own', () => { const [a] = [1, 2] })
		delete AIP.return
		run('deleted', () => { const [a] = [1, 2] })
		out.join('\n')`
	want := strings.Join([]string{
		"none: ",
		"break: ret:[object Array Iterator]:0:3",
		"done: ",
		"destructure: ret:[object Array Iterator]:0:2 a=1",
		"destructure all: ret:[object Array Iterator]:0:undefined",
		"destructure rest: ",
		"throw: ret:[object Array Iterator]:0:2 threw Error",
		"return: ret:[object Array Iterator]:0:8",
		"continue outer: ret:[object Array Iterator]:0:2 ret:[object Array Iterator]:0:2",
		"yield* return: ret:[object Array Iterator]:1:2 {}",
		"yield* throw: ret:[object Array Iterator]:0:2 threw TypeError",
		"break nonobj: nonobj threw TypeError",
		"break notcallable: threw TypeError",
		"getter: getter",
		"own: own",
		"deleted: ",
	}, "\n")
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got\n%s\nwant\n%s", on, got, want)
		}
		r.Close()
	}
}
