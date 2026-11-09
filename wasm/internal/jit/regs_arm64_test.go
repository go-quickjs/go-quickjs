package jit

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// The code writes no register the execution boundary forbids (SP, FP X29,
// LR X30, the platform's X18 and g, X28), and the registers with a fixed
// use -- the encoder's temporary X27 among them -- are never handed out.
func TestRegisterRules(t *testing.T) {
	forbidden := map[arm64.Reg]bool{18: true, 28: true, 29: true, 30: true, arm64.ZR: true}
	fixed := []arm64.Reg{rCtx, rL, rMem, rMemSize, rPoll, rRS, rTmp, rTmp2, 27}
	for _, r := range fixed {
		if forbidden[r] {
			t.Errorf("fixed register %d is forbidden", r)
		}
	}
	all := append(gprOrder[:], homeGPRs[:]...)
	all = append(all, rCall0, rCall1)
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
	for _, h := range homeGPRs {
		if h == rCall0 || h == rCall1 {
			t.Errorf("home %d is a call sequence's register", h)
		}
	}
	for _, x := range homeXRegs {
		if x == xTmp || x == xTmp2 || int(x) >= numXRegs {
			t.Errorf("float home %d is scratch or past the cache", x)
		}
	}
	if xTmp < numXRegs || xTmp2 < numXRegs {
		t.Error("a float scratch register is the cache's")
	}
}
