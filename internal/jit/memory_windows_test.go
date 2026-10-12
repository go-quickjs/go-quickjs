//go:build quickjs_jit && !android && !ios && amd64

package jit

import (
	"math"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"golang.org/x/sys/windows"
)

func TestWindowsExecutableProtectionAndRelease(t *testing.T) {
	loop, err := NewSSACode(nil, valueCode(t, 42, 0))
	if err != nil {
		t.Fatal(err)
	}
	address := uintptr(unsafe.Pointer(&loop.code[0]))
	var info windows.MemoryBasicInformation
	if err := windows.VirtualQuery(address, &info, unsafe.Sizeof(info)); err != nil {
		t.Fatal(err)
	}
	if info.Protect != windows.PAGE_EXECUTE_READ || info.State != windows.MEM_COMMIT {
		t.Fatalf("published code: protection %#x, state %#x", info.Protect, info.State)
	}
	if err := loop.Close(); err != nil {
		t.Fatal(err)
	}
	if err := windows.VirtualQuery(address, &info, unsafe.Sizeof(info)); err != nil {
		t.Fatal(err)
	}
	const memFree = 0x10000 // MEMORY_BASIC_INFORMATION.State's MEM_FREE value.
	if info.State != memFree {
		t.Fatalf("released code has state %#x, want MEM_FREE", info.State)
	}
}

// Code an arena packs into one chunk is executable and not writable, page by
// page; the chunk stays committed while any of it is owned, and is freed
// with the last.
func TestWindowsArenaProtectionAndRelease(t *testing.T) {
	a := NewArena()
	var codes []*SSACode
	for i := 0; i < 3; i++ {
		c, err := NewSSACode(a, valueCode(t, float64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	query := func(c *SSACode) windows.MemoryBasicInformation {
		var info windows.MemoryBasicInformation
		if err := windows.VirtualQuery(uintptr(unsafe.Pointer(&c.code[0])), &info, unsafe.Sizeof(info)); err != nil {
			t.Fatal(err)
		}
		return info
	}
	if a.Chunks() != 1 {
		t.Fatalf("three functions in %d mappings", a.Chunks())
	}
	for i, c := range codes {
		if info := query(c); info.Protect != windows.PAGE_EXECUTE_READ || info.State != windows.MEM_COMMIT {
			t.Fatalf("code %d: protection %#x, state %#x", i, info.Protect, info.State)
		}
	}
	last := codes[2]
	address := uintptr(unsafe.Pointer(&last.code[0]))
	for _, c := range codes[:2] {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var ctx abi.Context
	ctx.SetConsts(&testEncoding)
	if err := last.Run(0, &ctx); err != nil || ctx.Ret != math.Float64bits(2) {
		t.Fatalf("code beside released code: %v, %v", ctx.Ret, err)
	}
	if err := last.Close(); err != nil {
		t.Fatal(err)
	}
	var info windows.MemoryBasicInformation
	if err := windows.VirtualQuery(address, &info, unsafe.Sizeof(info)); err != nil {
		t.Fatal(err)
	}
	const memFree = 0x10000
	if info.State != memFree || a.Chunks() != 0 {
		t.Fatalf("released arena: state %#x, %d mappings", info.State, a.Chunks())
	}
}
