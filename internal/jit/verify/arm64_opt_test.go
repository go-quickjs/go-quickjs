package verify

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// The forms WebAssembly's optimizing tier adds (asm/arm64/advance.go): the
// loads and stores that advance their base, which memory.copy and
// memory.fill loop with, each with every register it takes and the ends of
// its offsets.

func TestARM64WasmOptAdvance(t *testing.T) {
	for r := arm64.Reg(0); r < 31; r++ {
		base := (r + 1) % 31
		for _, imm := range []int32{1, 8, 255, -1, -8, -256} {
			expectA64(t, fmt.Sprintf("ldrb w%d, [x%d],#%d", r, base, imm), func(a *arm64.Asm) { a.LoadPost(r, base, imm, 1) })
			expectA64(t, fmt.Sprintf("ldr x%d, [x%d],#%d", r, base, imm), func(a *arm64.Asm) { a.LoadPost(r, base, imm, 8) })
			expectA64(t, fmt.Sprintf("strb w%d, [x%d],#%d", r, base, imm), func(a *arm64.Asm) { a.StorePost(base, imm, r, 1) })
			expectA64(t, fmt.Sprintf("str x%d, [x%d],#%d", r, base, imm), func(a *arm64.Asm) { a.StorePost(base, imm, r, 8) })
			expectA64(t, fmt.Sprintf("ldrb w%d, [x%d,#%d]!", r, base, imm), func(a *arm64.Asm) { a.LoadPre(r, base, imm, 1) })
			expectA64(t, fmt.Sprintf("ldr x%d, [x%d,#%d]!", r, base, imm), func(a *arm64.Asm) { a.LoadPre(r, base, imm, 8) })
			expectA64(t, fmt.Sprintf("strb w%d, [x%d,#%d]!", r, base, imm), func(a *arm64.Asm) { a.StorePre(base, imm, r, 1) })
			expectA64(t, fmt.Sprintf("str x%d, [x%d,#%d]!", r, base, imm), func(a *arm64.Asm) { a.StorePre(base, imm, r, 8) })
		}
	}
	for q := arm64.FReg(0); q < 32; q++ {
		q2 := (q + 1) % 32
		base := arm64.Reg(q) % 31
		for _, imm := range []int32{16, 32, 1008, -16, -32, -1024} {
			expectA64(t, fmt.Sprintf("ldp q%d, q%d, [x%d],#%d", q, q2, base, imm), func(a *arm64.Asm) { a.LoadPairQPost(q, q2, base, imm) })
			expectA64(t, fmt.Sprintf("stp q%d, q%d, [x%d],#%d", q, q2, base, imm), func(a *arm64.Asm) { a.StorePairQPost(base, imm, q, q2) })
			expectA64(t, fmt.Sprintf("ldp q%d, q%d, [x%d,#%d]!", q, q2, base, imm), func(a *arm64.Asm) { a.LoadPairQPre(q, q2, base, imm) })
			expectA64(t, fmt.Sprintf("stp q%d, q%d, [x%d,#%d]!", q, q2, base, imm), func(a *arm64.Asm) { a.StorePairQPre(base, imm, q, q2) })
		}
	}
	// Offsets the forms cannot hold, and SP as a base, panic.
	for _, f := range []func(a *arm64.Asm){
		func(a *arm64.Asm) { a.LoadPost(1, 2, 256, 1) },
		func(a *arm64.Asm) { a.StorePre(2, -257, 1, 8) },
		func(a *arm64.Asm) { a.LoadPost(1, 2, 8, 4) },
		func(a *arm64.Asm) { a.LoadPairQPost(1, 2, 3, 24) },
		func(a *arm64.Asm) { a.StorePairQPre(3, -1040, 1, 2) },
		func(a *arm64.Asm) { a.StorePairQPost(3, 1024, 1, 2) },
		func(a *arm64.Asm) { a.LoadPre(1, arm64.ZR, 8, 8) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			f(new(arm64.Asm))
		}()
	}
}
