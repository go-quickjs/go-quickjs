//go:build quickjs_jit && !android && !ios && amd64

package jit

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

func mappingAt(t *testing.T, address uint64) string {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		startText, endText, ok := strings.Cut(fields[0], "-")
		if !ok {
			t.Fatalf("invalid mapping: %s", line)
		}
		start, startErr := strconv.ParseUint(startText, 16, 64)
		end, endErr := strconv.ParseUint(endText, 16, 64)
		if startErr != nil || endErr != nil {
			t.Fatalf("invalid mapping: %s", line)
		}
		if address >= start && address < end {
			return fields[1]
		}
	}
	return ""
}

func TestLinuxExecutableProtectionAndRelease(t *testing.T) {
	loop, err := NewSSACode(nil, valueCode(t, 42, 0))
	if err != nil {
		t.Fatal(err)
	}
	address := uint64(uintptr(unsafe.Pointer(&loop.code[0])))
	if permissions := mappingAt(t, address); permissions != "r-xp" {
		t.Fatalf("published code has permissions %q, want r-xp", permissions)
	}
	if err := loop.Close(); err != nil {
		t.Fatal(err)
	}
	if permissions := mappingAt(t, address); permissions != "" {
		t.Fatalf("released code is still mapped: %s", permissions)
	}
}

// Code an arena packs into one chunk is executable and not writable; the
// chunk stays mapped while any of it is owned, and is unmapped with the
// last.
func TestLinuxArenaProtectionAndRelease(t *testing.T) {
	a := NewArena()
	var codes []*SSACode
	for i := 0; i < 3; i++ {
		c, err := NewSSACode(a, valueCode(t, float64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
	}
	if a.Chunks() != 1 {
		t.Fatalf("three functions in %d mappings", a.Chunks())
	}
	for i, c := range codes {
		for _, at := range []int{0, len(c.code) - 1} {
			if permissions := mappingAt(t, uint64(uintptr(unsafe.Pointer(&c.code[at])))); permissions != "r-xp" {
				t.Fatalf("code %d has permissions %q, want r-xp", i, permissions)
			}
		}
	}
	last := codes[2]
	address := uint64(uintptr(unsafe.Pointer(&last.code[0])))
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
	if permissions := mappingAt(t, address); permissions != "" || a.Chunks() != 0 {
		t.Fatalf("released arena: %q, %d mappings", permissions, a.Chunks())
	}
}
