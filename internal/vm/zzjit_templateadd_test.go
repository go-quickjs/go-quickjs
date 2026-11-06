//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A template's part that is an addition stays converted: that the addition met
// a string once says nothing of what it makes now, and two numbers make a
// number, which the template gave back as it was. A meter sends the addition
// to an exit, whose interpreter converts it, so this takes a runtime without
// one. The answer is Node's.
func TestJITTemplateOfAnAddition(t *testing.T) {
	src := `
function f(a, b) { return ` + "`${a + b}`" + ` }
var last
for (var i = 0; i < 3000; i++) last = f('x', 'y')
for (var i = 0; i < 3000; i++) last = f(1, i);
[typeof last, last, typeof f(1, 2), f(1, 2).length].join()
`
	fn := compileForTest(t, src)
	for _, c := range []jitStressConfig{{}, {threshold: true}, {threshold: true, budget: 3, deopt: 7}} {
		r := New(Config{JIT: true})
		r.jitStress = c
		v, err := r.Run(fn)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "string,3000,string,1" {
			t.Errorf("%+v: %s", c, got)
		}
		r.Close()
		r.ReleaseClosed()
	}
}
