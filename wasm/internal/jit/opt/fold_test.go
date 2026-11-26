package opt

import "testing"

// An eqz of an eqz of x, and of the eqz widened to an i64 -- Go's code's
// test of a bool it made -- is x != 0: one comparison, no eqz left.
func TestEqzOfEqz(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"i32", `(i32.eqz (i32.eqz (local.get 0)))`},
		{"i64", `(i64.eqz (i64.extend_i32_u (i64.eqz (local.get 1))))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildText(t, `(module (func (param i32 i64) (result i32) `+tc.body+`))`, 0)
			eqz, ne := 0, 0
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					switch v.Op {
					case OpEqz:
						eqz++
					case OpNe:
						ne++
					}
				}
			}
			if eqz != 0 || ne != 1 {
				t.Errorf("%d eqz and %d ne, want 0 and 1\n%v", eqz, ne, f)
			}
		})
	}
}
