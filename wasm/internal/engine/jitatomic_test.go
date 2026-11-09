package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// atomicOps are the atomic read-modify-writes of each width and type.
var atomicOps = func() (ops []string) {
	for _, t := range []string{"i32", "i64"} {
		widths := []string{"", "8", "16"}
		if t == "i64" {
			widths = append(widths, "32")
		}
		for _, w := range widths {
			for _, k := range []string{"add", "sub", "and", "or", "xor", "xchg", "cmpxchg"} {
				if w == "" {
					ops = append(ops, t+".atomic.rmw."+k)
				} else {
					ops = append(ops, t+".atomic.rmw"+w+"."+k+"_u")
				}
			}
		}
	}
	return ops
}()

// atomicLoadsStores are the atomic loads and stores, each load followed by
// the store of its width.
var atomicLoadsStores = []string{
	"i32.atomic.load", "i32.atomic.store", "i32.atomic.load8_u", "i32.atomic.store8",
	"i32.atomic.load16_u", "i32.atomic.store16", "i64.atomic.load", "i64.atomic.store",
	"i64.atomic.load8_u", "i64.atomic.store8", "i64.atomic.load16_u", "i64.atomic.store16",
	"i64.atomic.load32_u", "i64.atomic.store32",
}

// atomicModule is a module of a function for each atomic instruction on
// memory mem, declared by mems, whose addresses are of type idx; "init"
// stores an i64 plainly.
func atomicModule(mems, idx string, mem int) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "(module %s\n", mems)
	fmt.Fprintf(&b, "  (func (export \"init\") (param %s i64) (i64.store %d (local.get 0) (local.get 1)))\n", idx, mem)
	for _, op := range atomicOps {
		t := op[:3]
		if strings.Contains(op, "cmpxchg") {
			fmt.Fprintf(&b, "  (func (export %q) (param %s %s %s) (result %s) (%s %d (local.get 0) (local.get 1) (local.get 2)))\n", op, idx, t, t, t, op, mem)
		} else {
			fmt.Fprintf(&b, "  (func (export %q) (param %s %s) (result %s) (%s %d (local.get 0) (local.get 1)))\n", op, idx, t, t, op, mem)
		}
	}
	for _, op := range atomicLoadsStores {
		t := op[:3]
		if strings.Contains(op, "load") {
			fmt.Fprintf(&b, "  (func (export %q) (param %s) (result %s) (%s %d (local.get 0)))\n", op, idx, t, op, mem)
		} else {
			fmt.Fprintf(&b, "  (func (export %q) (param %s %s) (%s %d (local.get 0) (local.get 1)))\n", op, idx, t, op, mem)
		}
	}
	b.WriteString(")")
	return b.String()
}

// atomicTestWidth is the access width of an atomic instruction's name.
func atomicTestWidth(op string) uint64 {
	rest := op[3:]
	switch {
	case strings.Contains(rest, "8"):
		return 1
	case strings.Contains(rest, "16"):
		return 2
	case strings.Contains(rest, "32") || strings.HasPrefix(op, "i32"):
		return 4
	}
	return 8
}

// Every atomic load, store and read-modify-write computes what the
// interpreter does -- the old value zero-extended from its width, the
// memory's bytes after -- on an i32 shared memory 0 and an unshared i64
// memory 1, with operands whose bits above the width are set, and a
// compare-and-exchange's expected value equal to memory's in the width's
// bits and not.
func TestJITAtomicsMatchInterpreter(t *testing.T) {
	const pattern = 0xF0E1D2C3B4A59687
	for _, tc := range []struct{ name, mems, idx string }{
		{"shared i32 memory 0", "(memory 1 1 shared)", "i32"},
		{"i64 memory 1", "(memory 1) (memory i64 1 1)", "i64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := 0
			if tc.idx == "i64" {
				mem = 1
			}
			var calls []jitCall
			init := func(at uint64) {
				calls = append(calls, jitCall{name: "init", args: []uint64{at &^ 7, pattern}})
			}
			for _, op := range atomicOps {
				width := atomicTestWidth(op)
				for _, at := range []uint64{64, 68, 70, 71, 65528, 65534, 65535} {
					if at%width != 0 || at+width > 65536 {
						continue
					}
					for _, v := range []uint64{0xFFFFFFFFFFFFFF85, 0x123456789ABCDEF0, 1} {
						if !strings.Contains(op, "cmpxchg") {
							init(at)
							calls = append(calls, jitCall{name: op, args: []uint64{at, v}})
							continue
						}
						old := uint64(pattern) >> (8 * (at % 8))
						high := ^uint64(0) << (8 * width % 64)
						if width == 8 {
							high = 0
						}
						for _, exp := range []uint64{old, old | high, old &^ high, old ^ 1} {
							init(at)
							calls = append(calls, jitCall{name: op, args: []uint64{at, exp, v}})
						}
					}
				}
			}
			for i := 0; i < len(atomicLoadsStores); i += 2 {
				l, s := atomicLoadsStores[i], atomicLoadsStores[i+1]
				for _, at := range []uint64{64, 70, 71, 65528} {
					if at%atomicTestWidth(l) != 0 {
						continue
					}
					init(at)
					calls = append(calls, jitCall{name: l, args: []uint64{at}},
						jitCall{name: s, args: []uint64{at, 0xFFFFFFFFFFFFFF85}},
						jitCall{name: l, args: []uint64{at}})
				}
			}
			sameAsInterpreter(t, atomicModule(tc.mems, tc.idx, mem), calls)
		})
	}
}

// An atomic access to an address that is not a multiple of its width
// traps before its bounds are checked; an i64 memory's address and offset
// whose sum carries trap out of bounds first. wait and notify, which exit
// to Go, check the same, and a wait on an unshared memory traps.
func TestJITAtomicTraps(t *testing.T) {
	const src = `(module
  (memory 1 1 shared)
  (memory $m64 i64 1 1)
  (memory $own 1)
  (func (export "ld32") (param i32) (result i32) (i32.atomic.load offset=2 (local.get 0)))
  (func (export "add16") (param i32) (result i32) (i32.atomic.rmw16.add_u (local.get 0) (i32.const 1)))
  (func (export "add8") (param i32) (result i32) (i32.atomic.rmw8.add_u offset=65535 (local.get 0) (i32.const 1)))
  (func (export "st64") (param i64) (i64.atomic.store $m64 offset=8 (local.get 0) (i64.const -1)))
  (func (export "cmpxchg64") (param i64) (result i64)
    (i64.atomic.rmw.cmpxchg $m64 offset=0xFFFFFFFFFFFFFFF8 (local.get 0) (i64.const 0) (i64.const 1)))
  (func (export "xor32own") (param i32) (result i32) (i32.atomic.rmw.xor $own offset=1 (local.get 0) (i32.const 1)))
  (func (export "constUnaligned") (result i32) (i32.atomic.rmw.or (i32.const 65535) (i32.const 1)))
  (func (export "constUnaligned64") (result i64) (i64.atomic.load $m64 offset=4 (i64.const 0)))
  (func (export "constPast") (result i64) (i64.atomic.load $m64 offset=8 (i64.const 65528)))
  (func (export "constWrap") (result i64) (i64.atomic.load $m64 offset=9 (i64.const -1)))
  (func (export "notify") (param i32) (result i32) (memory.atomic.notify offset=1 (local.get 0) (i32.const 1)))
  (func (export "notify64") (param i64) (result i32) (memory.atomic.notify $m64 offset=4 (local.get 0) (i32.const 1)))
  (func (export "wait32") (param i32 i32) (result i32) (memory.atomic.wait32 (local.get 0) (local.get 1) (i64.const 0)))
  (func (export "wait64own") (param i32) (result i32) (memory.atomic.wait64 $own (local.get 0) (i64.const 0) (i64.const 0)))
  (func (export "fence") (result i32) (atomic.fence) (i32.const 3)))`
	ua, oob := TrapUnalignedAccess, TrapMemOutOfBounds
	sameAsInterpreter(t, src, []jitCall{
		{name: "ld32", args: []uint64{2}, want: u64p(0)},
		{name: "ld32", args: []uint64{1}, trap: ua},
		{name: "ld32", args: []uint64{3}, trap: ua},
		{name: "ld32", args: []uint64{65530}, want: u64p(0)},
		{name: "ld32", args: []uint64{65534}, trap: oob},
		{name: "ld32", args: []uint64{65535}, trap: ua},
		{name: "ld32", args: []uint64{math.MaxUint32}, trap: ua},
		{name: "add16", args: []uint64{65534}, want: u64p(0)},
		{name: "add16", args: []uint64{65533}, trap: ua},
		{name: "add16", args: []uint64{65536}, trap: oob},
		{name: "add8", args: []uint64{0}, want: u64p(0)},
		{name: "add8", args: []uint64{1}, trap: oob},
		{name: "st64", args: []uint64{65520}},
		{name: "st64", args: []uint64{65521}, trap: ua},
		{name: "st64", args: []uint64{65528}, trap: oob},
		{name: "st64", args: []uint64{math.MaxUint64 - 7}, trap: oob},
		{name: "st64", args: []uint64{math.MaxUint64}, trap: oob},
		{name: "st64", args: []uint64{math.MaxUint64 - 8}, trap: ua},
		{name: "cmpxchg64", args: []uint64{8}, trap: oob},
		{name: "cmpxchg64", args: []uint64{9}, trap: oob},
		{name: "cmpxchg64", args: []uint64{1}, trap: ua},
		{name: "cmpxchg64", args: []uint64{0}, trap: oob},
		{name: "xor32own", args: []uint64{3}, want: u64p(0)},
		{name: "xor32own", args: []uint64{4}, trap: ua},
		{name: "xor32own", args: []uint64{65535}, trap: oob},
		{name: "constUnaligned", trap: ua},
		{name: "constUnaligned64", trap: ua},
		{name: "constPast", trap: oob},
		{name: "constWrap", trap: oob},
		{name: "notify", args: []uint64{3}, want: u64p(0)},
		{name: "notify", args: []uint64{4}, trap: ua},
		{name: "notify", args: []uint64{65535}, trap: oob},
		{name: "notify64", args: []uint64{math.MaxUint64 - 3}, trap: oob},
		{name: "notify64", args: []uint64{1}, trap: ua},
		{name: "notify64", args: []uint64{4}, want: u64p(0)},
		{name: "wait32", args: []uint64{0, 1}, want: u64p(1)},
		{name: "wait32", args: []uint64{0, 0}, want: u64p(2)},
		{name: "wait32", args: []uint64{2, 0}, trap: ua},
		{name: "wait64own", args: []uint64{0}, trap: TrapAtomicsWaitNotAllowed},
		{name: "fence", want: u64p(3)},
	})
}

// atomicWorkSrc is a module importing a shared memory -- as memory 0, or,
// after an unshared one, as memory 1 of i64 addresses -- whose "work" runs
// n turns of atomic updates of the shared memory, as worker w of several:
// counters taken up by additions, a subtraction and a loop of
// compare-and-exchange, a plain counter under a lock taken by exchange and
// released by an atomic store, a bit of w toggled by xor, then set by or
// and cleared by and.
func atomicWorkSrc(mem1 bool) string {
	imports, m, idx := `(import "env" "mem" (memory 1 4 shared))`, "0", "i32"
	if mem1 {
		imports, m, idx = `(import "env" "pad" (memory 1)) (import "env" "mem" (memory i64 1 4 shared))`, "1", "i64"
	}
	a := func(n int) string { return fmt.Sprintf("(%s.const %d)", idx, n) }
	return fmt.Sprintf(`(module
  %s
  (func (export "work") (param $w i32) (param $n i32)
    (local $i i32) (local $old i32) (local $r i32)
    (loop $l
      (drop (i32.atomic.rmw.add %[2]s %[3]s (i32.const 1)))
      (drop (i64.atomic.rmw.sub %[2]s %[4]s (i64.const 1)))
      (drop (i32.atomic.rmw8.add_u %[2]s %[5]s (i32.const 1)))
      (drop (i32.atomic.rmw16.add_u %[2]s %[6]s (i32.const 1)))
      (drop (i64.atomic.rmw32.add_u %[2]s %[7]s (i64.const 1)))
      (local.set $old (i32.atomic.load %[2]s %[8]s))
      (block $done
        (loop $cas
          (local.set $r (i32.atomic.rmw.cmpxchg %[2]s %[8]s (local.get $old) (i32.add (local.get $old) (i32.const 1))))
          (br_if $done (i32.eq (local.get $r) (local.get $old)))
          (local.set $old (local.get $r))
          (br $cas)))
      (loop $spin (br_if $spin (i32.atomic.rmw.xchg %[2]s %[9]s (i32.const 1))))
      (i64.store %[2]s %[10]s (i64.add (i64.load %[2]s %[10]s) (i64.const 1)))
      (i32.atomic.store %[2]s %[9]s (i32.const 0))
      (drop (i32.atomic.rmw.xor %[2]s %[11]s (i32.shl (i32.const 1) (local.get $w))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (drop (i32.atomic.rmw.or %[2]s %[12]s (i32.shl (i32.const 1) (local.get $w))))
    (drop (i32.atomic.rmw.and %[2]s %[13]s (i32.xor (i32.const -1) (i32.shl (i32.const 1) (local.get $w))))))
  ;; Waits for the flag at 64, then adds to the word at 65536+8, which a
  ;; grow by another thread made, without an exit between the two.
  (func (export "afterGrow") (result i32)
    (loop $spin (br_if $spin (i32.eqz (i32.atomic.load %[2]s %[14]s))))
    (i32.atomic.rmw.add %[2]s %[15]s (i32.const 5)))
  ;; The word past the old end is read before the flag lets the waiter
  ;; add to it.
  (func (export "grow") (result i32)
    (drop (memory.grow %[2]s %[16]s))
    (i32.atomic.load %[2]s %[15]s)
    (i32.atomic.store %[2]s %[14]s (i32.const 1))))`, imports, m, a(0), a(8), a(17), a(18), a(20), a(24), a(32), a(40), a(48), a(52), a(56), a(64), a(65536+8), a(1))
}

// Goroutines running native code on one shared memory, each in an instance
// of its own, update it atomically: every counter is the sum of their
// turns, the bytes beside the narrow ones untouched; and a memory grown by
// one is accessed at its new size by another that saw the grow only
// through an atomic flag.
func TestJITAtomicsConcurrent(t *testing.T) {
	for _, mem1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory1=%v", mem1), func(t *testing.T) {
			src := atomicWorkSrc(mem1)
			shared := NewMemory(1, 4, true, mem1, true)
			imports := []Extern{{Kind: module.ExternMemory, Memory: shared}}
			if mem1 {
				imports = append([]Extern{{Kind: module.ExternMemory, Memory: NewMemory(1, 1, true, false, false)}}, imports...)
			}
			first := nativeInstance(t, src, imports...)
			b := shared.Bytes()
			binary.LittleEndian.PutUint32(b[56:], math.MaxUint32)
			const workers, turns = 4, 200_001
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var wg sync.WaitGroup
			errs := make(chan error, workers)
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					inst, err := Instantiate(ctx, first.Code, imports)
					if err != nil {
						errs <- err
						return
					}
					ext, _ := inst.Export("work")
					if _, err := Invoke(ctx, ext.Func, []Val{{N: uint64(w)}, {N: turns}}); err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			if !nativeRan(first, "work") {
				t.Fatal("work has no native code")
			}
			b = shared.Bytes()
			total := uint64(workers * turns)
			mask := uint32(1)<<workers - 1
			xor := uint32(0)
			if turns%2 == 1 {
				xor = mask
			}
			for _, c := range []struct {
				name      string
				got, want uint64
			}{
				{"rmw.add", uint64(binary.LittleEndian.Uint32(b[0:])), total},
				{"rmw.sub", binary.LittleEndian.Uint64(b[8:]), -total},
				{"byte before rmw8", uint64(b[16]), 0},
				{"rmw8.add_u", uint64(b[17]), total % 256},
				{"rmw16.add_u", uint64(binary.LittleEndian.Uint16(b[18:])), total % 65536},
				{"rmw32.add_u", uint64(binary.LittleEndian.Uint32(b[20:])), total},
				{"cmpxchg", uint64(binary.LittleEndian.Uint32(b[24:])), total},
				{"lock", uint64(binary.LittleEndian.Uint32(b[32:])), 0},
				{"locked counter", binary.LittleEndian.Uint64(b[40:]), total},
				{"xor", uint64(binary.LittleEndian.Uint32(b[48:])), uint64(xor)},
				{"or", uint64(binary.LittleEndian.Uint32(b[52:])), uint64(mask)},
				{"and", uint64(binary.LittleEndian.Uint32(b[56:])), uint64(^mask)},
			} {
				if c.got != c.want {
					t.Errorf("%s: %#x, want %#x", c.name, c.got, c.want)
				}
			}

			// One goroutine waits natively for the flag, then adds past the
			// memory's old end; the other grows the memory and sets the flag.
			waiter, err := Instantiate(ctx, first.Code, imports)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				ext, _ := waiter.Export("afterGrow")
				res, err := Invoke(ctx, ext.Func, nil)
				if err == nil && res[0].N != 0 {
					err = fmt.Errorf("afterGrow = %d, want 0", res[0].N)
				}
				done <- err
			}()
			time.Sleep(10 * time.Millisecond)
			res, err := call(t, first, "grow")
			if err != nil || res[0].N != 0 {
				t.Fatalf("grow = %v, %v", res, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := binary.LittleEndian.Uint32(shared.Bytes()[65536+8:]); got != 5 {
				t.Fatalf("the word past the old end is %d, want 5", got)
			}
		})
	}
}
