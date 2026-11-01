package v8bench

import (
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestStringSwitchMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sum  int
	}{
		{"string_switch_first", 9},
		{"string_switch_last", 144},
		{"string_switch_miss", 0},
		{"string_switch_mixed", 51},
		{"string_switch_built", 51},
		{"string_switch_small", 13},
		{"string_switch_large", 207},
		{"string_switch_prefix", 87},
		{"string_switch_long", 70},
		{"string_switch_typeof", 25},
		{"string_switch_nonstring", 0},
		{"string_switch_nonliteral", 46},
		{"number_switch", 53},
		{"typeof_test", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := rt.Eval(fmt.Sprintf("%s(9) + ':' + global_res", tc.name))
			want := fmt.Sprintf("9:%d", tc.sum)
			if err != nil || v.String() != want {
				t.Fatalf("got %v, %v; want %q", v, err, want)
			}
		})
	}
}

func TestAllocationMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, want string }{
		{`object_literal_fixed(9) + ':' + JSON.stringify(global_res)`, `9:{"a":8,"b":9,"c":"key","d":true,"e":null,"f":8,"g":false,"h":"end"}`},
		{`array_push_large(1) + ':' + global_res`, "8192:16383"},
		{`upvalue_string_append(1025) + ':' + global_res`, "2048:12282"},
	} {
		v, err := rt.Eval(tc.src)
		if err != nil || v.String() != tc.want {
			t.Fatalf("%s = %v, %v; want %q", tc.src, v, err, tc.want)
		}
	}
}
