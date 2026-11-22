package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The equality functions are the text's type:.eq.* symbols, sorted, each
// once; a list is written back as it was read.
func TestEqLists(t *testing.T) {
	syms := parseNM([]byte("  140001dc0         64 T type:.eq.M20K4M8\n" +
		"  140002f80        128 T type:.eq.SM10\n" +
		"  140003000        128 T type:.eq.[8SM10K6]\n" +
		"  140004000         32 T github.com/x/vm.f\n" +
		"  140005000          8 R type:.eq.notcode\n"))
	got := eqOf(syms)
	if want := []string{"type:.eq.M20K4M8", "type:.eq.SM10", "type:.eq.[8SM10K6]"}; !slices.Equal(got, want) {
		t.Fatalf("eqOf = %q, want %q", got, want)
	}
	tg := &targets{configs: []*config{{goos: "windows", goarch: "amd64"}, {goos: "linux", goarch: "amd64", jit: true}}}
	path := filepath.Join(t.TempDir(), "layouteq.txt")
	l := eqLists{"windows/amd64 plain": got, "linux/amd64 jit": {"type:.eq.SM10"}}
	if err := writeEq(path, tg, l); err != nil {
		t.Fatal(err)
	}
	back, err := readEq(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range l {
		if !slices.Equal(back[k], v) {
			t.Fatalf("%s read back as %q, written %q", k, back[k], v)
		}
	}
	if none, err := readEq(filepath.Join(t.TempDir(), "absent.txt")); err != nil || len(none) != 0 {
		t.Fatalf("an absent file: %v, %v", none, err)
	}
}

// A check fails on an equality function the list has not, names where it
// is, and only reports one the build no longer has.
func TestEqCheck(t *testing.T) {
	c := &config{goos: "windows", goarch: "amd64"}
	syms := parseNM([]byte("  140001000         64 T github.com/x/vm.before\n" +
		"  140001040         64 T type:.eq.NEW\n" +
		"  140001080         64 T type:.eq.OLD\n"))
	added, removed := eqDiff([]string{"type:.eq.OLD", "type:.eq.GONE"}, eqOf(syms))
	if !slices.Equal(added, []string{"type:.eq.NEW"}) || !slices.Equal(removed, []string{"type:.eq.GONE"}) {
		t.Fatalf("added %q, removed %q", added, removed)
	}
	lines, failed := eqCheck(c, syms, eqLists{c.String(): {"type:.eq.NEW", "type:.eq.OLD", "type:.eq.GONE"}})
	if failed || len(lines) != 1 || !strings.Contains(lines[0], "type:.eq.GONE") {
		t.Fatalf("a removed one: %v %q", failed, lines)
	}
	if lines, failed := eqCheck(c, syms, eqLists{}); !failed || len(lines) != 1 || !strings.Contains(lines[0], "-eq") {
		t.Fatalf("no list: %v %q", failed, lines)
	}
	if got := symBefore(syms, 0x140001080); got != "github.com/x/vm.before" {
		t.Fatalf("symBefore = %s", got)
	}
}
