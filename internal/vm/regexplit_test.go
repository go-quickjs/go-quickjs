package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Two RegExp literals whose constants share an entry of the runtime's table
// of them (regexpLitSlot): the second, evaluated in a loop after the first
// left a spare RegExp there and was never evaluated again, is given its
// RegExp back after each built-in call too, and evaluates to it next, as a
// literal alone in its entry does. The first's spare had kept the entry
// for good, and the loop allocated a RegExp at every turn -- microbench's
// regexp_test, 33% slower after a test whose literal had happened to land
// there.
func TestRegExpLiteralSharedEntry(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	consts := make([]bytecode.Constant, 4*regexpLitCount)
	var a, b *bytecode.Constant
	for i := range consts {
		for j := i + 1; j < len(consts) && a == nil; j++ {
			if regexpLitSlot(&consts[i]) == regexpLitSlot(&consts[j]) {
				a, b = &consts[i], &consts[j]
			}
		}
	}
	if a == nil {
		t.Fatal("no two constants share an entry")
	}
	*a = bytecode.Constant{Kind: bytecode.ConstRegExp, Str: "a+"}
	*b = bytecode.Constant{Kind: bytecode.ConstRegExp, Str: "b+"}
	v, err := r.newRegExpLiteral(a)
	if err != nil {
		t.Fatal(err)
	}
	r.regexpLiteralDone(a, v.Object())
	var last *Object
	for i := range 3 {
		v, err := r.newRegExpLiteral(b)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && v.Object() != last {
			t.Fatalf("evaluation %d of the second literal made a new RegExp, not the one given back", i)
		}
		last = v.Object()
		r.regexpLiteralDone(b, last)
	}
}
