//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// fuseCorpus compares in every way a branch tests -- numbers, any two
// values, null and undefined, ??, typeof, truthiness, each through ! too --
// every comparison used by its branch alone. (One made once and tested by
// several branches, as the redundancy elimination leaves truthiness, is a
// boolean, which each tests.)
var fuseCorpus = []string{
	`function f(a,b,n){let s=0;for(let i=0;i<n;i++){const x=a[i],y=b[i];if(x<y)s++;if(x<=y)s+=2;if(x>y)s+=3;if(x>=y)s+=4;if(x==y)s+=5;if(x!=y)s+=6;if(!(x<y))s+=7}return s}`,
	`function f(x,y){let n=0;while(y!==null){if(x===null)return n;x=x.next;y=y.next;n++}return -n}`,
	`function f(o,n){let s=0;for(let i=0;i<n;i++){if(o===undefined)s++;if(o!==undefined)s+=2;if(o==null)s+=3;if(o!=null)s+=4;if((o??1)===1)s+=5}return s}`,
	`function f(x,y,n){let s=0;for(let i=0;i<n;i++){if(x===y)s++;if(x!==y)s+=2;if(x==y)s+=3;if(x!=y)s+=4}return s}`,
	`function f(x,n){let s=0;for(let i=0;i<n;i++){if(typeof x==="number")s++;if(typeof x!=="string")s+=2;if(typeof x==="boolean")s+=3;if(typeof x==="undefined")s+=4;if(typeof x==="function")s+=5}return s}`,
	`function f(x,y,z,n){let s=0;for(let i=0;i<n;i++){if(x)s++;if(!y)s+=2;while(z&&s<n)s++}return s}`,
}

// fuseBigCorpus is BigInts compared by their branches, as bigs has them.
var fuseBigCorpus = []string{
	`function f(a,b){let c=0;for(let i=0;i<9;i++){if(a<b)c++;if(a>=b)c+=2;if(!(a>b))c+=3;a=a+1n}return c}`,
}

// A comparison or a test only its branch uses is compared by the branch
// (mir's findFused): no boolean is made of it -- no SETcc on amd64, no CSET
// on arm64 -- in any function of the corpora, for every kind of comparison
// the branches test; while one kept for later is still made. The List
// benchmark's loop, x === null and y !== null, is a compare with null's word
// and a conditional jump each.
func TestMirFusedBranches(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000}
	tested := map[ssa.Op]int{}
	compile := func(name string, p *ir.Program, fb ssa.Feedback) map[string]int {
		f, err := ssa.BuildWith(p, fb)
		if err != nil {
			return nil
		}
		f.CallEntry = true
		ssa.Optimize(f)
		for _, b := range f.Blocks {
			if b.Kind != ssa.BlockIf {
				continue
			}
			v := b.Control
			for v.Op == ssa.OpNot {
				tested[v.Op]++
				v = v.Args[0]
			}
			tested[v.Op]++
		}
		sets := map[string]int{}
		for _, arch := range arches {
			compile := mir.CompileAMD64
			if arch == "arm64" {
				compile = mir.CompileARM64
			}
			mc, err := compile(f, enc)
			if err != nil {
				t.Fatalf("%s, %s: %v", name, arch, err)
			}
			n, err := setBools(arch, mc.Bytes[:mc.Text])
			if err != nil {
				t.Fatalf("%s, %s: %v", name, arch, err)
			}
			sets[arch] = n
		}
		return sets
	}
	n := 0
	for _, src := range fuseCorpus {
		for i, p := range lowered(t, src) {
			for _, fb := range []ssa.Feedback{nil, globals{}} {
				name := fmt.Sprintf("%s (%d, %T)", src, i, fb)
				sets := compile(name, p, fb)
				if sets == nil {
					continue
				}
				n++
				if sets["amd64"] != 0 || sets["arm64"] != 0 {
					t.Errorf("%s: booleans made: %d SETcc on amd64, %d CSET on arm64", name, sets["amd64"], sets["arm64"])
				}
			}
		}
	}
	for _, src := range fuseBigCorpus {
		for i, p := range lowered(t, src) {
			name := fmt.Sprintf("%s (%d, bigs)", src, i)
			sets := compile(name, p, bigs{p: p})
			if sets == nil {
				continue
			}
			n++
			if sets["amd64"] != 0 || sets["arm64"] != 0 {
				t.Errorf("%s: booleans made: %d SETcc on amd64, %d CSET on arm64", name, sets["amd64"], sets["arm64"])
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d functions compiled", n)
	}
	for _, op := range []ssa.Op{ssa.OpCmpF64, ssa.OpCmpI64, ssa.OpStrictNullish, ssa.OpLooseNullish, ssa.OpEqTagged, ssa.OpTypeIs, ssa.OpTruth, ssa.OpNot} {
		if tested[op] == 0 {
			t.Errorf("no branch on %v compiled", op)
		}
	}
	// Kept for later, a comparison is a boolean still.
	for i, p := range lowered(t, `function f(a,b){let s=0;for(let i=0;i<a.length;i++){const c=a[i]<b[i],d=a[i]===null;if(c)s++;if(d)s+=2;s+=c+d}return s}`) {
		sets := compile(fmt.Sprintf("kept (%d)", i), p, globals{})
		if sets != nil && (sets["amd64"] == 0 || sets["arm64"] == 0) {
			t.Errorf("kept (%d): no boolean made: %v", i, sets)
		}
	}

	// The List benchmark's isShorterThan: each test a compare and a jump.
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	checked := 0
	for i, p := range lowered(t, `function f(x,y){while(y!==null){if(x===null)return true;x=x.next;y=y.next}return false}`) {
		f, err := ssa.BuildWith(p, globals{})
		if err != nil {
			continue
		}
		ssa.Optimize(f)
		mc, err := mir.CompileAMD64(f, enc)
		if err != nil {
			t.Fatal(err)
		}
		for k, m := range mc.Marks {
			if !strings.HasSuffix(m.Tag, " strictnullish") || strings.HasPrefix(m.Tag, "cold ") {
				// A cold path appended at the test: an edge out of the
				// loop (mir's leaves), whose stretch is its own.
				continue
			}
			end := mc.Text
			if k+1 < len(mc.Marks) {
				end = mc.Marks[k+1].Off
			}
			var ops []string
			for off := m.Off; off < end; {
				in, err := x86asm.Decode(mc.Bytes[off:end], 64)
				if err != nil {
					t.Fatalf("(%d) at %#x: %v", i, off, err)
				}
				ops = append(ops, in.Op.String())
				off += in.Len
			}
			if len(ops) != 2 || ops[0] != "CMP" || !strings.HasPrefix(ops[1], "J") {
				t.Errorf("(%d) %s: %v, not a compare and a jump\n%s", i, m.Tag, ops, mc.Listing)
			}
			checked++
		}
	}
	if checked < 2 {
		t.Fatalf("%d of the loop's tests checked", checked)
	}
}

// setBools counts the instructions in code that make a boolean of a
// condition: SETcc on amd64, CSET on arm64.
func setBools(arch string, code []byte) (int, error) {
	n := 0
	switch arch {
	case "amd64":
		for pc := 0; pc < len(code); {
			in, err := x86asm.Decode(code[pc:], 64)
			if err != nil {
				return 0, fmt.Errorf("offset %#x: %v", pc, err)
			}
			if strings.HasPrefix(in.Op.String(), "SET") {
				n++
			}
			pc += in.Len
		}
	case "arm64":
		for pc := 0; pc+4 <= len(code); pc += 4 {
			in, err := arm64asm.Decode(code[pc:])
			if err != nil {
				return 0, fmt.Errorf("offset %#x: %v", pc, err)
			}
			if in.Op == arm64asm.CSET {
				n++
			}
		}
	}
	return n, nil
}
