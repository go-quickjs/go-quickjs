package vm

import "testing"

// A loop's test that goes back to the loop's head by itself, where it goes on
// to a later test by another way -- if (a) continue; if (b) break; -- was
// taken for a way out of the loop. So a loop nested in another stopped at its
// first continue, and went on after it as if it had finished: the outer loop
// has one thing to go on to from it, and asks no result. Alone in a function,
// the loop gave back undefined or a number left on the stack, or panicked. A
// for loop's continue goes to its update, not to its head, and had none of
// this. Each answer is Node's.
func TestLoopContinueBeforeALaterTest(t *testing.T) {
	t.Cleanup(func() { SetTreeTier(true) })
	const src = `
const out = []
function forIn(o) {
  for (const k in o) { if (k === 'a') continue; if (k === 'b') continue; return 'r' + k }
  return 'done'
}
function forOf(o) {
  for (const k of o) { if (k === 'a') continue; if (k === 'b') continue; return 'r' + k }
  return 'done'
}
function forLoop(o) {
  for (let i = 0; i < 2; i++) { if (o === 'a') continue; if (o === 'b') continue; return 'r' + i }
  return 'done'
}
function whileLoop(o) {
  let i = 0
  while (i++ < 2) { if (o === 'a') continue; if (o === 'b') continue; return 'r' + i }
  return 'done'
}
function doLoop(o) {
  let i = 0
  do { if (o === 'a') continue; if (o === 'b') continue; return 'r' + i } while (i++ < 2)
  return 'done'
}
function three(o) {
  for (const k of o) { if (k === 'a') continue; if (k === 'b') continue; if (k === 'c') continue; return 'r' + k }
  return 'done'
}
function nothingAfter(o) {
  for (const k of o) { if (k === 'a') continue; if (k === 'b') continue; return 'r' + k }
}
function nestedOf(o) {
  const log = []
  for (let i = 0; i < 2; i++) {
    for (const y of o) { if (y === 1) continue; if (y === 9) break; log.push(y) }
    log.push('e')
  }
  return log.join('')
}
function nestedIn(o) {
  const log = []
  for (let i = 0; i < 2; i++) {
    for (const y in o) { if (y === 'a') continue; if (y === 'q') break; log.push(y) }
    log.push('e')
  }
  return log.join('')
}
function nestedWhile() {
  const log = []
  let i = 0
  while (i++ < 2) {
    let y = 0
    while (y++ < 3) { if (y === 1) continue; if (y === 9) break; log.push(y) }
    log.push('e')
  }
  return log.join('')
}
function nestedMap(m) {
  const log = []
  for (let i = 0; i < 2; i++) {
    for (const [k, v] of m) { if (k === 1) continue; if (k === 9) break; log.push(v) }
    log.push('e')
  }
  return log.join('')
}
function* numbers(log) { log.push('a'); yield 1; log.push('b'); yield 2; log.push('c') }
function nestedGenerator() {
  const log = []
  for (let i = 0; i < 2; i++) {
    for (const y of numbers(log)) { if (i === 1) continue; if (i === 9) break }
    log.push('e')
  }
  return log.join('')
}
for (let round = 0; round < 3; round++) {
  out.push(forIn({ a: 1 }), forIn({ b: 1 }), forIn({ c: 1 }), forIn({ a: 1, b: 2, c: 3 }), forIn({ a: 1, b: 2 }))
  out.push(forOf(['a']), forOf(['b']), forOf(['c']), forOf(['a', 'b', 'c']), forOf(['a', 'b']))
  out.push(forLoop('a'), forLoop('b'), forLoop('c'))
  out.push(whileLoop('a'), whileLoop('b'), whileLoop('c'))
  out.push(doLoop('a'), doLoop('b'), doLoop('c'))
  out.push(three(['a']), three(['b', 'c']), three(['c', 'd']))
  out.push(String(nothingAfter(['a'])), nothingAfter(['a', 'c']))
  out.push(nestedOf([1, 2, 3]), nestedIn({ a: 1, b: 2, c: 3 }), nestedWhile())
  out.push(nestedMap(new Map([[1, 1], [2, 2], [3, 3]])), nestedGenerator())
}
out.join()
`
	const one = "done,done,rc,rc,done,done,done,rc,rc,done,done,done,r0,done,done,r1,done,done,r0,done,done,rd,undefined,rc,23e23e,bcebce,23e23e,23e23e,abceabce"
	want := one + "," + one + "," + one
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s", tree, got)
		}
		r.Close()
	}
}
