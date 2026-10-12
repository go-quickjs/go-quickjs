//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package jit

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// grow recurses n frames of 1 KiB each, to make the goroutine's stack grow
// (and move) under the Go a call from native code runs.
//
//go:noinline
func grow(n int) int {
	var pad [1024]byte
	pad[n%len(pad)] = byte(n)
	if n == 0 {
		return int(pad[0])
	}
	return grow(n-1) + int(pad[n%len(pad)])
}

// Native code calls Go through callGo, and the Go it calls does what Go
// may: grows the goroutine's stack, which moves it, collects garbage, takes
// a traceback through callGo to the Go function that entered the code, and
// writes the result into a keep cell, with its write barrier. The code goes
// on after each call where it said, its context in its register, and
// returns to its Go caller; a call Go refuses says so in GoStatus. A broken
// frame for callGo -- a wrong size, the return address not where the
// runtime looks -- fails the traceback, the stack's growth or the return.
func TestNativeCallsGo(t *testing.T) {
	code, err := callGoProgram(3)
	if err != nil {
		t.Fatal(err)
	}
	a := NewArena()
	placed, err := a.place(code)
	if err != nil {
		t.Skip(err)
	}
	defer a.release(placed)
	calls, frames := 0, 0
	keep := new(int)
	*keep = 7
	old := HostCall
	defer func() { HostCall = old }()
	HostCall = func(ctx *abi.Context) bool {
		calls++
		if grow(64) < 0 {
			return false
		}
		runtime.GC()
		pcs := make([]uintptr, 64)
		n := runtime.Callers(1, pcs)
		found := false
		it := runtime.CallersFrames(pcs[:n])
		for {
			f, more := it.Next()
			frames++
			if strings.HasSuffix(f.Function, ".enterNative") {
				found = true
			}
			if !more {
				break
			}
		}
		if !found {
			t.Errorf("call %d: the traceback does not reach the code's Go caller", calls)
		}
		ctx.Keep[ctx.GoKeep] = abi.Slot{Num: uint64(calls), Ref: unsafe.Pointer(keep)}
		return calls != 2
	}
	ctx := new(abi.Context)
	ctx.GoKeep = 3
	done := make(chan struct{})
	go func() {
		// A goroutine of its own, its stack small, so that it grows.
		defer close(done)
		enterNative(&placed[0], ctx)
	}()
	<-done
	if calls != 3 {
		t.Fatalf("Go was called %d times", calls)
	}
	// The second call was refused: GoStatus 1 once.
	if ctx.Ret != 1 {
		t.Fatalf("statuses added up to %d", ctx.Ret)
	}
	if k := ctx.Keep[3]; k.Num != 3 || k.Ref != unsafe.Pointer(keep) {
		t.Fatalf("keep cell %+v", k)
	}
	if frames < 3 {
		t.Fatalf("traceback of %d frames", frames)
	}
}

// enterNative enters code with ctx, as SSACode.Run does.
//
//go:noinline
func enterNative(code *byte, ctx *abi.Context) {
	enterSSA(code, ctx)
	runtime.KeepAlive(ctx)
}
