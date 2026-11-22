package main

import "testing"

// An anchor whose page was lost, or whose lever is forced, is recorded as
// many pages on as its pad's reserve needs, and every anchor after it by
// as much; one whose pad has room, or that kept its page, is left alone.
func TestReservePages(t *testing.T) {
	syms := parseNM([]byte("  140001000  64 T github.com/x/vm.first\n" +
		"  140003000  64 T github.com/x/vm.second\n" +
		"  140006000  64 T github.com/x/vm.third\n"))
	c := &config{goos: "windows", goarch: "amd64", anchors: []anchor{
		{lever: "A", symbol: "github.com/x/vm.first", at: 0x140001000},
		{lever: "XJ", symbol: "github.com/x/vm.second", at: 0x140003000},
		{lever: "Q", symbol: "github.com/x/vm.third", at: 0x140005000},
	}}
	// XJ keeps its page but is forced: 100 stores are 700 bytes of room,
	// 7,492 short of 8 KB, two pages. Q lost its page and has none.
	v := map[string]int{"A": 0, "XJ": 100, "Q": 0}
	if !reservePages(c, syms, v, map[string]bool{"XJ": true}) {
		t.Fatal("nothing moved")
	}
	want := []uint64{0x140001000, 0x140003000 + 2*codeMod, 0x140006000 + 3*codeMod}
	for i, a := range c.anchors {
		if a.at != want[i] {
			t.Fatalf("anchor %s at %#x, want %#x", a.lever, a.at, want[i])
		}
	}
	// With room enough, nothing moves.
	c.anchors[1].at, c.anchors[2].at = 0x140003000, 0x140006000
	if reservePages(c, syms, map[string]int{"XJ": 1200, "Q": 300}, map[string]bool{"XJ": true}) {
		t.Fatal("moved with room")
	}
}
