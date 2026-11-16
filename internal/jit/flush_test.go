//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || ((darwin || linux) && arm64))

package jit

import (
	"encoding/binary"
	"os"
	"runtime"
	"testing"
	"unsafe"
)

// storeConst is a function, entered by enterRaw, that stores v to the word
// its context points at and returns: arm64's movz x1, #v; str x1, [x0];
// ret, amd64's mov qword [rdi], v; ret.
func storeConst(v uint16) []byte {
	if runtime.GOARCH == "arm64" {
		var b []byte
		for _, w := range []uint32{0xD2800001 | uint32(v)<<5, 0xF9000001, 0xD65F03C0} {
			b = binary.LittleEndian.AppendUint32(b, w)
		}
		return b
	}
	return append(binary.LittleEndian.AppendUint32([]byte{0x48, 0xC7, 0x07}, uint32(v)), 0xC3)
}

// TestRewrittenCodeRuns writes code at one address again and again, each
// time a function storing another constant, and runs it after each write:
// a write whose instructions instruction fetch did not see -- a cache not
// cleaned to the point of unification, or one not invalidated, where the
// flush leaves a line out or misreads the line sizes (flushCode) -- runs
// the function before it and stores its constant. The code is placed
// across a page's end where pages are smaller than a chunk, so that the
// flush covers two pages.
func TestRewrittenCodeRuns(t *testing.T) {
	a := NewArena()
	page := os.Getpagesize()
	// Fill the first chunk to just before a page's end, so that the code
	// placed next straddles it.
	pad := make([]byte, page-2*codeAlign)
	for i := range pad {
		pad[i] = 0xCC
	}
	if _, err := a.place(pad); err != nil {
		t.Fatal(err)
	}
	body := append(storeConst(0), make([]byte, 3*codeAlign)...)
	code, err := a.place(body)
	if err != nil {
		t.Fatal(err)
	}
	c := a.chunks[len(a.chunks)-1]
	start := int(uintptr(unsafe.Pointer(&code[0])) - uintptr(unsafe.Pointer(&c.mem[0])))
	pages := c.mem[start/page*page : min(alignUp(start+len(code), page), len(c.mem))]
	var out uint64
	for i := 1; i <= 4000; i++ {
		// The function moves along the allocation eight bytes a turn, so
		// that each of its lines is written at every offset.
		at := (i * 8) % (len(code) - 16)
		instr := make([]byte, len(code))
		copy(instr[at:], storeConst(uint16(i)))
		// Its first instruction a jump to it, where it does not start at
		// the allocation's start: arm64's b, amd64's jmp rel32.
		if at != 0 {
			if runtime.GOARCH == "arm64" {
				binary.LittleEndian.PutUint32(instr, 0x14000000|uint32(at/4))
			} else {
				instr[0] = 0xE9
				binary.LittleEndian.PutUint32(instr[1:], uint32(at-5))
			}
		}
		if err := a.write(c, pages, code, instr); err != nil {
			t.Fatal(err)
		}
		enterRaw(&code[0], unsafe.Pointer(&out))
		if out != uint64(i) {
			t.Fatalf("write %d: the code stored %d: instruction fetch ran code written before", i, out)
		}
	}
}
