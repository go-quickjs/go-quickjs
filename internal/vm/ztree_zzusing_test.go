package vm

import "testing"

// TestTreeUsing pins that using and await using declarations give in the
// tree tier what they give in the interpreter, against Node's answers, and
// that the functions run as trees: resources disposed of in reverse, each
// turn of a loop; null and undefined bound and skipped; a value that is not
// disposable a TypeError; a disposal that throws, alone, beside a body that
// threw, which makes a SuppressedError, and beside another; break, continue
// and return out of a scope; and await using's disposals, async, sync and
// of null, rejected, and beside a body that threw. Each case leaves a
// value, or a promise of one, in p.
func TestTreeUsing(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct{ name, src, want string }{
		{"order", `var log = []; function r(n) { return { [Symbol.dispose]() { log.push('d' + n) } } } function f() { let s = 0; for (let i = 0; i < 3; i++) { using a = r('a' + i), b = r('b' + i); s += i; log.push('body' + i) } return s } var p = f() + ' ' + log.join(',')`, "3 body0,db0,da0,body1,db1,da1,body2,db2,da2"},
		{"nullish", `var log = []; function f() { for (let i = 0; i < 2; i++) { using a = null, b = undefined; log.push(i) } return log.join(',') } var p = f()`, "0,1"},
		{"notDisposable", `function f() { for (let i = 0; i < 2; i++) { try { using a = {}; } catch (e) { return e.name } } } var p = f()`, "TypeError"},
		{"suppressed", `var log = []; function f() { for (let i = 0; i < 1; i++) { try { using a = { [Symbol.dispose]() { throw 'D' } }; throw 'B' } catch (e) { return e.name + ':' + e.error + ':' + e.suppressed } } } var p = f()`, "SuppressedError:D:B"},
		{"disposeThrows", `function f() { for (let i = 0; i < 1; i++) { try { using a = { [Symbol.dispose]() { throw 'D' } }; } catch (e) { return 'caught ' + e } } } var p = f()`, "caught D"},
		{"breakContinueReturn", `var log = []; function r(n) { return { [Symbol.dispose]() { log.push('d' + n) } } } function f() { for (let i = 0; i < 5; i++) { using a = r(i); if (i == 1) continue; if (i == 3) break; log.push('b' + i) } for (let j = 0; ; j++) { using b = r('r' + j); if (j == 1) return log.join(',') } } var p = f()`, "b0,d0,d1,b2,d2,d3,dr0"},
		{"twoDisposersBothThrow", `function f() { for (let i = 0; i < 1; i++) { try { using a = { [Symbol.dispose]() { throw 'A' } }, b = { [Symbol.dispose]() { throw 'B' } }; } catch (e) { return e.name + ':' + e.error + ':' + e.suppressed } } } var p = f()`, "SuppressedError:A:B"},
		{"awaitUsing", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } function r(n) { return { async [Symbol.asyncDispose]() { log.push('d' + n) } } } async function f() { for (let i = 0; i < 2; i++) { await using a = r(i); log.push('b' + i) } log.push('end') } var p = f().then(() => log.join(','))`, "b0,d0,b1,d1,end"},
		{"awaitUsingSyncDisposer", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } async function f() { for (let i = 0; i < 1; i++) { await using a = { [Symbol.dispose]() { log.push('d') } }; log.push('b') } log.push('end') } var p = f().then(() => log.join(','))`, "b,d,end"},
		{"awaitUsingNull", `var log = []; function tick(n) { if (n) Promise.resolve().then(() => { log.push('t'); tick(n - 1) }) } async function f() { for (let i = 0; i < 1; i++) { await using a = null; log.push('b') } log.push('end') } var p = f().then(() => log.join(','))`, "b,end"},
		{"awaitUsingRejects", `async function f() { for (let i = 0; i < 1; i++) { try { await using a = { async [Symbol.asyncDispose]() { throw 'D' } }; } catch (e) { return 'caught ' + e } } } var p = f()`, "caught D"},
		{"awaitUsingBodyThrows", `async function f() { for (let i = 0; i < 1; i++) { try { await using a = { async [Symbol.asyncDispose]() { throw 'D' } }; throw 'B' } catch (e) { return e.name + ':' + e.error + ':' + e.suppressed } } } var p = f()`, "SuppressedError:D:B"},
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
