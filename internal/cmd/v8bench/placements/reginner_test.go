package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// regInner gives the eight placements each count of runReg's filler once,
// and -k 4's four placements four different ones. (Four distinct counts
// cannot sum alike at both phases of every target: the filler is a factor
// varied beside the design, not one balanced within it.)
func TestRegInnerBalanced(t *testing.T) {
	for _, f := range []struct {
		name string
		of   func(int) int
	}{{"regInner", regInner}, {"regTail", regTail}} {
		seen := map[int]bool{}
		for n := range 8 {
			m := f.of(n)
			if m < 0 || m >= regInnerMax || seen[m] {
				t.Fatalf("%s(%d) = %d: not a permutation of 0..%d", f.name, n, m, regInnerMax-1)
			}
			seen[m] = true
			if 3*(regInnerMax+m)+3 > padKeysLen || inner*n > padKeysLen {
				t.Errorf("placement %d reads past padKeys' %d keys", n, padKeysLen)
			}
		}
		four, shift := map[int]bool{}, map[int]bool{}
		for _, n := range placementsOf(4) {
			four[f.of(n)] = true
			shift[regInner(n)+regTail(n)] = true
		}
		if len(four) != 4 {
			t.Errorf("-k 4's placements %v share a count of %s", placementsOf(4), f.name)
		}
		// The tails move against the dispatch too, not by the same
		// count at every placement.
		if len(shift) < 2 {
			t.Errorf("-k 4's placements move the tails against the dispatch alike")
		}
	}
}

// regFill puts the calls in the loop's nop case, and the loop still
// parses; with none it is the loop as it is.
func TestRegFill(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "..", regGo))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(src), regNop) != 1 {
		t.Fatalf("%s has no single case for register nop", regGo)
	}
	if got := regFill(src, 0, 0); got != string(src) {
		t.Error("regFill with no calls changed the loop")
	}
	got := regFill(src, 3, 2)
	for _, call := range []string{"padRegInner(uint32(w.A()), 0)", "padRegInner(uint32(w.A()), 3)", "padRegInner(uint32(w.A()), 6)"} {
		if !strings.Contains(got, regNop+"\t\t\t") || !strings.Contains(got, call) {
			t.Errorf("regFill's loop has no %s after the nop case", call)
		}
	}
	if !strings.Contains(got, regLast+"\t\t\tpadRegInner(uint32(w.A()), 24)") {
		t.Error("regFill's tail filler is not in the last case")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), regGo, got, 0); err != nil {
		t.Errorf("regFill's loop does not parse: %v", err)
	}
	if pad := string(padFile(0, 0, 0, 0)); !strings.Contains(pad, "func padRegInner(a uint32, i int)") {
		t.Error("the first pad has no padRegInner")
	}
}
