package vm

import "testing"

// TestIteratorsClosedBeforeFinally pins that a return, a break or a
// continue leaving a for...of inside a try closes the loop's iterator
// before the try's finally clause runs, as the loop is left before the try
// is, on both tiers; that a failure in the iterator's return method then
// becomes the completion, which a catch around the loop catches and one
// inside it does not; and that a return leaving a finally clause's own loop
// does the same. The answers are Node's.
func TestIteratorsClosedBeforeFinally(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct{ name, src, want string }{
		{"returnThroughFinally", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function f() { try { for (const x of g('in')) return 'v' + x } finally { log.push('finally') } } f() + ' ' + log.join(',')`, "v1 close in,finally"},
		{"breakThroughFinally", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function f() { outer: for (const y of g('out')) { try { for (const x of g('in')) break outer } finally { log.push('finally') } } return log.join(',') } f()`, "close in,finally,close out"},
		{"continueThroughFinally", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function f() { for (const y of [1, 2]) { try { for (const x of g('in' + y)) continue } finally { log.push('finally' + y) } } return log.join(',') } f()`, "close in1,finally1,close in2,finally2"},
		{"closeThrowsOnReturn", `var log = []; function* h() { try { yield 1 } finally { throw 'R' } } function f() { try { try { for (const x of h()) return 1 } finally { log.push('F') } } catch (e) { return 'caught ' + e + ' ' + log.join(',') } } f()`, "caught R F"},
		{"closeErrorCaughtAroundLoop", `var log = []; function* h() { try { yield 1 } finally { throw 'R' } } function f() { try { try { for (const x of h()) return 1 } catch (e) { return 'c' + e } } finally { log.push('F') } } f() + ' ' + log.join(',')`, "cR F"},
		{"closeErrorNotCaughtInsideLoop", `var log = []; function* h() { try { yield 1 } finally { throw 'R' } } function f() { try { for (const x of h()) { try { return 1 } catch (e) { return 'inner ' + e } } } catch (e) { return 'outer ' + e } finally { log.push('F') } } f() + ' ' + log.join(',')`, "outer R F"},
		{"breakCloseErrorCaughtAroundLoop", `function* h() { try { yield 1 } finally { throw 'R' } } function f() { var r = 'none'; out: { try { for (const x of h()) break out } catch (e) { r = 'c' + e } } return r } f()`, "cR"},
		{"returnFromFinallyClause", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function f() { try { try { throw 1 } finally { for (const x of g('in')) return 2 } } finally { log.push('F') } } f() + ' ' + log.join(',')`, "2 close in,F"},
		{"nestedLoopsReturn", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function f() { try { for (const a of g('a')) for (const b of g('b')) return a + b } finally { log.push('F') } } f() + ' ' + log.join(',')`, "2 close b,close a,F"},
		{"generatorReturnThroughFinally", `var log = []; function* g(n) { try { yield 1; yield 2 } finally { log.push('close ' + n) } } function* f() { try { for (const x of g('in')) { yield x; return 'r' } } finally { log.push('F') } } var it = f(); it.next(); var r = it.next(); r.value + r.done + ' ' + log.join(',')`, "rtrue close in,F"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				r := New(Config{})
				v, err := r.Run(compileForTest(t, tt.src))
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				r.Close()
			}
		})
	}
}
