//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package jit

import (
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// storeRefPairs is how many pointers storeRefProgram moves at an entry:
// GoArgs' words, two for each.
const storeRefPairs = abi.MaxGoArgs / 2

type storeRefObj struct {
	id  int
	pad [7]int
}

type storeRefHolder struct{ p unsafe.Pointer }

// Native code that moves objects allocated before the collector marked, out
// of holders allocated then too, into a container allocated while it marks
// -- black, never scanned in that cycle -- through storeRef, keeps every one:
// the write barrier shades the pointer it stores, and the one it clears,
// which the collector may not have found yet. A raw store loses those it had
// not (QUICKJS_STOREREF_RAW=1 shows it: the objects' weak pointers are
// cleared while the container holds them, or the collector's check fails).
// The test runs again in a process of its own with GODEBUG=gccheckmark=1,
// where the collector checks after each cycle that it marked everything
// reachable.
func TestStoreRefShades(t *testing.T) {
	raw := os.Getenv("QUICKJS_STOREREF_RAW") == "1"
	storeRefMoves(t, raw)
	if os.Getenv("QUICKJS_STOREREF_CHILD") != "" || testing.Short() {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreRefShades$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_STOREREF_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}

func storeRefMoves(t *testing.T, raw bool) {
	code, err := storeRefProgram(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := NewArena()
	placed, err := a.place(code)
	if err != nil {
		t.Skip(err)
	}
	defer a.release(placed)
	const n = 1 << 16
	holders := make([]*storeRefHolder, n)
	weaks := make([]weak.Pointer[storeRefObj], n)
	for i := range holders {
		o := &storeRefObj{id: i}
		holders[i] = &storeRefHolder{p: unsafe.Pointer(o)}
		weaks[i] = weak.Make(o)
	}
	runtime.GC()
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var sink [][]byte
		for !stop.Load() {
			for range 1000 {
				sink = append(sink, make([]byte, 64))
			}
			sink = sink[:0]
			runtime.GC()
		}
	}()
	flag := (*uint32)(unsafe.Pointer(&writeBarrier))
	slots := make([]*unsafe.Pointer, n)
	ctx := new(abi.Context)
	ctx.SetConsts(&testEncoding)
	cycles := 0
	for moved := 0; moved < n; {
		for atomic.LoadUint32(flag)&0xff == 0 {
			runtime.Gosched()
		}
		cycles++
		box := make([]unsafe.Pointer, n)
		for moved < n && atomic.LoadUint32(flag)&0xff != 0 {
			for k := range storeRefPairs {
				slots[moved+k] = &box[moved+k]
				ctx.GoArgs[2*k].Num = uint64(uintptr(unsafe.Pointer(&box[moved+k])))
				ctx.GoArgs[2*k+1].Num = uint64(uintptr(unsafe.Pointer(&holders[moved+k].p)))
			}
			enterNative(&placed[0], ctx)
			moved += storeRefPairs
		}
		runtime.KeepAlive(box)
	}
	stop.Store(true)
	<-done
	runtime.GC()
	runtime.GC()
	lost := 0
	for i, slot := range slots {
		if holders[i].p != nil {
			t.Fatalf("holder %d was not cleared", i)
		}
		o := (*storeRefObj)(*slot)
		if w := weaks[i].Value(); w != o || o.id != i {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("%d of %d objects moved while the collector marked, in %d cycles, were collected", lost, n, cycles)
	}
	t.Logf("%d objects moved in %d cycles", n, cycles)
}
