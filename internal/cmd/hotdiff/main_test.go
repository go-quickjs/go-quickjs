package main

import (
	"reflect"
	"strings"
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
	bin := parse(out)
	if got := bin.funcs["example.com/internal/vm.(*Runtime).f"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized %q, want %q", got, want)
	}
	if a := bin.addr["example.com/internal/vm.(*Runtime).f"]; a != 0x1000 {
		t.Fatalf("address %#x, want 0x1000", a)
	}
}

func TestFindABI0(t *testing.T) {
	funcs := map[string]function{
		"example.com/internal/vm.nativeEnter.abi0":     {"RET"},
		"example.com/internal/vm.(*Runtime).nativeRun": {"NOP"},
	}
	if got := find(funcs, "internal/vm.nativeEnter"); !reflect.DeepEqual(got, function{"RET"}) {
		t.Fatalf("assembly function found as %q", got)
	}
	if got := find(funcs, "internal/vm.nativeExit"); got != nil {
		t.Fatalf("absent function found as %q", got)
	}
}

// -parity passes a hot function within its slack at the same offset, and
// fails one placed elsewhere modulo 4 KB, or a hook's slow function past
// its slack.
func TestParityCheck(t *testing.T) {
	fn := func(n int) function {
		f := make(function, n)
		for i := range f {
			f[i] = "NOP"
		}
		return f
	}
	base := &binary{funcs: map[string]function{}, addr: map[string]uint64{}}
	head := &binary{funcs: map[string]function{}, addr: map[string]uint64{}}
	sym := func(name string) string { return "example.com/" + name }
	for i, name := range append(append([]string(nil), hot...), paritySlow...) {
		base.funcs[sym(name)], head.funcs[sym(name)] = fn(10), fn(10)
		base.addr[sym(name)] = 0x401000 + uint64(i)*0x40
		head.addr[sym(name)] = 0x801000 + uint64(i)*0x40
	}
	head.funcs[sym("internal/vm.rhCallW")] = fn(10 + parityHotSlack["internal/vm.rhCallW"])
	if lines, failed := parityCheck(base, head, 96); failed {
		t.Fatalf("failed within the slack:\n%s", strings.Join(lines, "\n"))
	}
	head.addr[sym("internal/vm.(*Runtime).runReg")] += 0x20
	if _, failed := parityCheck(base, head, 96); !failed {
		t.Fatal("passed runReg 32 bytes on")
	}
	head.addr[sym("internal/vm.(*Runtime).runReg")] -= 0x20
	base.funcs[sym("internal/vm.rsSub")], head.funcs[sym("internal/vm.rsSub")] = fn(100), fn(197)
	if _, failed := parityCheck(base, head, 96); !failed {
		t.Fatal("passed a slow function 97 instructions longer")
	}
}
