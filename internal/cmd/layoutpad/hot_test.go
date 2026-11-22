package main

import "testing"

// A hot function far past its range's anchor is listed, with its range;
// one near its anchor, a cold one, one before every pad and an inlined
// one are not.
func TestHotList(t *testing.T) {
	top := parseTop([]byte("      flat  flat%   sum%        cum   cum%\n" +
		"     5.00s 50.00% 50.00%      5.00s 50.00%  runtime.mallocgc\n" +
		"     2.00s 20.00% 70.00%      2.00s 20.00%  github.com/x/vm.far\n" +
		"     2.00s 20.00% 90.00%      2.00s 20.00%  github.com/x/vm.near\n" +
		"    500ms  5.00% 95.00%      500ms  5.00%  github.com/x/vm.stub\n" +
		"    400ms  4.00% 99.00%      400ms  4.00%  github.com/x/vm.inl (inline)\n" +
		"     10ms  0.10% 99.10%       10ms  0.10%  github.com/x/vm.cold\n"))
	if top["github.com/x/vm.stub"] != 0.5 || top["github.com/x/vm.inl"] != 0 {
		t.Fatalf("parsed %v", top)
	}
	syms := parseNM([]byte("  140001000  64 T runtime.mallocgc\n" +
		"  140010000  32 T " + vmPrefix + "layoutPadQ\n" +
		"  140010020  64 T github.com/x/vm.anchor\n" +
		"  140010100  64 T github.com/x/vm.near\n" +
		"  140014000  64 T github.com/x/vm.cold\n" +
		"  140020000  64 T github.com/x/vm.far\n" +
		"  140030000  64 T github.com/x/vm.stub.abi0\n"))
	c := &config{goos: "windows", goarch: "amd64", anchors: []anchor{{lever: "Q", symbol: "github.com/x/vm.anchor"}}}
	got := hotList(c, syms, top, 0.002, 8192)
	if len(got) != 2 || got[0].name != "github.com/x/vm.far" || got[0].lever != "Q" || got[0].past != 0x20000-0x10020 || got[1].name != "github.com/x/vm.stub" {
		t.Fatalf("hotList = %+v", got)
	}
}
