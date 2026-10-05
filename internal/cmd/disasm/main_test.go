package main

import (
	"strings"
	"testing"
)

// TestDisasm pins what the disassembly says of a few functions: operands
// decoded by name, the tree tier's verdict and its reason, -func, and the
// source line before its instructions.
func TestDisasm(t *testing.T) {
	src := `var g = 1;
function sum(n) {
	var s = 0;
	for (var j = 0; j < n; j++) s += g;
	return s & 255;
}
function* gen() { yield 1 }
async function run(a) { a[a.length - 1]++; await 0 }
`
	fn, err := compile("t.js", src, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	p := printer{w: &out, lines: splitLines(src), tree: true, src: true}
	p.function(fn, "")
	got := out.String()
	for _, want := range []string{
		"function <main> at 1:1 (params=0 locals=1 stack=",
		"  tree: not built: top-level code\n",
		"  locals: 0:(temp)\n",
		"    function sum at 2:1 (params=1 locals=3 stack=",
		"      tree: built\n",
		"      locals: 0:n 1:s 2:j\n",
		"get_local2           2 0 ; \"j\" \"n\"\n",
		"get_global           0 ; \"g\"\n",
		"inc_local            2 ; \"j\"\n",
		"local_bin_imm        1 bit_and 255 ; \"s\"\n",
		"        ; 4: for (var j = 0; j < n; j++) s += g;\n",
		"    function gen at 7:1 (params=0 locals=0 stack=",
		") generator\n      tree: not built: a generator\n",
		") async\n      tree: not built: an async function\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}

	out.Reset()
	p = printer{w: &out, lines: splitLines(src), only: "gen"}
	p.function(fn, "")
	if got := out.String(); p.printed != 1 || !strings.HasPrefix(got, "function gen at 7:1 ") || strings.Contains(got, "sum") {
		t.Errorf("-func gen printed %d:\n%s", p.printed, got)
	}
}
