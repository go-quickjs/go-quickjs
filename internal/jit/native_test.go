//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package jit

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math"
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

func sameFloat(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b) || math.IsNaN(a) && math.IsNaN(b)
}

func TestNativeInvalidCodeSize(t *testing.T) {
	a := NewArena()
	for _, instructions := range [][]byte{nil, make([]byte, MaxCodeBytes+1)} {
		code, err := a.place(instructions)
		if code != nil || err == nil {
			t.Fatalf("place(%d bytes) = %v, %v", len(instructions), code, err)
		}
	}
	if a.Chunks() != 0 {
		t.Fatalf("refused code left %d mappings", a.Chunks())
	}
}

// runValue runs code that returns a number, and reports it.
func runValue(c *SSACode) (float64, error) {
	var ctx abi.Context
	ctx.SetConsts(&testEncoding)
	if err := c.Run(0, &ctx); err != nil {
		return 0, err
	}
	if ctx.ExitKind != abi.ExitReturn {
		return 0, errors.New("the code did not return")
	}
	return math.Float64frombits(ctx.Ret), nil
}

// This can run alone in a hardened executable. Allocation denial must be an
// ordinary, identifiable refusal, before any attempt to enter native code.
func TestExecutableMemoryPolicy(t *testing.T) {
	c, err := NewSSACode(nil, valueCode(t, 42, 0))
	if err != nil {
		if c != nil || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unavailable executable memory: %v, %v", c, err)
		}
		t.Logf("backend refused safely: %v", err)
		return
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if os.Getenv("QUICKJS_EXPECT_JIT_DENIED") != "" {
		t.Fatal("executable-memory policy unexpectedly allowed native code")
	}
	if v, err := runValue(c); err != nil || v != 42 {
		t.Fatalf("allowed native code: %v, %v", v, err)
	}
}

// Functions differing only in a constant, placed and released in turn, each
// in an arena of its own, exercise instruction-cache publication when the OS
// reuses freed addresses.
func TestNativeReusedAllocations(t *testing.T) {
	for i := 0; i < 128; i++ {
		c, err := NewSSACode(nil, valueCode(t, float64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
		v, runErr := runValue(c)
		closeErr := c.Close()
		if runErr != nil || closeErr != nil || v != float64(i) {
			t.Fatalf("allocation %d: %v, %v, %v", i, v, runErr, closeErr)
		}
	}
}

// A CPU profile taken while native code runs is one the profiler can write:
// its signals land in code with no Go frame information.
func TestNativeCPUProfile(t *testing.T) {
	c, err := NewSSACode(nil, valueCode(t, 7, 3*chunkBytes/4))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var profile bytes.Buffer
	if err := pprof.StartCPUProfile(&profile); err != nil {
		t.Fatal(err)
	}
	func() {
		defer pprof.StopCPUProfile()
		deadline := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(deadline) {
			if v, err := runValue(c); err != nil || v != 7 {
				t.Fatalf("profiled code: %v, %v", v, err)
			}
		}
	}()
	zr, err := gzip.NewReader(bytes.NewReader(profile.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil || len(data) == 0 {
		t.Fatalf("CPU profile: %d bytes, %v", len(data), err)
	}
}
