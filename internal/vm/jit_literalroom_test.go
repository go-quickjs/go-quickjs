//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

func TestJITLiteralArrayRoomWeakPools(t *testing.T) {
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{array: true, room: 4096})}
	s := &jitState{arrayRooms: []weak.Pointer[abi.ObjectPool]{weak.Make(pool)}}
	pool = nil
	runtime.GC()
	jitLiteralArraySweep[struct{}](s)
	if len(s.arrayRooms) != 0 {
		t.Fatal("the sample list retained a discarded native pool")
	}
}

// A native empty literal's pool inherits the room its interpreter's site
// confirmed at a turn's end, of the literal at its pc alone, within
// arrayRoomLimit; a site's room within a turn, or another literal's, it
// does not.
func TestJITLiteralArrayRoom(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	sites := []*arrayRoom{
		{pc: 3, room: arrayRoomLimit, confirmed: arrayRoomLimit}, {pc: 7, room: 4096, confirmed: 4096},
		{pc: 11, room: arrayRoomLimit + 1, confirmed: arrayRoomLimit + 1}, {pc: 13, room: 4096},
	}
	cl := &closure{closureCode: &closureCode{arrays: &sites}}
	for _, pc := range []int{5, 11, 13} {
		if s := jitLiteralArraySource(r, cl, nil, pc, 0); s.room != 0 {
			t.Fatalf("literal at %d inherited unrelated or oversized room %d", pc, s.room)
		}
	}
	src := jitLiteralArraySource(r, cl, nil, 7, 0)
	pool := &abi.ObjectPool{Source: unsafe.Pointer(src)}
	r.jitFillLiteral(&jitLiteral{array: true, pool: pool})
	if pool.Count != 1 || cap(src.sample.elemsOf()) != 4096 {
		t.Fatal("native pool did not inherit the interpreter's confirmed capacity")
	}
	// Shorter results shrink it; one past the limit, or short, is a small
	// literal's room.
	for _, c := range []struct{ length, room int }{
		{4096, 4096}, {512, 512}, {4096, 512}, {0, 0},
		{4096, 64}, {4096, 64}, {arrayRoomLimit + 1, 64},
	} {
		previous := src.sample
		previous.extOf().elems = make([]Value, c.length)
		pool.Count = 0
		if !r.jitRefillPool(pool) {
			t.Fatal("refill left an empty pool")
		}
		if src.room != c.room {
			t.Fatalf("after length %d: room %d, want %d", c.length, src.room, c.room)
		}
		if c.room >= arrayRoomLeast && pool.Size != 1 {
			t.Fatalf("large hint left a pool of %d buffers", pool.Size)
		}
		for i := range int(pool.Count) {
			a := (*Object)(pool.Objects[i])
			if a == previous || a == nil || a.class != ClassArray || a.proto() != r.proto.array ||
				len(a.elemsOf()) != 0 || cap(a.elemsOf()) != max(c.room, len(arrayObject{}.inline)) {
				t.Fatalf("after length %d: object %d has wrong fresh-array layout", c.length, i)
			}
		}
	}
	// The confirmed room takes a word more.
	if unsafe.Sizeof(arrayRoom{}) != 48 {
		t.Fatalf("arrayRoom is %d bytes", unsafe.Sizeof(arrayRoom{}))
	}
}

// An empty literal of register code that the interpreter ran for a few
// turns, its arrays growing to 4,096 elements, keeps that room once its
// function runs natively: its pool's arrays are made with it, one at a
// time, the answers the same at any length after, the collector idle and
// marking.
func TestJITLiteralArrayRoomNative(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if !jitSSABackend {
				t.Skip("no SSA backend on this architecture")
			}
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			jitDefaultsForTest(r)
			if _, err := r.Run(compileForTest(t, `
				function fill(n) {
					var a = [];
					for (var i = 0; i < n; i++) a.push(i);
					return a;
				}
				function render(n) {
					var a = fill(n);
					return a.length + ':' + a[0] + ':' + a[n - 1];
				}
			`)); err != nil {
				t.Fatal(err)
			}
			// The interpreter's turns alone first.
			cl := jitGlobalClosureForTest(r, "fill")
			cl.closureCode.jitRegRefused = true
			for range 4 {
				if _, err := r.Run(compileForTest(t, "fill(4096)")); err != nil {
					t.Fatal(err)
				}
			}
			if as := cl.arraySites(); len(as) != 1 || as[0].room != 4096 || as[0].confirmed != 4096 {
				t.Fatalf("the interpreter did not confirm the grown length: %+v", as)
			}
			cl.closureCode.jitRegRefused = false
			for range 8 {
				if _, err := r.Run(compileForTest(t, "render(4096)")); err != nil {
					t.Fatal(err)
				}
			}
			e := jitNativeForTest(t, r, "fill")
			inherited := false
			for _, l := range e.literals {
				s := (*jitPoolSource)(l.pool.Source)
				inherited = inherited || l.array && s.room == 4096 && l.pool.Size == 1
			}
			if !inherited {
				t.Fatal("native literal did not inherit the interpreter's capacity")
			}
			for _, n := range []int{1024, 1024, 3, 1024, 0, 2048, 2048, arrayRoomLimit + 1} {
				want := fmt.Sprintf("%d:0:%d", n, n-1)
				if n == 0 {
					want = "0:undefined:undefined"
				}
				v, err := r.Run(compileForTest(t, fmt.Sprintf("render(%d)", n)))
				if err != nil || !v.IsString() || v.String().Go() != want {
					t.Fatalf("length %d: got %v, %v; want %s", n, v, err, want)
				}
			}
		})
	}
}
