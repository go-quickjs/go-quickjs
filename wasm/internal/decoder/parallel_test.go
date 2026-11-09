package decoder

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// parallelModule is a module of n functions, each a chain of adds of some
// 900 bytes -- enough bodies to be validated in parallel -- but those of
// odd, whose bodies are odd's.
func parallelModule(t *testing.T, n int, odd map[int]string) []byte {
	t.Helper()
	chain := "(local.get 0)" + strings.Repeat(" (i32.const 1) i32.add", 300)
	var src strings.Builder
	src.WriteString("(module (tag $e)\n")
	for i := range n {
		if body, ok := odd[i]; ok {
			fmt.Fprintf(&src, "(func %s)\n", body)
			continue
		}
		fmt.Fprintf(&src, "(func (param i32) (result i32) %s)\n", chain)
	}
	src.WriteString(")")
	b, err := wat.Compile([]byte(src.String()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestValidateInParallel checks that validating a module's bodies in
// parallel gives the verdict and the error of validating them in order,
// and the same frame bounds: for a valid module, for one with invalid
// bodies, the first in order failing, and for one that mixes legacy
// exception handling and exnref in bodies apart.
func TestValidateInParallel(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(4, runtime.GOMAXPROCS(0))))
	const n = 600
	cases := []struct {
		name string
		odd  map[int]string
	}{
		{"valid", nil},
		{"invalid", map[int]string{
			150: "(result i32) (f32.const 1) (i32.const 1) i32.add",
			500: "(result i32) (i64.const 1)",
		}},
		{"mixed", map[int]string{
			20:  "(try (do nop) (catch $e))",
			580: "(local exnref)",
		}},
	}
	l := CoreLimits()
	for _, tc := range cases {
		b := parallelModule(t, n, tc.odd)
		m1, err := Decode(b, l)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if total := len(b); total < 4*parallelBytes {
			t.Fatalf("%s: a module of %d bytes is not validated in parallel", tc.name, total)
		}
		m2, _ := Decode(b, l)
		got, want := ValidateFunctions(m1, l), validateInOrder(m2, l)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: in parallel %v, in order %v", tc.name, got, want)
		}
		if tc.odd == nil && want != nil {
			t.Errorf("%s: %v", tc.name, want)
		}
		if tc.odd != nil && want == nil {
			t.Errorf("%s: valid", tc.name)
		}
		if want == nil {
			for i := range m1.Functions {
				if m1.Functions[i].Frame != m2.Functions[i].Frame || m1.Functions[i].Frame == (module.FrameBound{}) {
					t.Errorf("%s: function %d: bound %+v in parallel, %+v in order", tc.name, i, m1.Functions[i].Frame, m2.Functions[i].Frame)
				}
			}
		}
	}
}
