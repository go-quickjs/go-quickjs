//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64) || (linux && arm64))

package jit

import (
	"encoding/binary"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

// storeCode is code that stores v at its context's address and returns,
// padded with no-ops to at least n bytes.
func storeCode(v uint32, n int) []byte {
	var code []byte
	if runtime.GOARCH == "arm64" {
		for len(code)+12 < n {
			code = binary.LittleEndian.AppendUint32(code, 0xD503201F) // nop
		}
		// movz x1, #v; str x1, [x0]; ret
		for _, w := range []uint32{0xD2800001 | (v&0xffff)<<5, 0xF9000001, 0xD65F03C0} {
			code = binary.LittleEndian.AppendUint32(code, w)
		}
		return code
	}
	for len(code)+8 < n {
		code = append(code, 0x90) // nop
	}
	// mov qword [rdi], v; ret
	code = append(code, 0x48, 0xC7, 0x07)
	code = binary.LittleEndian.AppendUint32(code, v)
	return append(code, 0xC3)
}

// Code placed apart starts on a page no code placed before it shares, and
// is placed while the arena's other code runs on other goroutines without
// stopping it: were a page that code is in made writable meanwhile, the
// goroutines running it would fault.
func TestPlaceRawApart(t *testing.T) {
	if !RawSupported() {
		t.Skip("no native code")
	}
	a := NewArena()
	page := uintptr(os.Getpagesize())
	first, err := PlaceRaw(a, storeCode(1, 100))
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var runs atomic.Int64
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ctx uint64
			for !stop.Load() {
				first.Enter(unsafe.Pointer(&ctx))
				if ctx != 1 {
					panic("the first code stored something else")
				}
				runs.Add(1)
			}
		}()
	}
	placed := []*RawCode{first}
	// shares reports whether code placed before shares the page addr is on.
	shares := func(addr uintptr) bool {
		from, to := addr/page*page, addr/page*page+page
		for _, r := range placed {
			if r.Base() < to && r.Base()+uintptr(r.Size()) > from {
				return true
			}
		}
		return false
	}
	for round := range 200 {
		v := uint32(round*3 + 2)
		raws, err := PlaceRawApart(a, storeCode(v, 40), storeCode(v+1, 5000), storeCode(v+2, 70))
		if err != nil {
			t.Fatal(err)
		}
		if b := raws[0].Base(); shares(b) {
			t.Fatalf("round %d: the batch starts at %#x, on a page code placed before it is on", round, b)
		}
		for i, r := range raws {
			var ctx uint64
			r.Enter(unsafe.Pointer(&ctx))
			if ctx != uint64(v)+uint64(i) {
				t.Fatalf("round %d: code %d stored %d", round, i, ctx)
			}
		}
		placed = append(placed, raws...)
		// Code placed the usual way packs after it.
		r, err := PlaceRaw(a, storeCode(7, 30))
		if err != nil {
			t.Fatal(err)
		}
		placed = append(placed, r)
	}
	stop.Store(true)
	wg.Wait()
	if runs.Load() == 0 {
		t.Fatal("no goroutine ran the first code")
	}
	for _, r := range placed {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if mapped, live := a.Mapped(); mapped != 0 || live != 0 {
		t.Fatalf("after every release the arena maps %d bytes, %d live", mapped, live)
	}
}
