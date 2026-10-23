//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// moveCorpus is loops whose values go round through more than one phi: a
// try statement's body has an edge of its own to the update, as
// microbench_extra.js's try_catch has; loops of several variables; for...of
// loops whose steps Go makes (stepping).
var moveCorpus = []string{
	`function f(n){var i,s=0;for(i=0;i<n;i++){try{s=(s+i)|0}catch(e){s=0}}return s}`,
	`function f(n){let a=0,b=1,c=2;for(let i=0;i<n;i++){const t=a;a=b;b=c;c=(t+b)|0}return a+b+c}`,
	`function f(n){let s=0,t=0;for(let i=0;i<n;i++){if(i&1){s+=i}else{t+=i}}return s-t}`,
	`function f(x,y){let a=x,b=y;while(b!==null){if(a===null)return true;a=a.next;b=b.next}return false}`,
	`function f(a,n){let s=0;for(let j=0;j<n;j++){for(const x of a)s+=x}return s}`,
	// microbench.js's array_write.
	`function array_write(n){var tab,len,i,j;tab=[];len=10;for(i=0;i<len;i++)tab[i]=i;for(j=0;j<n;j++){tab[0]=j;tab[1]=j;tab[2]=j;tab[3]=j;tab[4]=j;tab[5]=j;tab[6]=j;tab[7]=j;tab[8]=j;tab[9]=j}return len*n}`,
	// microbench.js's array_for_of.
	`var global_res;function array_for_of(n){var r,i,j,sum,len=100;r=[];for(i=0;i<len;i++)r[i]=i;for(j=0;j<n;j++){sum=0;for(i of r){sum+=i}global_res=sum}return n*len}`,
}

// stepping is props whose loops' steps Go makes from native code (a call
// of Go, ssa.CallSite's Step), as the VM's for...of loops' over arrays.
type stepping struct{ props }

func (stepping) GoCalls() bool { return true }

// edgeMoves counts the register-to-register moves in the code's stretches
// for edges (mir's phiMoves), and in those of edges into a loop's header.
func edgeMoves(f *ssa.Func, mc *mir.Code) (all, loops int) {
	headers := map[string]bool{}
	for _, b := range f.Blocks {
		if b.LoopHeader {
			headers[fmt.Sprintf("b%d", b.ID)] = true
		}
	}
	for k, m := range mc.Marks {
		if !strings.HasPrefix(m.Tag, "edge ") {
			continue
		}
		end := mc.Text
		if k+1 < len(mc.Marks) {
			end = min(end, mc.Marks[k+1].Off)
		}
		to := m.Tag[strings.Index(m.Tag, "->")+2:]
		for _, in := range insts(mc.Bytes, m.Off, end) {
			_, r0 := in.Args[0].(x86asm.Reg)
			_, r1 := in.Args[1].(x86asm.Reg)
			if (in.Op == x86asm.MOV || in.Op == x86asm.MOVAPD) && r0 && r1 {
				all++
				if headers[to] {
					loops++
				}
			}
		}
	}
	return all, loops
}

// A phi and its arguments are coalesced where their intervals do not meet
// (mir's allocate: group), so that a value carried round a loop through two
// phis -- try_catch's sum, through the update's block and the header --
// keeps one register through them: the edges' moves drop, with no more
// spills than the allocator made without the groups (it spills by how many
// registers are free, never by which).
func TestMirPhisCoalesced(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	total, loops, spills := 0, 0, 0
	for _, src := range append(append(moveCorpus, jsCorpus...), keepCorpus...) {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, stepping{props{p: p}})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			a, l := edgeMoves(f, mc)
			total, loops = total+a, loops+l
			if src == moveCorpus[0] || src == moveCorpus[3] {
				// try_catch's loop, and List's walk: the sum and the
				// counter, or the two tails, each one move at most.
				if a > 2 {
					t.Errorf("%s (%d): %d moves on its edges, not 2", src, i, a)
				}
			}
			if strings.HasPrefix(src, "function array_write") && a > 6 {
				// Many groups for few registers: the allocation with the
				// hints alone, or the strict groups', where the loose
				// groups' had put 27 moves on its edges (allocateBest).
				t.Errorf("%s (%d): %d moves on its edges, not 6", src, i, a)
			}
			if src == moveCorpus[len(moveCorpus)-1] && a > 3 {
				// array_for_of: two groups, its sum's and its iterator's,
				// that waited for one register had put the iterator through
				// a move on both its inner loop's edges (5).
				t.Errorf("%s (%d): %d moves on its edges, not 3", src, i, a)
			}
			spills += strings.Count(mc.Locations(), "spill")
			if testing.Verbose() {
				t.Logf("%3d edge moves, %3d into headers, %2d spilled: %s (%d)", a, l, strings.Count(mc.Locations(), "spill"), src, i)
			}
		}
	}
	t.Logf("%d edge moves, %d into loop headers, %d values spilled", total, loops, spills)
	// Without the groups, at 77173866: 507 edge moves, 283 into headers,
	// 37 values spilled.
	if total > 160 || loops > 130 || spills > 37 {
		t.Errorf("%d edge moves, %d into loop headers, %d values spilled: want at most 160, 130, 37", total, loops, spills)
	}
}
