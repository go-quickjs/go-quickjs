//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package jit

import (
	"errors"
	"math"
	"runtime"
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// valueCode is the code of a function that returns v; a long one runs
// through n bytes of no-ops first, so that it spans pages.
func valueCode(t *testing.T, v float64, n int) *mir.Code {
	t.Helper()
	p := &ir.Program{Code: []ir.Instruction{{Op: ir.Return, Left: ir.Literal(ir.Float(v))}}, Maps: make([]ir.StateMap, 1)}
	f, err := ssa.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := mir.Compile(f, testEncoding)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return mc
	}
	nop := []byte{0x90}
	if runtime.GOARCH == "arm64" {
		nop = []byte{0x1f, 0x20, 0x03, 0xd5}
	}
	var sled []byte
	for len(sled) < n {
		sled = append(sled, nop...)
	}
	// Entries before the sled run through it.
	mc.Bytes = append(sled, mc.Bytes...)
	for pc, off := range mc.Entries {
		if off != 0 {
			mc.Entries[pc] = off + len(sled)
		}
	}
	return mc
}

// Code past MaxCodeBytes is refused as the function's own (ErrCodeSize), not
// as the host's failure to place code (ErrUnavailable), which has a runtime
// place no more: the arena places the next function's code, and runs it.
func TestCodePastMaxCodeBytes(t *testing.T) {
	a := NewArena()
	big := valueCode(t, 1, MaxCodeBytes)
	if len(big.Bytes) <= MaxCodeBytes {
		t.Fatalf("%d bytes of code, not past %d", len(big.Bytes), MaxCodeBytes)
	}
	if c, err := NewSSACode(a, big); c != nil || !errors.Is(err, ErrCodeSize) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewSSACode(%d bytes) = %v, %v", len(big.Bytes), c, err)
	}
	c, err := NewSSACode(a, valueCode(t, 2, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var ctx abi.Context
	ctx.SetConsts(&testEncoding)
	if err := c.Run(0, &ctx); err != nil || ctx.ExitKind != abi.ExitReturn || ctx.Ret != math.Float64bits(2) {
		t.Fatalf("the code placed after returned %+v, %v", ctx.Ret, err)
	}
}

// An arena packs a runtime's functions into a few mappings: each placed
// after the last, a long one across pages, all running, and running still
// as others next to them are released and more are placed, which opens
// and seals the pages they share. Releasing the last unmaps everything.
func TestArenaPacksCode(t *testing.T) {
	a := NewArena()
	var codes []*SSACode
	place := func(i int) {
		n := 0
		if i%50 == 25 {
			n = 3 * chunkBytes / 4
		}
		c, err := NewSSACode(a, valueCode(t, float64(i), n))
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	runAll := func(when string) {
		t.Helper()
		for i, c := range codes {
			if c.Size() == 0 {
				continue
			}
			var ctx abi.Context
			ctx.SetConsts(&testEncoding)
			if err := c.Run(0, &ctx); err != nil || ctx.ExitKind != abi.ExitReturn || ctx.Ret != math.Float64bits(float64(i)) {
				t.Fatalf("%s: code %d returned %+v, %v", when, i, ctx.Ret, err)
			}
		}
	}
	bytes := func() (n int) {
		for _, c := range codes {
			n += c.Size()
		}
		return n
	}
	for i := 0; i < 200; i++ {
		place(i)
	}
	if want := (bytes()+chunkBytes-1)/chunkBytes + 1; a.Chunks() > want {
		t.Fatalf("%d bytes of code in %d mappings, want at most %d", bytes(), a.Chunks(), want)
	}
	if next := uintptr(unsafe.Pointer(&codes[1].code[0])) - uintptr(unsafe.Pointer(&codes[0].code[0])); next < uintptr(codes[0].Size()) || next >= uintptr(codes[0].Size())+codeAlign {
		t.Fatalf("the second function is %d bytes after the first, which takes %d", next, codes[0].Size())
	}
	runAll("placed")
	for i := 1; i < len(codes); i += 2 {
		if err := codes[i].Close(); err != nil {
			t.Fatal(err)
		}
	}
	runAll("half released")
	for i := 200; i < 300; i++ {
		place(i)
	}
	runAll("placed among released")
	for _, c := range codes {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if a.Chunks() != 0 {
		t.Fatalf("an empty arena holds %d mappings", a.Chunks())
	}
	// An emptied arena maps again.
	codes = codes[:0]
	place(0)
	runAll("placed again")
	if err := codes[0].Close(); err != nil || a.Chunks() != 0 {
		t.Fatalf("released: %v, %d mappings", err, a.Chunks())
	}
}

// With a layout seed, an arena varies where code lands past its first
// offset, as placements has a benchmark do (LayoutSeed): each chunk after
// the first begins within 4 KiB of its start, and each function placed
// after a chunk's first follows the one before by 0 to 3 cache lines more
// than its size. Two seeds lay the same code out differently, and every
// function runs wherever it is.
func TestArenaLayoutSeed(t *testing.T) {
	defer func(was uint64) { LayoutSeed = was }(LayoutSeed)
	layout := func(seed uint64) []int {
		LayoutSeed = seed
		a := NewArena()
		var codes []*SSACode
		defer func() {
			for _, c := range codes {
				c.Close()
			}
		}()
		var offsets []int
		gaps := map[int]bool{}
		for i := 0; i < 300; i++ {
			n := 0
			if i%40 == 20 {
				n = chunkBytes / 2
			}
			c, err := NewSSACode(a, valueCode(t, float64(i), n))
			if err != nil {
				t.Fatal(err)
			}
			codes = append(codes, c)
			at := uintptr(unsafe.Pointer(&c.code[0]))
			for k, ch := range a.chunks {
				base := uintptr(unsafe.Pointer(unsafe.SliceData(ch.mem)))
				if at < base || at >= base+uintptr(len(ch.mem)) {
					continue
				}
				off := int(at - base)
				offsets = append(offsets, off)
				if len(codes) > 1 {
					prev := codes[len(codes)-2]
					pat := uintptr(unsafe.Pointer(&prev.code[0]))
					if pat >= base && pat < at {
						gap := int(at-pat) - prev.Size()
						if gap < 0 || gap >= layoutGapLines*codeAlign || gap%codeAlign != 0 {
							t.Fatalf("seed %d: function %d is %d bytes past the one before", seed, i, gap)
						}
						gaps[gap] = true
					} else if k > 0 && off >= layoutChunkSpan {
						t.Fatalf("seed %d: chunk %d's first code at %d", seed, k, off)
					}
				}
			}
		}
		if len(gaps) < 2 {
			t.Fatalf("seed %d: one gap between functions, %v", seed, gaps)
		}
		if a.Chunks() < 3 {
			t.Fatalf("seed %d: %d chunks", seed, a.Chunks())
		}
		for i, c := range codes {
			var ctx abi.Context
			ctx.SetConsts(&testEncoding)
			if err := c.Run(0, &ctx); err != nil || ctx.ExitKind != abi.ExitReturn || ctx.Ret != math.Float64bits(float64(i)) {
				t.Fatalf("seed %d: code %d returned %+v, %v", seed, i, ctx.Ret, err)
			}
		}
		return offsets
	}
	one, two := layout(1), layout(2)
	if slices.Equal(one, two) {
		t.Fatal("two seeds laid the code out alike")
	}
}
