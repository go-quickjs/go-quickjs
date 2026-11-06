package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// strAppendEvalSrc reads a binding a closure appends to by what the compiler
// cannot see when it compiles the appender: a direct eval in the function and
// in a closure, and a closure a with's body calls. The strings read must stay
// as they were read. internal/vm's TestStrAppend has the rest.
const strAppendEvalSrc = `
const out = []
const long = 'x'.repeat(70)
function hash(s) { let h = 7; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0; return s.length + ':' + h }
// What eval reads, and a with's object.
function evals() {
  var s = long
  const add = x => { s += x }
  add('a'); add('b')
  const t = eval('s')
  add('c'); add('d')
  return hash(t) + hash(s)
}
function evalNested() {
  var s = long
  const add = x => { s += x }
  const peek = () => (0, eval)('1') && eval('s')
  add('a'); add('b')
  const t = peek()
  add('c'); add('d')
  return hash(t) + hash(s)
}
function withs() {
  var s = long
  const add = x => { s += x }
  const o = { }
  add('a'); add('b')
  const t = new Function('o', 'f', 'with (o) { return f() }')(o, () => s)
  add('c')
  return hash(t) + hash(s)
}


for (let r = 0; r < 4; r++) out.push(evals(), evalNested(), withs())
out.join('|')
`

// strAppendEvalWant is Node's answer.
const strAppendEvalWant = "72:-192080021674:946932873|72:-192080021674:946932873|72:-192080021673:584735547|72:-192080021674:946932873|72:-192080021674:946932873|72:-192080021673:584735547|72:-192080021674:946932873|72:-192080021674:946932873|72:-192080021673:584735547|72:-192080021674:946932873|72:-192080021674:946932873|72:-192080021673:584735547"

func TestStrAppendEval(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(strAppendEvalSrc)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != strAppendEvalWant {
		t.Errorf("got\n%s\nwant\n%s", got, strAppendEvalWant)
	}
}
