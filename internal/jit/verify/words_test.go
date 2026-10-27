//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// wordCorpus compares values' words with the kinds' -- objects' and
// strings' found through where they came from (mir's reference) -- in
// strict and loose equality, typeof, truth and property reads.
var wordCorpus = []string{
	`function f(o,p,n){let s=0;for(let i=0;i<n;i++){if(o.a===p.a)s++;if(o.b==p.b)s+=2;if(o.c)s+=3}return s}`,
	`function f(o,k){if(typeof o.x==='undefined')return 1;if(o.y==null)return 2;return o.z===k?3:4}`,
	`function f(o){return o.a.b.c+o.d.e}`,
}

// A word is compared with a kind's in one instruction, the constant read
// from the context's table where it is one, not loaded into a register
// first (mir's cmpWord); and a value whose word was just compared with
// the kind's is not compared again before its reference is read
// (knownReference): x === y of two objects compares x's word, once, and
// y's none, the two words being one.
func TestMirWordsComparedOnce(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, String: 0xFFF8000000000009, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		CallGo: 0x72000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	stretches := 0
	for _, src := range wordCorpus {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, props{p: p})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			for k, m := range mc.Marks {
				tag := strings.Fields(m.Tag)
				if len(tag) != 2 || tag[0][0] != 'v' {
					continue
				}
				switch tag[1] {
				case "eqtagged", "objectof", "truth", "loosenullish", "typeis":
				default:
					continue
				}
				stretches++
				ins := stretch(mc, k)
				for j := 0; j+1 < len(ins); j++ {
					mov, cmp := ins[j], ins[j+1]
					if mov.Op != x86asm.MOV || cmp.Op != x86asm.CMP {
						continue
					}
					r, ok := mov.Args[0].(x86asm.Reg)
					m, isMem := mov.Args[1].(x86asm.Mem)
					if ok && isMem && (m.Base == x86asm.RDI || m.Base == x86asm.RIP) && cmp.Args[1] == r {
						t.Errorf("%s (%d): %s loads a constant to compare a word with: %v; %v", src, i, m, mov, cmp)
					}
				}
			}
		}
	}
	if stretches == 0 {
		t.Fatal("the corpus compares no words")
	}
}
