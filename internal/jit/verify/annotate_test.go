//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// With mir.Annotate set, amd64's code says what each stretch of it was
// emitted for (mir.Code's Marks), at the offsets the code has once its
// jumps are shortened: every mark at an instruction's start, in order,
// within the instructions, and one for every value given code -- what
// jitprof maps a profile's samples by. Without it, there are none.
func TestMirAnnotated(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000}
	defer func() { mir.Annotate = false }()
	n := 0
	for _, src := range jsCorpus {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, globals{})
			if err != nil {
				continue
			}
			f.CallEntry = true
			ssa.Optimize(f)
			mir.Annotate = false
			plain, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			if plain.Marks != nil || plain.Listing != "" {
				t.Fatalf("%s (%d): marks without Annotate", src, i)
			}
			mir.Annotate = true
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			// The same instructions: not the same bytes, which hold the
			// addresses of what each compile makes (its exits').
			if a, b := ops(plain), ops(mc); a != b {
				t.Fatalf("%s (%d): Annotate changed the code:\n%s\n%s", src, i, a, b)
			}
			if err := checkMarks(f, mc); err != nil {
				t.Fatalf("%s (%d): %v\n%s", src, i, err, mc.Listing)
			}
			n++
		}
	}
	if n < 20 {
		t.Fatalf("only %d functions compiled", n)
	}
}

// ops is c's instructions' operations, in order.
func ops(c *mir.Code) string {
	var out []byte
	for off := 0; off < c.Text; {
		in, err := x86asm.Decode(c.Bytes[off:c.Text], 64)
		if err != nil {
			return fmt.Sprintf("undecodable at %#x", off)
		}
		out = append(out, in.Op.String()...)
		out = append(out, ' ')
		off += in.Len
	}
	return string(out)
}

func checkMarks(f *ssa.Func, mc *mir.Code) error {
	starts := map[int]bool{}
	for off := 0; off < mc.Text; {
		in, err := x86asm.Decode(mc.Bytes[off:mc.Text], 64)
		if err != nil {
			return fmt.Errorf("at %#x: %v", off, err)
		}
		starts[off] = true
		off += in.Len
	}
	tags := map[string]bool{}
	last := -1
	for _, m := range mc.Marks {
		if m.Off < last {
			return fmt.Errorf("mark %q at %#x before the last's %#x", m.Tag, m.Off, last)
		}
		last = m.Off
		if m.Off < mc.Text && !starts[m.Off] {
			return fmt.Errorf("mark %q at %#x, inside an instruction", m.Tag, m.Off)
		}
		if m.Off > mc.Text || m.Off == mc.Text && m.Tag != "constants" {
			return fmt.Errorf("mark %q at %#x, past the instructions (%#x)", m.Tag, m.Off, mc.Text)
		}
		tags[m.Tag] = true
	}
	if mc.Listing == "" {
		return fmt.Errorf("no listing")
	}
	// The values given code: each has its mark, but a phi, or a value a
	// use makes itself (lazy), which the backend never emits apart.
	marked, values := 0, 0
	for _, b := range f.Blocks {
		if !tags[fmt.Sprintf("b%d", b.ID)] {
			continue // a block the backend dropped
		}
		for _, v := range b.Values {
			if v.Op == ssa.OpPhi {
				continue
			}
			values++
			if tags[fmt.Sprintf("v%d %s", v.ID, v.Op)] {
				marked++
			}
		}
	}
	if values > 0 && marked == 0 {
		return fmt.Errorf("none of %d values marked", values)
	}
	return nil
}
