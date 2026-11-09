package jit

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The code writes no register the execution boundary forbids (SP, BP and
// R14, g), and the registers with a fixed use are never handed out.
func TestRegisterRules(t *testing.T) {
	forbidden := map[amd64.Reg]bool{amd64.RSP: true, amd64.RBP: true, amd64.R14: true}
	fixed := []amd64.Reg{rCtx, rL, rMem, rPoll, rRS, rTmp}
	for _, r := range fixed {
		if forbidden[r] {
			t.Errorf("fixed register %d is forbidden", r)
		}
	}
	all := append(gprOrder[:], homeGPRs[:]...)
	for _, r := range all {
		if forbidden[r] {
			t.Errorf("cached register %d is forbidden", r)
		}
		for _, f := range fixed {
			if r == f {
				t.Errorf("register %d is both cached and fixed", r)
			}
		}
	}
	for _, x := range homeXRegs {
		if x == xTmp || int(x) >= numXRegs {
			t.Errorf("SSE home %d is scratch or past the cache", x)
		}
	}
}
