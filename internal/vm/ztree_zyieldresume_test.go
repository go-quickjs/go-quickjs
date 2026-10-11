package vm

import "testing"

// TestGeneratorResumption pins what a resumption keeps, in both tiers, now
// that the common one -- next() on a generator its tree suspended, from a
// for...of loop or a call -- is made in one place (resumeTree): a stack
// trace taken in the body, with next's frame under it; resumptions from
// different depths of the stack; an operand stack live across the yield;
// a body's own exception handlers and the closures it made; `with`; the
// generator running when it is resumed again; return and throw after a
// resumption made there; and an async function's awaits and its
// rejections.
func TestGeneratorResumption(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
		async           bool
		// tree says f runs as a tree.
		tree bool
	}{
		{name: "stackTrace", tree: true, src: `function* f() { for (let i = 0; i < 2; i++) yield new Error('e').stack.split('\n').slice(1, 4).map(l => l.trim().split(' ')[1]).join() }
function outer() { const r = []; for (const s of f()) r.push(s); return r.join(';') } outer()`, want: "f,Object.next,outer;f,Object.next,outer"},
		{name: "depths", tree: true, src: `function* f() { for (let i = 0; ; i++) yield i + ':' + new Error().stack.split('\n').length }
var g = f(); function d(n) { return n ? d(n - 1) : g.next().value } [d(0), d(5), d(1), g.next().value].join()`, want: "0:5,1:10,2:6,3:4"},
		{name: "liveStack", tree: true, src: `function* f() { var s = 0; for (let i = 0; i < 3; i++) s += 10 * (yield i) + (yield -i); return s }
var g = f(), out = [], r = g.next(); for (let k = 1; !r.done; k++) { out.push(r.value); r = g.next(k) } out.push(r.value); out.join()`, want: "0,0,1,-1,2,-2,102"},
		{name: "forOfInBody", tree: true, src: `function* f() { for (const x of [1, 2, 3]) for (const y of 'ab') yield x + y }
var s = []; for (const v of f()) s.push(v); s.join()`, want: "1a,1b,2a,2b,3a,3b"},
		{name: "handlers", tree: true, src: `function* f() { var s = ''; for (let i = 0; i < 3; i++) { try { yield i; if (i == 1) throw 'x' } catch (e) { s += 'c' + e } finally { s += 'f' } } return s }
var g = f(), v = []; for (let r = g.next(); ; r = g.next()) { v.push(r.value); if (r.done) break } v.join()`, want: "0,1,2,fcxff"},
		{name: "closures", tree: true, src: `function* f() { const fs = []; for (let i = 0; i < 3; i++) { yield fs.push(() => i * 10) } return fs.map(h => h()).join() }
var g = f(); for (const x of g); g = f(); g.next(); g.next(); g.next(); g.next().value`, want: "0,10,20"},
		{name: "with", src: `function* f(o) { for (var i = 0; i < 2; i++) with (o) { yield x + i; x += 10 } }
var s = []; for (const v of f({ x: 1 })) s.push(v); s.join()`, want: "1,12"},
		{name: "running", tree: true, src: `var g; function* f() { for (let i = 0; i < 2; i++) { yield i; try { g.next() } catch (e) { yield e.message } } }
g = f(); [...g].join()`, want: "0,the generator is already running,1,the generator is already running"},
		{name: "returnAfterResume", tree: true, src: `var s = ''; function* f() { try { for (let i = 0; ; i++) yield i } finally { s += 'F' } }
var g = f(); g.next(); g.next(); g.next(); [g.return(9).value, g.next().done, s].join()`, want: "9,true,F"},
		{name: "throwAfterResume", tree: true, src: `function* f() { for (let i = 0; ; i++) try { yield i } catch (e) { yield 'c' + e } }
var g = f(); g.next(); g.next(); [g.throw('x').value, g.next().value, g.next().value].join()`, want: "cx,2,3"},
		{name: "breakClosesGenerator", tree: true, src: `var s = ''; function* f() { try { for (let i = 0; ; i++) yield i } finally { s += 'F' } }
for (const v of f()) { s += v; if (v == 2) break } s`, want: "012F"},
		{name: "loopThrowsOut", tree: true, src: `function* f() { for (let i = 0; i < 5; i++) { if (i == 2) null.x; yield i } }
var s = ''; try { for (const v of f()) s += v } catch (e) { s += e.name } s`, want: "01TypeError"},
		{name: "await", async: true, tree: true, src: `async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await new Promise(r => r(i)); return s } var p = f(20)`, want: "190"},
		{name: "awaitRejected", async: true, tree: true, src: `async function f() { let s = ''; for (let i = 0; i < 3; i++) { try { s += await (i == 1 ? Promise.reject('r') : i) } catch (e) { s += e } } return s } var p = f()`, want: "0r2"},
		{name: "asyncGenerator", async: true, tree: true, src: `async function* f() { for (let i = 0; i < 3; i++) yield await i } async function main() { let s = ''; for await (const x of f()) s += x; return s } var p = main()`, want: "012"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				src := tt.src
				if tt.async {
					src = "var out = 'pending'; " + src + "; Promise.resolve(p).then(v => out = String(v), e => out = 'E ' + e)"
				}
				fn := compileForTest(t, src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err == nil && tt.async {
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
				if r.frameDepth != 0 {
					t.Errorf("tree %v: left %d frames", on, r.frameDepth)
				}
				// The suspension a tree hands out is the runtime's one,
				// which keeps no value once it is read.
				if !r.treeSig.value.IsUndefined() && r.treeSig.value != (Value{}) {
					t.Errorf("tree %v: the suspension kept %v", on, r.treeSig.value)
				}
				r.Close()
				if on && tt.tree {
					assertTreeIn(t, fn, "f")
				}
			}
		})
	}
}
