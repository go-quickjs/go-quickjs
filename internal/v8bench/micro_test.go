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
