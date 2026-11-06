//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// A number boxed for a captured local's cell is in no frame state, so that
// nothing but a use of the box keeps what it boxes. An operation may make the
// box again of its operand, and a Go call's argument was what had taken that
// register since: ” + s here, a call of Go, made "0" and "1" of NaN, the loop
// counter's bit. A meter sends that call to an exit, which reads the cell, so
// that this takes a runtime without one.
func TestJITBoxOperandAtGoCall(t *testing.T) {
	const src = `
const keep = []
function f() {
  var s = 'not a number';
  let other;
  const object = {};
  function captures() { return s }
  for (let i = 0; i < 8; i++) {
    for (const k in { a: 1, b: 2 }) {
      s++;
      (i & 1) !== 0 && (other = object);
      keep.push('' + s);
    }
  }
  return captures()
}
f()
keep.join()
`
	want := strings.TrimSuffix(strings.Repeat("NaN,", 16), ",")
	fn := compileForTest(t, src)
	for _, c := range []jitStressConfig{{threshold: true}, {threshold: true, budget: 1}, {threshold: true, budget: 3, deopt: 7}} {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = c
		v, err := r.Run(fn)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("%+v: %s", c, got)
		}
		r.Close()
		r.ReleaseClosed()
	}
}
