package main

import (
	"strings"
	"testing"
)

// TestDisasm pins what the disassembly says of a few functions: register
// code's operands decoded by name, -func, and the source line before its
// instructions.
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
	p := printer{w: &out, lines: splitLines(src), src: true}
	p.function(fn, "")
	got := out.String()
	for _, want := range []string{
		"function <main> at 1:1 (params=0 locals=1) top level\n",
		"  locals: 0:(temp)\n",
		"  register code: 18 instructions, 5 registers\n",
		"    function sum at 2:1 (params=1 locals=3)\n",
		"      locals: 0:n 1:s 2:j\n",
		"         3  get_global r5, name0 @2\n",
		"         5  step_jlt r4 += 1 (update), r2 -> 3\n",
		"         7  and_imm r5, r3, 255\n",
		"            ; 4: for (var j = 0; j < n; j++) s += g;\n",
		"    function gen at 7:1 (params=0 locals=0) generator\n",
		"         3  yield r3, r2\n",
		") async\n",
		"         3  add_index_imm.1 r2[r3], 1\n",
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

func TestJITReport(t *testing.T) {
	src := `function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }
function read(o) { return o.x }`
	fn, err := compile("t.js", src, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	p := printer{w: &out, jit: true}
	p.function(fn, "")
	for _, want := range []string{
		// Top-level code is compiled for its loops, which this has none of.
		"top level\n  jit IR: refused: jit: host operations without native loop or array work",
		"jit IR: eligible (this tool does not compile or execute native code)",
		"jit slots: 6",
		"jit exit: pc=5",
		"jit IR: refused: jit: host operations without native loop or array work",
		"jit callee IR: eligible for native callers and inlining",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "jit exit: pc=9") {
		t.Errorf("reported unreachable trailing return:\n%s", out.String())
	}
}
