package main

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	out := []byte("TEXT example.com/internal/vm.(*Runtime).f(SB) f.go\n" +
		"  f.go:1\t0x1000\t\t48\t\tMOVQ 0x18(SP), CX\t\n" +
		"  f.go:2\t0x1004\t\t90\t\tNOPL\t\n" +
		"  f.go:3\t0x1005\t\t75\t\tJNE 0x100c\t\n" +
		"  f.go:4\t0x1007\t\t48\t\tLEAQ go:string.*+12(SB), AX\t\n" +
		"  f.go:5\t0x100b\t\t90\t\tNOPL 0(AX)(AX*1)\t\n" +
		"  f.go:6\t0x100c\t\tc3\t\tRET\t\n" +
		"  f.go:6\t0x100d\t\tcc\t\tINT $0x3\t\n")
	want := function{"MOVQ 0x18(SP), CX", "JNE @3", "LEAQ DATA(SB), AX", "RET"}
	if got := parse(out)["example.com/internal/vm.(*Runtime).f"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized %q, want %q", got, want)
	}
}
