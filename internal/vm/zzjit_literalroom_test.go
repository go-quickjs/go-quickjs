//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

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

func TestJITLiteralArrayRoom(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	cl := &closure{closureCode: &closureCode{arrays: []*arrayRoom{
		{pc: 3, room: arrayRoomLimit, acrossTurns: true}, {pc: 7, room: 4096, acrossTurns: true},
		{pc: 11, room: arrayRoomLimit + 1, acrossTurns: true}, {pc: 13, room: 4096},
	}}}
	for _, pc := range []int{5, 11, 13} {
		if s := jitLiteralArraySource[struct{}](r, cl, nil, pc, 0); s.room != 0 {
			t.Fatalf("literal at %d inherited unrelated or oversized room %d", pc, s.room)
		}
	}
	src := jitLiteralArraySource[struct{}](r, cl, nil, 7, 0)
	pool := &abi.ObjectPool{Source: unsafe.Pointer(src)}
	r.jitFillLiteral(&jitLiteral{array: true, pool: pool})
	if pool.Count != 1 || cap(src.sample.elemsOf()) != 4096 {
		t.Fatal("native pool did not inherit the tree tier's confirmed capacity")
	}
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
		if c.room >= 256 && pool.Size != 1 {
			t.Fatalf("large hint left a pool of %d buffers", pool.Size)
		}
		for i := range int(pool.Count) {
			a := (*Object)(pool.Objects[i])
			if a == previous || a == nil || a.class != ClassArray || a.proto() != r.proto.array ||
				len(a.elemsOf()) != 0 || cap(a.elemsOf()) != max(c.room, len(arrayObject{}.inline)) {
				t.Fatalf("after length %d: object %d has wrong fresh-array layout", c.length, i)
			}
		}
		for i := int(pool.Count); i < len(pool.Objects); i++ {
			if pool.Objects[i] != nil {
				t.Fatalf("unused pool slot %d retains an object", i)
			}
		}
	}
	// Recording a tree site's PC occupies padding, so its size stays put.
	if unsafe.Sizeof(arrayRoom{}) != 40 || unsafe.Sizeof(*src) != 104 {
		t.Fatalf("feedback sizes changed: tree %d, native %d", unsafe.Sizeof(arrayRoom{}), unsafe.Sizeof(*src))
	}
	// A literal's explicit elements need room before any result grows.
	src = jitLiteralArraySource[struct{}](r, cl, nil, 5, 7)
	pool = &abi.ObjectPool{Source: unsafe.Pointer(src)}
	r.jitFillLiteral(&jitLiteral{array: true, pool: pool, n: 7})
	src.sample.extOf().elems = src.sample.elemsOf()[:7]
	pool.Count = 0
	r.jitRefillPool(pool)
	if a := (*Object)(pool.Objects[0]); len(a.elemsOf()) != 0 || cap(a.elemsOf()) != 8 || src.room != 0 {
		t.Fatal("a literal's explicit elements changed its learned room")
	}
}

func TestJITLiteralArrayRoomNative(t *testing.T) {
	jitNoInlineForTest(t, "fill")
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
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
			cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
			cl.jitRefused = true
			for range 4 {
				if _, err := r.Run(compileForTest(t, "fill(4096)")); err != nil {
					t.Fatal(err)
				}
			}
			if len(cl.arrays) != 1 || cl.arrays[0].room != 4096 {
				t.Fatal("tree tier did not confirm the grown length")
			}
			cl.jitRefused = false
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
				if l.array && s.room >= 256 && s.sample != nil && l.pool.Count == 0 {
					t.Fatal("native hint retained its completed array after the turn")
				}
			}
			if !inherited {
				t.Fatal("native literal did not inherit the tree tier's capacity")
			}
			for i := range e.literals {
				l := &e.literals[i]
				s := (*jitPoolSource)(l.pool.Source)
				if !l.array || s.room != 4096 {
					continue
				}
				r.jitFillLiteral(l)
				jitLiteralArraysLetGo[struct{}](r)
				if l.pool.Count != 1 || s.room != 4096 || s.sample == nil {
					t.Fatal("an unused native array was observed as a short result")
				}
			}
			// Refilling releases the old sample before it allocates. A
			// native caller's live arrays must remain independently rooted.
			stop := jitCollectingForTest(t)
			v, err := r.Run(compileForTest(t, `function pairs() {
				var sum = 0;
				for (var i = 0; i < 80; i++) {
					var x = fill(4096);
					x[0] = i;
					var y = fill(4096);
					y[0] = i + 1;
					sum += x[0] + y[0] + x[4095] + y[4095];
				}
				return sum;
			}
			pairs()`))
			stop()
			if err != nil || !v.IsNumber() || v.Number() != 661600 {
				t.Fatalf("collecting: got %v, %v; want 661600", v, err)
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

func TestJITLiteralArrayRoomInlined(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
	if _, err := r.Run(compileForTest(t, `
		function make() { return []; }
		function fill(n) {
			var a = make();
			for (var i = 0; i < n; i++) a.push(i);
			return a;
		}
	`)); err != nil {
		t.Fatal(err)
	}
	maker := r.global.getOwn(r.atoms.intern("make")).value().Object().fn().closure
	filler := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
	maker.jitRefused, filler.jitRefused = true, true
	call := compileForTest(t, "fill(4096)")
	for range 4 {
		if _, err := r.Run(call); err != nil {
			t.Fatal(err)
		}
	}
	if len(maker.arrays) != 1 || maker.arrays[0].room != 4096 || !maker.arrays[0].acrossTurns {
		t.Fatal("callee did not confirm its caller's grown array across turns")
	}
	maker.jitRefused, filler.jitRefused = false, false
	for range 8 {
		v, err := r.Run(call)
		if err != nil {
			t.Fatal(err)
		}
		els := v.Object().elemsOf()
		if len(els) != 4096 || els[0].Number() != 0 || els[4095].Number() != 4095 {
			t.Fatal("inlined literal changed its caller's result")
		}
	}
	e := jitNativeForTest(t, r, "fill")
	for _, l := range e.literals {
		if l.array && l.fn == maker.fn {
			src := (*jitPoolSource)(l.pool.Source)
			if src.room != 4096 || l.pool.Size != 1 || src.sample != nil && l.pool.Count == 0 {
				t.Fatal("inlined literal lost its callee's hint or retained a completed sample")
			}
			return
		}
	}
	t.Fatal("native caller did not inline its callee's literal")
}
