package compile

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// TestLowerRegStringParts checks a template's parts converted where they
// are (to_string, str_part) are StringParts where only the concat reads
// them (regStringParts), and stay conversions where the register is read
// again past it: the stack form's stringParts.
func TestLowerRegStringParts(t *testing.T) {
	fns := regCompiled(t, "function t(i) { return `a${i}b${i * 2}c${\"z\"}` }\n"+
		"function kept(i) { var p = i ? \"q\" : i; var s = `x${p}y`; return s + p }\n")
	count := func(p *ir.Program, op ir.Op) int {
		n := 0
		for _, in := range p.Code {
			if in.Op == op {
				n++
			}
		}
		return n
	}
	p := regLowered(t, fns["t"])
	if n := count(p, ir.StringPart); n != 1 {
		for i, in := range p.Code {
			t.Logf("%d r%d %+v", i, p.Origin[i], in)
		}
		t.Errorf("t: %d StringParts, want 1", n)
	}
	k := regLowered(t, fns["kept"])
	if n := count(k, ir.StringPart); n != 0 {
		t.Errorf("kept: %d StringParts of a register read again, want 0", n)
	}
}
